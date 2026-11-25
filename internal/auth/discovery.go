// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// ErrNoCompanionProject is returned when Google does not return or provision a companion project.
var ErrNoCompanionProject = errors.New("google did not return a cloud code companion project for this account")

// GetUserEmail retrieves the authenticated user's email address from Google's userinfo endpoint.
func GetUserEmail(ctx context.Context, cfg *config.Config, accessToken string) (string, error) {
	userInfoURL := config.DefaultUserInfoURL
	if cfg != nil {
		userInfoURL = cmp.Or(cfg.UserInfoURL, config.DefaultUserInfoURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := config.DefaultHTTPClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling userinfo endpoint %s: %w", userInfoURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo request failed with status %d", resp.StatusCode)
	}

	var data struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return "", fmt.Errorf("decoding userinfo response: %w", err)
	}

	if data.Email == "" {
		return "", errors.New("no email returned by google userinfo")
	}

	return data.Email, nil
}

// ExtractProjectID searches for companion project identifier keys within arbitrary JSON response objects.
func ExtractProjectID(data map[string]any) string {
	if data == nil {
		return ""
	}

	candidates := []string{"cloudaicompanionProject", "projectId", "project"}
	for _, key := range candidates {
		val, exists := data[key]
		if !exists || val == nil {
			continue
		}

		if str, ok := val.(string); ok && strings.TrimSpace(str) != "" {
			return strings.TrimSpace(str)
		}

		if m, ok := val.(map[string]any); ok {
			if idStr, ok := m["id"].(string); ok && strings.TrimSpace(idStr) != "" {
				return strings.TrimSpace(idStr)
			}
		}
	}

	return ""
}

// DefaultTierID extracts the preferred IDE subscription tier ID from loadCodeAssist response payload.
func DefaultTierID(data map[string]any) string {
	if data == nil {
		return "free-tier"
	}

	if tiers, ok := data["allowedTiers"].([]any); ok {
		for _, item := range tiers {
			if m, ok := item.(map[string]any); ok {
				if isDef, ok := m["isDefault"].(bool); ok && isDef {
					if idStr, ok := m["id"].(string); ok && idStr != "" {
						return idStr
					}
				}
			}
		}
	}

	if current, ok := data["currentTier"].(map[string]any); ok {
		if idStr, ok := current["id"].(string); ok && idStr != "" {
			return idStr
		}
	}

	return "free-tier"
}

// TierFromLoadCodeAssist extracts and normalizes the subscription tier reported by
// `v1internal:loadCodeAssist`. Google returns tier metadata in `currentTier`, `paidTier`, or the
// `allowedTiers` collection, and the identifiers vary between consumer and Workspace products, so
// every label is normalized through config.NormalizeAccountTier instead of matched verbatim.
func TierFromLoadCodeAssist(data map[string]any) string {
	if data == nil {
		return ""
	}

	// Paid tier metadata wins over currentTier: entitled Workspace/Enterprise accounts can report a
	// consumer-looking currentTier alongside a paidTier that carries the real endpoint entitlement.
	for _, key := range []string{"paidTier", "currentTier"} {
		if tierMap, ok := data[key].(map[string]any); ok {
			if tier := config.NormalizeAccountTier(strings.Join(tierLabels(tierMap), " ")); tier != "" {
				return tier
			}
		}
	}

	if tiers, ok := data["allowedTiers"].([]any); ok {
		for _, item := range tiers {
			tierMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if tier := config.NormalizeAccountTier(strings.Join(tierLabels(tierMap), " ")); tier != "" {
				return tier
			}
		}
	}

	return ""
}

// tierLabels collects the identifier and display-name fields that may describe a subscription tier.
func tierLabels(tierMap map[string]any) []string {
	labels := make([]string, 0, 3)
	for _, key := range []string{"id", "tierId", "name", "displayName"} {
		if label, ok := tierMap[key].(string); ok && strings.TrimSpace(label) != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

// DiscoverProject finds the Cloud Code companion project bound to the account.
//
// Protocol Quirk: Google Cloud Code Assist requires every API request to specify a companion
// project (`cloudaicompanionProject`) that holds the user's quota. Brand new consumer accounts
// have no project allocated until an explicit `onboardUser` handshake is executed.
// This function first queries `POST /v1internal:loadCodeAssist`. If no companion project is
// found, it triggers `POST /v1internal:onboardUser` using the default subscription tier ID,
// polling until the long-running onboarding operation returns `done: true` with the assigned project ID.
func DiscoverProject(ctx context.Context, cfg *config.Config, accessToken string) (string, error) {
	projectID, _, err := DiscoverProjectAndTier(ctx, cfg, accessToken)
	return projectID, err
}

// DiscoverProjectAndTier resolves both the companion project and the normalized subscription tier
// from the same loadCodeAssist round trip, avoiding a second upstream call during login.
func DiscoverProjectAndTier(ctx context.Context, cfg *config.Config, accessToken string) (string, string, error) {
	endpoints := config.DefaultEndpoints()
	if cfg != nil && len(cfg.Endpoints) > 0 {
		endpoints = cfg.Endpoints
	}

	headers := config.BuildUpstreamHeaders(cfg, accessToken, "")

	var lastLoadData map[string]any
	tier := ""

	// Step 1: Attempt to load existing Code Assist companion project across configured endpoints
	for _, endpoint := range endpoints {
		loadURL := strings.TrimRight(endpoint, "/") + "/v1internal:loadCodeAssist"
		bodyMap := map[string]any{
			"metadata": map[string]string{
				"ideType": "ANTIGRAVITY",
			},
		}

		respData, err := postJSON(ctx, loadURL, headers, bodyMap)
		if err != nil {
			continue
		}

		lastLoadData = respData
		tier = cmp.Or(tier, TierFromLoadCodeAssist(respData))
		if projectID := ExtractProjectID(respData); projectID != "" {
			return projectID, tier, nil
		}
	}

	// Step 2: If loadCodeAssist answered but without a companion project, onboard user
	if lastLoadData != nil && len(endpoints) > 0 {
		tierID := DefaultTierID(lastLoadData)
		onboardURL := strings.TrimRight(endpoints[0], "/") + "/v1internal:onboardUser"

		onboardBody := map[string]any{
			"tier_id": tierID,
			"metadata": map[string]string{
				"ide_type":    "ANTIGRAVITY",
				"ide_version": config.DefaultAntigravityVersion,
				"ide_name":    "antigravity",
			},
		}

		for attempt := range 5 {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return "", tier, ctx.Err()
				case <-time.After(2 * time.Second):
				}
			}

			respData, err := postJSON(ctx, onboardURL, headers, onboardBody)
			if err != nil {
				continue
			}

			if done, ok := respData["done"].(bool); ok && done {
				// The response field can be nested under "response"
				if respMap, ok := respData["response"].(map[string]any); ok {
					if projectID := ExtractProjectID(respMap); projectID != "" {
						return projectID, tier, nil
					}
				}
				if projectID := ExtractProjectID(respData); projectID != "" {
					return projectID, tier, nil
				}
				break
			}
		}
	}

	return "", tier, ErrNoCompanionProject
}

// postJSON is an internal helper that executes an HTTP POST request with a serialized JSON payload
// and custom headers, decoding the response into a generic map structure.
func postJSON(ctx context.Context, url string, headers map[string]string, body any) (map[string]any, error) {
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling request json: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating http post request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")

	client := config.DefaultHTTPClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("endpoint %s answered HTTP %d", url, resp.StatusCode)
	}

	var parsed map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding json from %s: %w", url, err)
	}

	return parsed, nil
}
