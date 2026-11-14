// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi and OpenCode.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
)

// LegacyProviderID represents the older provider identifier used by legacy proxy configurations.
const LegacyProviderID = "google-antigravity"

// PiSetupOptions encapsulates configuration flags and targets for setting up the Pi Coding Agent.
type PiSetupOptions struct {
	// PiDir is the root configuration directory path for Pi (defaults to ~/.pi/agent).
	PiDir string

	// GatewayURL is the base HTTP endpoint of the local gateway proxy.
	GatewayURL string

	// APIKey is the Bearer key required to authenticate against the gateway.
	APIKey string

	// ProviderID is the provider identifier registered in Pi's models.json.
	ProviderID string

	// Models provides the list of available catalog models to register.
	Models []catalog.CatalogModel

	// ModelFilter specifies comma-separated include/exclude glob patterns.
	ModelFilter string

	// NoVerify when true skips the post-setup live completion test.
	NoVerify bool

	// DryRun when true generates the configuration without modifying disk.
	DryRun bool

	// CleanLegacy when true removes legacy "google-antigravity" provider settings.
	CleanLegacy bool
}

// PiSetupResult contains execution details and backup references from Pi configuration.
type PiSetupResult struct {
	// ModelsPath is the absolute path to Pi's models.json file.
	ModelsPath string

	// BackupPath is the timestamped backup of models.json created before mutation.
	BackupPath string

	// AuthBackup is the timestamped backup of auth.json created before legacy cleanup.
	AuthBackup string

	// CleanedItems lists legacy artifacts or configuration entries purged during setup.
	CleanedItems []string

	// ModelCount is the number of models registered under the provider.
	ModelCount int

	// TestResult holds the outcome of the live completion probe.
	TestResult *TestResult

	// WrittenJSON contains the exact JSON content written to models.json.
	WrittenJSON string
}

// DefaultPiDir resolves the default ~/.pi/agent directory path, respecting PI_AGENT_DIR.
func DefaultPiDir() (string, error) {
	if env := os.Getenv("PI_AGENT_DIR"); env != "" {
		return filepath.Clean(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving user home directory: %w", err)
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// SetupPi writes the gateway provider and available models into Pi's models.json configuration.
func SetupPi(ctx context.Context, opts PiSetupOptions) (*PiSetupResult, error) {
	piDir := opts.PiDir
	if piDir == "" {
		var err error
		piDir, err = DefaultPiDir()
		if err != nil {
			return nil, fmt.Errorf("resolving pi directory: %w", err)
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

	modelsFilePath := filepath.Join(piDir, "models.json")
	authFilePath := filepath.Join(piDir, "auth.json")

	// Filter and convert chat models to Pi model schema
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

	// Read existing models.json if present
	modelsData := make(map[string]any)
	if raw, err := os.ReadFile(modelsFilePath); err == nil {
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &modelsData); err != nil {
				return nil, fmt.Errorf("parsing existing configuration %s: %w", modelsFilePath, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading existing configuration %s: %w", modelsFilePath, err)
	}

	// Read existing auth.json if present
	authData := make(map[string]any)
	if raw, err := os.ReadFile(authFilePath); err == nil {
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &authData); err != nil {
				return nil, fmt.Errorf("parsing existing auth file %s: %w", authFilePath, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading existing auth file %s: %w", authFilePath, err)
	}

	var cleanedItems []string
	if opts.CleanLegacy {
		cleanedItems = cleanPiLegacy(modelsData, authData)
	}

	// Ensure providers map exists
	providers, ok := modelsData["providers"].(map[string]any)
	if !ok || providers == nil {
		providers = make(map[string]any)
		modelsData["providers"] = providers
	}

	// Inspect existing provider block to preserve custom fields
	existingProvider, _ := providers[providerID].(map[string]any)
	if existingProvider == nil {
		existingProvider = make(map[string]any)
	}

	existingModels, _ := existingProvider["models"].([]any)
	existingModelsMap := make(map[string]map[string]any)
	for _, em := range existingModels {
		if emMap, ok := em.(map[string]any); ok {
			if id, ok := emMap["id"].(string); ok && id != "" {
				existingModelsMap[id] = emMap
			}
		}
	}

	// Build new models list
	var newModelsList []map[string]any
	var modelIDs []string

	for _, m := range eligibleModels {
		modelIDs = append(modelIDs, m.ID)
		mEntry := make(map[string]any)
		// Inherit existing custom fields
		if old, exists := existingModelsMap[m.ID]; exists {
			for k, v := range old {
				mEntry[k] = v
			}
		}

		mEntry["id"] = m.ID
		mEntry["name"] = PrettyModelName(m.ID)
		if m.Images {
			mEntry["input"] = []string{"text", "image"}
		} else {
			mEntry["input"] = []string{"text"}
		}
		mEntry["reasoning"] = m.Thinking

		if m.ContextWindow > 0 {
			mEntry["contextWindow"] = m.ContextWindow
		}
		if m.MaxOutputTokens > 0 {
			mEntry["maxTokens"] = m.MaxOutputTokens
		}

		newModelsList = append(newModelsList, mEntry)
	}

	existingProvider["baseUrl"] = gatewayURL
	existingProvider["api"] = "openai-completions"
	existingProvider["apiKey"] = opts.APIKey
	existingProvider["models"] = newModelsList
	providers[providerID] = existingProvider

	res := &PiSetupResult{
		ModelsPath:   modelsFilePath,
		CleanedItems: cleanedItems,
		ModelCount:   len(newModelsList),
	}

	if opts.DryRun {
		marshaled, _ := json.MarshalIndent(modelsData, "", "  ")
		res.WrittenJSON = string(marshaled)
		return res, nil
	}

	// Create timestamped backup of models.json and auth.json
	ts := time.Now().Format("20060102-150405")
	backup, err := BackupFile(modelsFilePath, ts)
	if err != nil {
		return nil, fmt.Errorf("creating models backup: %w", err)
	}
	res.BackupPath = backup

	if slices.Contains(cleanedItems, "auth.json: removed legacy placeholder") {
		authBackup, err := BackupFile(authFilePath, ts)
		if err != nil {
			return nil, fmt.Errorf("creating auth backup: %w", err)
		}
		res.AuthBackup = authBackup
		if err := AtomicWriteJSON(authFilePath, authData, 0o600); err != nil {
			return nil, fmt.Errorf("writing cleaned auth config: %w", err)
		}
	}

	if err := AtomicWriteJSON(modelsFilePath, modelsData, 0o600); err != nil {
		return nil, fmt.Errorf("writing pi models config: %w", err)
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

// cleanPiLegacy removes older local redirect/placeholder tokens that might interfere with direct gateway connection.
func cleanPiLegacy(models map[string]any, auth map[string]any) []string {
	var cleaned []string

	if providers, ok := models["providers"].(map[string]any); ok {
		if legacy, ok := providers[LegacyProviderID].(map[string]any); ok {
			baseURL, _ := legacy["baseUrl"].(string)
			if isLoopbackURL(baseURL) {
				delete(providers, LegacyProviderID)
				cleaned = append(cleaned, fmt.Sprintf("models.json: removed legacy %s provider", LegacyProviderID))
			}
		}
	}

	if legacyAuth, ok := auth[LegacyProviderID].(map[string]any); ok {
		refresh, _ := legacyAuth["refresh"].(string)
		access, _ := legacyAuth["access"].(string)
		if refresh == "proxy-managed" || access == "proxy-managed" {
			delete(auth, LegacyProviderID)
			cleaned = append(cleaned, "auth.json: removed legacy placeholder")
		}
	}

	return cleaned
}

// isLoopbackURL reports whether rawURL targets the local machine (127.0.0.1, localhost, or ::1).
func isLoopbackURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
