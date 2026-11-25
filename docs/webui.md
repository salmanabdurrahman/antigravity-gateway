# Web Dashboard Contract

Reference: `README.md` and `docs/distribution.md` describe installation and daemon lifecycle. This document is the authoritative contract for the embedded dashboard served by the running gateway daemon.

## 1. Scope & Trust Model

The dashboard is a single embedded HTML document (`internal/server/webui/index.html`) compiled into the binary. It is intentionally **read-only and unauthenticated for GET requests** because the daemon binds strictly to `127.0.0.1`:

- Any process running as the same OS user can already read `~/.agw/config.json` (mode `0600`) and the stored credentials, so an authenticated read endpoint would add no real protection.
- Nothing sensitive is rendered: account emails, project IDs, health scores, model metadata, and quota fractions only. Bearer tokens, refresh tokens, and API keys are never serialized into a response.

Mutation is different, and is guarded by a same-origin rule described in §4.

## 2. Endpoints

| Method | Path               | Auth | Purpose                                                          |
| :----- | :----------------- | :--- | :--------------------------------------------------------------- |
| GET    | `/ui`              | None | Embedded dashboard HTML.                                         |
| GET    | `/ui/api/status`   | None | Aggregated account, model, quota, strategy, and version status.  |
| POST   | `/ui/api/strategy` | None | Change the active rotation strategy (same-origin restricted).    |
| POST   | `/ui/api/refresh`  | None | Force a live catalog and quota refresh (same-origin restricted). |

`GET /` redirects browsers (`Accept: text/html`) to `/ui` with `302`; non-HTML clients receive the same health payload as `/health` (`200 application/json`).

### 2.1 `GET /ui/api/status`

Response `200 application/json`:

```json
{
  "version": "0.2.1",
  "strategy": "sticky",
  "accounts": [
    {
      "email": "user@example.com",
      "project_id": "companion-project",
      "disabled": false,
      "requests": 42,
      "rate_limits": 1,
      "health": 100,
      "last_used": "2026-11-16T02:55:00Z",
      "cooldowns": {}
    }
  ],
  "models": [
    {
      "id": "gemini-3-pro-high",
      "displayName": "Gemini 3 Pro (High)",
      "contextWindow": 1000000,
      "maxOutputTokens": 65536,
      "thinking": true,
      "thinkingBudget": 24576,
      "images": true
    }
  ],
  "quotas": {
    "gemini-3-pro-high": {
      "remainingFraction": 0.42,
      "resetTime": "2026-11-16T08:08:12Z"
    }
  },
  "account_quota": {
    "fiveHour": {
      "remainingFraction": 0.0,
      "resetTime": "2026-11-25T07:15:56Z"
    },
    "weekly": {
      "remainingFraction": 0.72,
      "resetTime": "2026-12-01T16:41:38Z"
    }
  }
}
```

Field naming follows the source structs: account snapshots use `snake_case` (`project_id`, `rate_limits`, `last_used`, `cooldowns`, `last_error`) while catalog models and quotas use `camelCase` (`displayName`, `contextWindow`, `maxOutputTokens`, `remainingFraction`, `resetTime`). A quota entry may include an optional `weekly` object with its own `remainingFraction` and `resetTime` when upstream reports a separate weekly allowance. Optional fields are omitted when empty: `health` is only present once the tracker has a score, `last_used`/`last_error` only after the first dispatch or failure, and `quotas` only when upstream reported quota data.

`account_quota` separates the rolling `fiveHour` window from the `weekly` allowance, merged across usable accounts to the most constrained window, and is omitted when upstream reports neither.

Fields are additive: clients must ignore unknown keys. The endpoint performs a best-effort catalog refresh, so the first request after a daemon start may take slightly longer than subsequent ones; use `POST /ui/api/refresh` for an explicit refresh.

### 2.2 `POST /ui/api/strategy`

Request body (maximum 8 KB):

```json
{ "strategy": "hybrid" }
```

Accepted values: `sticky`, `round-robin`, `hybrid`. Anything else returns `400` with the standard API error envelope and leaves the active strategy untouched.

Success returns `200 {"ok":true}`. The daemon applies the strategy in memory immediately and persists only the `strategy` field of the configuration file it was started with (`--config` when supplied, otherwise `~/.agw/config.json`); a persistence failure is logged but never fails the request, because the in-memory strategy is already active.

### 2.3 `POST /ui/api/refresh`

Request body: none. Success returns `200 {"ok":true,"models":<count>}` after a live catalog and quota refresh; an upstream failure returns `502` with the standard error envelope and leaves the previous snapshot in place.

## 3. Loopback Constraints

- The listener refuses non-loopback hosts: any configured `host` that is not `127.0.0.1`, `localhost`, or a loopback IP is coerced back to `127.0.0.1`.
- WebUI endpoints (`/ui`, `/ui/api/status`, `/ui/api/strategy`, `/ui/api/refresh`) validate that incoming HTTP `Host` headers represent a valid loopback interface (`127.0.0.1`, `localhost`, `[::1]`), defending against DNS rebinding attacks. Bracketed IPv6 authorities such as `[::1]:51400` are accepted, so a daemon bound to the IPv6 loopback keeps the dashboard usable. Requests bearing cross-site `Sec-Fetch-Site` headers are rejected with HTTP 403 Forbidden.
- Dashboard requests are unauthenticated, therefore **never expose the port through a tunnel, reverse proxy, or SSH `-R` forward**. If remote access is required, keep the daemon loopback-bound and use an authenticated SSH local forward to your own machine.

## 4. Same-Origin Mutation Rule

`POST /ui/api/strategy` and `POST /ui/api/refresh` reject cross-site writes that a malicious page could issue as a "simple" request without a CORS preflight:

- Every mutation must carry a same-origin signal: either an `Origin` header whose host matches the daemon's own loopback `Host`, or a `Sec-Fetch-Site` value of `same-origin`/`none`. Requests carrying neither are rejected with `403` rather than trusted.
- Cross-site `Sec-Fetch-Site` values and cross-port loopback origins are rejected with `403`.
- `POST /ui/api/strategy` additionally requires `Content-Type: application/json`; other media types receive `415`, so a plain HTML form cannot drive it.

Request bodies are capped at 8 KB via `http.MaxBytesReader`, and malformed JSON returns `400`. The dashboard HTML itself is served with `X-Frame-Options: DENY` and `Content-Security-Policy: frame-ancestors 'none'`.

## 5. Dashboard Behaviour

- Polling refreshes `/ui/api/status` every 5 seconds and pauses while the tab is hidden (`visibilitychange`), refreshing immediately when the tab becomes visible again.
- Feedback for strategy changes and quota refreshes uses an inline toast (`role="status"`, `aria-live="polite"`) instead of blocking dialogs.
- The account table shows per-account request counts, 429 counts, and the sanitized last error, while the quota card renders the rolling five-hour and weekly windows separately.
- When a status refresh fails, the header badge switches to a disconnected state and polling continues, so the dashboard recovers without a manual reload.
- All rendered strings pass through an HTML escaper before insertion, so model IDs and account emails cannot inject markup.

## 6. Verification

```bash
go test ./internal/server/... ./internal/server/webui/...
make check
```

The test suite covers route wiring (`TestUIEndpoints`), same-origin enforcement plus framing headers (`TestUIEndpointsSecFetchSiteAndCrossPortOrigin`), strategy persistence side effects (`TestUIStrategyPersistsToLaunchedConfigPath`), live quota refresh, dashboard metrics and quota-window rendering (`TestWebUIAccountMetricsAndQuotaWindows`), the embedded asset content, and the absence of blocking dialogs.

## 7. Non-Goals

- No authentication layer for GET endpoints (see §1).
- No write access to accounts, tokens, or `config.json` beyond the `strategy` field.
- No external asset loading: the dashboard ships inline CSS and JavaScript with zero network dependencies.
