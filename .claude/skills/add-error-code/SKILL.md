---
description: Register a new RZ-<AREA>-<NNN> error code in internal/errcode and define it in the owning design document so TestRegistry, TestMatchesDocs and repocheck pass.
when_to_use: Code or a document needs an error code that is not registered yet, or repocheck reports "is not registered in internal/errcode".
argument-hint: <AREA> <meaning>
---

# Add error code: $ARGUMENTS

Read `.claude/rules/errcode.md` first.

1. **Check it is needed.** Search `All()` in `internal/errcode/errcode.go` and the owning document for an existing code with the same meaning; reuse it if one fits. Codes are never reused for a new meaning.
2. **Pick the area** from foundation pack section 8.6 (`CFG`, `RT`, `UP`, `AUTH`, `RL`, `PLG`, `AI`, `CP`, `STS`). `Area.Owner()` names the owning document slug. A new area is out of scope for this skill: it needs a foundation pack amendment.
3. **Pick the number.** Where the owning table groups codes (AUTH: 001 to 007 authentication, 010 to 016 authorization, 020 upstream credentials), take the next unused number after the last code of the matching group; elsewhere take the next number after the area's highest code. Never renumber or reuse. If the group is unclear, ask the user. Confirm with a search across `docs/` that the ID is unused, including range rows such as `RZ-AUTH-010 to RZ-AUTH-014`.
4. **Document it first.** Add the code and its meaning to the owning document's error table, matching the neighboring rows, plus its HTTP status or signal only if that table has a status column (the CFG, PLG and CP tables have none). Respect the style guide and the document's word band. If the design is unsettled, add an `OQ-<docslug>-<n>` row instead and stop.
5. **Register it.** Add `{ID: "RZ-<AREA>-<NNN>", Area: Area<AREA>, Meaning: "<meaning>"}` to `All()` in ascending order within the area, adding `Status: <400-599>` when the table gives one HTTP status or `StatusNote: "<table text>"` when it gives another status or signal. Never set both.
6. **Test**: `go test ./internal/errcode/ ./internal/tool/repocheck/` (PowerShell: set `$env:CGO_ENABLED = "0"` first if cgo is unavailable). If `go` is missing, say the tests were not run.
7. **Report** the ID, the document section and a suggested commit title. The doc and code change go in one pull request; with a code change use its component scope (for example `feat(gateway): ...`), and for a registry-only change `feat(api): ...`, the scope the registry landed with.
