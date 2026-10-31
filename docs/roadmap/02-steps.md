# Steps

The work left in the current milestone, M1 Core gateway, in build order: a row depends only on rows above it. Take the first row and delete it when its work and tests are done and the gates pass. The linked document defines what each row builds; the [Roadmap and milestones](01-roadmap-and-milestones.md) holds the milestone's scope and exit criteria.

| Step | Paths | Design |
|---|---|---|
| The drills' first passing run at the `nightly` profile | `test/chaos/gameday/`, the nightly Chaos job | [Drills and failure rows](../engineering/03-testing-and-quality-strategy.md#drills-and-failure-rows), [Game days](../operations/04-high-availability-and-disaster-recovery.md#game-days) |
| Release `0.1.0`: candidate and cut | The settings `scripts/repo-settings.sh` applies, the `REPO_SETTINGS_TOKEN` secret, the release runbook | [Release runbook](../engineering/04-release-versioning-and-compatibility.md#release-runbook), [Repository settings](../engineering/02-repository-layout-and-conventions.md#repository-settings) |
