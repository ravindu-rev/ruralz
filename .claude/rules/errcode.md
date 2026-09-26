---
paths:
  - "internal/errcode/**"
---

# Error code registry

Authority: foundation pack section 8.6 (areas and owners) and the owning document of each area. Use `/add-error-code` for the full recipe.

- The registry is the literal slice returned by `All()` in `internal/errcode/errcode.go`, ordered by area, then number. There are no per-code Go constants; lookup is by ID string (`Lookup`).
- Areas and their owning document slugs come from `Area.Owner()`: CFG `configuration-model`, RT `data-plane`, UP and RL `traffic-management-and-resilience`, AUTH `security-and-identity`, PLG `wasm-plugin-system`, AI `ai-llm-gateway`, CP `control-plane-and-gitops`, STS `scalability-and-distributed-state`.
- A code never changes meaning, and is never reused or renumbered. AUTH numbers are grouped (001 to 007 authentication, 010 to 016 authorization, 020 upstream credentials, per the error table in `08-security-and-identity.md`); other areas are contiguous. No document fixes a wider numbering policy, so place a new code where the owning table puts it.

## Adding a code

1. Define it in the owning document's error table first. `TestMatchesDocs` requires every registered code to appear in some `docs/**/*.md`, and every code a document names to be registered; range rows such as `RZ-AUTH-010 to RZ-AUTH-014` count every code in between.
2. Add one row to `All()` in ascending order within its area, keeping each area's rows together.
3. Copy the owning table's status column: set `Status` (400 to 599) when it gives one HTTP status, `StatusNote` with the table's text when it gives anything else, and neither when the table has no status column (the CFG, PLG and CP tables are `| Code | Meaning |`, so those rows set only `ID`, `Area` and `Meaning`). Never set both. `Meaning` restates the document's meaning.
4. Run `go test ./internal/errcode/ ./internal/tool/repocheck/`.

## Adding an area

An area is a foundation pack change (section 14): update the section 2 Error codes row and the section 8.6 table, then the `Area` constant with its owner comment, `Owner()` and the `Pattern` alternation together. `Valid` and `TestRegistry` use `Owner()`; repocheck and `TestMatchesDocs` use `Pattern`.

## Tests

`TestRegistry` checks syntax, uniqueness, the ID prefix against its area, an owner, a non-empty meaning, the status range, Status and StatusNote not both set, and ascending IDs between adjacent rows of one area (`internal/errcode/errcode_test.go:17-48`). repocheck checks RZ codes in Go string literals everywhere except `internal/errcode/` and its skipped directories (`repocheck.go:49-54,136`), so a Go string literal elsewhere must name a registered code.
