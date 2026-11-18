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

// CacheMaxAge defines the duration after which the cached model catalog is considered stale (15 minutes).
const CacheMaxAge = 15 * time.Minute

// AccountRunner defines the contract required to execute tasks using an available Google account.
type AccountRunner interface {
	WithAccount(ctx context.Context, fn func(ctx context.Context, token string, state *rotator.AccountState) error) error
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
		httpClient: &http.Client{Timeout: DefaultFetchTimeout},
		logger:     logger,
		nowFn:      time.Now,
		snapshot: CatalogSnapshot{
			Models: make([]CatalogModel, 0),
			Quotas: make(map[string]ModelQuota),
		},
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
		if payload.FetchedAt > 0 {
			c.fetchedAt = time.UnixMilli(payload.FetchedAt)
		}
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

	if c.rotator == nil {
		return errors.New("cannot refresh catalog: no account rotator configured")
	}

	var fetchedSnap *CatalogSnapshot
	err := c.rotator.WithAccount(ctx, func(ctx context.Context, token string, state *rotator.AccountState) error {
		c.mu.RLock()
		cfg := c.cfg
		client := c.httpClient
		c.mu.RUnlock()

		snap, err := FetchCatalog(ctx, cfg, client, token, state.ProjectID())
		if err != nil {
			return err
		}
		fetchedSnap = snap
		if state != nil && len(snap.Quotas) > 0 {
			for modelID, q := range snap.Quotas {
				if q.RemainingFraction != nil {
					state.SetQuotaFraction(modelID, *q.RemainingFraction)
				}
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("refreshing catalog via rotator: %w", err)
	}

	if fetchedSnap != nil && len(fetchedSnap.Models) > 0 {
		c.mu.Lock()
		c.snapshot = *fetchedSnap
		now := c.nowFn()
		c.fetchedAt = now
		c.mu.Unlock()

		if saveErr := c.SaveCache(); saveErr != nil {
			c.logger.Warn("failed persisting refreshed model catalog to disk", "error", saveErr)
		}
	}

	return nil
}

// Ensure guarantees that a catalog is available. If empty, it loads from cache or triggers a refresh.
// If cached data is older than CacheMaxAge, it initiates a refresh.
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
	isStale := c.fetchedAt.IsZero() || c.nowFn().Sub(c.fetchedAt) > CacheMaxAge
	c.mu.RUnlock()

	if c.rotator == nil {
		return nil
	}

	if !hasModels || isStale {
		return c.Refresh(ctx)
	}

	return nil
}

// Get returns the CatalogModel matching the given identifier (case-insensitive).
func (c *ModelCatalog) Get(id string) (CatalogModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	target := strings.ToLower(id)
	for _, m := range c.snapshot.Models {
		if strings.ToLower(m.ID) == target {
			return m, true
		}
	}
	return CatalogModel{}, false
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
		Quotas:       make(map[string]ModelQuota, len(c.snapshot.Quotas)),
		DefaultModel: c.snapshot.DefaultModel,
	}
	for k, v := range c.snapshot.Quotas {
		snap.Quotas[k] = v
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
