@AGENTS.md

## Claude Code notes

- The guides in `.claude/rules/` load automatically when you read a file under their `paths:`.
- Run `make` through the Bash tool (Git Bash). In PowerShell, set `$env:CGO_ENABLED = "0"` before `go test`.
- No Workflow-tool runs, and no review, verification or checker subagents. Subagents only build independent parts of the work in parallel.
- Commit with `git commit -S -s` and no `Co-Authored-By:` trailer. Never commit or push unless the user asks.
- `.claude/settings.json` pre-approves the Go build, test and generate commands, the repo tools and the safe make targets; it asks before `git push` and before editing `docs/_meta/`, `go.mod`, `.github/`, `nolicensecheck.allow` and the security-sensitive packages, and denies edits to `api/schema/ruralz/` (regenerate it with `go generate ./pkg/config/v1alpha1`).
