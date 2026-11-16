// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"reflect"
	"strings"
	"testing"
)

// TestConvertRefsToHints verifies converting $ref paths into object type with description hints.
func TestConvertRefsToHints(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner": map[string]any{
				"$ref":        "#/$defs/UserRecord",
				"description": "Resource owner",
			},
		},
	}

	result := ConvertRefsToHints(schema).(map[string]any)
	props := result["properties"].(map[string]any)
	owner := props["owner"].(map[string]any)

	if _, hasRef := owner["$ref"]; hasRef {
		t.Error("expected $ref to be removed")
	}
	if owner["type"] != "object" {
		t.Errorf("expected type object, got %v", owner["type"])
	}
	desc, _ := owner["description"].(string)
	if !strings.Contains(desc, "See: UserRecord") {
		t.Errorf("expected 'See: UserRecord' hint in description, got %q", desc)
	}

	// Also verify root $defs and definitions are removed
	rootWithDefs := map[string]any{
		"type":        "object",
		"$defs":       map[string]any{"User": map[string]any{"type": "string"}},
		"definitions": map[string]any{"Role": map[string]any{"type": "string"}},
	}
	cleanDefs := ConvertRefsToHints(rootWithDefs).(map[string]any)
	if _, hasDefs := cleanDefs["$defs"]; hasDefs {
		t.Error("expected $defs to be removed")
	}
	if _, hasDefinitions := cleanDefs["definitions"]; hasDefinitions {
		t.Error("expected definitions to be removed")
	}
}

// TestMergeAllOfSchemas verifies merging allOf properties and unioning required arrays.
func TestMergeAllOfSchemas(t *testing.T) {
	schema := map[string]any{
		"allOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
				"required": []any{"id"},
			},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []any{"name"},
			},
		},
	}

	result := MergeAllOfSchemas(schema).(map[string]any)
	if _, hasAllOf := result["allOf"]; hasAllOf {
		t.Error("expected allOf to be removed")
	}

	props, ok := result["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected merged properties map, got %v", result["properties"])
	}
	if props["id"] == nil || props["name"] == nil {
		t.Errorf("expected both id and name properties, got %v", props)
	}

	req, ok := result["required"].([]any)
	if !ok || len(req) != 2 {
		t.Errorf("expected required [id, name], got %v", result["required"])
	}
}

// TestFlattenAnyOfOneOfSchemas verifies selecting the highest scored candidate in union constructs.
func TestFlattenAnyOfOneOfSchemas(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "null"},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filter": map[string]any{"type": "string"},
				},
			},
		},
	}

	result := FlattenAnyOfOneOfSchemas(schema).(map[string]any)
	if _, hasAnyOf := result["anyOf"]; hasAnyOf {
		t.Error("expected anyOf to be removed")
	}
	if result["type"] != "object" {
		t.Errorf("expected best scored object type, got %v", result["type"])
	}
	props, ok := result["properties"].(map[string]any)
	if !ok || props["filter"] == nil {
		t.Errorf("expected filter property preserved, got %v", result["properties"])
	}

	// Verify preserving parent properties when union branch defines additional properties
	schemaWithParentProps := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string"},
		},
		"oneOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{"type": "string"},
				},
			},
		},
	}
	mergedParent := FlattenAnyOfOneOfSchemas(schemaWithParentProps).(map[string]any)
	mergedProps := mergedParent["properties"].(map[string]any)
	if mergedProps["action"] == nil || mergedProps["target"] == nil {
		t.Errorf("expected both parent 'action' and union 'target' properties, got %v", mergedProps)
	}
}

// TestFlattenTypeArraysAndNullable verifies converting type arrays to scalar type and updating required.
func TestFlattenTypeArraysAndNullable(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tag": map[string]any{
				"type": []any{"string", "null"},
			},
			"id": map[string]any{
				"type": "string",
			},
		},
		"required": []any{"tag", "id"},
	}

	result := FlattenTypeArraysAndNullable(schema).(map[string]any)
	props := result["properties"].(map[string]any)
	tag := props["tag"].(map[string]any)

	if tag["type"] != "string" {
		t.Errorf("expected type string, got %v", tag["type"])
	}
	if tag["nullable"] != true {
		t.Errorf("expected nullable true, got %v", tag["nullable"])
	}

	// Nullable tag should be removed from parent required
	req := result["required"].([]any)
	if !reflect.DeepEqual(req, []any{"id"}) {
		t.Errorf("expected required ['id'], got %v", req)
	}
}

// TestMoveConstraintsToHints verifies converting validation keywords into description text hints.
func TestMoveConstraintsToHints(t *testing.T) {
	schema := map[string]any{
		"type":        "string",
		"description": "Username",
		"minLength":   3,
		"pattern":     "^[a-z]+$",
	}

	result := MoveConstraintsToHints(schema, true).(map[string]any)
	desc, _ := result["description"].(string)

	if !strings.Contains(desc, "minLength: 3") || !strings.Contains(desc, "pattern: ^[a-z]+$") {
		t.Errorf("expected constraints in description, got %q", desc)
	}
	if _, hasMin := result["minLength"]; hasMin {
		t.Error("expected minLength dropped when dropKeywords is true")
	}
	if _, hasPat := result["pattern"]; hasPat {
		t.Error("expected pattern dropped when dropKeywords is true")
	}
}

// TestEnsureValidArrayItems verifies ensuring array schemas have valid items child.
func TestEnsureValidArrayItems(t *testing.T) {
	schema := map[string]any{
		"type": "array",
	}

	result := EnsureValidArrayItems(schema).(map[string]any)
	items, ok := result["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Errorf("expected fallback items {type: string}, got %v", result["items"])
	}
}
