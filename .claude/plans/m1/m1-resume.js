export const meta = {
  name: 'm1-resume',
  description: 'Resume M1 work packages: review saved implementations or implement from scratch, then fix and commit',
  phases: [
    { title: 'Implement', detail: 'one engineer agent per work package, owned paths only' },
    { title: 'Review', detail: 'adversarial reviewer per work package' },
    { title: 'Fix', detail: 'apply review findings' },
    { title: 'Re-review', detail: 'second adversarial pass after fixes' },
    { title: 'Commit', detail: 'commit each passing work package to develop (the lead pushes)' },
  ],
}

const REPO = '/home/user/ruralz'
const M1 = '/home/user/ruralz/.claude/plans/m1'
const ARCH = `${M1}/specs/00-architecture.md`
const WPS = `${M1}/arch/wps.json`
const ids = args.ids
const extra = args.extra || {}
const reported = new Set(args.reported || [])
const titles = args.titles || {}
const savedReviews = args.reviews || {}
// deferMinor: run a fix round only for blocker or major findings; minor findings are
// returned in full (remainingMinor) for one batched cleanup at the end of the wave.
const deferMinor = !!args.deferMinor
// maxRounds: per work package cap on fix and re-review rounds (default 2).
const maxRounds = args.maxRounds || {}
const reportFile = (id) => `${M1}/reports/${id}.json`

const RULES = `
Repository: ${REPO} (Go module github.com/ravindu-rev/ruralz, branch develop). Ruralz is an API gateway; milestone M1 "Core gateway" is being implemented from a reviewed design:
- Integration architecture (binding): ${ARCH}. Section 0 = conventions binding every work package; 1.2 = package table (responsibility, allowed imports, third-party modules); 1.3 = lint/import rules; 2 = shared core contracts (already committed as Go code in the WP-01 packages); 2.16 = resolved conflicts; 3 = data flow; 4.1 = work package rules; 4.4 = each work package's Owns / Depends on / Specs / Scope / Done when.
- Machine-readable work packages: ${WPS} (JSON array; find your entry by id).
- Area specs with numbered normative requirements: ${M1}/specs/01-config-load.md ... 11-ops-quality.md. A reference like "04 req 34" or "04 34" means spec 04-..., section 2 requirement 34; "07 H 56-68" means spec 07 group H requirements 56-68. Specs cite the design documents under docs/ (binding; docs/_meta/foundation-pack.md fixes names).
Hard rules:
1. Write ONLY inside the paths your work package owns (its "dirs"/Owns list). Other agents are editing other paths in this same working tree right now.
2. Never run git commands that change state (no git add/commit/stash/checkout/reset/restore/clean). Read-only git (status, diff, log) is fine.
3. Never edit go.mod, go.sum, internal/tool/modpin, the Makefile, CI workflows or any WP-01 contract package (internal/{clock,phase,problem,expr,adminapi,errcode}, internal/config/{diag,tree,revision,hub}, internal/secret/*.go root, internal/statestore/*.go root, internal/filter/*.go root, internal/filter/filtertest, internal/telemetry/{catalog,emit}, internal/gateway/snapshot) unless you ARE that owner. If a contract or module is missing or wrong, work around it minimally inside your paths and report it as a contract change request.
4. Never run whole-tree mutating commands: no go mod tidy, no gofmt -w ., no golangci-lint fmt ./... . Scope everything to your own packages, e.g. go test -race -count=1 ./internal/x/..., go vet ./internal/x/..., bin/golangci-lint run ./internal/x/..., bin/golangci-lint fmt ./internal/x/... . golangci-lint may wait for another run's lock; that is fine. If a package you do not own fails to compile because another agent is mid-edit, ignore it; your own packages and their tests must build and pass.
5. Code conventions (architecture section 0 and docs/engineering/02-repository-layout-and-conventions.md): two-line license header "// Copyright 2026 Revington" + "// SPDX-License-Identifier: Apache-2.0" on every file; context first; every goroutine has an owner and stop path; bounded queues; %w wrapping; RZ codes only from internal/errcode; slog only via internal/telemetry loggers; no init(); no mutable package-level variables; no fmt.Print* outside internal/cli; clock via internal/clock; documented exported identifiers; American spelling.
6. Tests: go test -race; table tests; tests cite the requirement numbers they cover (in test names or comments); fuzz targets and benchmarks your spec names; aim for 80%+ statement coverage per package (90% for loader, validation, precedence and canonical form packages).
7. Documentation work packages edit only the document sections their row names, follow docs/_meta/style-guide.md (binding), and cite a URL from docs/_meta/research/ for any library/vendor claim.
8. The committed Go code of the WP-01 contract packages is authoritative where it differs from the architecture's section 2 text (the WP-01 tests fixed diag escaping, ConnCache.Put replacing in place, and a clocktest race). Read the contract source, not only the document.
9. The repository now has AGENTS.md, CLAUDE.md and .claude/rules/*.md (agent rules written for M0). Follow their hard rules and conventions; where they say an M1 package or module does not exist yet, or that no third-party module is admitted, the M1 architecture and the committed go.mod supersede them.
`

const IMPL_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string', description: 'what was built, 3-8 sentences' },
    paths: { type: 'array', items: { type: 'string' }, description: 'repository paths created or modified' },
    requirements: { type: 'string', description: 'requirement numbers implemented, and any in scope NOT implemented with reasons' },
    checks: { type: 'string', description: 'exact commands run and their results (tests, vet, lint, coverage per package)' },
    contractRequests: { type: 'array', items: { type: 'string' } },
    deviations: { type: 'array', items: { type: 'string' } },
    commitTitle: { type: 'string', description: 'Conventional Commits title for this work, e.g. feat(config): add the restricted YAML profile' },
  },
  required: ['status', 'summary', 'paths', 'requirements', 'checks', 'contractRequests', 'deviations', 'commitTitle'],
}
const REVIEW_SCHEMA = {
  type: 'object',
  properties: {
    verdict: { type: 'string', enum: ['pass', 'fix'] },
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          where: { type: 'string', description: 'file:line or requirement id' },
          issue: { type: 'string' },
          fix: { type: 'string' },
        },
        required: ['severity', 'where', 'issue', 'fix'],
      },
    },
    summary: { type: 'string' },
  },
  required: ['verdict', 'findings', 'summary'],
}

const implPrompt = (id) => `${RULES}
You are the engineer for work package ${id}. Read its entry in ${WPS} and its section in ${ARCH} 4.4, then architecture sections 0, 1 (your package rows and their allowed imports), 1.3, the section 2 contracts your packages use, 2.16, and section 3 where relevant. Read every spec requirement your scope cites, in full, plus the design-document sections those requirements reference when you need more precision.
${extra[id] || ''}
An earlier attempt at this work package may have been interrupted by a usage limit and left files in your owned paths. If so, read them first, keep what is correct and consistent with the spec, fix or replace the rest, and complete the package.
Build the work package completely: production code, tests, fuzz targets and benchmarks its scope and spec name, until its "Done when" holds. Prefer complete, correct, well-tested code over breadth; do not leave TODOs for in-scope requirements. When the scope is larger than you can finish, finish whole requirements rather than half of each, and report exactly what remains.
Before finishing, run (scoped to your packages): go build, go vet, go test -race -count=1 with -cover, bin/golangci-lint run, bin/golangci-lint fmt, and go run ./internal/tool/repocheck (it scans the whole tree; report only findings in your paths). Fix everything in your paths.
Return the structured report.`

const reviewPrompt = (id, impl) => `${RULES}
You are an adversarial code reviewer for work package ${id}. Your job is to find real defects before this lands; default to skepticism. Read its entry in ${WPS}, its section in ${ARCH} 4.4, the contracts it uses (section 2), and the spec requirements its scope cites.
${impl && impl.reportFile ? `The engineer's report is saved at ${impl.reportFile}; read it first.` : `The engineer reported:\n${JSON.stringify(impl, null, 1).slice(0, 6000)}`}
Review the files in its owned paths (read them fully; use git status and git diff to see what changed; untracked files are new). Check:
- every in-scope requirement is implemented correctly and has a test that would fail if it broke (cite requirement numbers); missing requirements are blockers;
- correctness bugs, races, goroutine leaks, unbounded queues, error handling, RZ codes and HTTP statuses, security issues (secrets in logs, SSRF, injection), edge cases;
- conformance to the section 2 contracts and allowed imports; files written outside owned paths;
- conventions (section 0) and the "Done when" criteria.
Run the checks yourself (scoped): go test -race -count=1 -cover, go vet, bin/golangci-lint run. Report each finding with severity (blocker = wrong or missing required behavior or failing checks; major = likely bug or significant gap; minor = style or small improvement), location and a concrete fix. Do NOT modify any file. verdict is "pass" only if there are no blocker or major findings.`

const fixPrompt = (id, review) => `${RULES}
You are the engineer for work package ${id}, fixing review findings. Read its entry in ${WPS} and ${ARCH} 4.4 as needed. Findings:
${JSON.stringify(review.findings, null, 1).slice(0, 12000)}
Fix every blocker and major finding, and every minor finding that is plainly correct. If you believe a finding is wrong, do not change the code for it and explain why in deviations. Stay inside the owned paths. Re-run the scoped checks (build, vet, go test -race -count=1 -cover, bin/golangci-lint run, bin/golangci-lint fmt, repocheck) until green. Return the structured report (paths = files changed in this pass).`

const rereviewPrompt = (id, review, fix) => `${RULES}
You are an adversarial re-reviewer for work package ${id}. A previous review raised:
${JSON.stringify(review.findings, null, 1).slice(0, 8000)}
The engineer reported these fixes:
${JSON.stringify(fix, null, 1).slice(0, 4000)}
Verify each finding is actually fixed (read the code, run the scoped checks: go test -race -count=1 -cover, go vet, bin/golangci-lint run), judge any "finding is wrong" rebuttals, and look for regressions or new defects introduced by the fixes. Do NOT modify files. verdict is "pass" only if no blocker or major finding remains.`

const COMMIT_SCHEMA = {
  type: 'object',
  properties: {
    committed: { type: 'boolean' },
    sha: { type: 'string' },
    pushed: { type: 'boolean' },
    files: { type: 'integer' },
    note: { type: 'string' },
  },
  required: ['committed', 'sha', 'pushed', 'files', 'note'],
}
const commitPrompt = (id, title, summary, paths) => `You commit one finished work package, ${id}, in the git repository ${REPO} (branch develop). Other agents are editing other paths in this same working tree right now, so be exact.
1. Read the work package's owned paths from ${WPS} (entry ${id}, field dirs). The engineer reported these changed paths: ${JSON.stringify(paths).slice(0, 3000)}
   ${reported.has(id) ? `(The engineer's full report, with its changed paths and summary, is at ${reportFile(id)}.)` : ''}
2. Stage ONLY files inside the owned paths: run git status --porcelain, pick the entries inside the owned paths (for a "dir/*.go (package root only)" entry, only files directly in that directory; for a document entry, only that file), and git add them by explicit path. Never use git add -A, git add ., or git commit -a. Then run git diff --cached --name-only and verify every staged file is inside the owned paths; unstage anything else with git restore --staged <path>.
3. If nothing is staged, report committed=false.
4. Validate the subject first: go run ./internal/tool/commitcheck title -title "<subject>". The proposed subject is: ${title}
   If the check fails, keep the meaning but fix the form: feat/fix/perf/refactor scopes are only gateway, control, cli, config, filter, plugin, ai, statestore, controlstream, console, sdk, api, deploy (or no scope); test adds e2e and conformance; build and ci take deps, tools or workflows; docs takes a document slug (a file name under docs/ without its numeric prefix) or no scope. For example feat(jsonval): ... becomes feat: add the strict JSON value library, and feat(gateway/executor): ... becomes feat(gateway): .... Re-run the check until it passes.
   Commit with: git -c commit.gpgsign=false commit -s -F <message file>, using the validated subject.
   The body: 2-6 lines, wrapped at 72 columns, describing what the work package adds (from this summary: ${String(summary).slice(0, 1500)}), ending with a line "Refs: ${id} (M1 architecture)". Do not add any Co-Authored-By or other trailers besides the Signed-off-by that -s adds. The author and committer come from the repository's git config; do not change them.
5. If git reports that the index.lock file exists, wait a few seconds and retry (another agent is committing), up to 10 times.
6. Do NOT push; the lead pushes develop. Never pull, rebase, merge, amend or reset. Report pushed=false.
Report committed, the commit sha, pushed, the number of files, and a note.`

const blocking = (r) => !r || r.verdict !== 'pass' || r.findings.some(f => f.severity !== 'minor')

const results = await pipeline(ids,
  (id) => reported.has(id)
    ? Promise.resolve({ status: 'done', commitTitle: titles[id] || `feat: add ${id}`, summary: `See the engineer report at ${reportFile(id)}.`, paths: [], reportFile: reportFile(id) })
    : agent(implPrompt(id), { label: `impl:${id}`, phase: 'Implement', schema: IMPL_SCHEMA }),
  (impl, id) => savedReviews[id]
    ? Promise.resolve({ impl, review: savedReviews[id] })
    : agent(reviewPrompt(id, impl), { label: `review:${id}`, phase: 'Review', schema: REVIEW_SCHEMA })
      .then(review => ({ impl, review })),
  async (prev, id) => {
    let { impl, review } = prev
    const rounds = []
    let current = review
    for (let round = 1; round <= (maxRounds[id] || 2) && current && (blocking(current) || (!deferMinor && current.findings.length > 0)); round++) {
      const fix = await agent(fixPrompt(id, current), { label: `fix${round}:${id}`, phase: 'Fix', schema: IMPL_SCHEMA })
      const again = await agent(rereviewPrompt(id, current, fix), { label: `rereview${round}:${id}`, phase: 'Re-review', schema: REVIEW_SCHEMA })
      rounds.push({ fix, review: again })
      current = again
      if (again && !blocking(again)) break
    }
    let commit = null
    if (args.commit && current && !blocking(current) && impl && impl.status !== 'blocked') {
      const lastFix = rounds.length ? rounds[rounds.length - 1].fix : null
      const paths = [...new Set([...(impl.paths || []), ...rounds.flatMap(x => (x.fix && x.fix.paths) || [])])]
      commit = await agent(commitPrompt(id, impl.commitTitle, impl.summary, paths), { label: `commit:${id}`, phase: 'Commit', schema: COMMIT_SCHEMA, effort: 'low' })
    }
    return { impl, review, rounds, final: current, commit }
  })

return results.map((r, i) => {
  const id = ids[i]
  if (!r) return { id, error: 'pipeline failed' }
  const fixes = r.rounds.map(x => x.fix).filter(Boolean)
  return {
    id,
    status: r.impl?.status,
    commitTitle: r.impl?.commitTitle,
    summary: r.impl?.summary,
    paths: [...new Set([...(r.impl?.paths || []), ...fixes.flatMap(f => f.paths || [])])],
    requirements: r.impl?.requirements,
    checks: (fixes.length ? fixes[fixes.length - 1].checks : r.impl?.checks),
    contractRequests: [...(r.impl?.contractRequests || []), ...fixes.flatMap(f => f.contractRequests || [])],
    deviations: [...(r.impl?.deviations || []), ...fixes.flatMap(f => f.deviations || [])],
    firstReview: r.review ? { verdict: r.review.verdict, counts: ['blocker', 'major', 'minor'].map(s => r.review.findings.filter(f => f.severity === s).length) } : null,
    finalVerdict: r.final?.verdict,
    remaining: (r.final?.findings || []).filter(f => f.severity !== 'minor'),
    remainingMinor: (r.final?.findings || []).filter(f => f.severity === 'minor'),
    commit: r.commit,
  }
})
