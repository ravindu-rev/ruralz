---
paths:
  - "docs/adr/**"
---

# Architecture Decision Records

Authority: `docs/_meta/style-guide.md` section 7 and `docs/_meta/manifest.yaml` `adr_common`. Creating an ADR touches several files: if the user has not started `/new-adr`, read `.claude/skills/new-adr/SKILL.md` and follow it step by step.

## Shape

- File `docs/adr/NNNN-<slug>.md`, four-digit number, the next free one after the highest existing ADR.
- Front matter: `id: ADR-NNNN`, `title` (quote it when it contains a colon), `status`, `date` (ISO), `deciders: [ruralz-core]`, `related` (repo-relative paths). The status must match the ADR's `adrs:` entry in the manifest.
- H1 `# ADR-NNNN: <title>`. H2s exactly and in order: Context and problem statement, Decision drivers, Considered options, Decision outcome, Pros and cons of the options, More information.
- Decision outcome opens with `Chosen option: "<option>", because <reason>.` and holds the H3s `### Consequences` (Good and Bad bullets) and `### Confirmation` (a concrete test, lint rule, CI gate or review checklist item).
- At least two considered options, the chosen one included, each with pros and cons. A rejected library is named only when the tech stack document lists it under "Alternatives not chosen".
- 400 to 1,500 words, counted per the manifest `word_count_rule`. No Summary, Scope or Open questions sections; cite the owning document's `OQ-<docslug>-<n>` instead.
- All prose rules from `.claude/rules/docs.md` apply: no competitor products, citations from `docs/_meta/research/`, `Planned (Mx)` tags, no em dashes.

## Lifecycle

- Status is `proposed`, `accepted`, `deprecated` or `superseded-by ADR-NNNN`. The current counts are in the count sentence of `docs/adr/README.md`.
- An accepted ADR changes only through a new ADR that supersedes it. In the old ADR, change only the front-matter `status` to `superseded-by ADR-NNNN` and add nothing to its body: existing ADRs sit close to the 1,500-word maximum.
- The chosen option must match the foundation pack section 7 row for that ADR. A new or changed row is a `docs/_meta/` change: two approvals and foundation pack section 14.
- Commit scope for an edit to one ADR is its slug without the number, such as `docs(plugin-abi-v1): ...`. Adding an ADR touches several documents, so omit the scope: `docs: add ADR-NNNN ...`. Link from code or other commits with `Refs: ADR-NNNN`.
