# Security Policy

## Supported Versions

We provide security updates and patches for the current release stream.

| Version | Supported          |
| :------ | :----------------- |
| 0.1.x   | :white_check_mark: |
| < 0.1.0 | :x:                |

## Reporting a Vulnerability

The Antigravity Gateway (`agw`) project takes security and credentials handling seriously. If you discover a security vulnerability, **please do not report it through public GitHub issues or public discussions**.

### Preferred Disclosure Method

1. **GitHub Private Vulnerability Reporting**:
   - Navigate to the repository's **Security** tab on GitHub.
   - Click **Report a vulnerability** to open a private draft advisory.
2. **Direct Contact**:
   - If GitHub Private Reporting is unavailable, contact the project maintainer directly at `salmanabdurrahman12345@gmail.com` with the subject line `[SECURITY] Antigravity Gateway Vulnerability Report`.

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
