# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| 0.1.x   | Yes                |
| < 0.1   | No                 |

## Reporting a Vulnerability

If you discover a security vulnerability in the OpenClaw Kubernetes Operator, please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

### How to Report

Use [GitHub private vulnerability reporting](https://github.com/stubbi/openclaw-operator/security/advisories/new) to send a detailed report.

### What to Include

- Description of the vulnerability
- Steps to reproduce
- Affected versions
- Potential impact
- Suggested fix (if any)

### Response Timeline

- **Acknowledgment**: Within 48 hours
- **Initial assessment**: Within 5 business days
- **Fix timeline**: Depends on severity
  - Critical: Patch within 7 days
  - High: Patch within 14 days
  - Medium/Low: Next scheduled release

### Disclosure Policy

- We follow coordinated disclosure. Please allow us reasonable time to fix the issue before public disclosure.
- We will credit reporters in the release notes (unless you prefer to remain anonymous).
- We will publish a security advisory on GitHub once a fix is available.

## Security Design

The OpenClaw Operator is built with security as a primary concern:

- **Non-root execution**: All containers run as non-root (UID 1000/65532)
- **Dropped capabilities**: All Linux capabilities are dropped by default
- **Seccomp profiles**: RuntimeDefault seccomp profile enabled
- **NetworkPolicies**: Default-deny with selective allowlisting
- **RBAC**: Least-privilege roles per instance
- **Validating webhook**: Blocks insecure configurations (e.g., running as root)
- **Distroless base image**: Minimal attack surface for the operator container
- **Container signing**: Release images are signed with Cosign
- **SBOM**: Software Bill of Materials generated for each release
- **Dependency scanning**: Automated via Dependabot and Trivy
