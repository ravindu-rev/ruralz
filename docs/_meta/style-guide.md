---
title: Ruralz Documentation Style Guide
version: v1
status: binding
last_updated: 2026-09-23
---

# Ruralz Documentation Style Guide

Binding for every file under `docs/`. Reviewers check these rules.

## 1. File and front matter

Every document (not ADRs; see section 7) starts with YAML front matter:

```yaml
---
title: Data Plane
status: draft            # draft | reviewed | approved | approved-with-escalations
owner: ruralz-core
last_updated: 2026-09-23
depends_on:
  - docs/architecture/01-system-overview.md
  - docs/architecture/02-configuration-model.md
adrs: [ADR-0008, ADR-0009]
milestone_tags_used: [M1, M2]
---
```

- `depends_on` paths are repo-relative and MUST exist.
- `adrs` lists every ADR the document cites.
- `milestone_tags_used` lists every `Mx` that appears in the body.

## 2. Structure

1. Exactly one H1, equal to `title`.
2. H2 headings MUST match the document's `outline_h2` list in `docs/_meta/manifest.yaml`, in order, spelled exactly. H3 and below are free.
3. Mandatory H2s for every document:
   - First: `## Summary` (120 words or fewer; states what the document decides and who should read it).
   - Second: `## Scope and non-goals`.
   - Last: `## Open questions` (a table with columns `ID | Question | Options | Owner | Blocking?`; IDs are `OQ-<docslug>-<n>`). Write `None.` if empty.
4. Use tables for comparisons, catalogs, matrices, and any list with three or more attributes per item.
5. RFC 2119 keywords (`MUST`, `SHOULD`, `MAY`) in capitals for normative statements.

## 3. Diagrams

- Mermaid only, inside a ```` ```mermaid ```` fence.
- Allowed types: `flowchart`, `sequenceDiagram`, `stateDiagram-v2`, `classDiagram`, `erDiagram`, `quadrantChart`, `gantt`, `mindmap`.
- At most 25 nodes per diagram. Split rather than crowd.
- One caption sentence in italics immediately above each diagram, e.g. `*Figure 2: a request traversing the Filter Chain.*`
- Node labels use canonical names from the foundation pack (`Ruralz Gateway`, `Ruralz Control`, `State Store`, ...).
- Every diagram the manifest requires MUST be present; extra diagrams are welcome.
- Do not use HTML in labels; quote labels that contain parentheses or slashes: `A["Ruralz Gateway (ruralzd)"]`.

## 4. Numbers, claims, and citations

- Every performance or scale figure (latency, throughput, memory, connections, percentages) MUST carry `(target)` or `(hypothesis)` on the same line, unless the same line cites a URL to a measurement.
- Competitor and vendor claims MUST cite a URL that appears in a file under `docs/_meta/research/`. Format: `([source](https://...))`.
- Dates are ISO `YYYY-MM-DD`. The competitor snapshot date is 2026-09-23.
- Feature availability tags: `Planned (Mx)` where `Mx` is `M0`..`M5`; `Not planned` with a reason; never `Supported` for anything not yet implemented (nothing is implemented yet).

## 5. Terminology and naming

- Use the canonical names and forbidden-alias table in `docs/_meta/foundation-pack.md`.
- Kinds in `PascalCase` and code formatting: `Route`, `Upstream`, `Policy`.
- YAML keys in `camelCase`. Config examples MUST use only kinds and fields defined in `docs/architecture/02-configuration-model.md`; if a field does not exist there yet, add it to Open questions instead of inventing it.
- CLI commands as `ruralz <noun> <verb>` in code formatting; every command mentioned MUST exist in `docs/reference/01-cli-and-api-surface.md`.
- Milestones as `M0`..`M5`; never "phase", "v1", "GA" as milestone names.

## 6. Prose

- American English. No emojis. No em dashes; use commas, colons, or separate sentences.
- Short paragraphs (five sentences or fewer). Prefer active voice.
- Code fences always declare a language (`yaml`, `go`, `bash`, `json`, `protobuf`, `mermaid`, `text`).
- Cross-references are relative Markdown links: `[ADR-0003](../adr/0003-configuration-format.md)`, `[Data plane](03-data-plane.md#router)`. Never bare paths.
- No placeholders: `TODO`, `TBD`, `XXX`, `lorem`, `???`. Unknowns go to Open questions.
- Length guidance: architecture documents 2,500 to 6,500 words; operations documents 1,500 to 4,500; comparison documents as long as the tables require; ADRs 400 to 1,500. The manifest `length_words` band of each document is binding (maximums raised about 25% on 2026-09-25 so review fixes add content instead of trading against earlier fixes).

## 7. ADR format (MADR 4.0 minimal plus `Confirmation`)

File: `docs/adr/NNNN-<slug>.md`. Template:

```markdown
---
id: ADR-0003
title: Configuration format is YAML with JSON Schema
status: accepted            # proposed | accepted | deprecated | superseded-by ADR-XXXX
date: 2026-09-23
deciders: [ruralz-core]
related:
  - docs/architecture/02-configuration-model.md
---

# ADR-0003: Configuration format is YAML with JSON Schema

## Context and problem statement

## Decision drivers

## Considered options

## Decision outcome

Chosen option: "...", because ...

### Consequences

- Good, because ...
- Bad, because ...

### Confirmation

How compliance with this decision is verified (test, lint rule, review checklist item).

## Pros and cons of the options

### Option A
...

## More information
```

`docs/adr/README.md` is the ADR index: a table `ID | Title | Status | Date | Owning document`.

## 8. Review artifacts

A document is `approved` when no blocking findings remain, or `approved-with-escalations` when the second revision still disputes a blocking finding; escalations are listed in the document's Open questions.
