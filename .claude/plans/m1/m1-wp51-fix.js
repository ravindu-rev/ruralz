export const meta = {
  name: 'wp51-targeted-fix',
  description: 'Fix WP-51 (validation.json-schema) from verified findings, then a four-lens review, reproduction and fix loop until no confirmed blocker or major remains; can resume from a findings file',
  phases: [
    { title: 'Fix', detail: 'fix the verified findings left after S2' },
    { title: 'Review', detail: 'four lenses: spec, resource bounds, integration, tests and code' },
    { title: 'Merge', detail: 'deduplicate findings' },
    { title: 'Verify', detail: 'independent reproduction of each blocker or major' },
  ],
}

const REPO = '/home/user/ruralz'
const M1 = `${REPO}/.claude/plans/m1`
const ARCH = `${M1}/specs/00-architecture.md`
const WPS = `${M1}/arch/wps.json`
const RUN = `${M1}/runs/w3-s2b-result.json`
const PKG = 'internal/filter/validation/jsonschema/'
const MAX_ROUNDS = (typeof args !== 'undefined' && args && args.maxRounds) || 3
// args (optional): maxRounds (the last review round, default 3); findingsFile, a JSON file { findings, minors, history } of findings already verified
// in an earlier run, and startRound, the round those findings came from. With them the run starts
// with fix<startRound> of those findings, then reviews rounds startRound+1 to MAX_ROUNDS (the last
// round reviews and verifies only). Without them it starts with fix0 of the S2 findings in RUN.
// Resume after review round 1 of run wf_6c65ceeb-200:
// { findingsFile: '/home/user/ruralz/.claude/plans/m1/runs/w3-s2-wp51-r1.json', startRound: 1 }
const A = (typeof args !== 'undefined' && args) || {}
const START = A.findingsFile ? (A.startRound || 1) : 0

const RULES = `
Repository: ${REPO} (Go module github.com/ravindu-rev/ruralz, branch develop). Ruralz is an API gateway; milestone M1 is implemented from a reviewed design: the integration architecture ${ARCH} (section 0 conventions, 1.2 package table and allowed imports, 2 shared contracts, 2.16 resolved conflicts such as R-63, 4.4 work packages), the work packages in ${WPS}, and the area specs ${M1}/specs/01-*.md to 11-*.md ("07 req 80" means spec 07 requirement 80). Design documents under docs/ are binding.
Work package WP-51 is the validation.json-schema Policy type, in ${PKG} (its only owned path). It was built in wave-3 section S2 and is not committed: its files are untracked in the working tree. Its S2 record, with the engineer's summary, deviations, contract requests and the findings still open after two fix rounds, is the WP-51 entry of ${RUN}.
Hard rules:
1. Write ONLY inside ${PKG}, and only when your role says you may write. Every other package is committed and must not change.
2. Never run git commands that change state (no add, commit, stash, checkout, reset, restore, clean). Read-only git is fine.
3. Never edit go.mod, go.sum, the Makefile, CI workflows or docs. Never run whole-tree mutating commands (no go mod tidy, no gofmt -w ., no golangci-lint fmt ./...). Scope commands to ./${PKG}...
4. Probes go in a scratch directory outside the repository and reach the package with go test -overlay. Leave no stray files or binaries in the repository (build with -o into the scratch directory).
5. Conventions: AGENTS.md hard rules and .claude/rules/*.md; license header "// Copyright 2026 Revington" + "// SPDX-License-Identifier: Apache-2.0"; RZ codes only from internal/errcode; no init(); no mutable package-level variables; documented exported identifiers; American spelling; tests cite the requirement numbers they cover.
6. The container has 4 CPUs and another agent may run tests at the same time; use -count=1 and give long runs time.
`
const CHECKS = `go build and go vet ./${PKG}..., go test -race -count=1 -cover -shuffle=on ./${PKG}..., CGO_ENABLED=0 go test -count=1 ./${PKG}..., GOTOOLCHAIN=go1.26.8 GOEXPERIMENT=jsonv2 CGO_ENABLED=0 go test -count=1 ./${PKG}..., each fuzz target in the package for at least 60s, bin/golangci-lint run ./${PKG}..., bin/golangci-lint fmt ./${PKG}..., and go run ./internal/tool/repocheck`

const IMPL_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string' },
    paths: { type: 'array', items: { type: 'string' }, description: 'files changed in this pass' },
    checks: { type: 'string', description: 'exact commands run and their results' },
    contractRequests: { type: 'array', items: { type: 'string' } },
    deviations: { type: 'array', items: { type: 'string' } },
  },
  required: ['status', 'summary', 'paths', 'checks', 'contractRequests', 'deviations'],
}
const FINDING = {
  type: 'object',
  properties: {
    severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
    where: { type: 'string' }, issue: { type: 'string' }, fix: { type: 'string' },
  },
  required: ['severity', 'where', 'issue', 'fix'],
}
const REVIEW_SCHEMA = {
  type: 'object',
  properties: { verdict: { type: 'string', enum: ['pass', 'fix'] }, findings: { type: 'array', items: FINDING }, summary: { type: 'string' } },
  required: ['verdict', 'findings', 'summary'],
}
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
  properties: { confirmed: { type: 'boolean' }, severity: { type: 'string', enum: ['blocker', 'major', 'minor'] }, evidence: { type: 'string' }, note: { type: 'string' } },
  required: ['confirmed', 'severity', 'evidence', 'note'],
}

const LENSES = [
  { key: 'spec', title: 'spec conformance', focus: `Check every requirement in WP-51's scope (its wps.json entry, architecture 4.4, spec 07 groups J and K, reqs 74 to 84, section 9 items 21 to 24, R-63, and docs/architecture/02-configuration-model.md "Body buffering and limits" and the validation.json-schema Policy rule) is implemented and tested: statuses and RZ codes (400 RZ-RT-009, 413 RZ-RT-003, 503 on budget), details, compile refusals as RZ-CFG-005. Check that valid bodies within maxRequestBodyBytes and typical schemas are accepted (R-63's accepted consequence: typical documents fit at the full limit).` },
  { key: 'bounds', title: 'resource bounds and hostile inputs', focus: `Try to break the resource accounting. For hostile schemas (accepted by compile) and hostile bodies, measure peak heap, CPU time and what the working-set reservation charged, through Factory.Build and Handle at the 10 MiB default: unevaluatedProperties and unevaluatedItems over long in-place $ref chains, wide enums, patternProperties, uniqueItems, deep nesting, failing bodies with many errors. A reservation smaller than the memory actually held, unbounded CPU, or a refusal of a valid ordinary body (false 413 or 503) are findings. Report each exact schema, body and measurement.` },
  { key: 'integration', title: 'contracts and integration', focus: `Check the package against the committed contracts it uses (read the real source: internal/filter (Factory, BuildEnv, Filter, Message, DecodedLimitFactor, Shared), internal/jsonval, internal/problem, internal/config/hub (PolicyCheck), internal/config/diag, internal/errcode, internal/phase), its allowed imports in architecture 1.2, and the needs of its later callers WP-70 (snapshot compiler) and WP-71 (built-in Filter tables): Factory and Check shapes, budget reservation and release on every path (including errors and cancellation), sharing compiled schemas across goroutines.` },
  { key: 'code', title: 'tests and code quality', focus: `Review as a senior Go reviewer: correctness, dead code, comments that do not match the code, package docs, allocation and complexity. For the tests: would each fail if its behavior broke (mutate with go test -overlay)? Are timing-sensitive tests robust on a loaded 4-CPU machine? Run ${CHECKS}, scoped to the package. A failing check is a blocker.` },
]

const fixPrompt = (findings, minors, first) => `${RULES}
You are the engineer for WP-51, fixing verified review findings. ${first ? `Read the WP-51 entry of ${RUN} first: its "remaining" holds the two major findings left after two fix rounds, with evidence (1: the working-set reservation leaves out jsonschema/v6's unevaluated-tracking maps; 2: the last round reserves CPU-time units as bytes of maxBufferedBytes, so valid bodies far under maxRequestBodyBytes get 413). Read its deviations and the code before you change anything. Fix both at their root cause: memory reservations must cover the memory validation actually holds (measured, with a bound you can state), and CPU work must be bounded without charging it as buffered bytes, so ordinary valid bodies at the full limit pass. Do not reintroduce the earlier findings its deviations describe.` : 'Each finding below was reproduced by an independent verifier (evidence included).'}
Fix every finding, and every minor finding, at its root cause (AGENTS.md "Quality standard"). For each finding, find every path that shares its root cause (each keyword, input shape, code path or caller of the same kind), fix them together, and name the class members in summary. Give every finding and every class member its own test that fails without the fix, small and edge cases included, and prove it by running the test against the code before your change (go test -overlay) or by a mutation the test kills; report that proof in checks. If you believe a finding is wrong, do not change the code for it and give the evidence in deviations.
Findings:
${JSON.stringify(findings, null, 1).slice(0, 30000)}
Minor findings:
${JSON.stringify(minors, null, 1).slice(0, 8000)}
Then run the scoped checks until green: ${CHECKS}. Return the structured report (paths = files changed in this pass).`

const reviewPrompt = (lens, round, history) => `${RULES}
You are an adversarial reviewer of WP-51 (round ${round}), lens: ${lens.title}. Default to skepticism; find real defects before this lands. Do NOT modify any repository file.
Read the WP-51 entry of ${WPS}, its 4.4 section in ${ARCH}, and the WP-51 entry of ${RUN} (its S2 history). Review every file in ${PKG}.
${lens.focus}
${history ? `Fixed in this run (do not re-report unless the fix is incomplete or broke something, and say so):\n${history.slice(0, 12000)}` : ''}
Severity: blocker = required behavior wrong or missing, a bound or contract broken, or a failing check; major = a likely bug or significant gap; minor = style or small improvement. Every blocker and major must include the exact input or command and what you observed. verdict is "pass" only if there are no blocker or major findings.`

const mergePrompt = (reviews) => `${RULES}
Four reviewers looked at WP-51 through different lenses. Merge their findings: group those with the same root cause or fix into one, keeping the most precise input, location and fix and the highest severity, and list the lenses in sources. Put every blocker and major in findings and every minor in minors. Do not drop or invent findings and do not judge validity. Do not modify files.
Reviews:
${JSON.stringify(reviews, null, 1).slice(0, 60000)}`

const verifyPrompt = (f) => `${RULES}
You independently verify one review finding on WP-51. Reproduce it on the current code with a concrete probe (go test -overlay with a scratch test file outside the repository, or the package's own tests); do not modify repository files. Confirm it only if you reproduce the defect, or if it is a plain requirement gap or failing check you can point to (cite the requirement text). If the code behaves correctly, or the behavior is required by the spec or a documented deviation, set confirmed=false and explain. Give your own severity.
Finding:
${JSON.stringify(f, null, 1)}`

const stopped = (x) => !x || (x.status === 'blocked' && !(x.paths || []).length)

phase('Fix')
const resumePrompt = () => `${RULES}
You are the engineer for WP-51, fixing verified review findings. Read ${A.findingsFile}: "findings" were each reproduced by an independent verifier in review round ${START} (their evidence field shows how); "minors" are minor findings. Fix every finding, and every minor finding, at its root cause (AGENTS.md "Quality standard"). For each finding, find every path that shares its root cause (each keyword, input shape, code path or caller of the same kind), fix them together, and name the class members in summary. Give every finding and every class member its own test that fails without the fix, small and edge cases included, and prove it by running the test against the code before your change (go test -overlay) or by a mutation the test kills; report that proof in checks. If you believe a finding is wrong, do not change the code for it and give the evidence in deviations. "history" (and "fix0" and "round1" where present) describe the earlier rounds; do not regress them, and read the WP-51 entry of ${RUN} for the S2 rounds. When the file has "decisions", they are the user's binding decisions on WP-51's contract (also in ${M1}/user-decisions.md and architecture R-77): implement against them.
Then run the scoped checks until green: ${CHECKS}. Return the structured report (paths = files changed in this pass).`
const fix0 = A.findingsFile
  ? await agent(resumePrompt(), { label: `fix${START}:WP-51`, phase: 'Fix', schema: IMPL_SCHEMA })
  : await agent(fixPrompt([], [], true), { label: 'fix0:WP-51', phase: 'Fix', schema: IMPL_SCHEMA })
if (stopped(fix0)) {
  log(`fix${START} did no work; stopping`)
  return { id: 'WP-51', clean: false, finalVerdict: 'blocked', stoppedAt: `fix${START}`, fix0, rounds: [], remainingConfirmed: [], remainingMinor: [] }
}
let history = A.findingsFile
  ? `The findings of review round ${START} in ${A.findingsFile} were fixed just before this review (read that file, including its "history" and, where present, its "decisions": the user's binding decisions on WP-51's contract, recorded as architecture R-77; judge the code against them, not against the superseded text); the S2 rounds are in the WP-51 entry of ${RUN}.`
  : `Fix round 0 (this run) addressed the two majors in the WP-51 entry of ${RUN} ("remaining"): the working-set reservation missing jsonschema/v6's unevaluated-tracking maps, and CPU-time units reserved as bytes causing false 413s.`
if (fix0.deviations && fix0.deviations.length) history += `\nEngineer deviations in fix round ${START}:\n` + JSON.stringify(fix0.deviations, null, 1).slice(0, 3000)

const rounds = []
let clean = false
let stoppedAt = null
let minors = []
for (let round = START + 1; round <= MAX_ROUNDS; round++) {
  const reviews = (await parallel(LENSES.map(lens => () =>
    agent(reviewPrompt(lens, round, history), { label: `review${round}:${lens.key}`, phase: 'Review', schema: REVIEW_SCHEMA })
      .then(r => r && { lens: lens.key, ...r })))).filter(Boolean)
  log(`round ${round}: ${reviews.map(r => `${r.lens} ${r.verdict} ${r.findings.length}`).join(', ')}`)
  const all = reviews.flatMap(r => r.findings.map(f => ({ ...f, lens: r.lens })))
  if (reviews.length < LENSES.length) log(`round ${round}: only ${reviews.length} of ${LENSES.length} lenses returned`)
  // Clean only when every lens reports nothing at all (AGENTS.md "Quality standard").
  if (!all.length && reviews.length === LENSES.length) {
    minors = []
    rounds.push({ round, reviews, confirmed: [], minors })
    clean = true
    break
  }
  const merged = await agent(mergePrompt(reviews), { label: `merge${round}`, phase: 'Merge', schema: MERGE_SCHEMA })
  const candidates = merged ? merged.findings : all.filter(f => f.severity !== 'minor')
  minors = merged ? merged.minors : all.filter(f => f.severity === 'minor')
  const verdicts = (await parallel(candidates.map((f, i) => () =>
    agent(verifyPrompt(f), { label: `verify${round}:${i + 1}`, phase: 'Verify', schema: VERDICT_SCHEMA })
      .then(v => ({ finding: f, verdict: v }))))).filter(Boolean)
  const confirmed = verdicts.filter(x => x.verdict && x.verdict.confirmed && x.verdict.severity !== 'minor')
  minors = [...minors, ...verdicts.filter(x => x.verdict && x.verdict.confirmed && x.verdict.severity === 'minor').map(x => x.finding)]
  log(`round ${round}: ${candidates.length} serious candidates, ${confirmed.length} confirmed`)
  if (!confirmed.length && !minors.length && reviews.length === LENSES.length) { rounds.push({ round, reviews, verdicts, confirmed: [], minors }); clean = true; break }
  if (round === MAX_ROUNDS) { rounds.push({ round, reviews, verdicts, confirmed, minors }); break }
  const fix = await agent(fixPrompt(confirmed.map(x => ({ ...x.finding, evidence: x.verdict.evidence })), minors, false), { label: `fix${round}:WP-51`, phase: 'Fix', schema: IMPL_SCHEMA })
  rounds.push({ round, reviews, verdicts, confirmed, minors, fix })
  if (stopped(fix)) { log(`fix round ${round} did no work; stopping`); stoppedAt = `fix${round}`; break }
  history += `\nRound ${round} fixed:\n` + JSON.stringify(confirmed.map(x => ({ severity: x.verdict.severity, where: x.finding.where, issue: String(x.finding.issue).slice(0, 600) })), null, 1)
  if (fix.deviations && fix.deviations.length) history += `\nEngineer deviations in round ${round}:\n` + JSON.stringify(fix.deviations, null, 1).slice(0, 2500)
  minors = []
}

const last = rounds[rounds.length - 1]
const fixes = [fix0, ...rounds.map(r => r.fix).filter(Boolean)]
return {
  id: 'WP-51',
  clean,
  finalVerdict: stoppedAt ? 'blocked' : clean ? 'pass' : 'fix',
  stoppedAt,
  paths: [...new Set(fixes.flatMap(f => f.paths || []))],
  checks: fixes[fixes.length - 1].checks,
  contractRequests: fixes.flatMap(f => f.contractRequests || []),
  deviations: fixes.flatMap(f => f.deviations || []),
  fix0: { status: fix0.status, summary: fix0.summary },
  rounds: rounds.map(r => ({
    round: r.round,
    lenses: r.reviews.map(v => ({ lens: v.lens, verdict: v.verdict, counts: ['blocker', 'major', 'minor'].map(s => v.findings.filter(f => f.severity === s).length), summary: v.summary })),
    confirmed: (r.confirmed || []).map(c => ({ ...c.finding, verifiedSeverity: c.verdict.severity, evidence: c.verdict.evidence })),
    refuted: (r.verdicts || []).filter(v => !v.verdict || !v.verdict.confirmed).map(v => ({ where: v.finding.where, issue: String(v.finding.issue).slice(0, 300), why: v.verdict ? v.verdict.note : 'verifier failed' })),
    fix: r.fix ? { status: r.fix.status, summary: r.fix.summary } : null,
  })),
  remainingConfirmed: clean ? [] : ((last && last.confirmed) || []).map(c => ({ ...c.finding, evidence: c.verdict.evidence })),
  remainingMinor: minors,
}
