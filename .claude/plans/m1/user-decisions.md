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

## Decisions on 2026-10-10 (YAML parser for the restricted profile)

From the brief `yaml-parser-decision.md` (run `wf_782bab9a-2c7`), answered by the user:

8. Parser: C2. A hand-written parser for exactly the restricted profile in `internal/config/profile`, with no third-party YAML module in production; `go.yaml.in/yaml/v3` becomes a test-only differential oracle, filtered by a checked-in list of known libyaml deviation classes and confined to `internal/config/profile` tests. `github.com/goccy/go-yaml` leaves `go.mod`, depguard and `modpin`. ADR-0020 supersedes ADR-0003 (still to be written; the user types `/new-adr` or asks for it in words, and approves it). The `go.mod` change still needs the user's approval. Stop rule and fallback as in the brief section 4: stop if the second review round confirms more than two blocker or major findings in parser-core code, or the parser's agent cost passes $250 before a clean round; then keep WP-33 as committed meanwhile and move to go-yaml v4 pinned at rc.3.
9. Strict YAML 1.2: the parser refuses what YAML 1.2 forbids even where libyaml-family parsers accept it (a closing `]` or `}` aligned with its key, `"a"#c`, QB6E-shaped quoted lines), with a dedicated message for the closing-bracket case and a loader-reference note before WP-75 records goldens.
10. Reserved directives (`%FOO`) are ignored, as YAML 1.2 says.
11. `docs/_meta` approved: a research addendum in `docs/_meta/research/tooling-and-licenses.md` with the upstream URLs of the brief section 5.5 item 2 (goccy PRs and issues, security page and master commits; v4 issues, main commits and security page; the proxy pages; v4 `docs/plugins.md`), and the corrections of section 6's stale lines (goccy's `010` octal, v4's `%YAML 1.2` refusal).

Lead defaults stated to the user on 2026-10-10, open to change: `MaxDocumentTokens` keeps its name but counts the parser's own nodes, is enforced during the parse, and gets a default measured so a 1 MiB input stays under 256 MiB; production converts each document to `tree.Node` and drops its raw nodes before the next, with a per-file budget that scales with `MaxResources` instead of a flat token bound, re-measured on the production parser; WP-33's review rounds add a hostile-input lens until WP-75. The v4 stale-simple-key fix is not filed upstream unless the user asks.

## Decisions on 2026-10-11 (WP-51 limits, after three review rounds)

From WP-51's eleven contract requests (`runs/w3-s2-wp51-r3.json`), answered by the user; recorded as architecture R-77 and in spec 07 reqs 75, 80, 84, 90 and section 9 item 21:

12. Memory: keep decoded values at 4 times and the decision pass's working set at 8 times `maxRequestBodyBytes` (request side at most 13 times, 130 MiB at the default), and document the measured fit (typical records up to about 70 to 84% of the limit; dense enum or union arrays less) instead of R-63's "typical documents fit at the full limit". The fix round tightens union and recursive charges where sound.
13. Real memory: WP-51 counts the real map, slice and string overhead itself and answers 413 before reserving when the total passes 12 times the limit; `jsonval` (WP-02) and R-63's stop for other consumers stay as they are.
14. Compile and CPU rules approved as WP-51 proposed: nesting limit with a minimum of 16, fan-out over schema paths that repeat no schema, number bounds only where the library parses numbers, `uniqueItems` comparison charges (a valid `uniqueItems` array of more than a few thousand objects is 413 at the default), location-walk work, compile budgets, and a new cap of 2^14 regex program instructions per pattern and 2^17 per document.
15. Performance: 07 req 84's 25 µs p50 and req 90's per-call compile unit become Open questions with the measured figures (about 89 µs; worst compile about 0.5 s and 9 MB), revisited with another validator after M1. The configuration model and performance document rows land at S8 (the configuration model has 2 words of headroom, so S8 trims to fit).
Lead items, no user decision: WP-70 treats an uncoded `Factory.Build` error as internal; WP-81 runs `BenchmarkWorkModel` and `BenchmarkPatternModel` as a calibration gate (`forwards.json`).

## Decisions on 2026-10-12 (quality standard and the open work)

16. Quality standard, recorded in AGENTS.md "Quality standard" and CLAUDE.md for every session: nothing closes with an open finding of any severity; a package is done only when a fresh review reports no findings at all, minor included (no `deferMinor`, which is now false in every run file, and the scripts pass only at zero findings); fix the whole class of each finding, not the instance; give every requirement, finding and class member its own test that fails without the fix, small cases included; inventory a dependency's behavior before bounding it. S1 is reopened (WP-33's five findings, WP-35's and WP-36's minors) and S2 stays open until WP-37, WP-38, WP-39 and WP-51 are at zero.
17. WP-51: no restricted profile. Every keyword and mechanism jsonschema/v6 acts on under draft 2020-12, including the older-draft keywords it still enforces (`dependencies`, `$recursiveRef`, `$recursiveAnchor`), must be bounded and tested. Method: a complete audit of the library (`m1-wp51-audit.js`, output `wp51-library-audit.md`), then one fix pass over every gap class with a test per case, then review rounds until zero findings; the post-fix8 state (`runs/wp51-postfix8.patch`) is the starting point.
18. Order: WP-51; then the minor findings deferred in WP-35 to WP-39; then WP-33 rebuilt with the hand-written parser (C2), which needs ADR-0020 and the `go.mod` change.
19. The wave-1 and wave-2 packages get a fresh review under the quality standard before M1 integration (the per-item list of the 28 wave-2 skips was not kept).

## Decisions on 2026-10-16 (WP-51 audit gap classes, closing S2)

From the library audit's section 4.2 (`wp51-library-audit.md`, run `wf_38c64070-bc2`), answered by the user; recorded as architecture R-77 and in spec 07 reqs 75 and 80:

20. GC-03 and GC-09: one walk-time compile budget of 1 MiB (pointer and URL bytes times their uses, plus count products such as resources x positions and anchors x dynamic anchors), refused with RZ-CFG-005 at the member, value, reference or `$id` that crosses it.
21. GC-04: non-canonical JSON Pointer array indexes in references (`00`, `+0`, `-0`) are accepted as aliases, as the library accepts them; WP-51 keys its repeat counts and seen sets by the decoded value instead of the pointer string, so aliases cannot escape the bounds.
22. GC-05: values of the eight integer keywords above 2^63-1 are clamped to 2^63-1 in a compiled copy of the document (equivalent for every body WP-51 accepts); `Raw` and the digest stay unchanged, and doc.go notes it. Model fields keep presence apart from the value, so no compiled value aliases a sentinel.
23. GC-15 with GC-11: the stack charge gets an absolute ceiling of 128 MiB per request (an eighth of Go's 1 GB maximum), regexp `add()` depth included, answered with 413 `RZ-RT-003` above it.
24. GC-02, GC-07, GC-11 and GC-20: the main fixes, with no new refusals: the walk mirrors exactly what the library compiles, with which base and in which order; anchor-name bytes and the decision wrapper's frames are charged; regexp `add()` depth is charged; the `propertyNames` dynamic-scope deviation is documented and tested.
25. GC-19: `config.schema` is decoded with the configuration's own depth limit, not a separate 64-level rule.
26. Scope of this pass, given with the user's typed go-ahead "add solid fixes/implementations until s2 is completed. including the previous gaps etc.": every audit gap class, round 9's four findings and eight minors (including minors 2 and 4, which no class covers), then the deferred minor findings of WP-35 to WP-39 (WP-35 and WP-36 included, so only WP-33's rebuild keeps S1 open), each package reviewed until a fresh review reports zero findings. WP-33's rebuild waits for ADR-0020.

## Decisions on 2026-10-24 (small steps, step branches and signing)

From the user's message of 2026-10-24: "the roadmap is too broad, split everything into much smaller parts, when the work is started it should be done in a separte branch and merge the changes to develop. to commit you should use the default ed25519 key in .ssh to sign and when a step of roadmap is done all the changes should be merged to develop and clear all other branches."

27. The roadmap is split into much smaller parts: `steps.md` lists every remaining M1 step. Each step changes one package, one part of a package or one small set of documents, is reviewed to zero findings and lands on its own.
28. Work on a step is done on its own branch created from `develop`. When the step is done, all its changes are merged into `develop`, and all other branches are cleared.
29. Commits are signed with the default ED25519 key in `~/.ssh` (`id_ed25519`). On this Linux machine it is `SHA256:6fQ5We1xTxIoII4Zh8tTpYJUbvP8sYixkcXvpjJ6nl8`; on another machine it is that machine's own default key.

Answered by the user the same day, on questions the first review of step 0.01 raised:

30. A step of a package that is split into several steps may land when its own scope reviews at zero findings, while the package's other known findings wait for its later steps. The package counts as done only after its last step. AGENTS.md's quality standard otherwise forbids letting a finding wait for a later pass, so at the user's request the same day it records this as its one exception.
31. Each step updates `docs/` in the same step as the behavior or decision it describes, or adds an Open questions row to the owning document when a decision is needed (AGENTS.md "Sources of truth"). The contract requests the old plan already deferred to S8, including decision 15's configuration model and performance rows, land in the decision steps 3.68 and 3.69.
32. Steps that touch the paths needing two approvals (`go.mod`, `.github/`, `api/`, `pkg/`, `docs/_meta/`, `test/bench/allocgate.json`, `nolicensecheck.allow` and the security-sensitive packages) merge into `develop` like any other step, with no separate approval: "auto merge, it's fine". This covers the `go.mod` approval decision 8 asked for and the earlier rule that `docs/_meta/` changes need the user's explicit approval. Decisions themselves, such as ADRs and the owner choices inside findings, are still the user's.
33. `wip/s2-handoff` stays on origin until the implementations it carries are done, that is until WP-51 and WP-35 to WP-39 have landed (step 3.20); then it is removed, with the S2 run journals, the scratch archive and the stash. Until then it is the one exception to clearing all other branches. The user's words: "no when the implemnetations are done those should be removed".

34. Step 0.01 lands after the fixes of its fifth review round, without a sixth: for this plan-only step the user's agreement stands in for a review that reports zero findings. The five rounds reported 22, 14, 12, 14 and 12 findings, each round new detail, and every one was fixed. A plan issue found later is fixed in the plan commit of the step that meets it.
35. A check this machine cannot run (the Windows and macOS jobs for `internal/cli`, `internal/buildinfo` and `pkg/`, the container State Store leg, suites that need `redis-server` or Docker) runs in CI on the step branch before the step merges: the branch is pushed and `pr-fast` and `pr-full` run on it through `workflow_dispatch` with `all: true`. The user installs and logs in the GitHub CLI on this machine for it.

Lead defaults stated to the user on 2026-10-24, open to change:
- "All other branches" keeps `main`, the default branch, as well as `develop`.
- Branches are named `step/<id>`. A done step lands by fast-forward (`git merge --ff-only`), so `develop` stays linear and every commit on it stays signed.
- A step branch stays local unless the step pauses; then it is pushed so another machine can resume it, and its pause commits are dropped before the step's real commits.
- Step 0.01 parks the S2 work in progress in `wip/` (one patch per package, WP-36's in two), and each package's first step applies its patch; `wip/s2-handoff` stays as a second copy until step 3.20 (decision 33).
- The S2 run journals and scratch archive stay off `develop`, on the machines that have them, until S2 closes (step 3.20).
- Decision 26 ("WP-33's rebuild waits for ADR-0020") comes before the brief's order, so ADR-0020 is written first (step 3.23), as `proposed` if the user chooses so, and takes effect at the switch (step 3.28), which keeps decision 8's fallback open.
- Decision 8's stop rule, with the parser split into steps 3.24 to 3.26: each parser step's second review round decides it; below the threshold the step keeps running rounds until zero findings while the parser steps' total agent usage stays under $250, tallied with `tools/usage.py` after each run.
