# OAuth 2.0 Client Strategy & Distribution Architecture

This document defines the authorized OAuth 2.0 client strategy, consent-screen requirements, authorization scopes, PKCE protocol mechanics, and token security boundaries for distributed Antigravity Gateway (`agw`) binaries.

## 1. Overview & Architectural Role

Antigravity Gateway acts as a local OpenAI-compatible reverse proxy (`http://127.0.0.1:51400/v1`) that routes requests to Google Cloud Code Assist upstream endpoints (`/v1internal:*` on the daily or production Cloud Code host, ordered by account tier).

To access these endpoints on behalf of an authenticated developer, the gateway must obtain valid Google OAuth 2.0 credentials and associate them with a companion Google Cloud project (`cloudaicompanionProject`) provisioned for the user.

## 2. Google Desktop OAuth 2.0 Compliance

The gateway strictly follows [Google OAuth 2.0 for Desktop Apps](https://developers.google.com/identity/protocols/oauth2/native-app) guidelines:

### 2.1 Public Client Classification & PKCE

Desktop and CLI applications are distributed to end-user workstations and are classified as **public clients** that cannot maintain confidential client secrets.

- **Proof Key for Code Exchange (PKCE)**: In accordance with RFC 7636, every authorization request generates a cryptographically random, high-entropy 32-byte code verifier (URL-safe base64, 43 characters) and derives a SHA-256 challenge (`code_challenge_method=S256`).
- **State Integrity**: A cryptographically random state parameter is generated per request to guard against cross-site request forgery (CSRF).

### 2.2 Loopback IP Redirection

In accordance with Google's deprecation of Out-Of-Band (OOB) redirect flows:

- The gateway opens an ephemeral HTTP loopback listener on `http://127.0.0.1:51121/oauth-callback` (or dynamic port if bound).
- The redirect URI `http://localhost:51121/oauth-callback` is registered with Google authorization servers.
- The authorization flow launches the system's default web browser (avoiding embedded webviews that trigger Google's `disallowed_useragent` policy).
- **Headless & Remote Fallback**: For headless servers or SSH environments, the CLI supports `--no-browser` and allows manual terminal pasting of the callback redirect URL containing the authorization code and state.

## 3. Approved OAuth Client Strategy

The gateway adopts a **Dual-Client Architecture** to satisfy both frictionless out-of-the-box usage and strict enterprise credential isolation:

### 3.1 Tier 1: Default Companion Client (Out-of-the-Box)

- **Application Type**: Desktop Application.
- **Role**: Pre-authorized companion client matching the Antigravity IDE configuration.
- **Client Identifier**: `1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com`
- **Companion Discovery**: Authorized to invoke `POST /v1internal:loadCodeAssist` and `POST /v1internal:onboardUser` to discover or automatically provision the companion cloud project required for Antigravity subscribers.
- **Consent Screen Status**: Validated for Google Antigravity and Google One AI subscribers, bypassing unverified app consent warnings for companion services.

### 3.2 Tier 2: Bring-Your-Own-Client (BYOC / Enterprise Self-Hosted)

For enterprise teams, organizations, or users requiring independent OAuth consent-screen branding and audit trails:

- **Configuration Overrides**:
  - Environment variables: `AGW_CLIENT_ID` and `AGW_CLIENT_SECRET`.
  - Configuration file: `~/.agw/config.json` (`client_id` and `client_secret` fields).
- **Custom Client Setup Requirements**:
  1. Create a Google Cloud Project in the [Google Cloud Console](https://console.cloud.google.com).
  2. Enable the **Cloud Code API** and related companion services.
  3. Configure the **OAuth Consent Screen** with application type **Desktop App**.
  4. Create OAuth 2.0 Credentials selecting **Desktop Application**.
  5. Add `http://localhost:51121/oauth-callback` to Authorized Redirect URIs.

## 4. Authorized Authorization Scopes

The gateway requests the following 5 OAuth scopes during authentication:

| Scope                                                   | Category      | Technical Justification                                                                                                                         |
| :------------------------------------------------------ | :------------ | :---------------------------------------------------------------------------------------------------------------------------------------------- |
| `https://www.googleapis.com/auth/cloud-platform`        | Sensitive     | Mandatory for communicating with Google Cloud Code Assist (`v1internal`) endpoints and dispatching streaming code generation requests.          |
| `https://www.googleapis.com/auth/userinfo.email`        | Non-sensitive | Identifies the authenticated Google account to partition multiple stored accounts, display account status, and target specific account logouts. |
| `https://www.googleapis.com/auth/userinfo.profile`      | Non-sensitive | Extracts account profile metadata for account status display.                                                                                   |
| `https://www.googleapis.com/auth/cclog`                 | Internal      | Required by Cloud Code Assist companion backend for request telemetry and session tracking.                                                     |
| `https://www.googleapis.com/auth/experimentsandconfigs` | Internal      | Required to query model feature flags, experimental capabilities, and companion project tier configurations.                                    |

## 5. Token Exchange & Security Lifecycle

1. **Authorization Code Exchange**: Exchanged against `https://oauth2.googleapis.com/token` using the code verifier generated in Step 2.
2. **Offline Access**: Requests `access_type=offline` and `prompt=consent` during login to ensure a durable `refresh_token` is issued.
3. **Proactive Refresh Engine**: Tokens within 120 seconds of expiration are proactively refreshed before dispatching upstream requests.
4. **Desktop Session Import**: `agw login --local` can reuse the session already signed in to the Antigravity IDE. Desktop sessions expose a short-lived bearer token but no `refresh_token`, so the credential is stored as `access_token` with `credential_source: local_desktop` and is never sent to the token endpoint as a refresh grant. Accounts without a refresh token resolve their bearer token directly, and an expired import reports an actionable re-import message. Re-importing never downgrades an account that already holds a refresh token.
5. **Credential Storage Isolation**:
   - Stored in `~/.agw/accounts.json` with strict POSIX `0600` permissions (`-rw-------`).
   - Atomic rename pattern (`temp file` -> `flush` -> `rename`) prevents corrupted state.
   - The SQLite credential database is only read by an absolute-path `sqlite3` binary, never one resolved through `PATH`.
6. **Account Identity**: every login and desktop import resolves the immutable Google user ID (`sub`) from the `userinfo` endpoint in the same round trip that yields the email address. Stored accounts, duplicate detection, and in-memory rotation state key on that identifier, so a changed or aliased address updates the existing entry instead of appending a duplicate. Entries written before identity capture are matched by email once and backfilled with the user ID; duplicate entries that later share one user ID collapse to the entry whose credentials can still be refreshed.
7. **Zero Secret Leakage**:
   - Access tokens, refresh tokens, and client secrets implement string redaction (`fmt.Stringer`).
   - Logging never outputs raw authorization headers or bearer credentials.
   - Status commands (`agw accounts`) mask tokens and display only email, project ID, and status.
   - Imported payloads are validated (length, control characters) and error messages report only the reason, never the token value.

## 6. Application Default Credentials & Enterprise Scope

`agw` intentionally ships only two credential sources: interactive OAuth 2.0 PKCE login and read-only import of an already signed-in desktop session.

- **Application Default Credentials (ADC) are not supported.** Reading `GOOGLE_APPLICATION_CREDENTIALS`, `gcloud` well-known files, or the GCE/GKE metadata server would add Google auth SDK dependencies, broaden the credential surface the daemon can read, and mix service-account principals into a pool whose quota and companion-project model is user-scoped.
- **Enterprise and Workspace accounts remain supported through the same flows.** Tier metadata from `v1internal:loadCodeAssist` orders upstream endpoints (production Cloud Code first for business and enterprise tiers), and a self-hosted OAuth client can be supplied through `AGW_CLIENT_ID` / `AGW_CLIENT_SECRET` or `client_id` / `client_secret` in `config.json`.
- **Future ADC support must be a separate authentication provider** with its own config surface, explicit opt-in, and secret-redaction audit rather than an implicit fallback inside the existing token refresher.

## 7. Pre-Distribution Requirements

Before publishing and distributing binary releases:

1. Ensure the default client remains active and valid on Google Cloud Console.
2. Verify that PKCE parameters and loopback redirect URIs continue to match Google's latest desktop app specifications.
3. Verify that upstream endpoint fallbacks (`daily-cloudcode-pa.googleapis.com` and `cloudcode-pa.googleapis.com`) remain accessible with issued access tokens. Personal tiers try the daily endpoint first; business and enterprise tiers try the production endpoint first, while an explicit `endpoints` list in `config.json` is kept in its configured order.
