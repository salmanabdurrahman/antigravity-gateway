// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// DefaultFetchTimeout defines the default timeout for fetching models when context has no deadline.
const DefaultFetchTimeout = 15 * time.Second

// FetchCatalog queries Google Cloud Code Assist (v1internal:fetchAvailableModels) across configured
// endpoints to retrieve the live model catalog and quota metrics for a project.
func FetchCatalog(ctx context.Context, cfg *config.Config, httpClient *http.Client, accessToken string, projectID string) (*CatalogSnapshot, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultFetchTimeout}
	}

	endpoints := config.DefaultEndpoints()
	if cfg != nil && len(cfg.Endpoints) > 0 {
		endpoints = cfg.Endpoints
	}

	reqPayload, err := json.Marshal(map[string]string{
		"project": projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling catalog request: %w", err)
	}

	headers := config.BuildHTTPHeaders(cfg, accessToken, "")

	var lastErr error
	for _, endpoint := range endpoints {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		targetURL := fmt.Sprintf("%s/v1internal:fetchAvailableModels", endpoint)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqPayload))
		if err != nil {
			lastErr = fmt.Errorf("creating http request for %s: %w", targetURL, err)
			continue
		}

		httpReq.Header = headers.Clone()

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("dispatching request to %s: %w", targetURL, err)
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ctx.Err()
			}
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("reading response from %s: %w", targetURL, readErr)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			return ParseCatalog(respBody)
		}

		errMsg := rotator.ExtractErrorMessage(string(respBody))
		if errMsg == "" {
			errMsg = http.StatusText(resp.StatusCode)
		}

		lastErr = fmt.Errorf("listing models failed on %s (HTTP %d): %s", endpoint, resp.StatusCode, errMsg)
		// 403 (Forbidden) and 404 (Not Found) may occur when endpoint does not serve this service; try next endpoint
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			continue
		}

		// For other non-200 responses (e.g. 401 unauthorized, 400 bad request), break early
		return nil, lastErr
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no cloud code endpoints configured or reachable")
}
