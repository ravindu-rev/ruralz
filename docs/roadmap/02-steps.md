# Steps

The work left in the current milestone, M1 Core gateway, in build order: a row depends only on rows above it. Take the first row and delete it when its work and tests are done and the gates pass. The linked document defines what each row builds; the [Roadmap and milestones](01-roadmap-and-milestones.md) holds the milestone's scope and exit criteria.

| Step | Paths | Design |
|---|---|---|
| Release `0.1.0` by the automated release flow: `develop` merges into `main`, and `release.yml` releases that push as `0.1.0` | `release.yml`, the release runbook | [Release runbook](../engineering/04-release-versioning-and-compatibility.md#release-runbook), [Release pipeline](../engineering/04-release-versioning-and-compatibility.md#release-pipeline), [Repository settings](../engineering/02-repository-layout-and-conventions.md#repository-settings) |
