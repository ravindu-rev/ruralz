# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub private vulnerability reporting:
[github.com/ravindu-rev/ruralz/security/advisories/new](https://github.com/ravindu-rev/ruralz/security/advisories/new).
Do not open a public issue, pull request or discussion for a suspected vulnerability.

Include what you can: the affected component (`ruralzd`, `ruralz-control`, Ruralz Console, the `ruralz` CLI, a PDK or the Helm chart), the version or commit, a description of the impact, and steps or a proof of concept that reproduce it.

The security response team acknowledges every report within 2 business days (target).

## How a report is handled

1. The team rates severity and privately prepares the fix, a regression test and patches for the release lines the table below names.
2. At disclosure, the patches, signatures, SBOMs and an advisory with a CVE identifier, affected and fixed versions and workarounds publish together, and the fix merges publicly the same day.

| Severity | Examples | Fixed release | Release lines |
|---|---|---|---|
| Critical | Plugin sandbox escape (default rating); authentication bypass; Revision or Plugin signature bypass | Within 7 days of triage (target) | N, N-1, N-2 |
| High | Unauthenticated traffic crashing a Node; secret disclosure on the admin port | Within 30 days (target) | N, N-1, N-2 |
| Medium | Denial of service needing admin credentials; bounded information leaks | Next patch, within 90 days (target) | N, N-1 |
| Low | Hardening gaps without a practical exploit | Next minor | N |

## One fix for everyone

Every user gets a security fix at the same time, through the public signed release. There is no embargoed, private or paid early channel, and commercial support never delivers a private fix. Ruralz is Apache-2.0 with no feature gating ([ADR-0002](docs/adr/0002-apache-2-license-no-feature-gating.md)).

## Supported versions

Ruralz has no release yet. From release `0.1.0`, the release lines above are those of the [Release, versioning and compatibility](docs/engineering/04-release-versioning-and-compatibility.md#security-fix-policy) policy, which owns this process.
