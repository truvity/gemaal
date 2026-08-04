# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via [GitHub Security Advisories](https://github.com/truvity/gemaal/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

| Version | Supported |
|---------|-----------|
| latest  | ✅        |
| older   | ❌        |

## Design notes relevant to a reviewer

- gemaal is a **watcher, not a deployer**: it never installs anything.
  Its only destructive verbs are uninstalling expired test releases and
  sweeping orphaned test artifacts. A compromised or malfunctioning
  gemaal delays cleanup; it cannot deploy workloads.
- Destructive reach is an **allow-list of roots**, not a pattern match:
  a fixed set of namespaces (selected by a tier label), named buckets,
  and parameter-store roots that must live under the test root.
  `/secrets/**` is refused structurally, and configuration that is not
  test-shaped refuses to load.
- **Dry-run is the default.** Executing deletions is a deliberate
  configuration flip, and every destructive action emits a structured
  deletion record.
