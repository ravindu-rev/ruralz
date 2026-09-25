# Resume procedure for the doc-set continuation (temporary; removed when the set is done)

## Continuing on another machine (e.g. local Claude Code on Windows)

`seed.json` already contains every finished stage up to 2026-09-24 23:0x UTC (cloud run wf_e5e0b29b-c2e merged).
Nothing else from the cloud container is needed.

1. `git pull`, then `cd scripts && npm ci`.
2. `python docs/_meta/run/gen_seed.py emit docs/_meta/run/ruralz-continue.js`
3. Dry run: Workflow({scriptPath: <abs path to ruralz-continue.js>, args: {today: "<YYYY-MM-DD>", repo: "<abs repo path, forward slashes>", maxConcurrent: 6, dryRun: true}}).
   Every finished item reports complete; the rest report their next stage.
4. Same call with dryRun false. When it ends or dies, merge its run dir
   (`~/.claude/projects/<project>/<session>/subagents/workflows/<runId>`) with `gen_seed.py merge`, commit, and relaunch.
5. When all 39 items are complete, run the finish phase of the plan (cross-doc critics, glossary, indexes, READMEs, verify, overview doc).

Runs of `ruralz-continue` (append each run dir here after it ends; all listed ones are already merged):

- /root/.claude/projects/-home-user-ruralz/8a9a4c6e-d048-5961-9dea-7de4711f34d4/subagents/workflows/wf_e5e0b29b-c2e (merged; halted on usage limit)
- /root/.claude/projects/-home-user-ruralz/8a9a4c6e-d048-5961-9dea-7de4711f34d4/subagents/workflows/wf_ed957123-cbd (current)

Liveness: the run is alive if its journal.jsonl or any agent-*.jsonl changed in the last ~10 minutes.

After a run ends or dies:

1. `python3 docs/_meta/run/gen_seed.py merge <run dir>` (every run dir not yet merged).
2. Apply finished statuses: `python3 docs/_meta/run/statuses.py | python3 docs/_meta/run/apply_status.py <today>`.
3. Commit and push `docs/` (seed.json included) directly to `main` (user decision 2026-09-25: no PRs).
4. `python3 docs/_meta/run/gen_seed.py emit docs/_meta/run/ruralz-continue.js`, dry-run it (args dryRun true), then relaunch
   with args `{today, repo: /home/user/ruralz, maxConcurrent: 2, dryRun: false}`.

On a usage-limit hit, the failed agent transcript says "resets h:mm (Asia/Colombo)": relaunch after that time.
