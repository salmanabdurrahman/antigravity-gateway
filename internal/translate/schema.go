// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

var (
	nameMaps = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	subschemas = []string{
		"additionalProperties",
		"unevaluatedProperties",
		"propertyNames",
		"items",
		"prefixItems",
		"additionalItems",
		"unevaluatedItems",
		"contains",
		"allOf",
		"anyOf",
		"oneOf",
		"not",
		"if",
		"then",
		"else",
	}

	claudeDrop = []string{
		"$schema",
		"$id",
		"$ref",
		"$defs",
		"definitions",
		"$comment",
		"if",
		"then",
		"else",
		"not",
		"patternProperties",
		"unevaluatedProperties",
		"unevaluatedItems",
		"contentEncoding",
		"contentMediaType",
		"exclusiveMinimum",
		"exclusiveMaximum",
		"propertyNames",
		"uniqueItems",
		"deprecated",
	}

	geminiLegacyDrop = append([]string{
		"const",
		"examples",
		"minimum",
		"maximum",
		"multipleOf",
		"minLength",
		"maxLength",
		"pattern",
		"minItems",
		"maxItems",
		"minProperties",
		"maxProperties",
		"title",
		"default",
		"additionalProperties",
	}, claudeDrop...)
)

// isNameMap reports whether key identifies a JSON schema map of named subschemas (e.g. "properties").
func isNameMap(key string) bool {
	return slices.Contains(nameMaps, key)
}

// isSubschema reports whether key identifies a singular subschema or direct schema array (e.g. "items", "allOf").
func isSubschema(key string) bool {
	return slices.Contains(subschemas, key)
}

// isClaudeDrop reports whether key must be omitted when translating schemas for Claude models.
func isClaudeDrop(key string) bool {
	return slices.Contains(claudeDrop, key)
}

// isGeminiLegacyDrop reports whether key must be omitted when falling back to Gemini legacy parameter formats.
func isGeminiLegacyDrop(key string) bool {
	return slices.Contains(geminiLegacyDrop, key)
}

// ForGeminiJSONSchema normalizes tool parameters into full JSON Schema for Gemini.
//
// Protocol Quirk: Gemini ignores "const" in parametersJsonSchema but honors a single-value "enum".
// Note that "$ref" and newer complex schema constructs are sanitized if upstream returns HTTP 400
// parametersJsonSchema rejection and falls back to ForGeminiLegacyParameters.
func ForGeminiJSONSchema(schema any) any {
	return forGeminiJSONSchemaInternal(schema, false)
}

// forGeminiJSONSchemaInternal recursively converts JSON Schema constructs into Gemini-compatible definitions,
// mapping single const values into enum arrays when appropriate.
func forGeminiJSONSchemaInternal(schema any, inNameMap bool) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = forGeminiJSONSchemaInternal(v, false)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(s))
		for k, v := range s {
			if inNameMap || isSubschema(k) {
				out[k] = forGeminiJSONSchemaInternal(v, false)
			} else if isNameMap(k) {
				out[k] = forGeminiJSONSchemaInternal(v, true)
			} else {
				out[k] = v
			}
		}

		// Convert const to single-value enum if enum is absent.
		if !inNameMap {
			if val, ok := out["const"]; ok && out["enum"] == nil {
				switch val.(type) {
				case string, float64, int, int64:
					out["enum"] = []any{val}
					delete(out, "const")
				}
			}
		}
		return out
	default:
		return schema
	}
}

// ForClaudeParameters cleans OpenAPI tool parameter schemas for Claude behind Cloud Code Assist.
// Claude receives tool definitions through the older "parameters" field and rejects newer JSON Schema
// keywords ($ref, $defs, allOf/anyOf unions, const, etc.).
func ForClaudeParameters(schema any) any {
	s, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	out := make(map[string]any)
	for k, v := range s {
		if isClaudeDrop(k) || strings.HasPrefix(k, "x-") {
			continue
		}
		if k == "const" {
			out["enum"] = []any{v}
		} else if (k == "anyOf" || k == "oneOf" || k == "allOf") && isSlice(v) {
			sliceVal := toSlice(v)
			if allPureConst(sliceVal) {
				enums := make([]any, len(sliceVal))
				allStrings := true
				for i, item := range sliceVal {
					c := item.(map[string]any)["const"]
					enums[i] = c
					if _, isStr := c.(string); !isStr {
						allStrings = false
					}
				}
				out["enum"] = enums
				if allStrings && out["type"] == nil {
					out["type"] = "string"
				}
			} else if hasMatchingTypeConst(sliceVal) {
				firstMap := sliceVal[0].(map[string]any)
				if out["type"] == nil {
					out["type"] = firstMap["type"]
				}
				enums := make([]any, len(sliceVal))
				for i, item := range sliceVal {
					enums[i] = item.(map[string]any)["const"]
				}
				out["enum"] = enums
			} else {
				mergeUnion(k, sliceVal, out, ForClaudeParameters)
			}
		} else if k == "type" && isSlice(v) {
			liftTypeArray(out, toSlice(v))
		} else if k == "properties" && isMap(v) {
			props := toMap(v)
			cleanProps := make(map[string]any, len(props))
			for pk, pv := range props {
				cleanProps[pk] = ForClaudeParameters(pv)
			}
			out["properties"] = cleanProps
		} else {
			if isMap(v) {
				out[k] = ForClaudeParameters(v)
			} else if k == "items" && isSlice(v) {
				itemsSlice := toSlice(v)
				cleanItems := make([]any, len(itemsSlice))
				for i, iv := range itemsSlice {
					cleanItems[i] = ForClaudeParameters(iv)
				}
				out[k] = cleanItems
			} else {
				out[k] = v
			}
		}
	}

	// Proto only accepts string enums.
	if enums, ok := out["enum"].([]any); ok {
		allStrings := true
		allBools := true
		for _, e := range enums {
			if _, isStr := e.(string); !isStr {
				allStrings = false
			}
			if _, isBool := e.(bool); !isBool {
				allBools = false
			}
		}
		if !allStrings {
			if out["type"] == nil && len(enums) > 0 && allBools {
				out["type"] = "boolean"
			}
			delete(out, "enum")
		}
	}

	return out
}

// ForGeminiLegacyParameters converts schemas for Gemini when upstream rejects parametersJsonSchema.
// It removes newer draft keywords and converts unions to standard OpenAPI 3.0 types.
func ForGeminiLegacyParameters(schema any) any {
	s, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	out := make(map[string]any)
	for k, v := range s {
		if isGeminiLegacyDrop(k) || strings.HasPrefix(k, "x-") {
			continue
		}
		if (k == "anyOf" || k == "oneOf" || k == "allOf") && isSlice(v) {
			sliceVal := toSlice(v)
			if allPureConst(sliceVal) {
				enums := make([]any, len(sliceVal))
				for i, item := range sliceVal {
					enums[i] = toString(item.(map[string]any)["const"])
				}
				out["enum"] = enums
				if out["type"] == nil {
					out["type"] = "string"
				}
			} else {
				mergeUnion(k, sliceVal, out, ForGeminiLegacyParameters)
			}
		} else if k == "type" && isSlice(v) {
			liftTypeArray(out, toSlice(v))
		} else if k == "properties" && isMap(v) {
			props := toMap(v)
			cleanProps := make(map[string]any, len(props))
			for pk, pv := range props {
				cleanProps[pk] = ForGeminiLegacyParameters(pv)
			}
			out["properties"] = cleanProps
		} else {
			if isMap(v) {
				out[k] = ForGeminiLegacyParameters(v)
			} else {
				out[k] = v
			}
		}
	}

	if enums, ok := out["enum"].([]any); ok {
		cleanEnums := make([]any, len(enums))
		for i, e := range enums {
			cleanEnums[i] = toString(e)
		}
		out["enum"] = cleanEnums
	}

	return out
}

// isSlice checks if the given interface value holds a generic slice ([]any).
func isSlice(v any) bool {
	_, ok := v.([]any)
	return ok
}

// toSlice safely casts the given interface value to []any, returning nil on mismatch.
func toSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// isMap checks if the given interface value holds a string-keyed map (map[string]any).
func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// toMap safely casts the given interface value to map[string]any, returning nil on mismatch.
func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// allPureConst checks if every element in slice is a single-entry map defining a "const" value.
func allPureConst(slice []any) bool {
	if len(slice) == 0 {
		return false
	}
	for _, item := range slice {
		m, ok := item.(map[string]any)
		if !ok || len(m) != 1 {
			return false
		}
		if _, ok := m["const"]; !ok {
			return false
		}
	}
	return true
}

// hasMatchingTypeConst checks if all slice items define the same type and carry a "const" value.
func hasMatchingTypeConst(slice []any) bool {
	if len(slice) == 0 {
		return false
	}
	var firstType string
	for i, item := range slice {
		m, ok := item.(map[string]any)
		if !ok || len(m) != 2 {
			return false
		}
		if _, hasConst := m["const"]; !hasConst {
			return false
		}
		tVal, hasType := m["type"].(string)
		if !hasType {
			return false
		}
		if i == 0 {
			firstType = tVal
		} else if tVal != firstType {
			return false
		}
	}
	return true
}

// liftTypeArray transforms JSON Schema type arrays (e.g. ["string", "null"]) into a single type and nullable flag.
func liftTypeArray(out map[string]any, types []any) {
	var nonNull []string
	hasNull := false
	for _, t := range types {
		if str, ok := t.(string); ok {
			if str == "null" {
				hasNull = true
			} else {
				nonNull = append(nonNull, str)
			}
		}
	}
	if hasNull {
		out["nullable"] = true
	}
	if len(nonNull) > 0 {
		out["type"] = nonNull[0]
	} else {
		out["type"] = "string"
	}
}

// mergeUnion collapses anyOf, oneOf, or allOf variants into a unified schema supported by legacy parsers.
func mergeUnion(key string, variants []any, out map[string]any, clean func(any) any) {
	var cleaned []map[string]any
	for _, v := range variants {
		c := clean(v)
		if m, ok := c.(map[string]any); ok && len(m) > 0 {
			cleaned = append(cleaned, m)
		}
	}
	if len(cleaned) == 0 {
		return
	}

	if key != "allOf" {
		nullIdx := -1
		for i, v := range cleaned {
			if v["type"] == "null" && len(v) == 1 {
				nullIdx = i
				break
			}
		}
		if nullIdx != -1 {
			var rest []map[string]any
			for i, v := range cleaned {
				if i != nullIdx {
					rest = append(rest, v)
				}
			}
			if len(rest) > 0 {
				maps.Copy(out, rest[0])
				out["nullable"] = true
				return
			}
		}
	}

	allObjects := true
	for _, v := range cleaned {
		if v["type"] != "object" || !isMap(v["properties"]) {
			allObjects = false
			break
		}
	}

	if key == "allOf" || allObjects {
		props := make(map[string]any)
		var requiredSets []map[string]bool
		for _, v := range cleaned {
			reqSet := make(map[string]bool)
			if reqSlice, ok := v["required"].([]any); ok {
				for _, r := range reqSlice {
					if rStr, ok := r.(string); ok {
						reqSet[rStr] = true
					}
				}
			}
			requiredSets = append(requiredSets, reqSet)
			if pMap, ok := v["properties"].(map[string]any); ok {
				for k, s := range pMap {
					if _, exists := props[k]; !exists {
						props[k] = s
					}
				}
			}
		}

		var required []string
		if key == "allOf" {
			seen := make(map[string]bool)
			for _, set := range requiredSets {
				for r := range set {
					if !seen[r] {
						seen[r] = true
						required = append(required, r)
					}
				}
			}
			slices.Sort(required)
		} else if len(requiredSets) > 0 {
			for r := range requiredSets[0] {
				allHave := true
				for _, set := range requiredSets[1:] {
					if !set[r] {
						allHave = false
						break
					}
				}
				if allHave {
					required = append(required, r)
				}
			}
			slices.Sort(required)
		}

		merged := make(map[string]any)
		if key == "allOf" {
			for _, v := range cleaned {
				maps.Copy(merged, v)
			}
		} else {
			maps.Copy(merged, cleaned[0])
		}

		if len(props) > 0 {
			merged["properties"] = props
		}
		if len(required) > 0 {
			reqAny := make([]any, len(required))
			for i, r := range required {
				reqAny[i] = r
			}
			merged["required"] = reqAny
		} else {
			delete(merged, "required")
		}

		maps.Copy(out, merged)
		return
	}

	// Complex unions do not survive trip to Claude; keep first variant.
	maps.Copy(out, cleaned[0])
}

// toString safely formats any scalar value as a string representation.
func toString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}
