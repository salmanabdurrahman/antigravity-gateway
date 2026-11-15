# OAuth 2.0 Client Strategy & Distribution Architecture

This document defines the authorized OAuth 2.0 client strategy, consent-screen requirements, authorization scopes, PKCE protocol mechanics, and token security boundaries for distributed Antigravity Gateway (`agw`) binaries.

## 1. Overview & Architectural Role

Antigravity Gateway acts as a local OpenAI-compatible reverse proxy (`http://127.0.0.1:51400/v1`) that routes requests to Google Cloud Code Assist upstream endpoints (`https://cloudcode-pa.googleapis.com/v1internal:*`).

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
4. **Credential Storage Isolation**:
   - Stored in `~/.agw/accounts.json` with strict POSIX `0600` permissions (`-rw-------`).
   - Atomic rename pattern (`temp file` -> `flush` -> `rename`) prevents corrupted state.
5. **Zero Secret Leakage**:
   - Access tokens, refresh tokens, and client secrets implement string redaction (`fmt.Stringer`).
   - Logging never outputs raw authorization headers or bearer credentials.
   - Status commands (`agw accounts`) mask tokens and display only email, project ID, and status.

## 6. Pre-Distribution Requirements

Before publishing and distributing binary releases:

1. Ensure the default client remains active and valid on Google Cloud Console.
2. Verify that PKCE parameters and loopback redirect URIs continue to match Google's latest desktop app specifications.
3. Verify that upstream endpoint fallbacks (`daily-cloudcode-pa.googleapis.com` and `cloudcode-pa.googleapis.com`) remain accessible with issued access tokens.
