// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package gameday runs the nightly Chaos job's automated drills
// (docs/engineering/03-testing-and-quality-strategy.md "Drills and failure
// rows"): the game days GD-1 to GD-5 as the High availability and disaster
// recovery document's "Game days" rows define them, the failure catalog
// rows FC-16, FC-19 and FC-20 as drills of their own, and the Behavior and
// Detection columns of FC-1, FC-3 and FC-11 on the runs of GD-1, GD-2 and
// GD-4. Every drill builds its runs on the process harness the way the
// chaos experiments of package chaos do (chaos.Harness, chaos.Run) and
// writes one ruralz.chaos.v1 record per run.
//
// The drills are tests under the e2e build tag; the untagged files hold
// the pure parts every platform tests: the drills' arithmetic and the
// harness DNS server of FC-19.
package gameday
