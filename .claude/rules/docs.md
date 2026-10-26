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
- Every `RZ-<AREA>-<NNN>` in a document is registered in `internal/errcode`, and every registered code keeps at least one mention in `docs/` (`TestMatchesDocs`; ranges such as `RZ-AUTH-010 to RZ-AUTH-014` count every code between). CI runs no Go test on docs-only changes, so the gates at the end cover it.
- Tests parse some documents: the "Ruralz Gateway metrics" and "Degraded states" tables of `docs/architecture/10-observability.md` (and every `ruralz_*` name or degraded reason under `docs/architecture/`), and the `## 10. Policy type registry` table of the foundation pack. Keep their shape.
- Commit scope is the file slug (`docs(data-plane): ...`); `docs/README.md` and `docs/adr/README.md` are `readme`; root `README.md` and multi-document changes use `docs: ...`.
