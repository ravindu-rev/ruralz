# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub private vulnerability reporting:
[github.com/ravindu-rev/ruralz/security/advisories/new](https://github.com/ravindu-rev/ruralz/security/advisories/new).
Do not open a public issue, pull request or discussion for a suspected vulnerability.

Include what you can: the affected component (`ruralzd`, `ruralz-control`, Ruralz Console, the `ruralz` CLI, a PDK or the Helm chart), the version or commit, a description of the impact, and steps or a proof of concept that reproduce it.

The security response team acknowledges every report within 2 business days (target).

## How a report is handled

1. The team rates severity and privately prepares the fix and a regression test, for the supported version below.
2. At disclosure, the fix merges publicly, which releases it with its signatures and SBOMs, and an advisory with a CVE identifier, affected and fixed versions and workarounds publishes with that release the same day.

| Severity | Examples | Fixed release |
|---|---|---|
| Critical | Plugin sandbox escape (default rating); authentication bypass; Revision or Plugin signature bypass | Within 7 days of triage (target) |
| High | Unauthenticated traffic crashing a Node; secret disclosure on the admin port | Within 30 days (target) |
| Medium | Denial of service needing admin credentials; bounded information leaks | Within 90 days (target) |
| Low | Hardening gaps without a practical exploit | Within 180 days (target) |

## One fix for everyone

Every user gets a security fix at the same time, through the public signed release. There is no embargoed, private or paid early channel, and commercial support never delivers a private fix. Ruralz is Apache-2.0 with no feature gating ([ADR-0002](docs/adr/0002-apache-2-license-no-feature-gating.md)).

## Supported versions

Only the latest minor version is supported. Every fix, security fixes included, ships in the next release, and a deployment gets it by upgrading to that release; older versions receive no patches. The [Release, versioning and compatibility](docs/engineering/04-release-versioning-and-compatibility.md#release-cadence-and-support-windows) policy owns this process.

| Version | Supported |
|---|---|
| `0.1`, from release `0.1.0`, the latest minor | Yes: bug and security fixes, in the next release |
| Every older version | No |
