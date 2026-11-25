// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
)

// fallbackManifestJSON is the bundled model manifest used when an operator runs client setup
// offline and no live or cached catalog is available. It is compiled into the binary so a fresh
// install never advertises retired model identifiers.
//
//go:embed fallback.json
var fallbackManifestJSON []byte

// fallbackModelsOnce memoizes decoding and validation of the bundled manifest.
var fallbackModelsOnce = sync.OnceValue(func() []CatalogModel {
	var models []CatalogModel
	if err := json.Unmarshal(fallbackManifestJSON, &models); err != nil {
		slog.Error("bundled fallback model manifest is invalid", "error", err)
		return nil
	}

	slices.SortFunc(models, func(a, b CatalogModel) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return models
})

// FallbackModels returns a deep copy of the bundled offline model manifest.
func FallbackModels() []CatalogModel {
	models := slices.Clone(fallbackModelsOnce())
	for i := range models {
		models[i].ThinkingBudget = cloneIntPointer(models[i].ThinkingBudget)
		models[i].MinThinkingBudget = cloneIntPointer(models[i].MinThinkingBudget)
	}
	return models
}

// cloneIntPointer copies an optional integer so callers cannot mutate the memoized manifest.
func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
