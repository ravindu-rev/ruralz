export const meta = {
  name: 'm1-section',
  description: 'Implement M1 work packages, then per package a four-lens adversarial review, independent reproduction of each serious finding and a fix, repeated until a round confirms none',
  phases: [
    { title: 'Implement', detail: 'one engineer per work package, owned paths only' },
    { title: 'Review', detail: 'four lenses per package: spec, adversarial, integration, tests and code' },
    { title: 'Merge', detail: 'deduplicate each round\'s findings' },
    { title: 'Verify', detail: 'independent reproduction of each blocker or major finding' },
    { title: 'Fix', detail: 'fix confirmed findings' },
  ],
}
// Args: ids, extra, reported, titles (as m1-resume.js); maxRounds per package (default 3
// review rounds; the last round reviews and verifies only). The lead commits.
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


const FINDING = REVIEW_SCHEMA.properties.findings.items
const MERGE_SCHEMA = {
  type: 'object',
  properties: {
    findings: { type: 'array', items: { type: 'object', properties: { ...FINDING.properties, sources: { type: 'array', items: { type: 'string' } } }, required: ['severity', 'where', 'issue', 'fix', 'sources'] } },
    minors: { type: 'array', items: FINDING },
  },
  required: ['findings', 'minors'],
}
const VERDICT_SCHEMA = {
  type: 'object',
  properties: {
    confirmed: { type: 'boolean', description: 'true only if you reproduced the defect on the current code, or it is a plain requirement gap or failing check you can point to' },
    severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
    evidence: { type: 'string', description: 'the exact probe or command you ran and its output' },
    note: { type: 'string' },
  },
  required: ['confirmed', 'severity', 'evidence', 'note'],
}
const FIX_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string' },
    paths: { type: 'array', items: { type: 'string' } },
    checks: { type: 'string' },
    contractRequests: { type: 'array', items: { type: 'string' } },
    deviations: { type: 'array', items: { type: 'string' } },
  },
  required: ['status', 'summary', 'paths', 'checks', 'contractRequests', 'deviations'],
}

const LENSES = [
  { key: 'spec', title: 'spec conformance', focus: 'Check every requirement in the package scope (wps.json entry, architecture 4.4, every spec requirement it cites, in full, and the design-document sections they reference) is implemented correctly and has a test that would fail if it broke. Check RZ codes, statuses, messages and positions against the specs. Missing or wrong required behavior is a blocker.' },
  { key: 'adversarial', title: 'adversarial inputs and failure modes', focus: 'Try to break it. Hostile and edge-case inputs (empty, huge, deeply nested, unicode, duplicates, nulls, wrong types), resource bounds and complexity (anything quadratic or unbounded), concurrency (races, goroutine leaks, shared mutable state, cancellation), error paths and partial failure, and security (secrets in logs or errors, injection, SSRF, path traversal) where relevant. Measure with probes; report the exact input and what you observed.' },
  { key: 'integration', title: 'contracts and integration', focus: 'Check the package against the section 2 contracts and the committed code it uses (read the real source of internal/config/{diag,tree,revision,schemaidx,...} and other dependencies, not only the architecture text), the allowed imports in 1.2 and 1.3, and the needs of the later work packages that will call it (find them in wps.json dependsOn and the architecture data flow in section 3): is the exported API what they need, are ownership, mutation and concurrency rules of shared values respected, are errors and diagnostics in the shapes callers expect?' },
  { key: 'code', title: 'tests and code quality', focus: 'Review as a senior Go reviewer: correctness, clarity, dead code, comments that do not match the code, package docs, allocation and complexity. For the tests: would each fail if its behavior broke (mutate with go test -overlay)? Are they deterministic under -race -shuffle=on on 4 CPUs? Do fuzz targets and benchmarks the spec names exist and run? Run go build, go vet, go test -race -count=1 -cover -shuffle=on, CGO_ENABLED=0 go test -count=1, bin/golangci-lint run, bin/golangci-lint fmt --diff and go run ./internal/tool/repocheck, scoped to the package. A failing check is a blocker.' },
]

const lensPrompt = (id, lens, round, impl, history) => `${RULES}
You are an adversarial reviewer of work package ${id} (round ${round}), lens: ${lens.title}. Default to skepticism. Do NOT modify any repository file; probes go in a scratch directory outside the repository and reach the package through go test -overlay.
Read the work package's entry in ${WPS} and its 4.4 section in ${ARCH}. ${impl && impl.reportFile ? `The engineer's report is at ${impl.reportFile}.` : `The engineer reported:\n${JSON.stringify(impl, null, 1).slice(0, 5000)}`}
Review the files in its owned paths (read them fully; git status shows untracked new files).
${lens.focus}
${history ? `Findings already fixed in earlier rounds (do not re-report them unless the fix is incomplete or broke something, and say so):\n${history.slice(0, 10000)}` : ''}
Severity: blocker = required behavior wrong or missing, a bound or contract broken, or a failing check; major = a likely bug or significant gap; minor = style or small improvement. Every blocker and major finding must include the exact input or command that shows it and what you observed. verdict is "pass" only if there are no blocker or major findings.`

const mergePrompt = (id, reviews) => `${RULES}
Four reviewers looked at work package ${id} through different lenses. Merge their findings: group findings that describe the same defect (same root cause or same fix) into one, keeping the most precise input, location and fix text and the highest severity, and list the lenses in sources. Put every blocker and major finding in findings and every minor in minors. Do not drop or invent findings and do not judge validity. Do not modify files.
Reviews:
${JSON.stringify(reviews, null, 1).slice(0, 60000)}`

const verifyPrompt = (id, f) => `${RULES}
You independently verify one review finding on work package ${id}. Reproduce it on the current code with a concrete probe (go test -overlay with a scratch test file outside the repository, or the package's own tests); do not modify repository files. Confirm it only if you reproduce the defect, or if it is a plain requirement gap or failing check you can point to (cite the requirement text). If the code behaves correctly, or the behavior is required by the spec or a documented deviation, set confirmed=false and explain. Give your own severity.
Finding:
${JSON.stringify(f, null, 1)}`

const hardenFixPrompt = (id, findings, minors) => `${RULES}
You are the engineer for work package ${id}, fixing verified review findings. Each was reproduced by an independent verifier (evidence included). Fix every one at its root cause, with a test that fails without the fix. Also fix each minor finding that is plainly correct; explain any you reject in deviations. Stay inside the owned paths.
Verified findings:
${JSON.stringify(findings, null, 1).slice(0, 30000)}
Minor findings:
${JSON.stringify(minors, null, 1).slice(0, 8000)}
Re-run the scoped checks until green (go build, go vet, go test -race -count=1 -cover -shuffle=on, CGO_ENABLED=0 go test -count=1, bin/golangci-lint run, bin/golangci-lint fmt, go run ./internal/tool/repocheck, and the fuzz targets for at least 60s each). Return the structured report.`

// stopped: the engineer did no work (failed, or reported blocked with no files). Reviewing an
// empty package only repeats the refusal, so the package stops with finalVerdict 'blocked'
// (README, "Starting a workflow run").
const stopped = (x) => !x || (x.status === 'blocked' && !(x.paths || []).length)

const harden = async (id, impl) => {
  const MAX = maxRounds[id] || 3
  const rounds = []
  let history = ''
  let minors = []
  let clean = false
  let stoppedAt = null
  for (let round = 1; round <= MAX; round++) {
    const reviews = (await parallel(LENSES.map(lens => () =>
      agent(lensPrompt(id, lens, round, impl, history), { label: `review${round}:${lens.key}:${id}`, phase: 'Review', schema: REVIEW_SCHEMA })
        .then(r => r && { lens: lens.key, ...r })))).filter(Boolean)
    const all = reviews.flatMap(r => r.findings.map(f => ({ ...f, lens: r.lens })))
    if (!all.some(f => f.severity !== 'minor')) {
      minors = all
      rounds.push({ round, reviews, confirmed: [], minors })
      clean = true
      break
    }
    const merged = await agent(mergePrompt(id, reviews), { label: `merge${round}:${id}`, phase: 'Merge', schema: MERGE_SCHEMA })
    const candidates = merged ? merged.findings : all.filter(f => f.severity !== 'minor')
    minors = merged ? merged.minors : all.filter(f => f.severity === 'minor')
    const verdicts = (await parallel(candidates.map((f, i) => () =>
      agent(verifyPrompt(id, f), { label: `verify${round}:${id}:${i + 1}`, phase: 'Verify', schema: VERDICT_SCHEMA })
        .then(v => ({ finding: f, verdict: v }))))).filter(Boolean)
    const confirmed = verdicts.filter(x => x.verdict && x.verdict.confirmed && x.verdict.severity !== 'minor')
    minors = [...minors, ...verdicts.filter(x => x.verdict && x.verdict.confirmed && x.verdict.severity === 'minor').map(x => x.finding)]
    log(`${id} round ${round}: ${candidates.length} serious candidates, ${confirmed.length} confirmed`)
    if (!confirmed.length) { rounds.push({ round, reviews, verdicts, confirmed: [], minors }); clean = true; break }
    if (round === MAX) { rounds.push({ round, reviews, verdicts, confirmed, minors }); break }
    const fix = await agent(hardenFixPrompt(id, confirmed.map(x => ({ ...x.finding, evidence: x.verdict.evidence })), minors), { label: `fix${round}:${id}`, phase: 'Fix', schema: FIX_SCHEMA })
    rounds.push({ round, reviews, verdicts, confirmed, minors, fix })
    if (stopped(fix)) { log(`${id}: fix round ${round} did no work; stopping`); stoppedAt = `fix${round}`; break }
    history +=`\nRound ${round} fixed:\n` + JSON.stringify(confirmed.map(x => ({ severity: x.verdict.severity, where: x.finding.where, issue: String(x.finding.issue).slice(0, 500) })), null, 1)
    if (fix && fix.deviations && fix.deviations.length) history += `\nEngineer deviations in round ${round}:\n` + JSON.stringify(fix.deviations, null, 1).slice(0, 2000)
    minors = []
  }
  return { rounds, clean, minors, stoppedAt }
}

const results = await pipeline(ids,
  (id) => reported.has(id)
    ? Promise.resolve({ status: 'done', commitTitle: titles[id] || `feat: add ${id}`, summary: `See the engineer report at ${reportFile(id)}.`, paths: [], reportFile: reportFile(id) })
    : agent(implPrompt(id), { label: `impl:${id}`, phase: 'Implement', schema: IMPL_SCHEMA }),
  async (impl, id) => {
    if (stopped(impl)) {
      log(`${id}: engineer did no work (${impl ? impl.status : 'no result'}); skipping review and fixes`)
      return { impl, h: { rounds: [], clean: false, minors: [], stoppedAt: 'impl' } }
    }
    return { impl, h: await harden(id, impl) }
  })

return results.map((r, i) => {
  const id = ids[i]
  if (!r) return { id, error: 'pipeline failed' }
  const { impl, h } = r
  if (h.stoppedAt === 'impl') {
    return {
      id, status: impl?.status, summary: impl?.summary, paths: impl?.paths || [], deviations: impl?.deviations || [],
      contractRequests: impl?.contractRequests || [], clean: false, finalVerdict: 'blocked', stoppedAt: 'impl', rounds: [], remaining: [], remainingMinor: [],
    }
  }
  const fixes = h.rounds.map(x => x.fix).filter(Boolean)
  const last = h.rounds[h.rounds.length - 1]
  return {
    id,
    status: impl?.status,
    commitTitle: impl?.commitTitle,
    summary: impl?.summary,
    paths: [...new Set([...(impl?.paths || []), ...fixes.flatMap(f => f.paths || [])])],
    requirements: impl?.requirements,
    checks: fixes.length ? fixes[fixes.length - 1].checks : impl?.checks,
    contractRequests: [...(impl?.contractRequests || []), ...fixes.flatMap(f => f.contractRequests || [])],
    deviations: [...(impl?.deviations || []), ...fixes.flatMap(f => f.deviations || [])],
    clean: h.clean,
    finalVerdict: h.stoppedAt ? 'blocked' : h.clean ? 'pass' : 'fix',
    stoppedAt: h.stoppedAt || null,
    rounds: h.rounds.map(x => ({
      round: x.round,
      lenses: x.reviews.map(v => ({ lens: v.lens, verdict: v.verdict, counts: ['blocker', 'major', 'minor'].map(s => v.findings.filter(f => f.severity === s).length) })),
      confirmed: (x.confirmed || []).map(c => ({ ...c.finding, verifiedSeverity: c.verdict.severity, evidence: c.verdict.evidence })),
      refuted: (x.verdicts || []).filter(v => !v.verdict || !v.verdict.confirmed).map(v => ({ where: v.finding.where, issue: String(v.finding.issue).slice(0, 300), why: v.verdict ? v.verdict.note : 'verifier failed' })),
    })),
    remaining: h.clean ? [] : (last.confirmed || []).map(c => ({ ...c.finding, evidence: c.verdict.evidence })),
    remainingMinor: h.minors,
  }
})
