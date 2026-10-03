# M1 spec, area 10 of 11: the `ruralz` CLI

Design reader output for milestone M1 "Core gateway". Scope: the `ruralz` command framework (standard library `flag`), the M1 commands `ruralz bundle validate|render|diff|build`, `ruralz dev run|tap`, `ruralz node drain|dump`, `ruralz version`, `ruralz completion`, their flags, streams, JSON shapes and exit codes. The engines behind the bundle verbs (loader, canonical form, diff, effective chains) belong to areas 1 and 2; the admin endpoints and the data directory belong to the data plane area. This spec owns the command wiring around them.

Conventions:

- MUST/SHOULD/MAY as in RFC 2119. "(target)" values come from the docs. **(proposed)** marks a value or shape this spec picks where the docs are silent; each one is listed again in section 9.
- Doc abbreviations: **CLI** `docs/reference/01-cli-and-api-surface.md`; **PACK** `docs/_meta/foundation-pack.md`; **CM** `docs/architecture/02-configuration-model.md`; **ZDU** `docs/operations/02-zero-downtime-upgrades-and-hot-reload.md`; **RM** `docs/roadmap/01-roadmap-and-milestones.md`; **DP** `docs/architecture/03-data-plane.md`; **SEC** `docs/architecture/08-security-and-identity.md`; **OBS** `docs/architecture/10-observability.md`; **SO** `docs/architecture/01-system-overview.md`; **CPG** `docs/architecture/04-control-plane-and-gitops.md`; **TOPO** `docs/operations/01-deployment-topologies.md`; **TECH** `docs/engineering/01-tech-stack-and-libraries.md`; **RL** `docs/engineering/02-repository-layout-and-conventions.md`; **TQ** `docs/engineering/03-testing-and-quality-strategy.md`; **RVC** `docs/engineering/04-release-versioning-and-compatibility.md`; **HA** `docs/operations/04-high-availability-and-disaster-recovery.md`.
- Sibling M1 specs in this directory, cited for alignment: **A1** `01-config-load.md` (loader, Environments, diagnostics), **A2** `02-config-revision.md` (canonical form, render, effective chains, diff, dump document), **A4** `04-dataplane-core.md` (data dir, `/readyz`, `/tap`, Drain), **A6** `06-security.md` (admin auth, `RURALZ_FETCH_ALLOW`).
- Existing code (M0): `internal/cli` (`Run`, `ExitOK=0`, `ExitNegative=1`, `ExitNoResult=2`, `ruralz version`), `internal/buildinfo`, `internal/errcode`, `cmd/ruralz/main.go` (wiring with `signal.NotifyContext(SIGINT, SIGTERM)`).

## 1. Scope

| # | M1 item in this area | Source (file, section) |
|---|---|---|
| S1 | `ruralz bundle validate`: `[DIR]`, Bundle flags, `--online`, `--output text\|json`, source-mapped diagnostics | RM "M1 scope" (CLI row); PACK "9. CLI command registry"; CLI "Command table", "Bundle verbs in detail", "Output formats and exit codes"; CM "Validation and diff semantics", "Diagnostics and source map" |
| S2 | `ruralz bundle render`: one Environment's one-file Bundle; `--effective --route NAME`; `--api-version VERSION --output-dir DIR` conversion that MUST yield the same Revision; `--output-file` | RM "M1 scope" (CLI row); CLI "Command table", "Bundle verbs in detail"; CM "Overlays", "Worked example", "Hub-and-spoke conversion"; ZDU "apiVersion migrations" |
| S3 | `ruralz bundle diff FROM TO` from directory, file or admin URL; `--from-env`, `--to-env`; exit 0/1/2 | RM "M1 scope" (CLI row); CLI "Bundle verbs in detail" (source form table), "Diff JSON compatibility"; CM "Diff semantics" |
| S4 | `ruralz bundle build`: Revision digest, `--offline`, `--output-file` (`ruralz.canonical.v1`) | RM "M1 scope"; CLI "Command table"; CM "Canonical form and Revision"; CPG "Building and recording a Revision" |
| S5 | `--env` and `--environments FILE` (local `Environment` files); CLI verbs without `--env` when no Environment exists | RM "M1 scope" (Configuration and CLI rows); CLI "Global flags"; CM "Environment", "Environment substitution" |
| S6 | `ruralz dev run`: local `ruralzd` with Hot Reload, `--secret-overrides`, `--ephemeral-ports` | RM "M1 scope" (CLI, Lifecycle T1 rows); CLI "Local ruralzd launched by the CLI", "Platform support"; TOPO "T1 and T2: development and file mode" |
| S7 | `ruralz dev tap`: stream redacted `/tap` metadata, `--route` client-side filter | CLI "Command table", "Plugin, AI and development verbs", "Ruralz Gateway admin API"; OBS "Debugging tools" |
| S8 | `ruralz node drain` (Linux only): local SIGTERM to the verified lock holder, await exit | CLI "Rollout, promotion and Node verbs", "Platform support"; ZDU "Drain timeline defaults", "In-place handover"; DP "Sources, Last-Known-Good and Drain" |
| S9 | `ruralz node dump`: save `/config/dump`, secrets omitted | CLI "Command table", "Ruralz Gateway admin API"; DP "Admin endpoints"; feature catalog "Configuration dump" |
| S10 | `ruralz version` (version, commit, flavor, apiVersions) | CLI "Command table", "Output formats and exit codes" (OQ-release-versioning-and-compatibility-6 (a)) |
| S11 | `ruralz completion bash\|zsh\|fish\|powershell` | CLI "Command table" |
| S12 | CLI framework: standard library `flag` (OQ-tech-stack-and-libraries-18, decided) with nested nouns, generated completions, no global state | RM "M1 scope" (CLI row "CLI framework"); CLI "Scope and non-goals"; TECH "Concerns not yet selected" |
| S13 | Global flags `--output`, `--output-file`, `--admin`, `--admin-token-file`, `--ca-file`, `--client-cert`, `--client-key` | CLI "Global flags", "API authentication" > "Admin API" |
| S14 | Streams (stdout data, stderr progress), RFC 3339 UTC, `rev-<12 hex>` vs full digest, exit codes 0/1/2/3/130 | CLI "Output formats and exit codes", "Exit codes" |
| S15 | Platform support (Linux, macOS, Windows) | CLI "Platform support"; TECH "Static builds" |
| S16 | "Declarative config and GitOps CLI": the bundle verbs as CI gates | RM "M1 scope" (CLI row); CLI "Worked example: pull request and promotion" (M1 lines) |
| S17 | Tests: every M1 pack 9 command has an end-to-end test; golden `--effective` tables and `ruralz.diff.v1` documents; e2e scenarios 2 and 6 | RM "M1 exit criteria" 5; TQ "Golden tests", "End-to-end tests" |

## 2. Normative requirements

### 2.1 Framework, command tree and dispatch

1. `cmd/ruralz/main.go` MUST only wire: `ctx, stop := cli.SignalContext(context.Background())`, `code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)`, `stop()`, `os.Exit(code)`. All logic lives in `internal/cli/...`. [RL "Where Go code goes"]
2. `ruralz` is one static `CGO_ENABLED=0` binary built for linux/amd64 and linux/arm64 (production), darwin/arm64, darwin/amd64 and windows/amd64 (production CLI). Every file under `internal/cli/...` MUST compile on all five; platform code uses `_linux.go`, `_unix.go` (`linux || darwin`) and `_other.go` build-constrained files. [CLI "CLI command tree"; TECH "Static builds"]
3. Commands take the form `ruralz <noun> <verb> [ARGS] [FLAGS]`; `ruralz version` and `ruralz completion` are the only exceptions; formats (the completion shell) are arguments. M1 registers exactly: `bundle validate`, `bundle render`, `bundle diff`, `bundle build`, `dev run`, `dev tap`, `node drain`, `node dump`, `version`, `completion`. No new noun; no hidden helper command (a completion callback command would be a third command outside the noun-verb form). [PACK "9. CLI command registry"; PACK "12. Naming conventions"]
4. Framework: standard library `flag` (OQ-tech-stack-and-libraries-18, decided). One `*flag.FlagSet` per invocation, `flag.ContinueOnError`, output discarded (the framework prints its own usage). The command table is an explicit value built per call by `cli.Registry()`; no `init()`, no package-level mutable variables (the three `internal/buildinfo` linker variables stay the only exception). [RL "Code conventions" (State)]
5. Interspersed flags: flags MAY appear before, between and after positionals (the docs place them after: CLI "Worked example", CM "Complete annotated example Bundle", HA "Ruralz Control rebuild from Git"). Algorithm: split `args` at the first token equal to `--`; parse the head in a loop (`fs.Parse(rest)`; if `fs.NArg() > 0`, append `fs.Arg(0)` to the positionals and continue with `fs.Args()[1:]`); append every token after `--` verbatim to the positionals. Consequences: `-flag` and `--flag` are equivalent; `--flag=value` and `--flag value` both work; bool flags accept `--flag` or `--flag=true|false`; a lone `-` is a positional; a flag value that is literally `--` MUST be written `--flag=--`. A repeated flag keeps the last value.
6. Help: `-h`, `-help`, `--help` at any level, `ruralz help` and `ruralz help <noun> [<verb>]` print that level's help to **stdout** and exit 0. Help lists usage, summary, positionals and flags in `--name PLACEHOLDER` form (never `flag.PrintDefaults`' `-name`), sorted by name, with defaults and allowed values. **(proposed; existing `version --help` prints to stderr and changes to stdout)**
7. Usage errors (no arguments, unknown noun or verb, noun without verb, unknown flag, missing flag value, wrong positional count, value outside an enum, unparsable duration, conflicting flags) print `ruralz <path>: <problem>` and `Run 'ruralz <path> --help' for usage.` to stderr, nothing to stdout, exit 2. Bare `ruralz` prints root usage to stderr and exits 2 (existing behavior).
8. Commands of pack 9 that are not built in M1 (for example `ruralz bundle push`, `ruralz rollout status`, `ruralz test run`, `ruralz node list`) SHOULD be recognized and exit 2 with `ruralz <noun> <verb> is Planned (<Mx>) and not in this build`; they are absent from help and completion. **(proposed)** [PACK "9. CLI command registry" Planned column]
9. Deprecation: a flag or verb spec MAY carry a deprecation text; using it prints `warning: --<flag> is deprecated: <text>` to stderr; it stays for 2 minor releases (target) before removal. None is deprecated in M1. [CLI "CLI command tree"; RVC "Versioned surfaces" (CLI row)]
10. Platform gate: `ruralz dev run` on Windows and `ruralz node drain` on macOS and Windows exit 2 before any other work, naming the platform and the reason (req 70, req 97). All other M1 commands run everywhere. [CLI "Platform support"]
11. Flags mean the same on every command that accepts them; each global flag is bound by one shared helper. Tokens come only from files (`--admin-token-file`), never flags or environment variables; no environment variable replaces `--admin` (OQ-cli-and-api-surface-2 (a), current). [CLI "Global flags"]
12. Command summary (M1):

| Command | Positionals | Command flags (default) | `--output` values (default) |
|---|---|---|---|
| `bundle validate` | `[DIR]` (`.`) | `--env NAME`, `--environments FILE`, `--online` | `text`, `json` (`text`) |
| `bundle render` | `[DIR]` (`.`) | `--env`, `--environments`, `--output-file PATH`, `--effective`, `--route NAME`, `--api-version VERSION`, `--output-dir DIR` | plain: `yaml`, `json` (`yaml`); with `--effective` or `--api-version`: `text`, `json` (`text`) |
| `bundle diff` | `FROM TO` | `--env`, `--environments`, `--from-env NAME`, `--to-env NAME`, `--admin-token-file PATH`, `--ca-file PATH`, `--client-cert PATH`, `--client-key PATH` | `text`, `json` (`text`) |
| `bundle build` | `[DIR]` (`.`) | `--env`, `--environments`, `--offline`, `--output-file PATH` | `text`, `json` (`text`) |
| `dev run` | `[DIR]` (`.`) | `--env`, `--environments`, `--secret-overrides FILE`, `--ephemeral-ports`, `--ready-timeout DURATION` (`30s`) | `text`, `json` (`text`) |
| `dev tap` | none | `--route NAME`, `--admin URL` (`http://127.0.0.1:9901`, proposed), `--admin-token-file`, `--ca-file`, `--client-cert`, `--client-key` | `text`, `json` (`text`) |
| `node drain` | none | `--data-dir PATH` (`$RURALZ_DATA_DIR`), `--timeout DURATION` (`5m`) | none |
| `node dump` | none | `--admin URL` (`http://127.0.0.1:9901`, proposed), `--admin-token-file`, `--ca-file`, `--client-cert`, `--client-key`, `--output-file PATH` | none (JSON document) |
| `version` | none | none | `text`, `json` (`text`) |
| `completion` | `SHELL` | none | none |

### 2.2 Streams, formats and exit codes

13. Data goes to stdout; progress, warnings, prompts, usage, errors and the logs of a launched `ruralzd` go to stderr. [CLI "Output formats and exit codes"]
14. Timestamps are RFC 3339 UTC with millisecond precision, `2006-01-02T15:04:05.000Z`. Human output shows Revisions as `rev-<12 hex>` (first 12 lowercase hex characters of the digest); JSON always carries the full `sha256:<64 hex>` in a `digest` member (plus `revision` display where listed). [CLI "Output formats and exit codes"; PACK "8.1 Bundle and Revision" (Identifier)]
15. An `--output` value outside the command's list exits 2: `ruralz <path>: unsupported --output "<v>" (want <a> or <b>)`.
16. JSON documents: UTF-8, two-space indentation, `SetEscapeHTML(false)`, one trailing newline, members in the order this spec lists, optional members omitted when absent, empty arrays written `[]` (never `null`). Streams (`dev tap`, `dev run`): one compact object per line (NDJSON), flushed per line. Output MUST be byte-identical on the floor job (Go 1.26, `GOEXPERIMENT=jsonv2`) and the release job. [TQ "Unit and property tests"; TECH "Version floor"]
17. `--output-file PATH` (render, build, node dump): data goes to PATH instead of stdout (build still prints its result to stdout, req 59); written atomically (temp file `.<name>.tmp-<16 hex random>` in PATH's directory, write, `fsync`, `rename`); mode 0644 before umask; an existing file is replaced; a missing parent directory exits 2; nothing is written on a non-zero exit. [CLI "Global flags"]
18. Exit codes (constants in `internal/cli/command`; they never change once released): `0` success with nothing to report; `1` a negative result; `2` no result; `3` waiting on a person (unused in M1, reserved for `rollout` in M2); `130` interrupted by SIGINT. [CLI "Exit codes"]
19. An invalid Bundle is 1 for `validate`, `render` and `build` (their result is validity) and 2 for `diff` (its result is a difference). Warnings (RZ-CFG-013, RZ-CFG-025) never change the code. [CLI "Exit codes"]
20. Per-command mapping:

| Command | 0 | 1 | 2 | 130 |
|---|---|---|---|---|
| `bundle validate` | no error diagnostic | at least one error diagnostic (incl. RZ-CFG-028 with `--online`) | usage; unreadable DIR or file; invalid `--environments` file; unknown `--env` | SIGINT |
| `bundle render` | rendered / table / conversion equal | invalid Bundle; `--api-version` conversion changed a Revision | usage; unreadable input; unknown Route; `--output-dir` not empty; unserved VERSION (RZ-CFG-007) | SIGINT |
| `bundle diff` | no changes | changes | usage; unreadable source; invalid Bundle on either side; M2 source form; admin error; RZ-CFG-027 or RZ-CFG-024 on a dump | SIGINT |
| `bundle build` | Revision built | invalid Bundle (incl. RZ-CFG-028 without `--offline`) | usage; unreadable input | SIGINT |
| `dev run` | `ruralzd` exited 0 on its own | initial render invalid | usage; platform; no `ruralzd`; busy port; not ready by `--ready-timeout`; `ruralzd` exited non-zero | SIGINT or SIGTERM |
| `dev tap` | server ended the stream | none | usage; refused (subscriber limit); 401; connect or TLS error; stream error | SIGINT or SIGTERM |
| `node drain` | signaled process exited | none | usage; platform; no, stale or unverified holder; other PID namespace; signal denied; `--timeout` | SIGINT or SIGTERM |
| `node dump` | saved | none | usage; admin error; RZ-CFG-027 digest mismatch | SIGINT |
| `version`, `completion` | printed | none | usage | none |

21. Signals: `cli.SignalContext` installs `signal.Notify` for `os.Interrupt` and `syscall.SIGTERM` on a buffered channel (capacity 1) served by one goroutine owned by the returned stop function; the first signal cancels the context with cause `command.Interrupted{Signal: s}`; a second SIGINT restores default handling (a third Ctrl-C kills). Both SIGINT and SIGTERM map to 130 after cleanup **(proposed; the docs name SIGINT only, section 9 item 9)**. `signal.NotifyContext`'s cause is an untyped string, so it cannot tell SIGINT from SIGTERM.
22. Errors print one line `ruralz <noun> <verb>: <message>` to stderr, wrapped with `%w` internally. Any `RZ-` code the CLI prints comes from `internal/errcode` (repocheck rejects unregistered literals). CLI-local failures (usage, I/O, platform) carry no RZ code. [RL "Code conventions" (Errors); RL "Ruralz-specific checks"]
23. Admin (server) errors with `--output json` print their RFC 9457 problem document verbatim to stdout and a one-line summary to stderr; in text mode only the summary goes to stderr (req 67). [CLI "Exit codes"]
24. The CLI emits no metrics, spans or `slog` records in M1; it writes human text through `fmt.Fprint*` to the injected writers (`fmt.Print*` to the process streams is allowed only under `internal/cli/`, forbidigo). [RL "golangci-lint linter set" (forbidigo)]

### 2.3 Bundle flags, Environment files, sources and limits

25. `--env NAME` and `--environments FILE` are bound identically on `validate`, `render`, `diff`, `build` and `dev run`. [CLI "Global flags"]
26. Neither flag: `${VAR}` values come from a snapshot of the CLI process environment (`os.Environ()` taken once at command start, passed to the loader; A1 req 22); no overlay applies, even when `overlays/` exists. [CM "Environment substitution" (renderer table); RM "M1 scope": "CLI verbs without `--env` when no Environment exists"]
27. `--env NAME` with `--environments FILE`: overlay `overlays/<spec.overlay>/` (default `metadata.name`), variables only from that Environment's `spec.variables`, never the process environment; an undefined variable without default is RZ-CFG-010 (error diagnostic). `NAME` absent from FILE exits 2 listing the available names (nearest-match hint, A1 req 22). A selected overlay directory that does not exist applies no patches and is not an error **(proposed)**. [CM "Overlays", "Environment"]
28. `--env` without `--environments` exits 2: `--env needs --environments FILE; fetching Environments from Ruralz Control is Planned (M2)`. [CM "Environment substitution" ("with neither source, an error")]
29. `--environments FILE` without `--env`: `bundle validate` runs once per Environment in FILE, in `metadata.name` byte order (A1 req 22), and exits with the worst code (2 > 1 > 0); every diagnostic gains `environment`. FILE without any `Environment` exits 2. `render`, `build` and `dev run` exit 2: `--environments without --env is only supported by bundle validate`; `diff` exits 2 when `--environments` is given and a directory or rendered-file side ends up without an Environment (neither `--env` nor its `--from-env`/`--to-env`). The `--api-version` conversion is the exception (req 47). [CLI "Global flags"; OQ-cli-and-api-surface-1 (a)]
30. The `--environments` file is loaded by area 1 (`loader.LoadEnvironments`): restricted YAML/JSON profile (RZ-CFG-001 to RZ-CFG-004), its own 64 MiB cap, documents of kind `Environment` or `Cluster` only (other kinds RZ-CFG-005), `Cluster` schema-checked then ignored (the CM example `control/environments.yaml` holds both kinds), no substitution, duplicate identity RZ-CFG-008, `spec.overlay` one path segment (RZ-CFG-005), promotion cycles RZ-CFG-022, absent `promotion.from` target RZ-CFG-009 (SHOULD). Any error diagnostic in this file prints to stderr (text form) and exits 2 for every command, because the input is unusable. [A1 req 21; CM "Environment", "Complete annotated example Bundle"]
31. `[DIR]` defaults to `.`. It MAY be a Bundle directory or a single file loaded as a one-file (rendered) Bundle (A1 req 1). A path that does not exist, is neither a directory nor a regular file, or cannot be read exits 2. A directory without `ruralz.yaml` is RZ-CFG-016 (error diagnostic, exit 1 for validate, render, build; 2 for diff). [A1 req 3]
32. Limits: 64 MiB of source text, 20,000 resources and 64 nesting levels (target) per render; exceeding one is RZ-CFG-001. No flag raises them in M1 (OQ-control-plane-and-gitops-25 decides later). At most 10,000 diagnostics per run (A1 req 50). [CLI "Bundle verbs in detail"; CM "Restricted YAML profile"; OQ-configuration-model-18 (a)]
33. Diagnostic text form, one line each (A1 req 49): `<file>:<line>:<column> <severity> <code> <Kind>/<name> <path>: <message>`, then ` (<hint>)`, then ` (<related message> <file>:<line>:<column>)` per related location, then ` [environment=<name>]`; absent parts omitted; `-` when no file. Example: `routes/orders-summary.yaml:23:19 error RZ-CFG-009 Route/orders-summary spec.composition.steps[name=stock].upstream: Upstream "inventroy" not found (did you mean "inventory"?)`. [CM "Diagnostics and source map"]
34. Diagnostic JSON: a bare array (`[]` when clean; OQ-cli-and-api-surface-1 (a)) of objects with members in order `code`, `severity` (`error` or `warning`), `file`, `line`, `column` (1-based), `resource` (`{"kind","name"}`), `path` (array of strings, integers and single-member objects such as `{"name":"stock"}` or `{"item":"request.body.read"}`), `message`, `hint`, `related` (`[{"file","line","column","message"}]`), `environment`; empty members omitted; sorted by `(environment, file, line, column, code, path, message)`. The CLI prints the list in the order area 1 returns. [CM "Diagnostics and source map"; A1 req 50]

### 2.4 `ruralz bundle validate`

35. Runs every offline stage of CM Figure 3 (discover, parse, base union, overlay, substitute, rendered schema, defaults and hub form, references, effective chains and guardrails, CEL compile and cost, inline Plugin spec and Policy `config`, canonicalization) and reports every error in one run. It needs no registry. [CM "Validation and diff semantics"]
36. `--online` adds the online Plugin stage after a green offline run (RZ-CFG-028; signatures RZ-CFG-033). M1 has no OCI client: a Bundle without `Plugin` resources passes the stage trivially; each `Plugin` yields an RZ-CFG-028 error diagnostic `Plugin artifact unavailable: online Plugin check is Planned (M2); use offline validation` (exit 1), aligned with A2 req 56. The stage sits behind the `online.Checker` interface (section 3) so M2 plugs in OCI fetch and Sigstore without touching the command. [CM "Validation and diff semantics"; PACK "8.14 Artifact signing"]
37. Text output: diagnostics (errors and warnings) to stdout, one per line; a clean Bundle prints nothing. JSON output: the bare array on stdout, also on exit 1. Diagnostics are validate's data, so they go to stdout (unlike render and build).
38. Exit: 0 when no `error` diagnostic exists, 1 otherwise; req 29 governs multi-Environment runs.
39. Performance: 10,000 resources in under 2 s and 20,000 in under 4 s on a four-core laptop (target), with the loader's worker count set to `GOMAXPROCS`. [CM "Validation and diff semantics"; A1 req 53]

### 2.5 `ruralz bundle render`

40. Plain render: the full offline pipeline runs first (req 35); on any error diagnostic, diagnostics go to stderr, stdout stays empty, exit 1. Otherwise the rendered Bundle (overlay merged, `${VAR}` substituted, itself a valid one-file Bundle, `$${` escapes written back so rendering it again yields the same Revision and identical bytes) goes to stdout or `--output-file`; warnings go to stderr. Serialization is area 2's `render.WriteBundle` (A2 reqs 44 to 47: canonical kind order then name, `---\n` separators, YAML 1.2-safe quoting). [CM "Overlays"; PACK "8.1" (Rendered Bundle)]
41. `--output yaml` (default) or `json`; JSON is an array of the same resource objects in the same order (A2 req 47; not itself loadable as a Bundle, section 9 item 12).
42. `--effective` requires `--route NAME` and vice versa (exit 2 otherwise); an unknown Route exits 2 with `ruralz bundle render: Route "<name>" not found` plus a nearest-match hint. The Bundle is fully validated first (exit 1 on errors). [CM "Worked example"; A2 req 50]
43. `--effective` text (default): a `text/tabwriter` table (minwidth 0, tabwidth 8, padding 2, space padding) with header `PHASE  LEG  POLICY  FROM  REASON`; rows from `precedence.Chain.Rows()` (A2 req 51): Phases in the order onRequestHeaders, onRequestBody, onRoute, onUpstreamRequest, onUpstreamResponseHeaders, onUpstreamResponseBody, onResponse, onLog, onChunk; client leg first, then Upstream legs in the Route's leg order (composition steps in authored order, else `upstreams` by name); execution order within a leg (pack 8.12: Filter class, scope, list position; response Phases reversed; `onLog` in request order); then removed Policies with Phase and Leg `none`, replaced before excluded. `LEG` is `client` or the Upstream name; `FROM` is `Gateway`, `Route` or `Upstream <name>`. Reason templates are A2's (golden-pinned): `leg <upstream> only`; `own slot[, not overridable]`; `slot <slot>, inherited[, not overridable]`; `slot <slot>, replaces <g>`; `slot <slot>`; `replaced in slot <slot> by <r>`; `excluded by Route`. For the CM example Bundle and `orders-summary`, rows MUST equal CM "Worked example" in Phase, Leg, Policy and From. [CM "Worked example"; PACK "8.12 Policy precedence"]
44. `--effective --output json`: `{"route","revision","digest","environment","rows":[{"phase","leg","policy","type","filterClass","slot","from","reason"}]}` (`environment` omitted without `--env`; A2 req 52). The expected span of a row is `ruralz.filter.<policy>` and none for `onLog` (OBS "Span model"); the text table does not show it (section 9 item 13).
45. `--effective` with `--api-version`, `--output-file` with `--api-version`, or `--output-dir` without `--api-version` exits 2.
46. `--api-version VERSION --output-dir DIR` requires `--environments FILE` (exit 2 otherwise). VERSION MUST be served (M1: `ruralz/v1alpha1` only, from `convert.Registry.Served()`), else RZ-CFG-007 on stderr, exit 2. DIR must not exist or be empty (exit 2). Every discovered base and overlay file is rewritten under VERSION into DIR at the same relative path (`overlays/<env>/` kept, `$patch` and `${VAR}`/`$${` text kept, unmerged, unsubstituted, comments lost per OQ-configuration-model-7 (a), `.json` stays JSON, document order kept), `.ruralzignore` copied, nothing else copied (A2 req 53, `render.ConvertTree`). [CLI "Bundle verbs in detail"; CM "Hub-and-spoke conversion"]
47. Then, for every Environment in FILE (or only `--env` when given), both trees are rendered and built; stdout gets one line per Environment, `environment <name>: <rev-src> -> <rev-dst> equal` or `... differ`, with the human diff of a differing pair on stderr. JSON: `{"apiVersion","outputDir","environments":[{"environment","from":{"revision","digest"},"to":{"revision","digest"},"equal"}]}` **(proposed)**. Exit 0 when every pair is equal, 1 otherwise, 2 on usage or I/O. In M1 the identity conversion MUST still run the full rewrite-and-compare path (A2 req 54). [RM "M1 scope": "`--api-version` conversion MUST yield the same Revision"; ZDU "apiVersion migrations" step 2]
48. `--environments` without `--env` exits 2 for plain and `--effective` render (req 29).

### 2.6 `ruralz bundle diff`

49. Exactly two positionals, FROM then TO (exit 2 otherwise). `--admin` is **not** registered on diff in M1: a Node is named by its admin URL as a positional (section 9 item 6). [CLI "Bundle verbs in detail"; CM "Diff semantics"]
50. Source classification, in order:
    1. prefix `http://` or `https://`: a live Node's admin base; the CLI fetches `<base>/config/dump` (req 64).
    2. prefix `oci://`: an OCI Revision, Planned (M2): exit 2 `ruralz bundle diff: Revision sources are Planned (M2)`.
    3. `^rev-[0-9a-f]{12}$` or `^sha256:[0-9a-f]{64}$`: a Revision from Ruralz Control, Planned (M2): same message, exit 2 (a local path with such a name is written `./rev-...`).
    4. otherwise a filesystem path: a directory is a source Bundle; a regular file is sniffed by content (A2 req 58): a JSON object whose `format` is `ruralz.canonical.v1` is canonical content (digest computed); a JSON object with members `content`, `digest` and `revision` is a saved `/config/dump` document (verified, req 51); anything else is a rendered one-file Bundle. A missing or unreadable path exits 2.
51. Dump and canonical sources are verified by area 2 (`canonical.Decoder.ReadDump` / `Verify`): content that does not hash to `digest` is RZ-CFG-027 and a field above the served schema level is RZ-CFG-024; both exit 2. `--env` does not apply to them and is ignored for that side; `--from-env`/`--to-env` naming a dump, canonical or admin side exits 2.
52. Directory and rendered-file sides render with their Environment: `--from-env` / `--to-env` override `--env` per side (each needs `--environments`, req 28); with no Environment a side uses the process environment snapshot (req 26). An invalid Bundle on either side prints diagnostics (text) to stderr and exits 2. FROM and TO load concurrently in two goroutines joined before comparison (A2 "Concurrency model").
53. Side descriptors (JSON `from`/`to`, area 2's `diff.SideRef`): `revision` (`rev-<12 hex>`), exactly one of `bundle` (DIR as typed), `file` (path as typed) or `admin` (URL as typed), `environment` when that side was rendered with one, and `digest` (full). Top-level `environment` appears when both Bundle sides used the same Environment, or when only one side is a Bundle and it used one. [CLI "Diff JSON compatibility"; A2 req 69]
54. Output: text is area 2's human form (`diff.WriteText`; header `ruralz bundle diff: <rev-from> -> <rev-to>` plus ` (environment: <env>)` or ` (environment: <from> -> <to>)`; `no changes` when empty); `--output json` is `ruralz.diff.v1` (`diff.WriteJSON`). [CM "Diff semantics"; A2 reqs 69, 70]
55. `ruralz.diff.v1` compatibility (owned by CLI): additive changes only; consumers MUST ignore unknown members; field ops stay `add`, `remove`, `replace`, `move` with zero-based positions in JSON; a changed meaning needs `ruralz.diff.v2`; `from` and `to` carry `digest` beside `revision`. [CLI "Diff JSON compatibility"]
56. Exit 0 when the diff is empty, 1 when it has changes, 2 on errors. `diff(A, A)` exits 0; `diff(A, B)` is empty exactly when the Revisions are equal, so `ruralz bundle diff DIR https://node:9901 --env prod --environments FILE --admin-token-file T` exiting 0 means "this commit is served". [CM "Diff semantics"; HA "Ruralz Control rebuild from Git" step 4; TQ "Required properties" (Diff)]
57. Credentials for admin URL sides come from `--admin-token-file`, `--ca-file`, `--client-cert`, `--client-key` (req 62 onward); they apply to both sides.

### 2.7 `ruralz bundle build`

58. Builds the Revision: full offline pipeline, `ruralz.canonical.v1` canonicalization, SHA-256 digest (areas 1 and 2); then the online Plugin stage unless `--offline` (req 36 semantics; RZ-CFG-028 per `Plugin` in M1, exit 1). The online stage never changes the digest. [CM "Canonical form and Revision", "Validation and diff semantics"; PACK "9" (`bundle build` row)]
59. Text output: one stdout line `rev-<12 hex> sha256:<64 hex>`; diagnostics (warnings; errors on failure) to stderr. JSON output (stdout, also on exit 1): `{"digest","revision","environment","diagnostics"}`; `environment` omitted without `--env`; `digest` and `revision` omitted when the build failed; `diagnostics` always present (`[]` when clean, warnings included). [CLI "Output formats and exit codes"; A2 req 55]
60. `--output-file PATH` writes the exact canonical bytes whose SHA-256 is the digest (atomic, req 17), only on success. [CLI "Command table"]
61. Exit 0 built, 1 invalid, 2 usage or unreadable input; `--environments` without `--env` exits 2.

### 2.8 Admin client flags and transport (`dev tap`, `node dump`, `bundle diff` URL sides)

62. `--admin URL`: scheme `http` or `https`, a host, optional path prefix; query, fragment or userinfo exit 2. Endpoint URL = base path without trailing `/` + `/config/dump` or `/tap`. Default for `dev tap` and `node dump`: `http://127.0.0.1:9901` **(proposed)** (9901 is the default `Gateway.spec.admin.port`). [CLI "Global flags"; PACK "8.4"]
63. `--admin-token-file PATH`: read whole (at most 4 KiB, proposed), surrounding whitespace trimmed, empty exits 2; sent only as `Authorization: Bearer <token>`; never printed, logged or placed in an error. On Unix the CLI SHOULD warn when the file grants any group or other permission **(proposed)**. [CLI "API authentication" > "Admin API"; SEC "Admin ports"]
64. Cleartext rule: over `http://` a token or client certificate is sent only when the dialed peer is loopback. The transport's `DialContext` resolves the host and refuses (before connecting) any address outside 127.0.0.0/8 and ::1 when a token is configured: exit 2 `refusing to send the admin token in cleartext to <addr>; use https`. `--client-cert` with an `http://` URL exits 2. Without credentials, `http://` to any host is allowed (only `/healthz` and `/readyz` answer without them). [CLI "Admin API": "never in cleartext beyond loopback"; SEC "Admin ports"]
65. TLS: trust = system pool plus every certificate in `--ca-file` (PEM; a file with no certificate exits 2); `--client-cert` and `--client-key` both or neither (exit 2), loaded with `tls.LoadX509KeyPair`; minimum TLS 1.2 (Go client default); HTTP/2 allowed; no option disables verification. [CLI "Global flags"]
66. Proxies: `http.ProxyFromEnvironment` (Go never proxies loopback). Redirects are never followed: a 3xx exits 2 naming `Location`. Timeouts **(proposed)**: dial 5 s, TLS handshake 10 s, response headers 30 s; `/config/dump` total 30 s and body cap 256 MiB (A2 req 58); problem documents capped at 64 KiB; `/tap` has no total timeout.
67. Non-2xx responses: parse `application/problem+json` with `internal/problem` (members `type`, `title`, `status`, `detail`, `code`, `requestId`); stderr `ruralz <path>: <status> <title>: <detail> (code <code>, requestId <id>)` (absent parts omitted); req 23 for JSON mode; exit 2. A 401 adds `hint: /tap, /config/dump and /debug/* need the operator token or a client certificate`. [CLI "Admin API conventions"; SEC "Admin ports"]
68. `node dump` and `dev tap` need the operator token or a client certificate: with neither `--admin-token-file` nor `--client-cert`, exit 2 before any request **(proposed)**. [CLI "API authentication" table]
69. Unknown JSON members in `/config/dump`, `/readyz` and `/tap` payloads are ignored (skew: CLI and Nodes within the oldest Node's minor or one newer). [RVC "Control plane and data plane version skew" (CLI and Nodes row)]

### 2.9 `ruralz dev run`

70. Platforms: Linux (as tagged), macOS (development only, like `ruralzd`); Windows exits 2: `ruralz dev run is not planned on windows: there is no ruralzd build; use WSL`. [CLI "Platform support"]
71. `ruralzd` lookup: the directory of `os.Executable()` (symlinks resolved) joined with `ruralzd` when it is a regular executable file, else `exec.LookPath("ruralzd")`, else exit 2 `ruralzd not found beside ruralz or on PATH`. [CLI "Local ruralzd launched by the CLI"]
72. Private directory `os.MkdirTemp("", "ruralz-dev-*")`, mode 0700, **(proposed layout)**: `config/ruralz.yaml` (the render; `RURALZ_CONFIG=<tmp>/config`, a rendered Bundle directory), `data/` (`RURALZ_DATA_DIR`, temporary), `admin/token` (`RURALZ_ADMIN_TOKEN_FILE`, mode 0600, 32 bytes from `crypto/rand` encoded base64url without padding), `secrets/` (`RURALZ_SECRET_ROOT`, created only with `--secret-overrides`, files 0600). `secrets/` never contains `data/` or `admin/` (SEC "Secrets" rule 5; A4 req 3). The directory is removed on every exit path (deferred); only a SIGKILL of the CLI leaks it. [CLI "Local ruralzd launched by the CLI" (Node state row); OQ-security-and-identity-7 (a), -22 (a)]
73. Child environment **(proposed)**: the CLI's environment minus every `RURALZ_*` variable except `RURALZ_STATE_STORE_URL`, `RURALZ_SECRET_*` (without `RURALZ_SECRET_ROOT`), `RURALZ_LOG_LEVEL` and `RURALZ_FETCH_ALLOW`; plus `RURALZ_CONFIG`, `RURALZ_DATA_DIR`, `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_SECRET_ROOT` (inherited value when no `--secret-overrides`, else `<tmp>/secrets`) and `RURALZ_FETCH_ALLOW` (inherited value, else `127.0.0.0/8,::1` so loopback mocks work, A6 rule 85 and risk 9). `RURALZ_ADMIN_TLS_DIR` and `RURALZ_ADMIN_METRICS_TOKEN_FILE` are not passed, so admin is plain HTTP with the operator token accepted from loopback.
74. Rendering: DIR is rendered for `--env` (from `--environments`) or from the process environment snapshot, and validated offline. The first render failing exits 1 with diagnostics on stderr **(proposed)**; `ruralzd` is not started. A render is written to `config/.ruralz.yaml.tmp-<random>`, `fsync`ed and renamed over `config/ruralz.yaml`; the Node treats the changed inode of a one-file Bundle as an immediate change (A4 req 59). [CLI "Local ruralzd launched by the CLI" (Rendering row); TOPO "T1 and T2"]
75. Watching (no watcher library is catalogued, so polling **(proposed)**): every 500 ms the CLI fingerprints DIR (every regular file not under a hidden path: slash relative path, size, mtime in ns, mode), the `--environments` file and the `--secret-overrides` file and its targets; a changed fingerprint starts a 250 ms settle (re-poll until two consecutive fingerprints agree, at most 5 s), then one re-render. Renders run one at a time on the supervisor goroutine; changes during a render coalesce into one follow-up render. A failing re-render prints diagnostics to stderr and a `renderFailed` event, swaps nothing, and `ruralzd` keeps its active Revision. [CLI "Local ruralzd launched by the CLI": "re-renders on each source change, swapping the file by atomic rename; ruralzd Hot Reloads it, keeping its active Revision if a render fails"]
76. Ports without `--ephemeral-ports`: before launch every listener `port` and the admin port (`Gateway.spec.admin.port`, default 9901) is test-bound with `net.Listen("tcp", ":<port>")` (no `SO_REUSEPORT`, so a socket held by another `ruralzd` with `SO_REUSEPORT` still fails) and closed; `EADDRINUSE` exits 2 `port <p> (listener <name>) is busy; use --ephemeral-ports`. Bind addresses are unchanged (OQ-cli-and-api-surface-11, current: no loopback setting). [CLI "Local ruralzd launched by the CLI" (Ports, Bind address rows)]
77. `--ephemeral-ports`: each listener port and the admin port get a free port from `net.Listen("tcp", ":0")` (closed before launch), written into the rendered resources (`spec.listeners[name=<n>].port`, `spec.admin.port`); the mapping prints on stdout before launch: `listener <name>: <old> -> <new>` per listener in name order, then `admin: <old> -> <new>`. Re-renders reuse the mapping; a new listener gets a new free port. [CLI "Local ruralzd launched by the CLI" (Ports row)]
78. `--secret-overrides FILE` **(format proposed)**: a YAML mapping (area 1 restricted profile) from `"<provider>:<name>"` (provider `env`, `file`, `kubernetes` or `vault`; the `secret.Ref.String()` form of A1) to a local path relative to FILE's directory. Each target is copied to `secrets/<first 16 hex of sha256("<provider>:<name>")>` (0600); every rendered `secretRef` with that provider and name becomes `{provider: file, name: <absolute path under secrets/>, key: <unchanged>}`. A missing or unreadable target, a malformed FILE or an unknown provider exits 2; an entry matching no reference warns on stderr; file contents are never printed or logged. Any other reference that cannot resolve keeps `/readyz` at 503: exit 2 with RZ-CFG-026 (req 81). [CLI "Local ruralzd launched by the CLI" (Secrets row); OQ-security-and-identity-22 (a)]
79. Digests: the Revision digest is the digest of the unmodified render; when ports or secrets were rewritten, the digest of the rewritten file is reported as `testDigest` beside it. Text: `revision rev-<12> sha256:<64>[ testDigest sha256:<64>]`. [CLI "Local ruralzd launched by the CLI" (Digest row)]
80. Launch: `exec.Command(<ruralzd>)` with no arguments (A4 req 4), stdin `/dev/null`, stdout and stderr copied to the CLI's stderr; own process group (`SysProcAttr.Setpgid`) so a terminal Ctrl-C reaches only the CLI; on Linux `Pdeathsig: SIGTERM`. After start, stdout gets `ruralzd pid <pid>; admin http://127.0.0.1:<adminport>; admin token file <path>`, and stderr gets a hint line with the exact `ruralz dev tap --admin ... --admin-token-file ...` command. [CLI "Local ruralzd launched by the CLI" (Node state row)]
81. Readiness: poll `GET http://127.0.0.1:<adminport>/readyz` every 250 ms **(proposed)** until 200; then stdout `ready`. After `--ready-timeout` (default 30 s, target; flag added to `dev run` since the readiness rule covers it, section 9 item 7) the CLI prints the last 503 body's reasons to stderr (A4 req 61: `{"status":"not_ready","reasons":[{"reason","code","detail"}]}`, e.g. `secrets_unresolved RZ-CFG-026 ...`), stops the child (req 83) and exits 2. A child that exits before ready exits 2 with its status. [CLI "Local ruralzd launched by the CLI" (Readiness row)]
82. After each swap the CLI SHOULD confirm activation by polling `/config/dump` with the dev token every 250 ms for up to 10 s **(proposed)**: `digest` equal to the swapped file's digest (`testDigest` when rewritten) prints `active rev-<12>`; otherwise a stderr warning `ruralzd did not activate rev-<new>; it serves rev-<old>`.
83. Stop: on SIGINT or SIGTERM the CLI sends SIGTERM to the child, waits up to 35 s (the 30 s Drain exit bound plus 5 s margin, target), then SIGKILL; removes the private directory; exits 130. A child that exits on its own ends the CLI with 0 (child 0) or 2 (non-zero or signaled), after cleanup. [ZDU "Drain timeline defaults"; CLI "Exit codes" (130 for `dev run`)]
84. JSON output (`--output json`) **(proposed)**: NDJSON events on stdout, each with `time` and `event`: `ports` (`listeners:[{"name","from","to"}]`, `admin:{"from","to"}`), `started` (`pid`, `admin`, `adminTokenFile`), `revision` (`revision`, `digest`, `testDigest`), `ready`, `active` (`digest`), `notActivated` (`digest`, `active`), `renderFailed` (`diagnostics`), `exited` (`code`).
85. Plugins: nothing special in M1. OQ-wasm-plugin-system-7 (a) (Plugins only by digest from a registry, signatures under `warn`) lands with Plugins in M2 through `RURALZ_TRUST_POLICY_FILE`. [CLI "Plugin, AI and development verbs"]

### 2.10 `ruralz dev tap`

86. `GET <admin>/tap` with the admin credentials; 200 starts a stream of newline-delimited JSON objects, one per sampled exchange (`application/x-ndjson`, A4 req 75). [CLI "Admin API conventions"; DP "Admin endpoints"]
87. `/tap` admits at most 4 subscribers with 1 MiB buffers (target); any non-200 (a fifth subscriber gets 503) exits 2 with the problem document (req 67). [CLI "Plugin, AI and development verbs"]
88. Dropped events: the CLI warns on stderr `warning: <n> tap events dropped (slow subscriber)` for every stream line that is an object with an integer member `dropped` **(proposed; requires the data plane to emit such a line before the next delivered event after drops, section 9 item 4)**; the line is not forwarded to stdout.
89. `--route NAME` filters client-side: only objects whose `route` equals NAME are printed. The Node's cost is the same, about 2 µs per sampled event (target). [CLI "Plugin, AI and development verbs"]
90. Text output, one line per exchange, from A4 req 75 fields: `<time> <method> <host><path> <status> <durationMs>ms route=<route> upstream=<upstream> consumer=<consumer> code=<code> traceId=<traceId>`; absent fields omitted; `time` reformatted per req 14. Header maps are never printed in text mode. JSON output: each accepted exchange line re-emitted verbatim (unknown members kept). Lines longer than 1 MiB are skipped with a stderr warning. [OBS "Debugging tools"]
91. Redaction is the Node's (`/tap` redacts credentials, TB-9); the CLI adds no value. AI bodies appear only as sizes and token counts (SHOULD, M3). [SO "Trust boundaries" TB-9; SEC "Secrets" rule 2]
92. End: server EOF exits 0; a read error exits 2; SIGINT or SIGTERM closes the response body, flushes stdout and exits 130. [CLI "Exit codes"]

### 2.11 `ruralz node drain`

93. Implements OQ-data-plane-4 option (b) for M1: a local SIGTERM starts a Drain, like a process manager. Only the verified lock holder of the data directory is signaled. A remote drain is OQ-cli-and-api-surface-4 (current (b): local only). [CLI "Rollout, promotion and Node verbs"; ADR-0015]
94. `--data-dir PATH`: default `$RURALZ_DATA_DIR`, else `/var/lib/ruralz` (the data plane's proposed `ruralzd` default, A4 req 2; shared constant `nodedir.DefaultRoot`); made absolute with `filepath.Abs`. `--timeout DURATION` default 5m (target), must be > 0 (exit 2).
95. Holder record (OQ-cli-and-api-surface-10 (a), data plane owned, A4 req 5): the holder keeps `flock(LOCK_EX)` on `${RURALZ_DATA_DIR}/lock` and, right after taking it, atomically writes `${RURALZ_DATA_DIR}/holder.json`: `{"format":"ruralz.holder.v1","pid":<int>,"startTime":<uint64, /proc/<pid>/stat field 22, clock ticks since boot>,"nodeId":"<ULID>","version":"<version>","pidNamespace":"<readlink /proc/self/ns/pid, e.g. pid:[4026531836]>"}`. `pidNamespace` is an additive member this area requires (section 4). The CLI reads the record with `nodedir.ReadHolderAt` and never creates or locks anything.
96. Verification, in order; each failure exits 2 with the reason (CLI: "No holder, a stale record, another PID namespace or a denied signal exits 2"):
    1. `holder.json` missing: `no lock holder recorded in <dir>`; unparsable or `format` not `ruralz.holder.v1`: `unreadable holder record`.
    2. `pidNamespace` present and different from the CLI's `readlink /proc/self/ns/pid`: `the lock holder runs in another PID namespace`.
    3. `os.FindProcess(pid)`; on Linux 5.3 and newer this opens a pidfd, pinning the process identity for the later signal.
    4. `/proc/<pid>/stat` absent, its state `Z` or `X`, or its `starttime` different from `startTime`: `stale holder record (pid <pid> is not the recorded process)`. `comm` is skipped by parsing after the last `)`.
    5. `/proc/locks` MUST hold a line (not a `->` waiter) of class `FLOCK`, mode `WRITE`, whose PID equals `pid` and whose inode (the decimal third part of `MAJ:MIN:INODE`, major and minor in hex) equals the inode of `stat(<dir>/lock)`: else `no verified lock holder`. The device is not compared (btrfs subvolumes and overlayfs report `st_dev` differently from the superblock device; section 9 item 5).
    6. Step 4 is repeated after step 5 to close the race with PID reuse.
97. Platforms: Linux only. macOS exits 2 `ruralz node drain is not planned on darwin: it needs /proc/locks`; Windows exits 2 `ruralz node drain is not planned on windows: there is no ruralzd and no SIGTERM`. [CLI "Platform support"]
98. Signal: `p.Signal(syscall.SIGTERM)` to that process only; `EPERM` exits 2 `signal denied`; `os.ErrProcessDone` exits 2 as stale. stderr: `ruralz node drain: sent SIGTERM to ruralzd pid <pid> (node <nodeId>); waiting up to <timeout>`.
99. Await: poll `/proc/<pid>/stat` every 100 ms **(proposed)** until it is absent, its `starttime` changed, or its state is `Z`/`X`; then stderr `ruralzd pid <pid> exited after <duration>` and exit 0. During a Zero-Downtime Upgrade the lock moves to the successor at Drain start; the CLI still awaits the signaled PID. `--timeout` elapsed exits 2 `pid <pid> still running after <timeout>`; SIGINT exits 130 while the Node keeps draining. Nothing is written to stdout. [CLI "Rollout, promotion and Node verbs"; ZDU "In-place handover" step 7]
100. Expected Node timeline after SIGTERM (for test assertions; owned by ZDU): `/readyz` 503 at 0 s, accept window 5 s, stop accepting at 5 s, Drain deadline 25 s, exit by 30 s (target); systemd `TimeoutStopSec` 40 s. Under systemd operators use `systemctl stop`, under Kubernetes Pod deletion. [ZDU "Drain timeline defaults"; CLI "Rollout, promotion and Node verbs"]

### 2.12 `ruralz node dump`

101. `GET <admin>/config/dump` with the admin credentials; the body is A2 req 73's document, the RFC 8785 serialization of `{"content":<canonical document>,"digest":"sha256:…","lastKnownGood":"sha256:…","revision":"rev-…"}` (`lastKnownGood` omitted when none), secrets omitted, `secretRef` shown. [DP "Admin endpoints"; A2 req 73]
102. The CLI verifies the body with `canonical.Decoder.ReadDump` (limit 256 MiB): content not hashing to `digest` is RZ-CFG-027 (exit 2); then writes the **received bytes unchanged** to stdout or `--output-file` (atomic), so the file is a valid `bundle diff` source (A2 req 74). stderr: `saved rev-<12> (lastKnownGood rev-<12>)`. [CLI "Command table"]
103. `node dump` has no `--output` flag: the document is JSON already.

### 2.13 `ruralz version`

104. Unchanged M0 behavior: text is aligned `key: value` lines (`version`, `commit`, `flavor`, `apiVersions`); `--output json` is `{"version","commit","flavor","apiVersions"}`. `pluginAbi` and `controlStream` are omitted until Plugin ABI v1 and the Control Stream are served, Planned (M2). No positionals. [CLI "Output formats and exit codes" (OQ-release-versioning-and-compatibility-6 (a))]
105. `apiVersions` lists the served configuration apiVersions from one source (`buildinfo.APIVersion` MUST equal `convert.Registry.Served()`; a unit test asserts it).

### 2.14 `ruralz completion`

106. `ruralz completion SHELL`, SHELL one of `bash`, `zsh`, `fish`, `powershell` (exactly one positional; else exit 2), writes a static script to stdout, exit 0. [CLI "Command table"]
107. The script is generated at run time from `cli.Registry()` (no build-time generation, no callback command): nouns, verbs, every flag in `--name` form per command, enum values (`--output` values per command and mode, SHELL values), file completion for `FILE`/`PATH` values and `FROM`/`TO`, directory completion for `DIR`/`--output-dir`/`--data-dir`. Planned-only and deprecated entries are excluded.
108. Per shell: bash, a `_ruralz` function over `COMP_WORDS`/`COMP_CWORD` registered with `complete -o bashdefault -o default -F _ruralz ruralz` (works with bash 4.2+); zsh, a `#compdef ruralz` function using `_arguments` and `_describe`; fish, `complete -c ruralz ...` lines with `__fish_seen_subcommand_from` conditions; PowerShell, `Register-ArgumentCompleter -Native -CommandName ruralz -ScriptBlock {...}` returning `[System.Management.Automation.CompletionResult]` objects. Help text gives install lines: `source <(ruralz completion bash)`; `ruralz completion zsh > "${fpath[1]}/_ruralz"`; `ruralz completion fish > ~/.config/fish/completions/ruralz.fish`; `ruralz completion powershell | Out-String | Invoke-Expression`.

### 2.15 Security, telemetry, compatibility

109. The CLI never resolves `secretRef` values; it handles references only (SEC "Secrets" rule 1; depguard denies `internal/secret` under `internal/cli`, A1). The only secret bytes it touches are `--secret-overrides` targets, copied without inspection, and the admin token. A canary secret MUST never appear in CLI stdout or stderr (TQ "End-to-end tests").
110. Network access: only the admin URLs the user names and, in `dev run`, loopback admin of the launched child. No telemetry export, no update checks.
111. Skew: the CLI supports Nodes at its minor or one older for `dev tap`, `node dump` and file-mode Revisions; unknown payload members are ignored. [RVC "Control plane and data plane version skew"]
112. New flags or verbs ship only in minor releases; exit codes and `ruralz.diff.v1` are stable surfaces. [RVC "Versioned surfaces"]

## 3. Proposed Go packages and API

All under `internal/cli/...` except the shared contracts noted. Package names singular, no `util`/`common`; every file starts with the Revington Apache-2.0 header; context first; no `init()`; no package-level mutable variables.

| Package | Responsibility | Imports (Ruralz / third-party) |
|---|---|---|
| `internal/cli` | `Run`, `SignalContext`, `Registry`, root and noun help, `version`, `completion` wiring, planned-command table | `command`, `completion`, `bundle`, `dev`, `node`, `buildinfo` / none |
| `internal/cli/command` | `Spec`, `Command`, `Flags` (interspersed parse, specs for help and completion), `IO`, exit codes, `ExitError`, `Interrupted`, JSON and NDJSON writers, atomic file write | `errcode` / none |
| `internal/cli/completion` | bash, zsh, fish, PowerShell generators from `[]command.Spec` | `command` / none |
| `internal/cli/adminclient` | Admin URL validation, loopback cleartext guard, TLS, bearer token, problem documents, `/config/dump`, `/tap`, `/readyz` | `command`, `internal/problem`, `internal/config/canonical` / none |
| `internal/cli/bundle` | `validate`, `render`, `diff`, `build` commands; Environment selection; diff source classification and loading | `command`, `adminclient`, `internal/config/...` (areas 1, 2) / none directly |
| `internal/cli/launch` | Local `ruralzd` launcher shared by `dev run` and M2 `test run --bundle`: private dir, render, watch, ports, secret overrides, child process, readiness | `command`, `bundle`, `adminclient`, `internal/config/...` / none |
| `internal/cli/dev` | `dev run`, `dev tap` commands | `command`, `launch`, `adminclient`, `bundle` / none |
| `internal/cli/node` | `node drain` (`drain_linux.go`, `drain_other.go`), `node dump`; `/proc` parsers | `command`, `adminclient`, `internal/nodedir` / none |
| `internal/nodedir` (data plane, A4) | Data dir names, `holder.json` type; this area needs a read-only `ReadHolderAt` and the additive `PIDNamespace` member | stdlib; `golang.org/x/sys/unix` in its lock file only |
| `internal/problem` (data plane, A4) | RFC 9457 document type and decoder | none |

```go
// Package cli: internal/cli
package cli

// Run executes one invocation and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int

// SignalContext cancels ctx on the first SIGINT or SIGTERM with cause
// command.Interrupted{Signal: s}; stop unregisters and waits for its goroutine.
func SignalContext(parent context.Context) (ctx context.Context, stop context.CancelFunc)

// Registry returns a freshly built command table (M1 commands plus planned stubs).
func Registry() []command.Spec
```

```go
// Package command: internal/cli/command
package command

const (
	ExitOK          = 0
	ExitNegative    = 1
	ExitNoResult    = 2
	ExitWaiting     = 3 // M2: rollout --wait paused, RZ-CP-006 hold
	ExitInterrupted = 130
)

type IO struct {
	Stdout, Stderr io.Writer
	Environ        []string                    // snapshot taken once in Run
	LookupEnv      func(string) (string, bool) // over Environ
	Now            func() time.Time
	GOOS           string // runtime.GOOS; injectable for platform-gate tests
}

type Completion uint8

const (
	CompleteNone Completion = iota
	CompleteFile
	CompleteDir
	CompleteValues
)

type Arg struct {
	Name     string // "DIR", "FROM", "TO", "SHELL"
	Optional bool
	Default  string
	Complete Completion
	Values   []string
}

type Spec struct {
	Path       []string // {"bundle","validate"}; {"version"}
	Summary    string
	Args       []Arg
	Planned    string                 // "M2": recognized, not built
	Platform   func(goos string) error // nil: every platform
	Deprecated string
	New        func() Command // fresh value per invocation; nil when Planned != ""
}

type Command interface {
	Flags(f *Flags)                                     // binds flags to the command's fields
	Run(ctx context.Context, io IO, args []string) error // args: positionals after parsing
}

type FlagSpec struct {
	Name, Placeholder, Default, Usage, Deprecated string
	Bool     bool
	Values   []string // enum values (help, completion, validation)
	Complete Completion
}

type Flags struct{ /* *flag.FlagSet, []FlagSpec, set names */ }

func NewFlags(path string) *Flags
func (f *Flags) String(p *string, name, placeholder, value, usage string, c Completion)
func (f *Flags) Enum(p *string, name, value string, values []string, usage string)
func (f *Flags) Bool(p *bool, name, usage string)
func (f *Flags) Duration(p *time.Duration, name string, value time.Duration, usage string)
func (f *Flags) Parse(args []string) (positionals []string, err error) // req 5; ErrHelp on -h
func (f *Flags) IsSet(name string) bool
func (f *Flags) Specs() []FlagSpec

type ExitError struct {
	Code int
	Err  error // nil when the command already printed its result
}

func (e *ExitError) Error() string
func (e *ExitError) Unwrap() error
func Usagef(format string, args ...any) error // code 2, usage hint appended
func NoResult(err error) error                // code 2
func NoResultf(format string, args ...any) error
func Negative() error                         // code 1, nothing more to print

type Interrupted struct{ Signal os.Signal }

func (Interrupted) Error() string

// Code maps a command's return value: nil 0; *ExitError its code; a context
// whose cause is Interrupted 130; anything else 2 (printed by Run).
func Code(ctx context.Context, err error) int

func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error // req 17
func WriteJSON(w io.Writer, v any) error                               // req 16
type LineWriter struct{ /* bufio.Writer; Flush per line */ }
func NewLineWriter(w io.Writer) *LineWriter
func (l *LineWriter) Object(v any) error // NDJSON line
func (l *LineWriter) Line(s string) error
```

```go
// Package adminclient: internal/cli/adminclient
package adminclient

type Options struct {
	URL            string
	TokenFile      string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
}

// Bind registers --admin-token-file, --ca-file, --client-cert, --client-key and,
// when defaultURL != "", --admin with that default.
func (o *Options) Bind(f *command.Flags, defaultURL string)

type Client struct{ /* base *url.URL, *http.Client, token string (never printed) */ }

func New(o Options) (*Client, error)                    // req 62..66; usage errors are *command.ExitError{2}
func (o Options) ForURL(raw string) (*Client, error)    // diff: URL from a positional
func (c *Client) RequireCredential() error              // req 68
func (c *Client) ConfigDump(ctx context.Context) (raw []byte, err error) // body ≤ 256 MiB, 30 s
func (c *Client) Readyz(ctx context.Context) (status int, body ReadyzBody, err error)
func (c *Client) Tap(ctx context.Context) (*TapStream, error)

type ReadyzBody struct {
	Status   string        `json:"status"`
	Revision string        `json:"revision,omitempty"`
	Reasons  []ReadyReason `json:"reasons,omitempty"`
}
type ReadyReason struct {
	Reason string `json:"reason"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type TapStream struct{ /* body, bufio.Reader with 1 MiB line cap */ }
type TapLine struct {
	Raw     []byte // exact line, for --output json
	Dropped int64  // > 0: drop notice (req 88)
	Event   TapEvent
}
type TapEvent struct {
	Time       time.Time `json:"time"`
	TraceID    string    `json:"traceId"`
	Route      string    `json:"route"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	Code       string    `json:"code"`
	DurationMs float64   `json:"durationMs"`
	Upstream   string    `json:"upstream"`
	Consumer   string    `json:"consumer"`
}

func (s *TapStream) Next() (TapLine, error) // io.EOF at a clean end
func (s *TapStream) Close() error

// ProblemError is returned for every non-2xx answer.
type ProblemError struct {
	Status  int
	Raw     []byte
	Problem problem.Document
}

func (e *ProblemError) Error() string
```

```go
// Package bundle: internal/cli/bundle (engines from areas 1 and 2)
package bundle

func Commands() []command.Spec // validate, render, diff, build

// Selection binds --env and --environments.
type Selection struct{ Env, EnvironmentsFile string }

func (s *Selection) Bind(f *command.Flags)

// Target is one render input: no Environment (process environment) or one Environment.
type Target struct {
	Environment *v1alpha1.Environment // nil: process environment, no overlay
	Name        string                // "" without an Environment
}

// Targets implements reqs 26..30; multi is true only for validate.
func (s Selection) Targets(ctx context.Context, io command.IO, multi bool) ([]Target, error)
func (s Selection) Named(ctx context.Context, io command.IO, name string) (Target, error) // --from-env / --to-env

type SourceKind uint8

const (
	SourceDir SourceKind = iota
	SourceFile
	SourceAdmin
	SourceRevision // M2
	SourceOCI      // M2
)

func Classify(arg string) SourceKind // req 50 steps 1..4, without touching the filesystem

// Pipeline wraps area 1 and area 2 objects built once per invocation.
type Pipeline struct{ /* schemaview.Set, schemaidx.Index, registry, normalizer, encoder, decoder, resolver, diff engine */ }

func NewPipeline(workers int) (*Pipeline, error)
type Built struct {
	Result   *loader.Result
	Revision canonical.Revision              // zero when Result has errors
	Chains   map[string]*precedence.Chain
}
func (p *Pipeline) Build(ctx context.Context, src loader.Source, t Target, io command.IO) (*Built, error)
func (p *Pipeline) Side(ctx context.Context, arg string, t *Target, admin adminclient.Options, io command.IO) (*diff.Side, error)

// OnlineChecker is the extension point for --online and build (M2: OCI + Sigstore).
type OnlineChecker interface {
	CheckPlugins(ctx context.Context, b *hub.Bundle) diag.List
}
func M1OnlineChecker() OnlineChecker // RZ-CFG-028 per Plugin (req 36)
```

```go
// Package launch: internal/cli/launch
package launch

type Options struct {
	Dir             string
	Target          bundle.Target
	SecretOverrides string
	EphemeralPorts  bool
	ReadyTimeout    time.Duration // 30 s
	Binary          string        // "" = FindRuralzd
	Poll            time.Duration // 500 ms
	Settle          time.Duration // 250 ms
}

type PortMap struct {
	Listener string // "" for admin
	From, To int
}

type Event struct {
	Time        time.Time
	Kind        string // ports, started, revision, ready, active, notActivated, renderFailed, exited
	Ports       []PortMap
	PID         int
	Admin       string
	TokenFile   string
	Digest      revision.Digest
	TestDigest  revision.Digest
	Active      revision.Digest
	Diagnostics diag.List
	Code        int
}

type Session struct{ /* tmp dir, child *exec.Cmd, adminclient, events chan Event (cap 16) */ }

func FindRuralzd() (string, error)
func ParseSecretOverrides(path string) (map[string]string, error) // "provider:name" -> absolute path

// Start renders, prepares the private dir, launches ruralzd and waits for /readyz.
func Start(ctx context.Context, io command.IO, p *bundle.Pipeline, o Options) (*Session, error)
func (s *Session) Events() <-chan Event
// Run watches and re-renders until ctx ends or the child exits; returns the child's status.
func (s *Session) Run(ctx context.Context) (childExit int, err error)
// Stop sends SIGTERM, waits 35 s, then SIGKILL, and removes the private dir. Idempotent.
func (s *Session) Stop() error
```

```go
// Package node: internal/cli/node
package node

func Commands() []command.Spec // drain, dump

// drain_linux.go
type lockEntry struct {
	Class, Mode string // FLOCK, POSIX, OFDLCK, LEASE; READ, WRITE
	Waiter      bool   // "->" lines
	PID         int    // -1 for OFD locks
	Major, Minor uint32
	Inode       uint64
}
type procStat struct {
	State     byte
	StartTime uint64
}
func parseLocks(r io.Reader) ([]lockEntry, error)
func parseStat(b []byte) (procStat, error) // fields after the last ')'
type drainer struct {
	proc   string                          // "/proc", injectable
	signal func(*os.Process, os.Signal) error
	poll   time.Duration                   // 100 ms
	now    func() time.Time
}
func (d *drainer) drain(ctx context.Context, io command.IO, dataDir string, timeout time.Duration) error
```

```go
// Package completion: internal/cli/completion
package completion

type Shell string

const (
	Bash       Shell = "bash"
	Zsh        Shell = "zsh"
	Fish       Shell = "fish"
	PowerShell Shell = "powershell"
)

func Shells() []Shell
func Write(w io.Writer, sh Shell, program string, cmds []Entry) error

type Entry struct {
	Path    []string
	Summary string
	Args    []command.Arg
	Flags   []command.FlagSpec
}
```

Concurrency model:

- Every command runs on the caller's goroutine. Goroutines exist only where stated, each owned, cancelled and awaited by its starter (`sync.WaitGroup`; `golang.org/x/sync` has no catalog row): `SignalContext` (1, until stop), `bundle diff` side loading (2, joined before comparison), `launch.Session` (child waiter 1 feeding a capacity-1 channel; watcher ticker 1 feeding a capacity-1 coalescing channel with non-blocking sends; child output copier 2; the supervisor loop is the caller's goroutine; the events channel has capacity 16 and the printer is the caller), area 1 and 2 worker pools inside their calls (W = `GOMAXPROCS` for the CLI).
- Bounded memory: `/tap` lines ≤ 1 MiB, dump ≤ 256 MiB, problem documents ≤ 64 KiB, token file ≤ 4 KiB, 10,000 diagnostics, source limits of req 32.
- Cancellation: every blocking call takes `ctx`; admin requests use `http.NewRequestWithContext`; `dev run` Stop runs with a fresh 35 s timer after ctx cancellation.

Exported for other areas: `cli.Run` (cmd wiring, e2e harness), `command` exit codes and `ExitError` (every future noun), `adminclient` (e2e tests against real Nodes; M2 REST client reuses its TLS and token code), `launch` (M2 `ruralz test run --bundle`), `bundle.Classify` and `bundle.Pipeline.Side` (M2 Revision sources plug in here), `completion` (docs generation).

## 4. Dependencies on other areas

Needs:

| From | What | Used by |
|---|---|---|
| Area 1 (config load, A1) | `loader.Load(ctx, Source{Dir|File}, Options{Environment, Variables, Limits, Workers})` returning `Result{Rendered, Bundle, Diagnostics}` and an error only for unusable input; `loader.LoadEnvironments`, `Environments.Get/Names`, `OverlayName`; `diag.List` with `HasErrors`, `WriteText`, `WriteJSON` exactly per reqs 33, 34; `DefaultLimits` (64 MiB, 20,000, depth 64, 10,000 diagnostics); restricted-profile YAML parse for the `--secret-overrides` file; `secret.Ref.String()` (`provider:name`) | validate, render, build, diff, dev run |
| Area 2 (config revision, A2) | `canonical.Encoder.Encode` (digest and bytes), `canonical.Decoder.ReadDump`/`Verify` (RZ-CFG-027, RZ-CFG-024), `revision.Digest` (`String`, `Short`), `precedence.Resolver` and `Chain.Rows()`, `render.WriteBundle`, `render.WriteEffective`, `render.ConvertTree`, `convert.Registry.Served()`, `diff.Engine.Compare`, `diff.WriteText`, `diff.WriteJSON`, `diff.Side`/`SideRef`; a rendered-resource patch API to rewrite `spec.listeners[name].port`, `spec.admin.port` and `secretRef` objects before `WriteBundle` (for `dev run` test renders) | render, build, diff, dev run, node dump |
| CEL area | Nothing directly; CEL compile and cost run inside area 1's validation | validate |
| Data plane (A4) | `ruralzd` in file mode from `RURALZ_CONFIG` (path) with no arguments; reload on a changed `ruralz.yaml` inode; `RURALZ_DATA_DIR`, `RURALZ_ADMIN_TOKEN_FILE` (plain HTTP admin accepting the operator token from loopback), `RURALZ_SECRET_ROOT`; SIGTERM starts a Drain, exit 0 after it; `/readyz` 503 body with `reasons[{reason,code,detail}]`; `/config/dump` as A2 req 73; `/tap` NDJSON events (A4 req 75) plus a `{"dropped":N}` notice line (this area's request); 503 problem document for the fifth subscriber; `internal/nodedir` with `LockFile = "lock"`, `HolderFile = "holder.json"`, `HolderFormat = "ruralz.holder.v1"`, `DefaultRoot = "/var/lib/ruralz"`, read-only `ReadHolderAt(root string) (Holder, error)`, and additive `PIDNamespace string \`json:"pidNamespace"\`` written by the holder; `flock(LOCK_EX)` (a BSD lock, visible in `/proc/locks` as `FLOCK` with the holder's PID; never an OFD lock, whose PID shows as -1); `internal/problem.Document` decoder | dev run, dev tap, node dump, node drain, diff |
| Security (A6) | Admin authentication semantics (constant-time compare, loopback or TLS only, 401 problem documents), `RURALZ_FETCH_ALLOW` format (comma-separated IPs/CIDRs), `RURALZ_SECRET_ROOT` containment refusal | dev run, admin client |
| Telemetry | None in M1 (the CLI emits no signals) | none |
| State Store, Filter SPI | None | none |
| `internal/errcode` | Registered RZ-CFG codes printed by the CLI (001 to 037; notably 001, 007, 010, 016, 024, 026, 027, 028) | all |
| `internal/buildinfo` | `Get()`, `APIVersion`, text and JSON writers | version |

Provides: the `ruralz` binary behavior every M1 end-to-end test drives; `adminclient` and `launch` for the e2e harness; golden `--effective` tables and `ruralz.diff.v1` documents produced through the CLI for the conformance corpus (shared with areas 1 and 2); the exit-code and stream contract CI pipelines depend on.

## 5. Libraries

Direct imports of this area: the standard library only (`flag`, `os`, `os/exec`, `os/signal`, `syscall`, `net`, `net/http`, `net/url`, `crypto/tls`, `crypto/x509`, `crypto/rand`, `crypto/sha256`, `encoding/json`, `encoding/base64`, `encoding/hex`, `bufio`, `io`, `io/fs`, `path/filepath`, `text/tabwriter`, `strconv`, `strings`, `time`, `context`, `errors`, `runtime`, `sync`).

Linked transitively through areas 1 and 2 (catalog rows, TECH "Library catalog"): `github.com/goccy/go-yaml` v1.19.2 (YAML 1.2 parser; restricted profile for Bundles, `--environments` and `--secret-overrides`), `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3 (rendered-view validation), `cel.dev/cel-go` v0.32.0 (CEL compile and cost at validation). The `ruralz` binary also links `golang.org/x/sys` v0.48.0 through `internal/nodedir`'s lock file on Linux and macOS (catalog row "Zero-Downtime Upgrade socket steering"; A4 asks to widen its purpose text to flock).

Not used, with reasons: `spf13/cobra` (OQ-tech-stack-and-libraries-18 decided `flag`); `golang.org/x/term`, `fsnotify`, `golang.org/x/sync` (no catalog rows; no color, polling watcher, `sync.WaitGroup`); `golang.org/x/sys/unix` directly in the CLI (the standard library suffices: `os.FindProcess` uses pidfd on Linux, `syscall.Kill`, `/proc` parsing); testcontainers (no Docker daemon here; the CLI's integration tests need none).

## 6. Test plan

Hermetic unit tests run shuffled, under `-race` and under `CGO_ENABLED=0`, on Linux, and the CLI unit and golden tests also on darwin/arm64, darwin/amd64 and windows/amd64 with identical output (TQ "CI stages"). Golden files live under `internal/cli/*/testdata/`, LF only (global `.gitattributes`), regenerated with `-update`. Integration tests carry `//go:build integration`, end-to-end tests `//go:build e2e`.

### 6.1 Unit and table tests

- `command.Flags.Parse`: flags before, between and after positionals; `--x=v`, `--x v`, `-x v`; bool `--online`, `--online=false`; `--` then `--env` kept as a positional; `--reason=--`; lone `-`; unknown flag → exit 2; missing value → exit 2; repeated flag → last wins; `-h`, `--help`, `-help` → help on stdout, exit 0; enum violation (`validate --output yaml`) → exit 2 naming allowed values; bad duration (`--timeout 5x`, `--timeout 0`) → exit 2.
- Dispatch: `ruralz` → 2, usage on stderr, stdout empty; `ruralz help`, `ruralz bundle --help`, `ruralz help bundle diff` → 0 on stdout; `ruralz bundle` → 2; `ruralz bundle frobnicate` → 2; `ruralz rollout status` and `ruralz bundle push` → 2 "Planned (M2)"; the existing M0 tests (`TestUsageErrors`, `TestHelp`, version text/JSON) keep passing, with `version --help` output now on stdout.
- `command.Code`: nil → 0; `Negative()` → 1; `NoResult` and plain errors → 2; `ExitError{3}` → 3; context cancelled with cause `Interrupted{SIGINT}` and with `Interrupted{SIGTERM}` → 130; `errors.Join` and `%w` wrapping preserved.
- `SignalContext`: sending SIGINT to the test process (Unix) cancels with the typed cause; `stop` joins the goroutine (goleak-style check with `runtime.NumGoroutine` before and after).
- Platform gates with injected `GOOS`: `dev run` on `windows`; `node drain` on `darwin` and `windows`; exact messages; exit 2.
- Environment selection (`Selection.Targets`) table: no flags → process environment target; `--env prod --environments f` → overlay `prod`, variables only; `--env` alone → 2 (M2 message); `--environments` alone for validate → one target per Environment in name order; for render, build, diff, dev run → 2; unknown `--env` → 2 with hint; file with only `Cluster` documents → 2; file with a `Route` (RZ-CFG-005), duplicate Environment (RZ-CFG-008), promotion cycle (RZ-CFG-022), YAML anchor (RZ-CFG-003), BOM (RZ-CFG-001), over 64 MiB (RZ-CFG-001) → diagnostics on stderr, exit 2; `spec.overlay: ../x` → RZ-CFG-005, exit 2; missing overlay directory → renders without patches, exit 0.
- `bundle validate` exit and stream matrix: clean → 0, stdout empty (text) or `[]\n` (JSON); warning only (RZ-CFG-013 secret-like variable, RZ-CFG-025 via a synthetic lifecycle table) → 0 with the warning on stdout; one negative fixture per offline RZ-CFG code available in M1 (001, 002, 003, 004, 005, 006, 007, 008, 009, 010, 011, 012, 014, 015, 016, 017, 018, 019, 020, 021, 023, 029, 030, 031, 032, 034, 035, 036, 037; RZ-CFG-022 arises only from the `--environments` file and exits 2, covered above) → 1 with the exact text line and JSON object (member order, `path` form, `hint`, `related`); DIR missing, DIR a FIFO, unreadable file (skipped on Windows and as root) → 2; DIR without `ruralz.yaml` → RZ-CFG-016, 1; one-file Bundle path → validated; 20,001 resources, depth 65 → RZ-CFG-001, 1; multi-Environment run with one Environment invalid → 1, diagnostics tagged `[environment=<name>]` and `"environment"`; `--online` without Plugins → 0; with a `Plugin` → RZ-CFG-028, 1; RZ-CFG-026 is never raised by the CLI (secrets unresolved offline).
- `bundle render`: invalid → 1, stdout empty; `--output json`; `--output text` without `--effective` → 2; `--route` without `--effective`, `--effective` without `--route` → 2; unknown Route → 2 with hint; `--api-version` without `--output-dir` or without `--environments` → 2; non-empty `--output-dir` → 2; `--api-version ruralz/v9` → RZ-CFG-007, 2; `--output-file` with `--api-version` → 2; conversion equal → 0 with one `equal` line per Environment; conversion differing (test converter injecting a changed default) → 1 with the human diff on stderr; `--output-file` → stdout empty, file content equals the stdout variant, parent missing → 2.
- `bundle diff` classification table: `./shop`, `shop`, `dump.json`, `http://127.0.0.1:9901`, `https://node-a.shop.example:9901/`, `rev-162af81f5de4` → M2, 2; `sha256:<64 hex>` → M2, 2; `oci://r@sha256:…` → M2, 2; `rev-XYZ` → path, missing → 2; `ftp://x` → path, missing → 2; `https://n:9901/?a=1` → 2; three positionals or one → 2; `--from-env` on an admin side → 2; invalid Bundle on FROM → 2; dump file whose content does not hash to its digest → RZ-CFG-027, 2; dump using a field above the served level (synthetic) → RZ-CFG-024, 2; admin side answering 401 (`httptest`) → 2 with the problem summary, and in JSON mode the problem document on stdout; admin side answering 302 → 2; body over the cap (test lowers it) → 2; token file with an `http://192.0.2.1:9901` URL → 2 before any connection (the test dialer records no attempt); `http://localhost:…` resolving to loopback → allowed.
- `bundle build`: text line format; JSON members and omission rules; `--output-file` bytes hash to the printed digest; `--offline` with a `Plugin` → 0; without → RZ-CFG-028, 1; invalid → 1 with JSON `diagnostics` and no `digest`.
- Admin client: CA file without PEM → 2; client cert without key → 2; client cert with `http://` → 2; token file empty or whitespace → 2; token over 4 KiB → 2; group-readable token file → stderr warning (Unix); `Authorization` header never appears in any error string (grep test over every error path).
- `dev tap` against an `httptest` NDJSON server: text line golden for a full event and a sparse event; `--route` filter; JSON pass-through byte-exact including unknown members; `{"dropped":3}` → stderr warning, not on stdout; a 1 MiB + 1 line → skipped with warning, stream continues; 503 problem → 2; 401 → 2 with hint; server EOF → 0; cancelled context → 130 with stdout flushed; neither token nor client cert → 2 before any request.
- `node dump` against `httptest`: verbatim bytes to stdout and to `--output-file` (atomic, mode 0644); digest mismatch → RZ-CFG-027, 2; non-JSON body → 2; 401 → 2; `lastKnownGood` absent → summary without it.
- `node drain` parsers (Linux build): `parseLocks` over `1: FLOCK  ADVISORY  WRITE 16582 fe:00:1884246 0 EOF`, POSIX, `OFDLCK ... -1 ...`, `1: -> FLOCK ...` waiter, LEASE, truncated and garbage lines; `parseStat` with `comm` containing spaces and `)`, a missing field, a non-numeric `starttime`.
- `node drain` verification with a fake `/proc` tree and injected signaler: record missing → 2; unparsable → 2; wrong `format` → 2; other `pidNamespace` → 2 "another PID namespace"; process absent → 2 stale; `starttime` mismatch → 2 stale; state `Z` → 2 stale; no lock line, lock line for another PID, OFD line, waiter line only → 2 "no verified lock holder"; signaler `EPERM` → 2 "signal denied"; `ErrProcessDone` → 2; success then process disappears → 0; never disappears → 2 after `--timeout`; context cancelled while waiting → 130; `--data-dir` absent and `RURALZ_DATA_DIR` unset → default `/var/lib/ruralz` used; relative `--data-dir` made absolute.
- `dev run` units: `ParseSecretOverrides` (valid map; relative paths resolved against the file's directory; unknown provider → error; anchors → RZ-CFG-003; missing target → error); private-dir layout and modes (0700 dirs, 0600 token and secrets; token 43 base64url characters); child environment filter table (keeps `RURALZ_STATE_STORE_URL`, `RURALZ_SECRET_DB`, `RURALZ_LOG_LEVEL`; drops `RURALZ_ADMIN_TLS_DIR`, `RURALZ_CONFIG`, inherited `RURALZ_DATA_DIR`; sets `RURALZ_FETCH_ALLOW=127.0.0.0/8,::1` only when unset); busy-port detection (the test holds a listener, including one opened with `SO_REUSEPORT` on Linux) → 2 naming port and listener; ephemeral mapping printed in name order and applied to the rendered file; `testDigest` present only when rewritten; watcher coalescing (ten rapid edits → at most two renders) and settle; failed re-render keeps the previous file byte-identical.
- `completion`: each shell's script contains every registered path and flag and no planned command (property over `Registry()`); unknown shell → 2; missing or extra positional → 2.
- `version`: JSON has exactly the four members; `buildinfo.APIVersion` equals the served list.

### 6.2 Golden tests

- Help output for root, each noun and each of the ten commands.
- Completion scripts for bash, zsh, fish, PowerShell.
- `bundle validate` text and JSON for the negative corpus (shared fixtures with area 1; the CLI golden pins stream routing and formatting).
- `bundle render` YAML and JSON of an M1 example Bundle with and without `--env staging` and `--env prod` (including a `$${` escape and a substituted integer port).
- `bundle render --effective --route orders-summary` text and JSON for the CM example Bundle: rows equal CM "Worked example" in Phase, Leg, Policy, From.
- `bundle build` text and JSON for the golden corpus (digests shared with area 2).
- `bundle diff` text and JSON for a directory pair reproducing CM "Diff semantics" (M1 subset), for `--from-env staging --to-env prod`, and for directory vs saved dump.
- `bundle render --api-version ruralz/v1alpha1` output tree (file list and bytes) and the per-Environment lines.
- `dev tap` text lines; `dev run` text and JSON event sequences from the fake child (timestamps injected through `IO.Now`).
- Every golden runs on the floor job too and MUST match byte for byte (req 16).

### 6.3 Property tests (`testing.F`, seeds in `pr-fast`, long runs nightly)

- Parse round trip: for random flag/positional interleavings of a command, `Parse` yields the same positionals and flag values as the canonical "flags first" order.
- Diff through the CLI: `diff A A` exits 0 with empty JSON `resources`; `diff A B` exits 0 exactly when `build A` and `build B` print equal digests; the added set of `diff A B` equals the removed set of `diff B A`.
- Render idempotence through the CLI: `render DIR > f; build f` equals `build DIR` (same Environment), including `$${` escapes.
- Validate JSON is always a JSON array and exit 1 iff some element has `"severity":"error"`.
- Dump round trip: `node dump` output used as diff FROM against the same Bundle as TO exits 0.

### 6.4 Fuzz targets (seeded from golden and negative corpora; no panic, no hang)

`FuzzFlagsParse`, `FuzzClassify`, `FuzzAdminURL` (`adminclient.New` never panics, never accepts userinfo), `FuzzParseLocks`, `FuzzParseStat`, `FuzzHolderRecord`, `FuzzTapLine` (1 MiB cap honored), `FuzzProblemDocument`, `FuzzSecretOverrides`, `FuzzReadyzBody`.

### 6.5 Integration tests (`//go:build integration`, Linux, real processes, no Docker)

- Lock holder helper: the test binary re-executes itself (`os.Args[0]`, `RURALZ_TEST_HELPER=holder`) to `syscall.Flock(LOCK_EX)` `lock`, write `holder.json` (with `pidNamespace`) and, on SIGTERM, exit after 1 s. `ruralz node drain --data-dir D --timeout 10s` → 0 within 2 s; helper ignoring SIGTERM with `--timeout 2s` → 2; helper killed with SIGKILL, record left → 2 stale; record pointing at the test's own PID without a lock → 2; helper in a new PID namespace (skipped without `CLONE_NEWPID` permission) → 2; handover simulation (helper A holds the lock, on SIGTERM releases it, helper B takes it and rewrites `holder.json`, A exits after 2 s) → drain waits for A and exits 0 while B keeps running.
- Fake `ruralzd` (a helper mode serving `/readyz` and `/config/dump` on the admin port read from `RURALZ_CONFIG`, reloading on inode change, honoring SIGTERM): `ruralz dev run --ephemeral-ports` reaches `ready`; an edit reaches `active`; an invalid edit keeps the old file and the child's digest; `--ready-timeout 1s` with a helper that stays 503 (`secrets_unresolved`, RZ-CFG-026) → 2 with reasons; SIGINT to the CLI → child receives SIGTERM, private dir removed, exit 130; helper crash → 2; helper exit 0 → 0.
- `redis-server` is not used by this area.

### 6.6 End-to-end tests (`//go:build e2e`; real `ruralzd` and `ruralz` built into `t.TempDir()`)

One test per M1 pack 9 command (RM "M1 exit criteria" 5) plus TQ scenarios 2 and 6, runnable without Docker; the Docker Compose variant runs where a daemon exists.

- `bundle validate`, `render`, `build` on the M1 example Bundle and one per Environment with `--environments`.
- `dev run` on the example Bundle with `--ephemeral-ports`; `curl` through the mapped port; Hot Reload on edit; `dev tap --route` sees the request (text and JSON); `node dump` with the printed token file; `bundle diff DIR http://127.0.0.1:<admin>` → 0; inject a difference in DIR (not yet reloaded: watch paused via a second copy) → 1 (TQ scenario 6).
- `bundle render --effective --route` output matches the chain the Node executes, observed through sampled `ruralz.filter.<policy>` spans (TQ scenario 2).
- `node drain` against a standalone `ruralzd` (`RURALZ_DATA_DIR` set) → 0 within 30 s (target) and `/readyz` 503 observed during the Drain.
- `version`, `completion bash` (`bash -n` syntax check when bash exists; zsh `-n` and fish `--no-execute` when present, else skipped).
- Canary secret: a `file` `secretRef` value and a `--secret-overrides` target containing a canary string never appear in any CLI stdout or stderr of the run (TQ "End-to-end tests").

### 6.7 Benchmarks

`BenchmarkValidate10k` and `BenchmarkValidate20k` through `cli.Run` on a generated Bundle (budget 2 s and 4 s on four cores, target); `BenchmarkTapLine` (decode plus format under 5 µs per line, proposed).

## 7. Open questions blocking M1 in this area

From the Open questions tables, rows whose Blocking column names M1 and that touch the CLI, plus the RM exit criterion 7 list:

| ID | Question | Option to adopt | What the code does |
|---|---|---|---|
| OQ-cli-and-api-surface-10 (Yes, `ruralz node drain` (M1); owner data-plane) | Where does the lock holder record its PID and start time? | (a) A file under `${RURALZ_DATA_DIR}` (proposed) | `holder.json` beside `lock` (A4 req 5) with `format`, `pid`, `startTime`, `nodeId`, `version`, plus `pidNamespace` requested here; drain verifies record, `/proc/<pid>/stat` and `/proc/locks` (reqs 95, 96) |
| OQ-security-and-identity-7 (Yes, Planned (M1)) | Which settings hold admin credentials? | (a) Three `RURALZ_ADMIN_*` settings, amending pack section 2 (proposed) | `dev run` writes `RURALZ_ADMIN_TOKEN_FILE` in its private dir and uses it for `/config/dump`; the admin client sends `--admin-token-file` as a bearer token or presents `--client-cert`/`--client-key`; `RURALZ_ADMIN_TLS_DIR` and `RURALZ_ADMIN_METRICS_TOKEN_FILE` are stripped from the child |
| OQ-security-and-identity-22 (Yes, Planned (M1); RM exit 7) | How are secrets and Node connections restricted? | (a) `RURALZ_SECRET_ROOT`, `RURALZ_SECRET_` prefix, `RURALZ_FETCH_ALLOW`, `security` impact class, … (proposed) | `--secret-overrides` copies into `<tmp>/secrets` set as `RURALZ_SECRET_ROOT`, keeping `data/` and `admin/` outside it; the child keeps `RURALZ_SECRET_*` env secrets; `RURALZ_FETCH_ALLOW` defaults to loopback in `dev run`; the diff `security` impact comes from area 2 |
| OQ-zero-downtime-upgrades-and-hot-reload-7 (Yes, Planned (M1) handover on T5) | Which systemd directives keep the new process as main process across a handover? | (a) Research, then a shipped unit and helper | No CLI code: `node drain` signals the lock holder's PID, not systemd's `MainPID`, so it stays correct across a handover; the shipped unit uses `systemctl stop` for Drain (release area) |
| OQ-traffic-management-and-resilience-6 (Yes, canonical form (M1)) | Deadline defaults and timeout fields | (a) with (c) per-protocol Route timeouts (recommended; A1, A2 adopt) | No CLI code; golden digests printed by `build` and compared by `diff` change with the materialized defaults, so CLI goldens are regenerated once area 2 freezes the corpus |

Non-blocking questions this area applies as decided or current (no further decision needed for M1):

| ID | Adopted | Effect |
|---|---|---|
| OQ-tech-stack-and-libraries-18 | `flag` (decided) | Section 3 framework |
| OQ-cli-and-api-surface-1 | (a) diagnostics stay a bare array with `environment` | Req 34 |
| OQ-cli-and-api-surface-2 | (a) nothing replaces `--admin` | No environment variable read for admin flags |
| OQ-cli-and-api-surface-4 | (b) local SIGTERM only | `node drain` has no URL flag |
| OQ-cli-and-api-surface-11 | (b)-equivalent, current: bind address unchanged | Req 76 |
| OQ-configuration-model-7 | (a) conversion drops comments | Req 46 |
| OQ-configuration-model-10 | (a) flags as listed; no `ruralzd` overlay selector | `dev run` renders for `ruralzd` |
| OQ-configuration-model-18 | (a) depth 64 | Req 32 |
| OQ-control-plane-and-gitops-25 | No flag raises limits until decided | Req 32 |
| OQ-data-plane-4 | (b) for M1 | Req 93 |
| OQ-release-versioning-and-compatibility-6 | (a) `ruralz version` members | Req 104 |
| OQ-wasm-plugin-system-7 | (a) Plugins by digest from a registry | M2; req 85 |

## 8. Deferred (M2 and later): do not build now

| Item | Milestone | Extension point left in M1 |
|---|---|---|
| `bundle push`, `bundle import openapi`, `bundle export openapi|postman|dot`, `bundle audit` | M2 | Planned-command table (req 8); `bundle.Commands()` appends new specs; `Selection.Targets(multi=true)` reused by `audit` |
| `test run` (cases `ruralz.test.v1`, `--bundle`, `--target`, `--secret-overrides`, `--ready-timeout`) | M2 | `internal/cli/launch` is noun-neutral: `Start`/`Run`/`Stop`, port rewriting, secret overrides and readiness are what `test run --bundle` needs |
| `rollout` verbs, `node list|token|revoke`, `control` verbs, `ai` verbs, `plugin` verbs | M2, M3 | Nouns fixed by pack 9; `ExitWaiting = 3` reserved; `adminclient` TLS and token code reusable by a REST client |
| `--control URL`, `--token-file PATH`, REST client, step-up TOTP prompt (exit 2 without a terminal) | M2 | Not registered in M1 (unknown flag → 2); `--env` without `--environments` already names "Planned (M2)" |
| Diff sources `rev-<12 hex>`, `sha256:<64 hex>`, `oci://…`; `digestPrefix` lookup (OQ-cli-and-api-surface-3) | M2 | `bundle.Classify` recognizes them; `Pipeline.Side` gains two cases |
| Online Plugin check with OCI fetch and Sigstore (RZ-CFG-028, RZ-CFG-033), registry credentials (OQ-cli-and-api-surface-7) | M2 | `OnlineChecker` interface (req 36) |
| `dev run` Plugins by digest with `warn` trust (`RURALZ_TRUST_POLICY_FILE`) | M2 | Child environment builder (req 73) adds variables in one place |
| `ruralz version` `pluginAbi`, `controlStream` members | M2 | `buildinfo.Info` gains optional members; JSON omits empty ones |
| Remote drain (OQ-cli-and-api-surface-4 (a)) | open | `node drain` keeps `--data-dir`; a future `--node` flag would call REST |
| Environment variables for `--control`/`--admin` (OQ-cli-and-api-surface-2) | open | All admin flags bound by `adminclient.Options.Bind` |
| Loopback bind setting for launched `ruralzd` (OQ-cli-and-api-surface-11) | open | Child environment builder |
| HTTP/3 UDP ephemeral ports | M3 | Port rewriter checks TCP only; a UDP check hooks in beside it for `http3: true` |
| `dev tap` AI bodies as sizes and token counts | M3 | Text formatter prints only known scalar members |
| `--output csv` (`ai cost`) | M3 | `Flags.Enum` per command |
| Windows `dev run` | Not planned | Platform gate (req 70) |

## 9. Risks and ambiguities

1. **`flag` stops at the first positional.** Every doc example puts flags after positionals (`ruralz bundle diff $CONTROL rev-… ./bundle --env prod …`). Resolution: interspersed parsing (req 5); the only cost is `--flag=--` for a literal `--` value.
2. **Command ownership overlaps with area 2.** A2 section 3 proposes `func Render/Diff/Build(ctx, args, stdout, stderr) int` in `internal/cli/bundle`. Resolution: `internal/cli/bundle` implements `command.Command` values (this area) calling A2 engines; A2's engines and formats stay authoritative. Agree before implementation to avoid two flag parsers.
3. **`/config/dump`, `/readyz`, `/tap` wire shapes are undefined in the docs.** This spec consumes A2 req 73 (dump) and A4 reqs 61 and 75 (readyz, tap). They become versioned surfaces once the CLI parses them; recommend moving the types into a neutral package (for example `internal/adminapi`) so `ruralzd` and the CLI share them, with a contract test.
4. **Dropped-event warning has no carrier.** CLI says `dev tap` "warns of dropped events", but A4 only counts drops in `ruralz_tap_events_dropped_total`. Resolution requested from the data plane: emit `{"dropped":<n>}` on the stream before the next delivered event after drops (req 88). Without it the CLI cannot warn.
5. **Lock holder details.** The docs fix neither file names, lock type nor PID-namespace detection. Adopted A4's `lock` and `holder.json`; this area requires `flock` (BSD) locks, because OFD locks show PID -1 in `/proc/locks`, and an additive `pidNamespace` member, because "another PID namespace exits 2" is otherwise indistinguishable from a stale record. Device numbers are not compared (btrfs and overlayfs report `st_dev` unlike `/proc/locks`); inode plus PID plus start time is sufficient.
6. **`--admin` on `bundle diff`.** CLI "Global flags" lists `ruralz bundle diff` as accepting `--admin`, but the source form table makes the Node a positional admin URL, and no example uses `--admin` with diff. Resolution: not registered on diff in M1 (unknown flag → 2); recommend the CLI doc drop diff from the `--admin` row, keeping `--admin-token-file`, `--ca-file`, `--client-cert`, `--client-key`.
7. **`--ready-timeout` for `dev run`.** The command table lists it only for `test run`, but the readiness row of "Local ruralzd launched by the CLI" covers both launchers. Resolution: `dev run` registers it (default 30 s, target); the CLI doc should add it to the `dev run` row.
8. **`--admin` defaults.** Undefined; `http://127.0.0.1:9901` proposed for `dev tap` and `node dump`. Alternative: make it required. A loopback default never leaks a token (req 64).
9. **SIGTERM exit code.** Only SIGINT → 130 is documented. Resolution: SIGTERM also exits 130 after cleanup; alternative: re-raise SIGTERM for a conventional 143 status. Needs the CLI doc owner's confirmation before `0.1.0`, since exit codes never change once released.
10. **`--environments` file with `Cluster` documents.** The CM example file holds both kinds; A1 schema-checks and ignores `Cluster`. Adopted.
11. **Missing overlay directory for a selected Environment.** Unspecified; treated as no patches (a typo in `spec.overlay` is then silent). Alternative: warn. No RZ code exists for a warning here, so no warning in M1.
12. **`render --output json`.** "Same resources as JSON" is taken as an array (A2 req 47), which the loader cannot load as a Bundle (a `.json` Bundle file holds resource documents). Acceptable for tooling; document it.
13. **`--effective` expected spans.** OBS "Debugging tools" says the command shows "the resolved Filter Chain and its expected spans", but CM's table has no span column. Resolution: span name derivable (`ruralz.filter.<policy>`); A2's JSON `rows` could add an additive `span` member later; text keeps CM's five columns.
14. **Planned-command stubs.** Req 8 prints "Planned (Mx)" for unbuilt pack 9 commands; harmless but not required by the docs. Drop if reviewers prefer a plain "unknown command".
15. **`dev run` child environment filtering.** Undocumented; inheriting everything could pass an operator's `RURALZ_ADMIN_TLS_DIR` or `RURALZ_DATA_DIR` to the dev Node. Filtering (req 73) is proposed; `RURALZ_SECRET_ROOT` inherited without overrides can still contain the temp dir (for example `/tmp`), in which case `ruralzd` refuses to start (A4 req 3) and `dev run` reports exit 2.
16. **`--secret-overrides` format and rule 5.** The file format is undefined; proposed a YAML map `"provider:name": path`. With overrides set, `RURALZ_SECRET_ROOT` points at the private dir, so every other `file` reference outside it fails RZ-CFG-026 under proposed SEC rule 5; this matches "Any other unresolvable reference keeps `/readyz` at 503", but users must override every `file` reference.
17. **Polling watcher.** No watcher library is catalogued; 500 ms polling plus 250 ms settle is proposed for `dev run` (A4 polls at 1 s). Fine for development Bundles; very large trees cost CPU.
18. **Help on stdout.** Moving `version --help` output from stderr to stdout changes M0 behavior (not yet released). Explicit help is data, so stdout; usage errors stay on stderr.
19. **`build` and `validate --online` with Plugins in M1.** Exit 1 with RZ-CFG-028 (A2 req 56) reads as "invalid" although the check could not run; exit 2 ("no result") is the alternative. Aligned with A2 for one behavior across areas.
20. **`--ca-file` scope.** CLI "Global flags" says "All but `ruralz control join`"; in M1 only diff, dev tap and node dump dial 8090 or 9901, so only they register it. M2 commands add it.
21. **Integer precision in dumps.** Re-verifying a dump re-parses canonical JSON; area 2's decoder must keep numbers as `json.Number` so integers above 2^53 do not change the digest (A2 R-16, R-24).
22. **Docker Compose e2e.** RM names a Docker Compose file-mode test; no daemon here. The process-based e2e suite (section 6.6) covers the same commands; the Compose job runs in CI where a daemon exists.
23. **Floor job JSON.** `encoding/json` under `GOEXPERIMENT=jsonv2` runs on the v2 implementation; golden JSON (indentation, escaping) MUST match the release job, checked by running the goldens in both jobs.
24. **`x/sys/unix` in the CLI binary.** Importing `internal/nodedir` links its flock code (Linux, macOS). The CLI only needs `ReadHolderAt`; if the release owner objects, split the record type into a dependency-free file or package.
