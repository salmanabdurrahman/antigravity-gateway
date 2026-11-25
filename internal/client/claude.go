// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi, OpenCode, and Claude Code.
package client

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
)

// DefaultClaudeModel specifies the fallback primary model configured for Claude Code CLI when
// no matching Claude Sonnet model is found in the available catalog.
const DefaultClaudeModel = "claude-sonnet-5-5-high"

// DefaultClaudeOpusModel specifies the fallback Opus model configured for Claude Code CLI when
// no matching Claude Opus model is found in the available catalog.
const DefaultClaudeOpusModel = "claude-opus-5-5-high"

// ClaudeSetupOptions encapsulates parameters for configuring Claude Code CLI (~/.claude/settings.json).
type ClaudeSetupOptions struct {
	// SettingsPath overrides the destination settings.json file path (defaults to ~/.claude/settings.json or $CLAUDE_CONFIG_DIR).
	SettingsPath string

	// GatewayURL is the base HTTP endpoint of the local gateway proxy (e.g. http://127.0.0.1:51400).
	GatewayURL string

	// APIKey is the Bearer key required to authenticate against the gateway.
	APIKey string

	// Model specifies the primary ANTHROPIC_MODEL configured in settings.json. When empty, the
	// best matching Claude Sonnet model from Models is selected (falling back to DefaultClaudeModel).
	Model string

	// Models provides the available catalog models used to resolve default Claude Sonnet, Opus,
	// and Haiku identifiers dynamically without hardcoding unavailable IDs.
	Models []catalog.CatalogModel

	// DisableExperimentalMCP omits ENABLE_EXPERIMENTAL_MCP_CLI from the generated settings env map.
	DisableExperimentalMCP bool

	// NoVerify when true skips the post-setup live completion test probe.
	NoVerify bool

	// DryRun when true generates configuration without modifying disk.
	DryRun bool
}

// ClaudeSetupResult contains execution results, backup paths, and verification outcomes for Claude CLI setup.
type ClaudeSetupResult struct {
	// SettingsPath is the target settings.json path.
	SettingsPath string

	// BackupPath is the timestamped backup created before mutation.
	BackupPath string

	// WrittenJSON contains the serialized JSON payload for dry-run inspection.
	WrittenJSON string

	// TestResult holds the outcome of the 1-token test completion probe.
	TestResult *TestResult
}

// DefaultClaudeSettingsPath resolves ~/.claude/settings.json respecting $CLAUDE_CONFIG_DIR and $HOME.
func DefaultClaudeSettingsPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving user home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// SetupClaude configures Claude Code CLI's settings.json to route requests through Antigravity Gateway.
// It creates a timestamped backup before modifying disk and verifies connectivity with a 1-token probe.
func SetupClaude(ctx context.Context, opts ClaudeSetupOptions) (*ClaudeSetupResult, error) {
	settingsPath := opts.SettingsPath
	if settingsPath == "" {
		var err error
		settingsPath, err = DefaultClaudeSettingsPath()
		if err != nil {
			return nil, err
		}
	}

	gatewayURL := strings.TrimRight(opts.GatewayURL, "/")
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:51400"
	}

	apiKey := cmp.Or(opts.APIKey, "test")
	model := opts.Model
	if model == "" {
		model = resolveClaudeModel(opts.Models, "sonnet", DefaultClaudeModel)
	}
	opusModel := resolveClaudeModel(opts.Models, "opus", DefaultClaudeOpusModel)
	haikuModel := resolveClaudeModel(opts.Models, "haiku", model)

	settingsMap := make(map[string]any)
	if existingBytes, err := os.ReadFile(settingsPath); err == nil && len(existingBytes) > 0 {
		if unmarshalErr := json.Unmarshal(existingBytes, &settingsMap); unmarshalErr != nil {
			return nil, fmt.Errorf("parsing existing claude settings %s: %w", settingsPath, unmarshalErr)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading existing claude settings %s: %w", settingsPath, err)
	}

	envMap, ok := settingsMap["env"].(map[string]any)
	if !ok || envMap == nil {
		envMap = make(map[string]any)
	}

	envMap["ANTHROPIC_BASE_URL"] = gatewayURL
	envMap["ANTHROPIC_AUTH_TOKEN"] = apiKey
	envMap["ANTHROPIC_MODEL"] = model
	envMap["ANTHROPIC_DEFAULT_OPUS_MODEL"] = opusModel
	envMap["ANTHROPIC_DEFAULT_SONNET_MODEL"] = model
	envMap["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = haikuModel
	envMap["CLAUDE_CODE_SUBAGENT_MODEL"] = model
	if opts.DisableExperimentalMCP {
		delete(envMap, "ENABLE_EXPERIMENTAL_MCP_CLI")
	} else {
		envMap["ENABLE_EXPERIMENTAL_MCP_CLI"] = "true"
	}

	settingsMap["env"] = envMap

	payload, _ := json.MarshalIndent(settingsMap, "", "  ")
	res := &ClaudeSetupResult{
		SettingsPath: settingsPath,
		WrittenJSON:  string(payload),
	}

	if !opts.DryRun {
		backup, err := BackupFile(settingsPath, "")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("creating backup: %w", err)
		}
		res.BackupPath = backup

		if err := AtomicWriteJSON(settingsPath, settingsMap, 0o600); err != nil {
			return nil, fmt.Errorf("writing settings to %s: %w", settingsPath, err)
		}
	}

	if !opts.DryRun && !opts.NoVerify {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		testRes, err := TestAnthropicCompletion(probeCtx, gatewayURL, apiKey, model)
		if err != nil {
			return res, fmt.Errorf("connectivity verification failed: %w", err)
		}
		res.TestResult = testRes
	}

	return res, nil
}

// resolveClaudeModel selects the highest-ranked matching Claude model identifier for tier
// ("sonnet", "opus", or "haiku") from available catalog models using numeric version comparison and
// effort ranking (high > medium > base > low), falling back to preferredFallback when no catalog
// model matches tier.
func resolveClaudeModel(models []catalog.CatalogModel, tier string, preferredFallback string) string {
	if len(models) == 0 {
		return preferredFallback
	}

	var matches []string
	for _, m := range models {
		lower := strings.ToLower(m.ID)
		if strings.Contains(lower, "claude") && strings.Contains(lower, tier) {
			matches = append(matches, m.ID)
		}
	}
	if len(matches) == 0 {
		return preferredFallback
	}
	slices.SortFunc(matches, compareClaudeModelRank)
	return matches[len(matches)-1]
}

// compareClaudeModelRank orders two Claude model identifiers ascending by numeric version
// components, then effort tier (low < base < medium < high), then lexical tie-break.
func compareClaudeModelRank(a, b string) int {
	va := parseClaudeVersionTuple(a)
	vb := parseClaudeVersionTuple(b)
	for i := range max(len(va), len(vb)) {
		var na, nb int
		if i < len(va) {
			na = va[i]
		}
		if i < len(vb) {
			nb = vb[i]
		}
		if c := cmp.Compare(na, nb); c != 0 {
			return c
		}
	}
	if c := cmp.Compare(claudeEffortRank(a), claudeEffortRank(b)); c != 0 {
		return c
	}
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

// parseClaudeVersionTuple extracts numeric version tokens from a Claude model identifier.
func parseClaudeVersionTuple(id string) []int {
	var nums []int
	for part := range strings.SplitSeq(strings.ToLower(id), "-") {
		if n, err := strconv.Atoi(part); err == nil {
			nums = append(nums, n)
		}
	}
	return nums
}

// claudeEffortRank assigns an ordering score to effort suffixes so higher-capability variants win.
func claudeEffortRank(id string) int {
	lower := strings.ToLower(id)
	switch {
	case strings.Contains(lower, "-high"):
		return 4
	case strings.Contains(lower, "-medium"):
		return 3
	case strings.Contains(lower, "-low"):
		return 1
	default:
		return 2
	}
}
