---
description: Create a new Ruralz Architecture Decision Record in docs/adr with the MADR outline and update every index, manifest entry, foundation pack row and document reference that lists or counts ADRs.
when_to_use: The user asks to write, add, propose or supersede an ADR, or to record an architecture decision.
argument-hint: <decision title>
disable-model-invocation: true
---

# New ADR: $ARGUMENTS

Read `.claude/rules/adr.md` and `.claude/rules/docs.md` first. An ADR records a decision the user has made or wants proposed; do not decide the option yourself. If the chosen option, the owning document or the status (`proposed` or `accepted`) is unclear, ask before writing.

## 1. Gather

- Next number: highest `docs/adr/NNNN-*.md` plus one. Slug: short, lower-case, hyphenated; it becomes the `docs(<slug>)` scope for later edits, so check that no other file under `docs/` has it.
- Owning document: the design document that will state the decision in full.
- Evidence: every external fact needs a URL already in `docs/_meta/research/*`. If one is missing, stop and tell the user a research addendum is needed first.
- Superseding? Read the old ADR and run `grep -rn '<old ADR-ID>' docs README.md` to find every document that cites it.

## 2. Write `docs/adr/NNNN-<slug>.md`

Front matter `id`, `title` (quoted if it has a colon), `status`, `date` (today, ISO), `deciders: [ruralz-core]`, `related`. Then `# ADR-NNNN: <title>` and the six H2s in order, with `Chosen option: "...", because ...` plus `### Consequences` and `### Confirmation` under Decision outcome. At least two options, each with pros and cons. 400 to 1,500 words.

## 3. Bookkeeping (all in the same change)

1. `docs/adr/README.md`: add the row `| [ADR-NNNN](NNNN-<slug>.md) | <title> | <status> | <date> | [<Owning Doc Title>](../<path>) |` and update the count sentence ("of the N ADRs below, X are `accepted` and Y are `proposed`").
2. `docs/README.md`: add the row `| [ADR-NNNN](adr/NNNN-<slug>.md) | <title> | <status> | <date> | <Owning Doc Title> |` to the ADR index table (link under `adr/`, owning document as plain title). Update the Summary sentence "indexes the N Architecture Decision Records", the count sentence under ADR index, the `docs/adr/` document count in the Organization table, the front-matter `adrs` list and the `ADR-0001` to `ADR-NNNN` range in the Conventions table; bump `last_updated`.
3. `docs/_meta/manifest.yaml`: append an `adrs:` entry with `id`, `path`, `title`, `status`, `owning_doc`, `depends_on`, `research` and a one-sentence `decision`, copying `wave`, `mode` and `lenses` from an existing entry. In the `docs-index` entry, append the ID to its `adrs` list and update "lists all N ADRs" in its acceptance and the ADR index `min_table_rows` value. Append the ID to the owning document's manifest `adrs` list.
4. `docs/_meta/foundation-pack.md`: add or change the section 7 row and extend the `ADR-0001`..`ADR-NNNN` range in section 2; bump `last_updated`. Tell the user this `docs/_meta/` change needs two approvals and falls under section 14.
5. Owning document: link the ADR where the decision is stated and add it to the front-matter `adrs` list; bump `last_updated`.
6. Library decisions: when the ADR selects a library, add or change its research-backed catalog row in `docs/engineering/01-tech-stack-and-libraries.md` (linking the ADR) and list each rejected library under "Alternatives not chosen" with a research URL. If the tech stack document is not the owning document, also add the ADR to its front-matter `adrs` list and bump `last_updated`, keeping the edit word-neutral.
7. Superseding: set the old ADR's status to `superseded-by ADR-NNNN` in its front matter, its manifest `adrs:` entry and both index tables, adding a superseded count to both count sentences. Change nothing else in the old ADR. Point the foundation pack section 7 row at the new ADR. Wherever the grep from step 1 finds a document stating the old decision or its status, cite the new ADR instead, add it to that document's front-matter `adrs` (and its manifest `adrs` list) and bump `last_updated`, keeping each edit word-neutral.

## 4. Check

- H2 order, the two H3s, word count, no em dashes, no `TODO`/`TBD`, relative links resolve, every count updated consistently.
- Every `RZ-` code you mention is registered: run `go test -run TestMatchesDocs ./internal/errcode/`, or check `All()` by hand and say so.
- Suggested commit title: `docs: add ADR-NNNN <short subject>` (several documents change, so no scope), with `git commit -s`. Do not commit unless the user asks.
