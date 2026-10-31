# Security Policy

## Supported versions

The project is in the design/scaffolding phase. Until the first tagged release, only
the `main` branch receives security fixes.

| Version | Supported |
| ------- | --------- |
| `main` (unreleased) | :white_check_mark: |
| Tagged releases | none yet |

The table will be extended once versioned releases exist; older releases will only
receive fixes for critical issues.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
discussions, pull requests or social media.**

Use GitHub **Private Vulnerability Reporting** for this repository:

1. Open <https://github.com/gitlayzer/csi-driver-glusterfs/security/advisories/new>.
2. Describe the issue: affected component (controller, node plugin, `pkg/gluster`,
   `pkg/mounter`, deployment manifests), the driver/image version or commit, the
   Kubernetes and GlusterFS versions, and the impact.
3. Include reproduction steps or a proof of concept, and any suggested fix.

If you cannot use Private Vulnerability Reporting, contact **@gitlayzer** on GitHub
directly and ask for a private channel; do not include details in a public message.

## What to expect

- We aim to acknowledge a report within 5 business days.
- We will investigate, keep you informed of progress, and coordinate a disclosure date
  with you.
- We will credit you in the advisory and the release notes unless you prefer to stay
  anonymous.

## Scope notes

The driver runs with elevated privileges in clusters and talks to the GlusterFS
management plane, so the following classes of issues are relevant:

- Escaping a provisioned subdirectory or volume path (path traversal, missing validation
  of CSI/StorageClass/PV inputs).
- Command execution issues in the gluster CLI / SSH executor paths (arguments must be
  passed as an argv slice, never as a shell string).
- Disclosure of secrets from CSI request `Secrets` maps, cluster configuration or SSH
  keys in logs, errors or metrics.
- Privilege escalation via the node plugin's mount or host-path handling.

When in doubt, report privately; we would rather triage a non-issue than miss a real one.
