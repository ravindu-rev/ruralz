---
paths:
  - "internal/errcode/**"
---

# Error code registry

- The registry is the slice returned by `All()` in `internal/errcode/errcode.go`, ordered by area, then number; lookup is by ID string.
- Areas and owning documents come from `Area.Owner()`: CFG configuration-model, RT data-plane, UP and RL traffic-management-and-resilience, AUTH security-and-identity, PLG wasm-plugin-system, AI ai-llm-gateway, CP control-plane-and-gitops, STS scalability-and-distributed-state.
- A code never changes meaning and is never reused or renumbered. AUTH numbers are grouped (001 to 007 authentication, 010 to 016 authorization, 020 upstream credentials); other areas take the next number.
- Adding a code: add it to the owning document's error table, then one row to `All()` in order. Copy the table's status column: `Status` for one HTTP status, `StatusNote` for other text, neither when the table has no status column; never both. Run `go test ./internal/errcode/ ./internal/tool/repocheck/` with the other gates at the end.
- A new area is a foundation pack change: section 8.6, the `Area` constant, `Owner()` and the `Pattern` alternation together.
