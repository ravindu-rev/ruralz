# Ruralz Documentation Style Guide

Writing rules for every file under `docs/`.

## 1. Current state only

- A document states the design as it is now. It carries no history: no dated notes or change logs, no "previously", "replaces" or "moved from" narrative, no review or approval status, no closed questions and no plan or work-package IDs. When the design changes, rewrite the text in place.
- Documents have no front matter, except ADRs (section 7).
- Availability is carried by tags (section 4), never by status sentences.

## 2. Structure

1. Exactly one H1, the document's title.
2. Design documents: first H2 `## Summary` (120 words or fewer; what the document decides and who should read it), second `## Scope and non-goals`, last `## Open questions`. H3 and below are free.
3. Open questions: a table `ID | Question | Options | Owner | Blocking?` that holds open questions only, or `None.` when there are none. IDs are `OQ-<docslug>-<n>`, the next number after the highest in the document. Options read `(a) ...; (b) ...`, marking the one the text follows `(current)` or `(proposed)`. Owner is the slug of the document that must resolve the question, or `ruralz-core`. Blocking? is `No` or `Yes, for <what> (Mx)`. An answered question leaves the table and its answer is written into the body.
4. The step list (`docs/roadmap/02-steps.md`) is an H1, a short introduction and one table.
5. Use tables for catalogs, matrices and any list with three or more attributes per item.
6. RFC 2119 keywords (`MUST`, `SHOULD`, `MAY`) in capitals for normative statements.

## 3. Diagrams

- Mermaid only, inside a ```` ```mermaid ```` fence.
- Allowed types: `flowchart`, `sequenceDiagram`, `stateDiagram-v2`, `classDiagram`, `erDiagram`, `quadrantChart`, `gantt`, `mindmap`.
- At most 25 nodes per diagram. Split rather than crowd.
- One caption sentence in italics immediately above each diagram, e.g. `*Figure 2: a request traversing the Filter Chain.*`
- Node labels use canonical names from the foundation pack (`Ruralz Gateway`, `Ruralz Control`, `State Store`, ...).
- Do not use HTML in labels; quote labels that contain parentheses or slashes: `A["Ruralz Gateway (ruralzd)"]`.

## 4. Numbers, claims, and citations

- Every performance or scale figure (latency, throughput, memory, connections, percentages) MUST carry `(target)` or `(hypothesis)` on the same line, unless the same line cites a URL to a measurement.
- Claims about external libraries, standards, licenses and releases cite their source inline: `([source](https://...))`.
- Documents describe only Ruralz, on its own terms, and never name or compare competing gateway products. Libraries, standards and provider APIs Ruralz builds on may be named.
- Dates are ISO `YYYY-MM-DD`.
- Feature availability tags: `Planned (Mx)` where `Mx` is `M0`..`M5`, or `Not planned` with a reason; never `Supported`, `Implemented` or `Shipped`. A `Planned (Mx)` tag stays until milestone Mx exits and its release ships; one pass then removes that milestone's tags: a shipped capability carries no tag, its text states it as the design, and the Feature Catalog records the release that shipped it.

## 5. Terminology and naming

- Use the canonical names and forbidden-alias table in `docs/_meta/foundation-pack.md`.
- Kinds in `PascalCase` and code formatting: `Route`, `Upstream`, `Policy`.
- YAML keys in `camelCase`. Config examples MUST use only kinds and fields defined in `docs/architecture/02-configuration-model.md`; a field that does not exist there yet is a question for the owner, never an invention.
- CLI commands as `ruralz <noun> <verb>` in code formatting; every command mentioned MUST exist in `docs/reference/01-cli-and-api-surface.md`.
- Milestones as `M0`..`M5`; never "phase", "v1", "GA" as milestone names.

## 6. Prose

- American English. No emojis. No em dashes; use commas, colons, or separate sentences.
- Short paragraphs (five sentences or fewer). Prefer active voice.
- Code fences always declare a language (`yaml`, `go`, `bash`, `json`, `protobuf`, `mermaid`, `text`).
- Cross-references are relative Markdown links: `[ADR-0003](../adr/0003-configuration-format.md)`, `[Data plane](03-data-plane.md#router)`. Never bare paths.
- No placeholders: `TODO`, `TBD`, `XXX`, `lorem`, `???`. Unknowns go to Open questions.

## 7. ADR format (MADR 4.0 minimal plus `Confirmation`)

File: `docs/adr/NNNN-<slug>.md`, the next number after the highest existing ADR. Template:

```markdown
---
id: ADR-0003
title: Configuration format is YAML with JSON Schema
status: accepted            # proposed | accepted
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

How compliance with this decision is verified (a test, lint rule or CI gate).

## Pros and cons of the options

### Option A
...

## More information
```

An ADR states the current decision: when the decision changes, the ADR is edited in place, and no ADR is kept as superseded. `docs/adr/README.md` is the ADR index: a table `ID | Title | Status | Owning document`.
