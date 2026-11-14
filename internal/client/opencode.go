// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi and OpenCode.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
)

// OpenCodePackageOpenAICompatible defines the OpenCode runtime package for standard OpenAI endpoints.
const OpenCodePackageOpenAICompatible = "@opencode/ai/providers/openai-compatible"

// OpenCodeSetupOptions encapsulates settings for auto-configuring the OpenCode coding agent.
type OpenCodeSetupOptions struct {
	// ConfigPath is the custom filesystem path to opencode.json.
	ConfigPath string

	// GatewayURL is the base HTTP URL of the local reverse proxy.
	GatewayURL string

	// APIKey is the Bearer authentication token for gateway requests.
	APIKey string

	// ProviderID is the provider key name registered in OpenCode.
	ProviderID string

	// Models provides the list of available catalog models to register.
	Models []catalog.CatalogModel

	// ModelFilter specifies optional glob patterns to include/exclude specific models.
	ModelFilter string

	// NoVerify when true skips the post-setup live completion probe.
	NoVerify bool

	// DryRun when true calculates and returns the configuration JSON without writing to disk.
	DryRun bool
}

// OpenCodeSetupResult contains execution details and backup references from OpenCode configuration.
type OpenCodeSetupResult struct {
	// ConfigPath is the absolute path to the modified configuration file.
	ConfigPath string

	// BackupPath is the timestamped backup file created before writing changes.
	BackupPath string

	// ModelCount is the number of models registered under the provider.
	ModelCount int

	// TestResult contains the outcome of the post-setup verification probe, if performed.
	TestResult *TestResult

	// WrittenJSON contains the exact JSON content written to disk or generated during dry run.
	WrittenJSON string
}

// DefaultOpenCodeConfigPath returns ~/.config/opencode/opencode.json, respecting OPENCODE_CONFIG env.
func DefaultOpenCodeConfigPath() (string, error) {
	if env := os.Getenv("OPENCODE_CONFIG"); env != "" {
		return filepath.Clean(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json"), nil
}

// SetupOpenCode injects the gateway provider and model specifications into OpenCode configuration.
func SetupOpenCode(ctx context.Context, opts OpenCodeSetupOptions) (*OpenCodeSetupResult, error) {
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		var err error
		cfgPath, err = DefaultOpenCodeConfigPath()
		if err != nil {
			return nil, fmt.Errorf("resolving opencode config path: %w", err)
		}
	}

	providerID := opts.ProviderID
	if providerID == "" {
		providerID = DefaultProviderID
	}

	gatewayURL := strings.TrimRight(opts.GatewayURL, "/")
	if !strings.HasSuffix(gatewayURL, "/v1") {
		gatewayURL += "/v1"
	}

	// Filter models
	mf, err := NewModelFilter(opts.ModelFilter)
	if err != nil {
		return nil, fmt.Errorf("compiling model filter: %w", err)
	}

	var eligibleModels []catalog.CatalogModel
	for _, m := range opts.Models {
		if mf.Matches(m.ID) {
			eligibleModels = append(eligibleModels, m)
		}
	}

	if len(eligibleModels) == 0 && len(opts.Models) > 0 {
		return nil, fmt.Errorf("no models matched filter %q", opts.ModelFilter)
	}

	// Read existing config if present
	configData := make(map[string]any)
	if raw, err := os.ReadFile(cfgPath); err == nil {
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &configData); err != nil {
				return nil, fmt.Errorf("parsing existing configuration %s: %w", cfgPath, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading existing configuration %s: %w", cfgPath, err)
	}

	if _, ok := configData["$schema"]; !ok {
		configData["$schema"] = "https://opencode.ai/config.json"
	}

	providers, ok := configData["providers"].(map[string]any)
	if !ok || providers == nil {
		providers = make(map[string]any)
		configData["providers"] = providers
	}

	existingProvider, _ := providers[providerID].(map[string]any)
	if existingProvider == nil {
		existingProvider = make(map[string]any)
	}

	existingModels, _ := existingProvider["models"].(map[string]any)
	if existingModels == nil {
		existingModels = make(map[string]any)
	}

	// Build models map
	newModelsMap := make(map[string]any)
	var modelIDs []string

	for _, m := range eligibleModels {
		modelIDs = append(modelIDs, m.ID)
		mEntry := make(map[string]any)
		if old, exists := existingModels[m.ID].(map[string]any); exists {
			for k, v := range old {
				mEntry[k] = v
			}
		}
		mEntry["name"] = PrettyModelName(m.ID)
		newModelsMap[m.ID] = mEntry
	}

	existingProvider["name"] = DefaultProviderName
	existingProvider["package"] = OpenCodePackageOpenAICompatible

	settings, _ := existingProvider["settings"].(map[string]any)
	if settings == nil {
		settings = make(map[string]any)
	}
	settings["baseURL"] = gatewayURL
	if opts.APIKey != "" {
		settings["apiKey"] = opts.APIKey
	}
	existingProvider["settings"] = settings
	existingProvider["models"] = newModelsMap
	providers[providerID] = existingProvider

	res := &OpenCodeSetupResult{
		ConfigPath: cfgPath,
		ModelCount: len(newModelsMap),
	}

	if opts.DryRun {
		marshaled, _ := json.MarshalIndent(configData, "", "  ")
		res.WrittenJSON = string(marshaled)
		return res, nil
	}

	ts := time.Now().Format("20060102-150405")
	backup, err := BackupFile(cfgPath, ts)
	if err != nil {
		return nil, fmt.Errorf("creating opencode backup: %w", err)
	}
	res.BackupPath = backup

	if err := AtomicWriteJSON(cfgPath, configData, 0o600); err != nil {
		return nil, fmt.Errorf("writing opencode configuration: %w", err)
	}

	// 1-token test verification if enabled
	if !opts.NoVerify && len(modelIDs) > 0 {
		testModel := PickTestModel(modelIDs)
		if testModel != "" {
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			testRes, testErr := TestChatCompletion(probeCtx, gatewayURL, opts.APIKey, testModel)
			if testErr != nil {
				res.TestResult = testRes
				return res, fmt.Errorf("gateway verification probe failed: %w", testErr)
			}
			res.TestResult = testRes
		}
	}

	return res, nil
}
