---
paths:
  - "docs/**/*.md"
  - "README.md"
---

# Design documents

Writing rules: `docs/_meta/style-guide.md`. Names: `docs/_meta/foundation-pack.md`.

- Documents state the design as it is now. No history: no dated notes, change logs, closed questions, "previously" or "replaces" narrative, review status, process notes or plan IDs. When the design changes, rewrite the text in place.
- Open questions tables hold open questions only: `ID | Question | Options | Owner | Blocking?`, or `None.`. A new ID is `OQ-<docslug>-<n>` with the next number after the highest in that document. When the owner answers, delete the row and write the answer into the body.
- The roadmap (`docs/roadmap/01-roadmap-and-milestones.md`) holds milestones not yet exited; when a milestone exits, delete its section and fill `docs/roadmap/02-steps.md` for the next one. The step doc is a table in build order: delete a row when its work is done.
- ADRs (`docs/adr/NNNN-<slug>.md`, written only when the owner asks): front matter `id`, `title`, `status` (`proposed` or `accepted`); H1 `# ADR-NNNN: <title>`; H2s Context and problem statement, Decision drivers, Considered options, Decision outcome (opening `Chosen option: "...", because ...`, with `### Consequences` and `### Confirmation`), Pros and cons of the options, More information. A changed decision is edited in place; nothing is kept as superseded. Add or change its row in `docs/adr/README.md` and `docs/README.md`.
- Use only kinds and fields from the configuration model, CLI commands from the CLI reference and Policy types from foundation pack section 10. A missing one is a question for the owner, never an invention.
- Every `RZ-<AREA>-<NNN>` in a document is registered in `internal/errcode`, and every registered code keeps at least one mention in `docs/` (`TestMatchesDocs`; ranges such as `RZ-AUTH-010 to RZ-AUTH-014` count every code between). The PR pipeline's unit tests check it on every pull request, documentation-only ones included.
- Tests parse some documents. Keep their shape:
  - the "Ruralz Gateway metrics" and "Degraded states" tables of `docs/architecture/10-observability.md`, and every `ruralz_*` name or degraded reason under `docs/architecture/`;
  - the `## 10. Policy type registry` table of the foundation pack, each row's last cell, Planned, empty for a released type, which the registry serves, or the type's `Planned (M<n>)` (`TestTableMatchesFoundationPack` in `internal/config/registry`);
  - in the configuration model, the captioned blocks of "## Complete annotated example Bundle" and "## REST example Bundle", which must equal the files under `examples/`; the five-cell effective tables of those sections and of "### Worked example"; the `text` and `json` blocks of "### Diagnostics and source map"; and the one `text` and one `json` block of "### Diff semantics", which must equal the prod diff of the newest `shop-bundle-diff` golden entry (`TestDiffExampleMatchesDocument` in `test/conformance/config`): a change to `examples/shop-bundle/` adds a `shop-bundle-diff-<k>` entry and updates both blocks from its outputs;
  - the table of "### Required properties" in `docs/engineering/03-testing-and-quality-strategy.md`, read by its Area and Milestone columns (every Milestone cell `Planned (M<n>)` until that milestone exits, then the bare `M<n>`), and the first "## M<n>" heading of the roadmap, the milestone in progress (`TestRequiredProperties` in `test/property`): a row of an exited milestone, one before it, needs a target in that package's registry in either form, and a bare `M<n>` of a milestone not yet exited fails, so a milestone exits only once its rows have targets, and renaming an Area renames its registry entry;
  - for the root tests of `internal/cli`, the command tables of "## 9. CLI command registry" in the foundation pack and of "### Command table" in `docs/reference/01-cli-and-api-surface.md`, each row's last cell, Planned: empty, or only later parts each `<part> Planned (M<n>)`, for a released command, which the table runs; else `Planned (M<n>)` for a command Planned at that milestone, alone or before `; ` and such later parts; a command of the milestone in progress (the roadmap's first "## M<n>" heading) may already run while its row keeps that tag.
- Commit scope is the file slug (`docs(data-plane): ...`); `docs/README.md` and `docs/adr/README.md` are `readme`; root `README.md` and multi-document changes use `docs: ...`.
