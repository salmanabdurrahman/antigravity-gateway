# Security Policy

## Supported Versions

We provide security updates and patches for the current release stream.

| Version | Supported          |
| :------ | :----------------- |
| 0.2.x   | :white_check_mark: |
| < 0.2.0 | :x:                |

## Reporting a Vulnerability

The Antigravity Gateway (`agw`) project takes security and credentials handling seriously. If you discover a security vulnerability, **please do not report it through public GitHub issues or public discussions**.

### Preferred Disclosure Method

1. **GitHub Private Vulnerability Reporting**:
   - Navigate to the repository's **Security** tab on GitHub.
   - Click **Report a vulnerability** to open a private draft advisory.
2. **Direct Contact**:
   - If GitHub Private Reporting is unavailable, contact the project maintainer directly at `salmanabdurrahmann12@gmail.com` with the subject line `[SECURITY] Antigravity Gateway Vulnerability Report`.

### What to Include

To help us triage and resolve the report quickly, please include:

- Description of the vulnerability and its potential impact.
- Affected component (`internal/auth`, `internal/server`, `internal/rotator`, etc.).
- Steps to reproduce or proof-of-concept (PoC) code.
- Any proposed remediation or mitigation.

### Response & Timeline

- **Acknowledgment**: Within 48 hours of receipt.
- **Triage & Assessment**: Within 5 business days.
- **Coordinated Disclosure**: We adhere to coordinated vulnerability disclosure. A patched release and CVE advisory (if applicable) will be published simultaneously with public attribution to the reporter.

## Core Security Invariants

When evaluating potential vulnerabilities, consider the gateway's core architectural security guarantees:

1. **Loopback Isolation**: The HTTP proxy daemon and OAuth callback server bind exclusively to `127.0.0.1`. Binding to `0.0.0.0` or exposing network ports across local LAN interfaces is strictly prohibited.
2. **Filesystem Permissions**: All account tokens, state files, and configurations stored in `~/.agw/` must be written with strict POSIX permissions (`0700` for directory, `0600` for files) via atomic rename patterns.
3. **Redaction & Zero Secret Exposure**: Authentication tokens (`access_token`, `refresh_token`), bearer keys, and authorization headers must never be logged, printed to standard streams, or leaked into error envelopes.

### Public OAuth Client Credentials

The bundled Google OAuth **desktop** client identifier and secret are public by design, matching RFC 8252 §8.5 and Google's native-app guidance: an installed application cannot keep a client secret confidential, and the value is embedded in every distributed binary.

- Treat an exposed `client_id` / `client_secret` for the bundled desktop client as expected, not as a credential leak.
- User access and refresh tokens remain per-account, obtained through PKCE, and are never shared by the default client.
- Secret-scanning findings for that value should be dismissed as a public-client credential rather than rotated silently; rotation breaks zero-config `agw login`.
- Deployments that require a private client identity should configure BYOC through `AGW_CLIENT_ID` / `AGW_CLIENT_SECRET` or the `client_id` / `client_secret` fields in `config.json`.
- If client-identity abuse is suspected, disable or rotate the OAuth client in Google Cloud Console and publish a release that points at the replacement identity.
