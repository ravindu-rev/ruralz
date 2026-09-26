---
paths:
  - "docs/**/*.md"
  - "README.md"
---

# Design documents

Binding sources: `docs/_meta/style-guide.md` (every file under `docs/`), `docs/_meta/foundation-pack.md` (names and fixed decisions) and `docs/_meta/manifest.yaml` (per-document outline, diagrams, acceptance criteria, word band and `machine_checks` such as `required_terms`, `links_to`, `has_table`, `min_table_rows` and `max_lines`). No doc linter runs in CI; reviewers check these rules, so check them yourself.

## Which rules apply

- The Structure rules below apply to the documents in the manifest `docs:` list.
- Root `README.md`: no front matter and no mandatory H2s (`front_matter: false`, `mandatory_h2: false`); its H2s are exactly its manifest `outline_h2`, it stays at 150 lines or fewer, and it keeps its `required_terms` and links.
- ADRs follow `.claude/rules/adr.md`. `docs/adr/README.md` is an H1, a count sentence and the index table. `docs/_meta/research/*.md` have no front matter; the style guide and foundation pack keep their own (`status: binding`).

## Structure

- Front matter: `title`, `status` (`draft | reviewed | approved | approved-with-escalations`), `owner: ruralz-core`, `last_updated` (bump by hand, ISO date), `depends_on` (repo-relative paths that exist), `adrs` (every ADR the body cites), `milestone_tags_used` (every `Mx` in the body).
- One H1 equal to `title`. H2 headings match the document's `outline_h2` in the manifest exactly and in order; H3 and below are free.
- First H2 `## Summary` (120 words or fewer: what the document decides, who should read it); second `## Scope and non-goals`; last `## Open questions`.
- Open questions are a table `ID | Question | Options | Owner | Blocking?`, or `None.`. Add the row to the document you are editing; the ID is `OQ-<docslug>-<n>` with that file's slug and the next number after the highest ever used, closed IDs included; never renumber or reuse. Options read `(a) ...; (b) ...`, marking the one matching the text `(current)` or `(proposed)`. Owner is the slug of the document that must resolve it, or `ruralz-core`. Blocking? is `No` or `Yes, for <what> (Mx)`. Close a question with a `Closed:` list after the table or in place with `(chosen, YYYY-MM-DD)` and `No (answered)`, following the document's existing style.
- The manifest `length_words` band is binding, counted per its `word_count_rule`, and most documents and every ADR sit close to their maximum. Count the body before and after an edit and keep the net change at zero or below by tightening existing text. Keep every term, link, table and row the document's `machine_checks` require.

## Content rules

- Documents describe only Ruralz, on its own terms. Never name or compare competing gateway products. Libraries, standards and provider APIs Ruralz builds on may be named.
- Availability tags: `Planned (M0)` to `Planned (M5)`, or `Not planned` with a reason. Never `Supported`, `Implemented` or `Shipped`. Do not change tags because M0 code landed; that is OQ-roadmap-and-milestones-4.
- Every performance or scale figure carries `(target)` or `(hypothesis)` on the same line unless that line cites a measurement URL.
- Claims about external libraries, standards, licenses and releases cite a URL already present in `docs/_meta/research/*`, as `([source](https://...))`. A new fact needs a research addendum first.
- A design may depend only on a library with a catalog row in `docs/engineering/01-tech-stack-and-libraries.md`. A rejected option may be named when that document lists it under "Alternatives not chosen" with a research URL.
- Config examples use only kinds and fields from `docs/architecture/02-configuration-model.md`; CLI commands only from `docs/reference/01-cli-and-api-surface.md` (`ruralz <noun> <verb>`); Policy types only from foundation pack section 10. A missing one becomes an Open questions row, never an invention.
- Any `RZ-<AREA>-<NNN>` you write must be registered in `All()` in `internal/errcode/errcode.go`, and removing the last mention of a registered code fails too (`TestMatchesDocs`); `RZ-AUTH-010 to RZ-AUTH-014` counts every code in between. CI skips Go tests on docs-only pull requests, so run `go test -run TestMatchesDocs ./internal/errcode/` yourself, or check the registry by hand and say so.
- A capability mentioned anywhere must appear in `docs/features/01-feature-catalog.md`.

## Names and prose

- Canonical names and their "Never write" column: foundation pack sections 2 and 11. Forbidden-alias patterns with exemptions: `docs/_meta/manifest.yaml` `forbidden_aliases` (backend, API endpoint, controller, management plane, dashboard, sync protocol, cache cluster, WASM module and others). Kinds in `PascalCase` code format; YAML keys `camelCase`. Milestones are `M0` to `M5`, never "phase", "v1" or "GA".
- American English, no emojis, no em dashes (use commas, colons or separate sentences), paragraphs of five sentences or fewer, active voice, RFC 2119 keywords in capitals for normative statements. Dates are ISO `YYYY-MM-DD`.
- Use tables for catalogs, matrices and any list with three or more attributes per item.
- Code fences declare a language from `yaml go bash json protobuf mermaid text`.
- Cross-references are relative Markdown links, with an anchor when they point at a section, such as `[Data plane](03-data-plane.md#router)`; never bare paths. Index tables use the target document's title.
- No placeholders: `TODO`, `TBD`, `XXX`, `lorem`, `???`.

## Diagrams

Mermaid only (`flowchart`, `sequenceDiagram`, `stateDiagram-v2`, `classDiagram`, `erDiagram`, `quadrantChart`, `gantt`, `mindmap`), at most 25 nodes, one italic caption directly above (`*Figure N: ...*`), canonical names in labels, no HTML, and quotes around labels with parentheses or slashes: `A["Ruralz Gateway (ruralzd)"]`. Keep every diagram the manifest requires.

## Process

- Changes anywhere under `docs/_meta/` need two approvals (`scripts/ci-approvals.sh`). Changes to the binding files (foundation pack, style guide, manifest) also need an ADR or an Open questions entry (foundation pack section 14); a research addendum does not.
- Commit scope is the file slug under `docs/` (file name without numeric prefix and `.md`): `docs(data-plane): ...`; `docs/README.md` and `docs/adr/README.md` are both `readme`. Root `README.md` has no slug: use `docs: ...`. Manifest `slug:` values such as `root-readme` and `docs-index` are not commit scopes. Omit the scope for multi-document changes.
- `docs/README.md` links every document and ADR and records review status; keep it in sync when adding one. `docs/glossary.md` definitions lose to the owning section.
