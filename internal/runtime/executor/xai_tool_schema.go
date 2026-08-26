package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// xAI rejects function tools whose parameter root is not an object schema:
// a missing or scalar root type, or a root anyOf/oneOf/allOf union with a
// branch that is not (or does not resolve to) an object. This file
// normalizes every function tool schema before it is sent upstream.
const xaiWrappedArgumentsKey = "input"

type xaiParametersMode int

const (
	xaiParametersUnchanged xaiParametersMode = iota
	// xaiParametersTyped adds "type":"object" to the root and to untyped
	// inline union branches, preserving the original constraints.
	xaiParametersTyped
	// xaiParametersMerged flattens a root union whose branches all resolve to
	// objects into one object with the union of their properties and no
	// required list.
	xaiParametersMerged
	// xaiParametersWrapped nests the original schema under a single required
	// "input" property; tool call arguments are unwrapped on the way back.
	xaiParametersWrapped
)

var xaiRootUnionKeys = []string{"anyOf", "oneOf", "allOf"}

// normalizeXAIFunctionParameters returns a parameter schema whose root is an
// object type that xAI accepts. The bool result is false only when the input
// is not valid JSON.
func normalizeXAIFunctionParameters(raw []byte) ([]byte, xaiParametersMode, bool) {
	if len(raw) == 0 {
		return []byte(`{"type":"object","properties":{}}`), xaiParametersTyped, true
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, xaiParametersUnchanged, false
	}
	root, isObject := value.(map[string]any)
	if !isObject {
		return xaiWrapParameters(value), xaiParametersWrapped, true
	}

	rootType, hasType := root["type"]
	rootIsObject := xaiSchemaTypeValueIsObjectOnly(rootType)
	if hasType && !rootIsObject {
		return xaiWrapParameters(root), xaiParametersWrapped, true
	}

	var branches []any
	for _, key := range xaiRootUnionKeys {
		if list, ok := root[key].([]any); ok {
			branches = append(branches, list...)
		}
	}
	if len(branches) == 0 {
		if rootIsObject {
			return raw, xaiParametersUnchanged, true
		}
		if _, hasProps := root["properties"]; hasProps || len(root) == 0 {
			return xaiMarshalSchema(xaiWithObjectType(root)), xaiParametersTyped, true
		}
		return xaiWrapParameters(root), xaiParametersWrapped, true
	}

	needsMerge := false
	resolved := make([]map[string]any, 0, len(branches))
	for _, branch := range branches {
		schema, isRef, ok := xaiResolveObjectBranch(root, branch)
		if !ok {
			return xaiWrapParameters(root), xaiParametersWrapped, true
		}
		if isRef {
			needsMerge = true
		}
		resolved = append(resolved, schema)
	}
	if !needsMerge {
		typed := xaiWithObjectType(root)
		for _, key := range xaiRootUnionKeys {
			list, ok := typed[key].([]any)
			if !ok {
				continue
			}
			for index, branch := range list {
				if schema, ok := branch.(map[string]any); ok {
					list[index] = xaiWithObjectType(schema)
				}
			}
		}
		return xaiMarshalSchema(typed), xaiParametersTyped, true
	}

	merged := make(map[string]any, len(root))
	for key, val := range root {
		merged[key] = val
	}
	for _, key := range xaiRootUnionKeys {
		delete(merged, key)
	}
	delete(merged, "required")
	if additional, ok := merged["additionalProperties"].(bool); ok && !additional {
		delete(merged, "additionalProperties")
	}
	properties := make(map[string]any)
	if rootProps, ok := root["properties"].(map[string]any); ok {
		for name, prop := range rootProps {
			properties[name] = prop
		}
	}
	for _, schema := range resolved {
		branchProps, ok := schema["properties"].(map[string]any)
		if !ok {
			continue
		}
		for name, prop := range branchProps {
			if _, exists := properties[name]; !exists {
				properties[name] = prop
			}
		}
	}
	merged["type"] = "object"
	merged["properties"] = properties
	return xaiMarshalSchema(merged), xaiParametersMerged, true
}

func xaiWrapParameters(original any) []byte {
	return xaiMarshalSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{xaiWrappedArgumentsKey: original},
		"required":   []any{xaiWrappedArgumentsKey},
	})
}

func xaiWithObjectType(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema)+1)
	for key, val := range schema {
		out[key] = val
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	if _, ok := out["properties"]; !ok && len(out) == 1 {
		out["properties"] = map[string]any{}
	}
	return out
}

func xaiMarshalSchema(schema map[string]any) []byte {
	out, err := json.Marshal(schema)
	if err != nil {
		return []byte(`{"type":"object","properties":{}}`)
	}
	return out
}

// xaiResolveObjectBranch follows local $ref chains and reports whether the
// branch is an object schema. isRef is true when the branch had to be
// dereferenced, which means its constraints cannot stay inline at the root.
func xaiResolveObjectBranch(root map[string]any, branch any) (schema map[string]any, isRef bool, ok bool) {
	current, isMap := branch.(map[string]any)
	if !isMap {
		return nil, false, false
	}
	for hop := 0; hop < 8; hop++ {
		ref, hasRef := current["$ref"].(string)
		if !hasRef {
			break
		}
		isRef = true
		next, found := xaiLookupLocalRef(root, ref)
		if !found {
			return nil, true, false
		}
		current = next
	}
	if _, stillRef := current["$ref"]; stillRef {
		return nil, true, false
	}
	branchType, hasType := current["type"]
	if hasType && !xaiSchemaTypeValueIsObjectOnly(branchType) {
		return nil, isRef, false
	}
	if !hasType {
		for _, key := range []string{"const", "enum", "items"} {
			if _, has := current[key]; has {
				return nil, isRef, false
			}
		}
	}
	return current, isRef, true
}

func xaiLookupLocalRef(root map[string]any, ref string) (map[string]any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var node any = root
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		object, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	schema, ok := node.(map[string]any)
	return schema, ok
}

func xaiSchemaTypeValueIsObjectOnly(schemaType any) bool {
	switch typed := schemaType.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "object")
	case []any:
		if len(typed) == 0 {
			return false
		}
		for _, item := range typed {
			name, ok := item.(string)
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "object") {
				return false
			}
		}
		return true
	}
	return false
}

// unwrapXAIWrappedToolCallArguments restores the caller's argument shape for
// tools whose schema was wrapped under "input" by normalizeXAIFunctionParameters.
// It covers output items in response.output_item.* events and the terminal
// response.* payloads; argument delta events stay in wrapped form.
func unwrapXAIWrappedToolCallArguments(data []byte, wrapped map[string]struct{}) []byte {
	if len(wrapped) == 0 || len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	data = unwrapXAIWrappedToolCallAtPath(data, "item", wrapped)
	output := gjson.GetBytes(data, "response.output")
	if output.Exists() && output.IsArray() {
		for index := range output.Array() {
			data = unwrapXAIWrappedToolCallAtPath(data, fmt.Sprintf("response.output.%d", index), wrapped)
		}
	}
	return data
}

func unwrapXAIWrappedToolCallAtPath(data []byte, path string, wrapped map[string]struct{}) []byte {
	if gjson.GetBytes(data, path+".type").String() != "function_call" {
		return data
	}
	name := strings.TrimSpace(gjson.GetBytes(data, path+".name").String())
	if _, ok := wrapped[name]; !ok {
		return data
	}
	arguments := gjson.GetBytes(data, path+".arguments")
	if arguments.Type != gjson.String {
		return data
	}
	parsed := gjson.Parse(arguments.String())
	if !parsed.IsObject() {
		return data
	}
	inner := parsed.Get(xaiWrappedArgumentsKey)
	if !inner.Exists() {
		return data
	}
	updated, err := sjson.SetBytes(data, path+".arguments", inner.Raw)
	if err != nil {
		return data
	}
	return updated
}
