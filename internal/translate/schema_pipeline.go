// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// AppendDescriptionHint appends a parenthetical hint to a schema description field.
// Format: "existing description (hint)" or just "hint" if description was empty.
func AppendDescriptionHint(description string, hint string) string {
	if hint == "" {
		return description
	}
	trimmed := strings.TrimSpace(description)
	if trimmed == "" {
		return hint
	}
	return fmt.Sprintf("%s (%s)", trimmed, hint)
}

// ScoreSchemaOption evaluates a subschema option to determine its specificity and richness.
// Higher scores indicate more informative schemas for union flattening (anyOf/oneOf).
//
// Scoring tiers:
//   - 3: Object with properties defined (most informative)
//   - 2: Array with items defined
//   - 1: Any non-null scalar type
//   - 0: Null or unspecified empty schema
func ScoreSchemaOption(schema any) int {
	m, ok := schema.(map[string]any)
	if !ok || len(m) == 0 {
		return 0
	}
	t, _ := m["type"].(string)
	if t == "object" || m["properties"] != nil {
		return 3
	}
	if t == "array" || m["items"] != nil {
		return 2
	}
	if t != "" && t != "null" {
		return 1
	}
	return 0
}

// schemaSubschemaKeywords group the JSON Schema keywords whose values hold nested subschemas.
// Cloud Code Assist validates the entire schema tree, so every normalization walker must recurse
// through these composition keywords instead of only properties/items.
var (
	// singleSubschemaKeywords map a keyword to exactly one nested subschema object or boolean.
	singleSubschemaKeywords = []string{
		"items", "additionalItems", "additionalProperties", "contains", "propertyNames",
		"not", "if", "then", "else", "unevaluatedItems", "unevaluatedProperties",
	}

	// listSubschemaKeywords map a keyword to an ordered array of nested subschemas.
	listSubschemaKeywords = []string{"anyOf", "oneOf", "allOf", "prefixItems"}

	// mapSubschemaKeywords map a keyword to a name-to-subschema object.
	mapSubschemaKeywords = []string{"dependentSchemas", "patternProperties"}
)

// walkPropertySchemas applies transform to every subschema of a properties object, preserving names.
func walkPropertySchemas(out map[string]any, transform func(any) any) {
	props, ok := out["properties"].(map[string]any)
	if !ok {
		return
	}
	next := make(map[string]any, len(props))
	for name, subschema := range props {
		next[name] = transform(subschema)
	}
	out["properties"] = next
}

// walkNestedSchemas applies transform to every subschema reachable through the JSON Schema
// composition keywords (items, additionalProperties, prefixItems, dependentSchemas, not,
// if/then/else, and the union keywords). Boolean subschema values are handed to transform, which
// returns them unchanged, so additionalProperties: false survives untouched. Keywords named in skip are
// left for the caller, which already walked them with extra context; skipping them keeps nested-array
// schemas linear instead of re-walking the items subtree at every level.
func walkNestedSchemas(out map[string]any, transform func(any) any, skip ...string) {
	for _, key := range mapSubschemaKeywords {
		if m, ok := out[key].(map[string]any); ok {
			next := make(map[string]any, len(m))
			for name, subschema := range m {
				next[name] = transform(subschema)
			}
			out[key] = next
		}
	}

	for _, key := range singleSubschemaKeywords {
		if slices.Contains(skip, key) {
			continue
		}
		if subschema, ok := out[key]; ok {
			out[key] = transform(subschema)
		}
	}

	for _, key := range listSubschemaKeywords {
		if list, ok := out[key].([]any); ok {
			next := make([]any, len(list))
			for i, subschema := range list {
				next[i] = transform(subschema)
			}
			out[key] = next
		}
	}
}

// ConvertRefsToHints recursively traverses a schema, converting "$ref" definitions
// into object declarations with "See: DefName" description hints.
//
// Protocol Quirk: Google Cloud Code Assist rejects schemas containing "$ref" or "$defs"
// with HTTP 400 validation errors. Converting references into object types annotated with
// description hints preserves documentation for the model while keeping payloads valid.
func ConvertRefsToHints(schema any) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = ConvertRefsToHints(v)
		}
		return out
	case map[string]any:
		out := maps.Clone(s)
		if ref, ok := out["$ref"].(string); ok && ref != "" {
			parts := strings.Split(ref, "/")
			defName := parts[len(parts)-1]
			if defName == "" {
				defName = "unknown"
			}
			hint := "See: " + defName
			desc, _ := out["description"].(string)
			delete(out, "$ref")
			out["type"] = "object"
			out["description"] = AppendDescriptionHint(desc, hint)
			return out
		}

		delete(out, "$defs")
		delete(out, "definitions")

		walkPropertySchemas(out, ConvertRefsToHints)
		walkNestedSchemas(out, ConvertRefsToHints)

		return out
	default:
		return schema
	}
}

// MergeAllOfSchemas merges allOf subschema arrays into a single unified schema map.
func MergeAllOfSchemas(schema any) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = MergeAllOfSchemas(v)
		}
		return out
	case map[string]any:
		out := maps.Clone(s)

		if allOfSlice, ok := out["allOf"].([]any); ok && len(allOfSlice) > 0 {
			mergedProps := make(map[string]any)
			reqSet := make(map[string]bool)
			otherFields := make(map[string]any)

			for _, sub := range allOfSlice {
				subMap, ok := sub.(map[string]any)
				if !ok {
					continue
				}
				subClean := MergeAllOfSchemas(subMap).(map[string]any)

				if pMap, ok := subClean["properties"].(map[string]any); ok {
					maps.Copy(mergedProps, pMap)
				}
				if rList, ok := subClean["required"].([]any); ok {
					for _, r := range rList {
						if rStr, ok := r.(string); ok {
							reqSet[rStr] = true
						}
					}
				}
				for k, v := range subClean {
					if k != "properties" && k != "required" && k != "allOf" {
						if _, exists := otherFields[k]; !exists {
							otherFields[k] = v
						}
					}
				}
			}

			delete(out, "allOf")
			for k, v := range otherFields {
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}

			if len(mergedProps) > 0 {
				existingProps, _ := out["properties"].(map[string]any)
				if existingProps == nil {
					existingProps = make(map[string]any)
				}
				for k, v := range mergedProps {
					existingProps[k] = v
				}
				out["properties"] = existingProps
			}

			if len(reqSet) > 0 {
				existingReq, _ := out["required"].([]any)
				for _, r := range existingReq {
					if rStr, ok := r.(string); ok {
						reqSet[rStr] = true
					}
				}
				sortedReq := slices.Sorted(maps.Keys(reqSet))
				reqList := make([]any, len(sortedReq))
				for i, r := range sortedReq {
					reqList[i] = r
				}
				out["required"] = reqList
			}
		}

		walkPropertySchemas(out, MergeAllOfSchemas)
		walkNestedSchemas(out, MergeAllOfSchemas)

		return out
	default:
		return schema
	}
}

// FlattenAnyOfOneOfSchemas collapses anyOf and oneOf unions by scoring candidates
// and adopting the most structured schema while adding description hints for allowed types.
func FlattenAnyOfOneOfSchemas(schema any) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = FlattenAnyOfOneOfSchemas(v)
		}
		return out
	case map[string]any:
		out := maps.Clone(s)

		for _, unionKey := range []string{"anyOf", "oneOf"} {
			slice, ok := out[unionKey].([]any)
			if !ok || len(slice) == 0 {
				continue
			}

			// If all options in anyOf/oneOf are constant values, preserve them as an enum
			if allPureConstOrTypedConst(slice) {
				enums := make([]any, len(slice))
				allStrings := true
				var sampleType string
				for i, item := range slice {
					m := item.(map[string]any)
					c := m["const"]
					enums[i] = c
					if _, isStr := c.(string); !isStr {
						allStrings = false
					}
					if t, ok := m["type"].(string); ok && sampleType == "" {
						sampleType = t
					}
				}
				delete(out, unionKey)
				out["enum"] = enums
				if out["type"] == nil {
					if allStrings {
						out["type"] = "string"
					} else if sampleType != "" {
						out["type"] = sampleType
					}
				}
				continue
			}

			var typeNames []string
			var bestOption map[string]any
			bestScore := -1
			hasNullBranch := false

			for _, opt := range slice {
				optMap, ok := opt.(map[string]any)
				if !ok {
					continue
				}
				tName, _ := optMap["type"].(string)
				if tName == "null" {
					hasNullBranch = true
					continue
				}
				if tName == "" && optMap["properties"] != nil {
					tName = "object"
				}
				if tName != "" && tName != "null" {
					if !slices.Contains(typeNames, tName) {
						typeNames = append(typeNames, tName)
					}
				}

				score := ScoreSchemaOption(optMap)
				if score > bestScore {
					bestScore = score
					bestOption = optMap
				}
			}

			delete(out, unionKey)

			if bestOption != nil {
				flattened := FlattenAnyOfOneOfSchemas(bestOption).(map[string]any)
				parentDesc, _ := out["description"].(string)
				if hasNullBranch {
					out["nullable"] = true
					parentDesc = AppendDescriptionHint(parentDesc, "nullable")
					out["description"] = parentDesc
				}

				for k, v := range flattened {
					if k == "description" {
						if subDesc, ok := v.(string); ok && subDesc != "" && subDesc != parentDesc {
							out["description"] = AppendDescriptionHint(parentDesc, subDesc)
						}
					} else if k == "properties" {
						if existingProps, ok := out["properties"].(map[string]any); ok {
							if newProps, ok := v.(map[string]any); ok {
								for pk, pv := range newProps {
									existingProps[pk] = pv
								}
								out["properties"] = existingProps
								continue
							}
						}
						out[k] = v
					} else if _, exists := out[k]; !exists || k == "type" || k == "items" {
						out[k] = v
					}
				}

				if len(typeNames) > 1 {
					hint := "Accepts: " + strings.Join(typeNames, " | ")
					currDesc, _ := out["description"].(string)
					out["description"] = AppendDescriptionHint(currDesc, hint)
				}
			}
		}

		walkPropertySchemas(out, FlattenAnyOfOneOfSchemas)
		walkNestedSchemas(out, FlattenAnyOfOneOfSchemas)

		return out
	default:
		return schema
	}
}

// FlattenTypeArraysAndNullable converts type arrays (e.g. ["string", "null"]) into a single type
// and updates the parent required array to remove nullable properties.
func FlattenTypeArraysAndNullable(schema any) any {
	return flattenTypeArraysInternal(schema, nil, "")
}

// flattenTypeArraysInternal recursively processes type arrays and tracks nullable property names.
func flattenTypeArraysInternal(schema any, nullableProps map[string]bool, currentProp string) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = flattenTypeArraysInternal(v, nullableProps, "")
		}
		return out
	case map[string]any:
		out := maps.Clone(s)

		if typeSlice, ok := out["type"].([]any); ok && len(typeSlice) > 0 {
			hasNull := false
			var nonNullTypes []string

			for _, t := range typeSlice {
				if tStr, ok := t.(string); ok {
					if tStr == "null" {
						hasNull = true
					} else if tStr != "" {
						nonNullTypes = append(nonNullTypes, tStr)
					}
				}
			}

			chosenType := "string"
			if len(nonNullTypes) > 0 {
				chosenType = nonNullTypes[0]
			}
			out["type"] = chosenType

			desc, _ := out["description"].(string)
			if len(nonNullTypes) > 1 {
				out["description"] = AppendDescriptionHint(desc, "Accepts: "+strings.Join(nonNullTypes, " | "))
				desc = out["description"].(string)
			}

			if hasNull {
				out["description"] = AppendDescriptionHint(desc, "nullable")
				out["nullable"] = true
				if nullableProps != nil && currentProp != "" {
					nullableProps[currentProp] = true
				}
			}
		}

		childNullable := make(map[string]bool)
		if props, ok := out["properties"].(map[string]any); ok {
			newProps := make(map[string]any, len(props))
			for k, v := range props {
				newProps[k] = flattenTypeArraysInternal(v, childNullable, k)
			}
			out["properties"] = newProps

			if reqSlice, ok := out["required"].([]any); ok && len(childNullable) > 0 {
				var newReq []any
				for _, r := range reqSlice {
					if rStr, ok := r.(string); ok && !childNullable[rStr] {
						newReq = append(newReq, r)
					}
				}
				if len(newReq) > 0 {
					out["required"] = newReq
				} else {
					delete(out, "required")
				}
			}
		}

		if items, ok := out["items"]; ok {
			out["items"] = flattenTypeArraysInternal(items, nullableProps, "")
		}

		// Composition keywords (additionalProperties, prefixItems, dependentSchemas, not,
		// if/then/else) carry nested subschemas whose type arrays need the same normalization. items was
		// already normalized above, so it is skipped here to avoid re-walking the subtree.
		walkNestedSchemas(out, func(subschema any) any {
			return flattenTypeArraysInternal(subschema, nil, "")
		}, "items")

		return out
	default:
		return schema
	}
}

// MoveConstraintsToHints converts unsupported constraints (pattern, minLength, format, etc.)
// into description hints so models remain instructed without triggering schema validation errors.
func MoveConstraintsToHints(schema any, dropKeywords bool) any {
	constraints := []string{
		"minLength",
		"maxLength",
		"pattern",
		"minimum",
		"maximum",
		"minItems",
		"maxItems",
		"format",
	}

	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = MoveConstraintsToHints(v, dropKeywords)
		}
		return out
	case map[string]any:
		out := maps.Clone(s)

		var hints []string
		for _, c := range constraints {
			if val, exists := out[c]; exists {
				hints = append(hints, fmt.Sprintf("%s: %v", c, val))
				if dropKeywords {
					delete(out, c)
				}
			}
		}

		if len(hints) > 0 {
			desc, _ := out["description"].(string)
			out["description"] = AppendDescriptionHint(desc, strings.Join(hints, ", "))
		}

		walkPropertySchemas(out, func(subschema any) any {
			return MoveConstraintsToHints(subschema, dropKeywords)
		})
		walkNestedSchemas(out, func(subschema any) any {
			return MoveConstraintsToHints(subschema, dropKeywords)
		})

		return out
	default:
		return schema
	}
}

// EnsureValidArrayItems ensures that all array schemas carry a valid "items" object definition.
//
// Protocol Quirk: Google Cloud Code Assist rejects ARRAY schemas without an items object
// with HTTP 400 "items: missing field". Supplying a string fallback prevents these failures.
func EnsureValidArrayItems(schema any) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = EnsureValidArrayItems(v)
		}
		return out
	case map[string]any:
		out := maps.Clone(s)

		t, _ := out["type"].(string)
		if strings.EqualFold(t, "array") {
			if items, exists := out["items"]; !exists || items == nil {
				out["items"] = map[string]any{"type": "string"}
			} else if itemSlice, isArr := items.([]any); isArr {
				if len(itemSlice) > 0 {
					out["items"] = EnsureValidArrayItems(itemSlice[0])
				} else {
					out["items"] = map[string]any{"type": "string"}
				}
			} else {
				out["items"] = EnsureValidArrayItems(items)
			}
		}

		if props, ok := out["properties"].(map[string]any); ok {
			newProps := make(map[string]any, len(props))
			for k, v := range props {
				newProps[k] = EnsureValidArrayItems(v)
			}
			out["properties"] = newProps
		}

		// Composition keywords may hide array subschemas without items, which upstream also rejects. items
		// was already validated above, so it is skipped here.
		walkNestedSchemas(out, EnsureValidArrayItems, "items")

		return out
	default:
		return schema
	}
}

// allPureConstOrTypedConst checks if all elements in a union define a "const" value.
func allPureConstOrTypedConst(slice []any) bool {
	if len(slice) == 0 {
		return false
	}
	for _, item := range slice {
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		if _, hasConst := m["const"]; !hasConst {
			return false
		}
	}
	return true
}

// SanitizeComplexSchema runs the multi-phase schema normalization pipeline on tool parameters.
func SanitizeComplexSchema(schema any, dropConstraints bool) any {
	res := ConvertRefsToHints(schema)
	res = MergeAllOfSchemas(res)
	res = FlattenAnyOfOneOfSchemas(res)
	res = FlattenTypeArraysAndNullable(res)
	res = MoveConstraintsToHints(res, dropConstraints)
	res = EnsureValidArrayItems(res)
	return res
}
