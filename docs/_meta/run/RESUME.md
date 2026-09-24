# Resume procedure for the doc-set continuation (temporary; removed when the set is done)

Runs of `ruralz-continue` (append each run dir here after it ends):

- /root/.claude/projects/-home-user-ruralz/8a9a4c6e-d048-5961-9dea-7de4711f34d4/subagents/workflows/wf_e5e0b29b-c2e

Liveness: the run is alive if its journal.jsonl or any agent-*.jsonl changed in the last ~10 minutes.

After a run ends or dies:

1. `python3 docs/_meta/run/gen_seed.py merge <run dir>` (every run dir not yet merged).
2. Apply finished statuses: from the run result, pipe `{path: status}` for docs into `apply_status.py <today>`.
3. Commit and push `docs/` (seed.json included) directly to `main` (user decision 2026-09-25: no PRs).
4. `python3 docs/_meta/run/gen_seed.py emit docs/_meta/run/ruralz-continue.js`, dry-run it (args dryRun true), then relaunch
   with args `{today, repo: /home/user/ruralz, maxConcurrent: 2, dryRun: false}`.

On a usage-limit hit, the failed agent transcript says "resets h:mm (Asia/Colombo)": relaunch after that time.
