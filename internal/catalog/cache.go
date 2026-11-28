// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// CacheMaxAge defines the fallback duration after which the cached model catalog is considered stale
// (15 minutes). Configured gateways resolve the window through Config.CatalogMaxAge instead, which
// follows the operating profile: the safe profile polls hourly, the manual profile keeps this value.
const CacheMaxAge = 15 * time.Minute

// BackgroundRefreshTimeout bounds one background refresh across every pooled account. Each upstream
// request keeps its own DefaultFetchTimeout, so this only guards a hung multi-account loop.
const BackgroundRefreshTimeout = 5 * time.Minute

// AccountRunner defines the contract required to execute tasks using an available Google account.
type AccountRunner interface {
	WithAccount(ctx context.Context, fn func(ctx context.Context, token string, state *rotator.AccountState) error) error
}

// AccountIterator is an optional extension implemented by rotators that can execute work against
// every usable account. Catalog refresh uses it to collect per-account quotas instead of stopping
// after the first successful account.
type AccountIterator interface {
	ForEachAccount(ctx context.Context, fn func(ctx context.Context, token string, state *rotator.AccountState) error) error
}

// CachePayload defines the on-disk JSON envelope for cached model catalog data.
type CachePayload struct {
	// FetchedAt is the Unix timestamp in milliseconds when the catalog was retrieved.
	FetchedAt int64 `json:"fetchedAt"`

	// Models lists the cached catalog model definitions.
	Models []CatalogModel `json:"models"`

	// Quotas caches upstream quota availability per model identifier.
	Quotas map[string]ModelQuota `json:"quotas,omitempty"`

	// DefaultModel records the upstream default model identifier.
	DefaultModel string `json:"defaultModel,omitempty"`

	// AccountQuota caches the separated rolling and weekly allowance windows.
	AccountQuota *AccountQuotaSummary `json:"accountQuota,omitzero"`
}

// ModelCatalog coordinates in-memory model snapshots, atomic disk caching, and background refreshes.
type ModelCatalog struct {
	// mu synchronizes access to the cached snapshot and fetchedAt timestamp.
	mu sync.RWMutex

	// refreshMu prevents duplicate concurrent background fetches against upstream endpoints.
	refreshMu sync.Mutex

	// snapshot stores the active list of models and quota mappings in memory.
	snapshot CatalogSnapshot

	// fetchedAt records when the snapshot was last refreshed from Google Cloud Code Assist.
	fetchedAt time.Time

	// lastRefreshAttempt records the last time a background or foreground refresh was attempted.
	lastRefreshAttempt time.Time

	// byID indexes models by lowercase ID for O(1) lookups without linear scanning.
	byID map[string]CatalogModel

	// cfg holds application settings and upstream endpoint URLs.
	cfg *config.Config

	// rotator executes requests using an active Google account.
	rotator AccountRunner

	// cacheFile is the absolute filesystem destination path for models-cache.json.
	cacheFile string

	// httpClient is used to dispatch model catalog requests.
	httpClient *http.Client

	// logger is the structured logger instance for catalog events.
	logger *slog.Logger

	// nowFn provides the current timestamp, injectable for deterministic testing.
	nowFn func() time.Time
}

// NewModelCatalog initializes a ModelCatalog instance with caching and background refresh capabilities.
func NewModelCatalog(cfg *config.Config, rot AccountRunner, cacheFile string, logger *slog.Logger) *ModelCatalog {
	if logger == nil {
		logger = slog.Default()
	}
	return &ModelCatalog{
		cfg:        cfg,
		rotator:    rot,
		cacheFile:  cacheFile,
		httpClient: config.DefaultHTTPClient(DefaultFetchTimeout),
		logger:     logger,
		nowFn:      time.Now,
		snapshot: CatalogSnapshot{
			Models: make([]CatalogModel, 0),
			Quotas: make(map[string]ModelQuota),
		},
		byID: make(map[string]CatalogModel),
	}
}

// SetNowFn overrides the time provider (primarily for deterministic unit testing).
func (c *ModelCatalog) SetNowFn(fn func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nowFn = fn
}

// SetHTTPClient overrides the HTTP client used for fetching catalog updates.
func (c *ModelCatalog) SetHTTPClient(client *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.httpClient = client
}

// LoadCache loads the cached catalog from the configured disk path.
func (c *ModelCatalog) LoadCache() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cacheFile == "" {
		return nil
	}

	data, err := os.ReadFile(c.cacheFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading model cache file %s: %w", c.cacheFile, err)
	}

	var payload CachePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("parsing model cache JSON: %w", err)
	}

	if len(payload.Models) > 0 {
		c.snapshot.Models = payload.Models
		c.snapshot.Quotas = payload.Quotas
		c.snapshot.DefaultModel = payload.DefaultModel
		c.snapshot.AccountQuota = payload.AccountQuota
		if payload.FetchedAt > 0 {
			c.fetchedAt = time.UnixMilli(payload.FetchedAt)
		}
		c.rebuildIndexLocked()
	}

	return nil
}

// SaveCache persists the current in-memory model catalog atomically to disk with POSIX 0600 permissions.
func (c *ModelCatalog) SaveCache() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.cacheFile == "" || len(c.snapshot.Models) == 0 {
		return nil
	}

	payload := CachePayload{
		FetchedAt:    c.fetchedAt.UnixMilli(),
		Models:       c.snapshot.Models,
		Quotas:       c.snapshot.Quotas,
		DefaultModel: c.snapshot.DefaultModel,
		AccountQuota: c.snapshot.AccountQuota,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling model cache payload: %w", err)
	}

	return writeAtomic(c.cacheFile, data)
}

// Refresh performs a live fetch across non-cooling accounts via the rotator and writes the updated cache to disk.
func (c *ModelCatalog) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	return c.refreshLocked(ctx)
}

// refreshLocked executes the catalog fetch and cache persistence. Callers must hold refreshMu.
func (c *ModelCatalog) refreshLocked(ctx context.Context) error {
	now := c.nowFn()
	c.mu.Lock()
	c.lastRefreshAttempt = now
	c.mu.Unlock()

	if c.rotator == nil {
		return errors.New("cannot refresh catalog: no account rotator configured")
	}

	c.mu.RLock()
	cfg := c.cfg
	client := c.httpClient
	c.mu.RUnlock()

	merged := &CatalogSnapshot{
		Models: make([]CatalogModel, 0),
		Quotas: make(map[string]ModelQuota),
	}
	accounts := 0
	var lastErr error

	fetch := func(ctx context.Context, token string, state *rotator.AccountState) error {
		tier := ""
		projectID := ""
		if state != nil {
			tier = state.Tier()
			projectID = state.ProjectID()
		}

		snap, err := FetchCatalogForTier(ctx, cfg, client, token, projectID, tier)
		if err != nil {
			return err
		}
		if snap == nil || len(snap.Models) == 0 {
			return errors.New("upstream returned an empty model catalog")
		}

		if state != nil {
			// Per-account quota state feeds hybrid scoring; every usable account must be refreshed,
			// not only the first account that answers.
			for modelID, quota := range snap.Quotas {
				if quota.RemainingFraction != nil {
					state.SetQuotaFraction(modelID, *quota.RemainingFraction)
				}
			}
		}

		// Weekly windows are fetched separately because the model catalog does not always embed them.
		if summary, summaryErr := FetchAccountQuotaSummary(ctx, cfg, client, token, projectID, tier); summaryErr != nil {
			c.logger.Debug("account quota summary unavailable", "account", stateEmail(state), "error", summaryErr)
		} else {
			snap.AccountQuota = MergeQuotaSummary(snap.AccountQuota, summary)
			if state != nil && summary != nil && summary.FiveHour != nil && summary.FiveHour.RemainingFraction != nil {
				state.SetQuotaFraction("*", *summary.FiveHour.RemainingFraction)
			}
		}

		mergeCatalogSnapshot(merged, snap)
		accounts++
		return nil
	}

	if iterator, ok := c.rotator.(AccountIterator); ok {
		err := iterator.ForEachAccount(ctx, fetch)
		if err != nil {
			lastErr = err
		}
	} else {
		err := c.rotator.WithAccount(ctx, fetch)
		if err != nil {
			lastErr = err
		}
	}

	// A canceled or timed-out refresh must never be persisted as fresh: a partial catalog would hide
	// whole model families for CacheMaxAge, and accounts later in the pool would keep stale quotas.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if accounts == 0 {
		if lastErr != nil {
			return fmt.Errorf("refreshing catalog via rotator: %w", lastErr)
		}
		return errors.New("refreshing catalog via rotator: no account produced a catalog")
	}
	if lastErr != nil {
		c.logger.Warn("some accounts failed to refresh catalog; using partial results", "error", lastErr, "accounts", accounts)
	}

	merged.DefaultModel = resolveMergedDefaultModel(merged)

	c.mu.Lock()
	c.snapshot = *merged
	c.fetchedAt = now
	c.rebuildIndexLocked()
	c.mu.Unlock()

	if saveErr := c.SaveCache(); saveErr != nil {
		c.logger.Warn("failed persisting refreshed model catalog to disk", "error", saveErr)
	}

	return nil
}

// stateEmail returns the account email for diagnostics without failing on a nil state.
func stateEmail(state *rotator.AccountState) string {
	if state == nil {
		return ""
	}
	return state.Email()
}

// mergeCatalogSnapshot merges one account's catalog into the aggregate snapshot. Models are unioned
// by identifier and quotas keep the most permissive fraction, so one depleted Free account never
// hides a higher-tier model or throttles a sibling account's available capacity.
func mergeCatalogSnapshot(dst *CatalogSnapshot, src *CatalogSnapshot) {
	if src == nil {
		return
	}
	if dst.Quotas == nil {
		dst.Quotas = make(map[string]ModelQuota)
	}
	if src.DefaultModel != "" && dst.DefaultModel == "" {
		dst.DefaultModel = src.DefaultModel
	}

	index := make(map[string]int, len(dst.Models))
	for i, model := range dst.Models {
		index[strings.ToLower(model.ID)] = i
	}

	for _, model := range src.Models {
		key := strings.ToLower(model.ID)
		if position, ok := index[key]; ok {
			existing := &dst.Models[position]
			existing.ContextWindow = max(existing.ContextWindow, model.ContextWindow)
			existing.MaxOutputTokens = max(existing.MaxOutputTokens, model.MaxOutputTokens)
			existing.Thinking = existing.Thinking || model.Thinking
			existing.Images = existing.Images || model.Images
			if existing.DisplayName == "" {
				existing.DisplayName = model.DisplayName
			}
			if existing.ThinkingBudget == nil {
				existing.ThinkingBudget = model.ThinkingBudget
			}
			if existing.MinThinkingBudget == nil {
				existing.MinThinkingBudget = model.MinThinkingBudget
			}
			continue
		}
		index[key] = len(dst.Models)
		dst.Models = append(dst.Models, model)
	}

	for modelID, quota := range src.Quotas {
		key := strings.ToLower(modelID)
		if existing, ok := dst.Quotas[key]; ok {
			dst.Quotas[key] = mergeModelQuota(existing, quota)
			continue
		}
		dst.Quotas[key] = quota
	}

	dst.AccountQuota = MergeQuotaSummary(dst.AccountQuota, src.AccountQuota)

	slices.SortFunc(dst.Models, func(a, b CatalogModel) int {
		return strings.Compare(a.ID, b.ID)
	})
}

// mergeModelQuota combines two per-model quotas, keeping the most permissive remaining fraction.
func mergeModelQuota(current ModelQuota, candidate ModelQuota) ModelQuota {
	merged := current
	if candidate.RemainingFraction != nil && (merged.RemainingFraction == nil || *candidate.RemainingFraction > *merged.RemainingFraction) {
		merged.RemainingFraction = candidate.RemainingFraction
		if candidate.ResetTime != "" {
			merged.ResetTime = candidate.ResetTime
		}
	} else if merged.ResetTime == "" && candidate.ResetTime != "" {
		merged.ResetTime = candidate.ResetTime
	}

	if candidate.Weekly != nil {
		if merged.Weekly == nil {
			merged.Weekly = candidate.Weekly
		} else if candidate.Weekly.RemainingFraction != nil &&
			(merged.Weekly.RemainingFraction == nil || *candidate.Weekly.RemainingFraction > *merged.Weekly.RemainingFraction) {
			merged.Weekly = candidate.Weekly
		}
	}

	return merged
}

// resolveMergedDefaultModel picks a stable default model from the merged catalog.
func resolveMergedDefaultModel(snapshot *CatalogSnapshot) string {
	if snapshot == nil || len(snapshot.Models) == 0 {
		return ""
	}
	if snapshot.DefaultModel != "" {
		for _, model := range snapshot.Models {
			if strings.EqualFold(model.ID, snapshot.DefaultModel) {
				return model.ID
			}
		}
	}
	return snapshot.Models[0].ID
}

// rebuildIndexLocked populates the lowercase byID map. Caller must hold c.mu.
func (c *ModelCatalog) rebuildIndexLocked() {
	c.byID = make(map[string]CatalogModel, len(c.snapshot.Models))
	for _, m := range c.snapshot.Models {
		c.byID[strings.ToLower(m.ID)] = m
	}
}

// Ensure guarantees that a catalog is available.
// If no models are present, it performs a blocking load or refresh.
// If models are already cached but stale, it immediately returns to keep request hot-paths
// unblocked while refreshing asynchronously in the background (stale-while-revalidate).
func (c *ModelCatalog) Ensure(ctx context.Context) error {
	c.mu.RLock()
	hasModels := len(c.snapshot.Models) > 0
	c.mu.RUnlock()

	if !hasModels {
		if err := c.LoadCache(); err != nil {
			c.logger.Warn("failed loading model cache from disk", "error", err)
		}
	}

	c.mu.RLock()
	hasModels = len(c.snapshot.Models) > 0
	now := c.nowFn()
	maxAge := CacheMaxAge
	if c.cfg != nil {
		maxAge = c.cfg.CatalogMaxAge()
	}
	isStale := c.fetchedAt.IsZero() || now.Sub(c.fetchedAt) > maxAge
	recentAttempt := !c.lastRefreshAttempt.IsZero() && now.Sub(c.lastRefreshAttempt) < 1*time.Minute
	c.mu.RUnlock()

	if c.rotator == nil {
		return nil
	}

	// Cold start: block if no models are available at all
	if !hasModels {
		return c.Refresh(ctx)
	}

	// Stale-while-revalidate: dispatch background refresh if stale and not throttled
	if isStale && !recentAttempt {
		if c.refreshMu.TryLock() {
			go func() {
				defer c.refreshMu.Unlock()
				refreshCtx, cancel := context.WithTimeout(context.Background(), BackgroundRefreshTimeout)
				defer cancel()
				if err := c.refreshLocked(refreshCtx); err != nil {
					c.logger.Warn("background catalog refresh failed", "error", err)
				}
			}()
		}
	}

	return nil
}

// Get returns the CatalogModel matching the given identifier (case-insensitive) in O(1) time.
func (c *ModelCatalog) Get(id string) (CatalogModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	m, ok := c.byID[strings.ToLower(id)]
	return m, ok
}

// Info returns the catalog entry if available, or synthesizes a CatalogModel with family defaults.
func (c *ModelCatalog) Info(id string) CatalogModel {
	if m, ok := c.Get(id); ok {
		return m
	}
	return FamilyDefaults(id)
}

// Snapshot returns a thread-safe copy of the current catalog snapshot and fetch timestamp.
func (c *ModelCatalog) Snapshot() (CatalogSnapshot, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snap := CatalogSnapshot{
		Models:       slices.Clone(c.snapshot.Models),
		Quotas:       cloneModelQuotas(c.snapshot.Quotas),
		DefaultModel: c.snapshot.DefaultModel,
		AccountQuota: cloneQuotaWindowSummary(c.snapshot.AccountQuota),
	}

	return snap, c.fetchedAt
}

// writeAtomic writes data to a temporary file with POSIX 0600 permissions,
// syncing it to disk before renaming over the target path to prevent corruption.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, config.DirPerm); err != nil {
		return fmt.Errorf("creating directory %s for cache file: %w", dir, err)
	}

	tempFile, err := os.CreateTemp(dir, "models-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary cache file in %s: %w", dir, err)
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := tempFile.Chmod(config.FilePerm); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("setting file mode 0600 on %s: %w", tempPath, err)
	}

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("writing cache data to %s: %w", tempPath, err)
	}

	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("flushing temporary cache file %s: %w", tempPath, err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("closing temporary cache file %s: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("renaming temporary cache %s to destination %s: %w", tempPath, path, err)
	}

	return nil
}
