# Security Policy

## Supported Versions

Security fixes are applied to the latest released chart and controller
version. We do not backport fixes to older releases; please track the most
recent release.

| Component | Supported |
| --- | --- |
| Latest release (chart + controller image) | :white_check_mark: |
| Older releases | :x: |

Releases are published as:

- Controller image: `ghcr.io/pinclr/image-patcher-operator` (mirror: `quay.io/pinclr/image-patcher-operator`)
- Helm chart (OCI): `oci://ghcr.io/pinclr/charts/image-patcher`

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report privately via one of:

- GitHub's [private vulnerability reporting](https://github.com/pinclr/image-patcher/security/advisories/new) (preferred), or
- Email **security@pinclr.com** (or **robert@pinclr.com**) with the details.

Please include, as far as you can:

- A description of the issue and its impact.
- Steps to reproduce or a proof of concept.
- Affected version(s) (chart version and/or controller `appVersion`).
- Any suggested remediation.

### What to expect

- **Acknowledgement** within 5 business days.
- An initial assessment and severity classification, and ongoing updates as
  we work toward a fix.
- Coordinated disclosure: we will agree on a disclosure timeline with you,
  publish a GitHub Security Advisory, and credit you (unless you prefer to
  remain anonymous).

Thank you for helping keep image-patcher and its users safe.
