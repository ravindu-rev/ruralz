@AGENTS.md

## Claude Code notes

- The area guides in `.claude/rules/` load automatically when you read a file under their `paths:`. They are short; still open the cited doc section before changing behavior it owns.
- Skills: `/preflight` runs the local equivalent of CI stages 1 to 6 and reports what it skipped; `/add-error-code`, `/change-config-schema` and `/new-adr` walk the multi-file changes. `/new-adr` runs only when the user types it, because an ADR records a decision; if the user asks for an ADR in words, read `.claude/skills/new-adr/SKILL.md` and follow it.
- `.claude/settings.json` pre-approves `go build`, `vet`, `test`, `generate`, `list` and `mod verify`, the three repo tools, `make help`, `hygiene`, `generate`, `build`, `test` and `floor FLOOR_GOTOOLCHAIN=go1.26.8`, and `git status` and `ls-files` (`go generate`, `make generate` and `make build` write files). `make lint`, `tools` and `supply-chain` download tools, so they still prompt; `/preflight` grants its own git and lint commands for its turn. It asks before `git push` and before editing `docs/_meta/`, `go.mod`, `.github/`, `nolicensecheck.allow` and the security-sensitive packages, and denies edits to the generated schema under `api/schema/ruralz/`; regenerate it with `go generate ./pkg/config/v1alpha1` instead. Edits to `pkg/` and `api/` do not prompt but still need two approvals.
- Run `make` through the Bash tool (Git Bash). In PowerShell, set `$env:CGO_ENABLED = "0"` before `go test`.
- Commit with `git commit -s` so the `Signed-off-by:` name and email equal the configured git author, and keep the `Co-Authored-By:` trailer as well. Never commit or push unless the user asks.
- Before finishing a change, run `/preflight` or the commands it lists, and report any check you could not run.
