# Steps

The work left in the current milestone, M1 Core gateway, in build order: a row depends only on rows above it. Take the first row and delete it when its work and tests are done and the gates pass. The linked document defines what each row builds; the [Roadmap and milestones](01-roadmap-and-milestones.md) holds the milestone's scope and exit criteria.

| Step | Paths | Design |
|---|---|---|
| The drills' first passing run at the `nightly` profile | `test/chaos/gameday/`, the nightly Chaos job | [Drills and failure rows](../engineering/03-testing-and-quality-strategy.md#drills-and-failure-rows), [Game days](../operations/04-high-availability-and-disaster-recovery.md#game-days) |
| The `target` chaos profile on the four RH-1 hosts | `test/chaos/` (the placement, and RH-1L returning every request's record once the load ends), `internal/statestore/statestoretest/redisserver/` (deployments on RH-1S), `test/bench/labhost/` (process signals and the record return), `internal/testkit/topology/` (Nodes in one cpuset) | [Scale profiles](../engineering/03-testing-and-quality-strategy.md#scale-profiles), [Reference hardware](../architecture/12-performance-budgets-and-benchmarking.md#reference-hardware) |
| RH-1 hosts, runner and first budget runs and soak | `deploy/benchhost/` (its runbook on the rented hosts), `docs/operations/03-capacity-planning.md` (the measured coefficients) | [Reference hardware](../architecture/12-performance-budgets-and-benchmarking.md#reference-hardware), [M1 exit criteria](01-roadmap-and-milestones.md#m1-exit-criteria), [Capacity planning](../operations/03-capacity-planning.md#coefficients) |
| Release `0.1.0`: candidate and cut | The settings `scripts/repo-settings.sh` applies, the `REPO_SETTINGS_TOKEN` secret, the release runbook | [Release runbook](../engineering/04-release-versioning-and-compatibility.md#release-runbook), [Repository settings](../engineering/02-repository-layout-and-conventions.md#repository-settings) |
