# User decisions at the M1 wave-2 boundary (2026-10-03)

1. ADR-0010: write ADR-0018 superseding ADR-0010 (OpenTelemetry Prometheus exporter with an external producer; otel confinement). Follow .claude/skills/new-adr/SKILL.md, including the docs/_meta manifest, foundation pack section 7 row and section 2 range, docs/README.md and docs/adr/README.md bookkeeping. OQ-observability-16 closes with (a) citing ADR-0018. WP-32's "ADR-0010 amendment" scope item is replaced by ADR-0018.
2. docs/_meta approved:
   a. Research addendum in docs/_meta/research/tooling-and-licenses.md for the 21 CI tooling URLs (triage requests 90/95/98), dated 2026-10-03, with the action SHAs.
   b. Foundation pack section 7 Upgrades row: net.ipv4.tcp_migrate_req complements CBPF steering; hosts SHOULD set it; 3 s linger (target) (OQ-zero-downtime-upgrades-and-hot-reload-12 (a)); section 14 entry.
   Not approved: the Go net/http research addendum (request 170) -> docs stay silent on Go internals.
3. Scale CI host: RH-1 self-hosted (OQ-testing-and-quality-strategy-11 (b)). WP-80 runs chaos-scale.yml on [self-hosted, rh-1] with the RH1_PROVISIONED fallback annotation; WP-98 drops "the larger runner group for chaos-scale"; WP-83 roadmap note "run on a GitHub larger runner" changes to RH-1.
4. ADR-0011: supersede it with a new ADR (ADR-0019) that adds the 50 ms (target) comprehension stop to the CEL cost row; close OQ-configuration-model-21 with (a).
Defaults (lead): no schema $id in M1 (request 92 (a)); ADR-0015 left unchanged (request 169 (a)).
Commit trailers: CLAUDE.md requires Co-Authored-By; commits since wave-2 carry it.

## Decisions on 2026-10-09 (wave-2 completion)

5. OQ-data-plane-18 closes with (b): the admin port's 404 (unknown path) and 405 (method not allowed) problem documents carry two new codes, RZ-RT-020 (404, no admin endpoint at this path) and RZ-RT-021 (405, method not allowed on this admin endpoint), registered like RZ-RT-019 for the admin /tap limit. WP-45 uses them.
6. docs/_meta approved: the manifest per-document adrs lists, the foundation pack section 7 Pending selections row, the research file tidy, the foundation pack Process settings row for RURALZ_STATE_STORE_MAC_KEY_FILE (only opaque entries carry the HMAC tag; counters carry none) and the OQ-traffic-management-and-resilience-16 amendment row (restore "past one minute" and the max(1, 2 x limit / N_published) clamp), each with a section 14 entry.
7. Commits are signed with the user's SSH key and carry no Co-Authored-By trailer (CLAUDE.md).
