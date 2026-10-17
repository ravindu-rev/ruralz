export const meta = {
  name: 'm1-wp51-close',
  description: 'Close WP-51: fix every library-audit gap class in dependency order with a proven test per member, check each group adversarially, then full review rounds until one reports zero findings',
  phases: [
    { title: 'Class fix', detail: 'one engineer per group of gap classes, in the audit order' },
    { title: 'Class check', detail: 'every member row fixed, each with a test that fails without the fix' },
    { title: 'Review', detail: 'four lenses plus five audit-area re-checks' },
    { title: 'Merge', detail: 'deduplicate each round' },
    { title: 'Verify', detail: 'independent reproduction of each blocker or major finding' },
    { title: 'Fix', detail: 'fix every confirmed finding and every minor' },
  ],
}
// Args (all optional): maxRounds (full review rounds, default 6; a round that reports
// findings is fixed only when another round follows), maxAttempts (fix attempts per group,
// default 3), startGroup (group id to start at, for a resume after earlier groups landed),
// scratchpad (the session's scratchpad directory; probes go in <scratchpad>/wp51close).
// Continuing a stopped run in another session (resume works only in the session that ran it):
// carryFile (a JSON file built from the stopped run's journal, as runs/w3-s2-wp51-carry.json:
// history of the finished groups, carried open items, and the startGroup's open items),
// startAttempt (the startGroup attempt to run first; its check items come from the file) and
// carriedCount (how many open items the file's "carried" field holds).
// Input: runs/w3-s2-close-input.json (groups, decisions, sources). The lead commits.

const REPO = 'C:/Users/ravin/Projects/Ruralz'
const M1 = `${REPO}/.claude/plans/m1`
const ARCH = `${M1}/specs/00-architecture.md`
const WPS = `${M1}/arch/wps.json`
const PKG = 'internal/filter/validation/jsonschema/'
const LIB = 'C:/Users/ravin/go/pkg/mod/github.com/santhosh-tekuri/jsonschema/v6@v6.0.3'
const AUDIT = `${M1}/wp51-library-audit.md`
const AUDIT_RESULT = `${M1}/runs/w3-s2-wp51-audit-result.json`
const R9 = `${M1}/runs/w3-s2-wp51-r9.json`
const INPUT = `${M1}/runs/w3-s2-close-input.json`
const A = (typeof args !== 'undefined' && args) || {}
const SP = `${A.scratchpad || '/tmp/claude-0/-home-user-ruralz/758d7ca3-81cf-5fbc-a967-12d97bce3d6b/scratchpad'}/wp51close`
const MAX_ROUNDS = A.maxRounds || 6
const MAX_ATTEMPTS = A.maxAttempts || 3
const CARRY = A.carryFile || null
const START_ATTEMPT = CARRY && A.startAttempt > 1 ? A.startAttempt : 1
const CARRIED_IN_FILE = CARRY ? A.carriedCount || 0 : 0

// Groups follow the audit's section 4.2 order: GC-21, GC-01 and GC-02 change which positions
// the walk sees; GC-08 makes memory tests deterministic before later classes add them;
// GC-03 and GC-09 are one budget; GC-11 feeds GC-15's stack term.
const GROUPS = [
  { id: 'G01', classes: ['GC-21'], note: 'Format assertion on library-only routes and the request-time use of the compile-time regexp cache. CC-01 is a process crash: close it first.' },
  { id: 'G02', classes: ['GC-01'], note: '$recursiveRef and $recursiveAnchor as first-class references, with the class backstop shared with GC-02 (no compiled position the walk did not walk live).' },
  { id: 'G03', classes: ['GC-02'], note: 'The walk mirrors exactly which positions the library compiles, with which base and in which order (decision 24: no new refusal).' },
  { id: 'G04', classes: ['GC-08'], note: 'Deterministic memory measurements replace every sampled peak, before any later class adds memory tests.' },
  { id: 'G05', classes: ['GC-03', 'GC-09'], note: 'One walk-time compile budget of 1 MiB with byte terms (GC-03) and count terms (GC-09) (decision 20).' },
  { id: 'G06', classes: ['GC-04', 'GC-05'], note: 'Non-canonical pointer indexes accepted as aliases, with repeat counts and seen sets keyed by the decoded value (decision 21); the eight integer keywords above 2^63-1 clamped in a compiled copy, presence kept apart from value (decision 22).' },
  { id: 'G07', classes: ['GC-06', 'GC-07'], note: 'Regex parser work the lexical pass does not count; anchor-name bytes and the decision wrapper frames charged (decision 24).' },
  { id: 'G08', classes: ['GC-11', 'GC-15'], note: 'Regexp matcher costs from the compiled program with add() depth charged, and an absolute 128 MiB stack ceiling with 413 above it (decision 23).' },
  { id: 'G09', classes: ['GC-12', 'GC-13', 'GC-14'], note: 'Kept memory restated from what the compiled Schema really keeps; the number model at the library actual parse sites; values outside the decoder data model through the exported API.' },
  { id: 'G10', classes: ['GC-16', 'GC-17'], note: 'Pin every walk rule and resolution mirror per keyword, form and route, and prove every work-model term with a test a mutation fails.' },
  { id: 'G11', classes: ['GC-10', 'GC-18', 'GC-19', 'GC-20'], note: 'Bound the raw config.schema decode before the size check (GC-10) and decode it with the configuration own depth limit (decision 25); keyword locations per error kind; deterministic compile diagnostics; document and pin the library deviations (decision 24). Also round-9 minors 2 and 4 in the round-9 record, which no class covers: close them as that record describes.' },
]
const startAt = A.startGroup ? GROUPS.findIndex(g => g.id === A.startGroup) : 0
const groups = GROUPS.slice(startAt < 0 ? 0 : startAt)

const RULES = `
Repository: ${REPO} (Go module github.com/ravindu-rev/ruralz, branch develop). Ruralz is an API gateway; milestone M1 is implemented from a reviewed design: the integration architecture ${ARCH} (section 0 conventions, 1.2 package table and allowed imports, 2 shared contracts, 2.16 resolved conflicts such as R-63 and R-77, 4.4 work packages), the work packages in ${WPS}, and the area specs ${M1}/specs/01-*.md to 11-*.md ("07 req 80" means spec 07 requirement 80). Design documents under docs/ are binding.
Work package WP-51 is the validation.json-schema Policy type in ${PKG} (its only owned path). It wraps github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 (source: ${LIB}) and bounds it from outside. The committed package on develop (e1e9657, the post-fix8 state) is the starting point. A complete library audit, ${AUDIT} (run result ${AUDIT_RESULT}), lists 321 rows, and every partial and gap row belongs to one of 21 gap classes GC-01 to GC-21 in its section 4; section 4.2 gives the order and dependencies, section 5 maps round 9's open findings (${R9}) to classes. This run closes every class, round 9's four findings and eight minors, and then reviews until a fresh review reports zero findings of any severity.
The user's decisions bind: ${M1}/user-decisions.md items 12 to 17 and 20 to 26 (the 2026-10-16 decisions on the audit: a 1 MiB walk-time compile budget; non-canonical pointer indexes accepted as aliases; integer keywords above 2^63-1 clamped; a 128 MiB absolute stack ceiling with 413; the main fixes, no new refusals, for GC-02, GC-07, GC-11 and GC-20; config.schema decoded with the configuration's own depth limit), recorded in R-77 and spec 07 reqs 75 and 80. Follow the AGENTS.md "Quality standard": fix the whole class, a test for every case that fails without the fix, deterministic tests, inventory before bounding.
Hard rules:
1. Write ONLY inside ${PKG}, and only when your role says you may write. Every other package is committed and must not change. Another workflow is fixing internal/config/{schemaview,overlay,subst,defaults,convert,precedence,canonical} and test/fixtures/json-schema-test-suite right now: never touch those paths, and ignore their transient build or test failures.
2. Never run git commands that change state (no add, commit, stash, checkout, reset, restore, clean). Read-only git is fine.
3. Never edit go.mod, go.sum, the Makefile, CI workflows or docs. Never run whole-tree mutating commands (no go mod tidy, no gofmt -w ., no golangci-lint fmt ./...). Scope commands to ./${PKG}...
4. Probes go under ${SP}/<your-label>/ and reach the package with go test -overlay. Leave no stray files or binaries in the repository (build with -o into the scratch directory).
5. Conventions: AGENTS.md hard rules and .claude/rules/*.md; license header "// Copyright 2026 Revington" + "// SPDX-License-Identifier: Apache-2.0"; RZ codes only from internal/errcode (no new code); no init(); no mutable package-level variables; documented exported identifiers; American spelling; tests cite the requirement, row or class they cover.
6. The machine has 8 logical CPUs and other agents run tests at the same time; use -count=1, give long runs time, and never assert wall-clock time or a sampled heap (use counters, exact live-heap probes or benchmarks).
7. A finding whose fix lies outside your path (spec or architecture text, a design document, another package's contract) is handled by filing it in contractRequests with the exact change; the lead applies contract requests after the run. Reviewers report such a finding only when it is not filed in an engineer's contractRequests, or is filed wrongly. The working tree may hold partial edits from a stopped attempt: read git diff for ${PKG} before you change anything.
8. The checkout is on Windows 11 at ${REPO}. The Bash tool is Git Bash: use forward-slash paths (C:/Users/... or /c/Users/...) and env prefixes as usual (CGO_ENABLED=0 go test ...); in PowerShell set $env:CGO_ENABLED = "0" before go test. go test -race needs cgo, and gcc is on PATH. python3 is a Microsoft Store stub here: use python or node. Run every bin/golangci-lint command with GOTOOLCHAIN=go1.27.1 (the go.mod toolchain), for example GOTOOLCHAIN=go1.27.1 bin/golangci-lint run ./${PKG}...: under the local go1.27.2 it fails with "export data version 5 is greater than maximum supported version 4" typechecking errors yet still prints "0 issues." with exit code 7, so a lint run counts as green only with exit code 0. The run files, reports and findings were written on another machine: read /home/user/ruralz as ${REPO} and /root/go/pkg/mod as C:/Users/ravin/go/pkg/mod; evidence under /tmp/claude-0 is gone, so reproduce each item from the input it states.
9. Only the lead commits, after the run, signed and without a co-author. Never read, copy or use a signing key, and never push.
`
const CHECKS = `go build and go vet ./${PKG}..., go test -race -count=1 -cover -shuffle=on ./${PKG}..., CGO_ENABLED=0 go test -count=1 ./${PKG}..., GOTOOLCHAIN=go1.26.8 GOEXPERIMENT=jsonv2 CGO_ENABLED=0 go test -count=1 ./${PKG}..., each fuzz target in the package for at least 60s, GOTOOLCHAIN=go1.27.1 bin/golangci-lint run ./${PKG}..., GOTOOLCHAIN=go1.27.1 bin/golangci-lint fmt ./${PKG}..., and go run ./internal/tool/repocheck`

const FINDING = {
  type: 'object',
  properties: {
    severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
    where: { type: 'string' }, issue: { type: 'string', description: 'the defect, with the exact input or command and what was observed' }, fix: { type: 'string' },
  },
  required: ['severity', 'where', 'issue', 'fix'],
}
const FIX_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string', description: 'what changed, and the class members fixed together' },
    members: { type: 'array', items: { type: 'object', properties: {
      row: { type: 'string', description: 'audit row id, round-9 item, or finding' },
      status: { type: 'string', enum: ['fixed', 'wrong', 'open'] },
      test: { type: 'string', description: 'the test (name and table row) that covers it' },
      proof: { type: 'string', description: 'how the test was shown to fail without the fix (overlay run or killed mutation)' },
    }, required: ['row', 'status', 'test', 'proof'] } },
    paths: { type: 'array', items: { type: 'string' }, description: 'files changed in this pass' },
    checks: { type: 'string', description: 'exact commands run and their results' },
    contractRequests: { type: 'array', items: { type: 'string' } },
    deviations: { type: 'array', items: { type: 'string' }, description: 'findings or rows judged wrong, with evidence' },
  },
  required: ['status', 'summary', 'members', 'paths', 'checks', 'contractRequests', 'deviations'],
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

const stopped = (x) => !x || (x.status === 'blocked' && !(x.paths || []).length)
const brief = (fs) => JSON.stringify((fs || []).map(f => ({ severity: f.severity, where: f.where, issue: String(f.issue).slice(0, 500) })), null, 1)

const classFixPrompt = (g, attempt, open, history) => `${RULES}
You are the engineer for WP-51, group ${g.id}: gap classes ${g.classes.join(', ')}. ${g.note}
Read in ${AUDIT}: sections 4.1 and 4.2; the section of each of your classes in section 4 (root cause, members, fix for the class, tests, contract); every member row in the matrix of section 3 with its "Library paths and evidence" entry; and section 5 for the round-9 items your classes carry (the round-9 record is ${R9}). Read the library code each row cites. Read the decisions named above; they override the audit where they differ.
${history ? `Earlier groups of this run already changed the package (do not undo their fixes):\n${history.slice(0, 12000)}` : ''}
${attempt > 1 && open === null ? `This is attempt ${attempt}, continued from a run that stopped while attempt ${attempt} ran, so the working tree may hold its partial edits. Attempts 1 to ${attempt - 1} are recorded in the "groups" entry ${g.id} of ${CARRY}, and the independent check of attempt ${attempt - 1} found the open items in its "open" field; close every one. If the file has an "attempt${attempt}Progress" field, read it first: it records what the stopped attempt already changed and checked, so finish and verify that work rather than redo it. Evidence paths under the old session's /tmp/claude-0 scratchpad are gone: reproduce each item from the input it states.` : attempt > 1 ? `This is attempt ${attempt}. The independent check of your group found these open items; close every one:\n${JSON.stringify(open, null, 1).slice(0, 20000)}` : ''}
Implement the class fix: fix every member row, and every other path you find that shares the root cause (list them in summary). Give every member row and every small or edge case its own test that fails without the fix; prove each by running it against the code without the fix (go test -overlay with pre-fix copies of the changed files in your scratch directory) or by a mutation the test kills, and record the proof per member. Update doc.go and every comment the change makes stale. If a member row is wrong (already bounded, or required by the spec), change no code for it and give the evidence in deviations, marking it "wrong" in members.
Run the scoped checks until green: ${CHECKS}. Return the structured report.`

const classCheckPrompt = (g, fix) => `${RULES}
You independently check group ${g.id} of the WP-51 fix: gap classes ${g.classes.join(', ')} of ${AUDIT}. Default to skepticism. Do NOT modify any repository file.
The engineer reported:
${JSON.stringify(fix, null, 1).slice(0, 14000)}
For every member row of these classes (audit section 4 lists them; section 3 has each row's evidence) and every round-9 item they carry:
1. Confirm the behavior is now bounded or correct: read the code, and for every P, C1, C2, C3, V1 and V2 row run its hostile input from the audit evidence through CompileSchema, Check, Factory.Build or Handle and measure.
2. Confirm its named test exists, is deterministic, and fails without the fix: revert the fix in a scratch copy and run the test through go test -overlay, or kill a mutation.
3. Confirm no earlier fix regressed: run the full package tests (${CHECKS}).
4. Look for paths with the same root cause that neither the audit nor the engineer listed.
Judge every "wrong" claim in deviations on its evidence. Report each open item as a finding with the exact input or command and what you observed. verdict is "pass" only if there are no findings of any severity.`

const LENSES = [
  { key: 'spec', title: 'spec conformance', focus: `Check every requirement in WP-51's scope (its wps.json entry, architecture 4.4 and R-63 and R-77, spec 07 reqs 74 to 84 and 90 and section 9 items 21 to 24, docs/architecture/02-configuration-model.md "Body buffering and limits" and the validation.json-schema rule, and the decisions) is implemented and tested: statuses and RZ codes (400 RZ-RT-009, 413 RZ-RT-003, 503 on budget), details, compile refusals as RZ-CFG-005, and that ordinary valid schemas and bodies are accepted.` },
  { key: 'bounds', title: 'resource bounds and hostile inputs', focus: `Try to break the resource accounting with hostile schemas and bodies at the 10 MiB default and at small and large configured limits: measure peak live heap, CPU work and the charges through Factory.Build and Handle. A reservation smaller than the memory actually held, unbounded or quadratic CPU, a panic or a refusal of a valid ordinary body are findings. Report each exact schema, body and measurement.` },
  { key: 'integration', title: 'contracts and integration', focus: `Check the package against the committed contracts it uses (internal/filter Factory, BuildEnv, Filter, Message and Shared; internal/jsonval; internal/problem; internal/config/hub; internal/config/diag; internal/errcode; internal/phase), its allowed imports in architecture 1.2, and the needs of its later callers WP-70 and WP-71: Factory and Check shapes, budget reservation and release on every path including errors and cancellation, sharing compiled schemas across goroutines.` },
  { key: 'code', title: 'tests and code quality', focus: `Review as a senior Go reviewer: correctness, dead code, comments that do not match the code, package docs, allocation and complexity. For the tests: would each fail if its behavior broke (mutate with go test -overlay)? Is every test deterministic on a loaded 4-CPU machine? Run ${CHECKS}. A failing check is a blocker.` },
  { key: 'audit-compile', title: 'audit re-check, compile rows', focus: `Re-check every row of section 3.1 (CMP) of ${AUDIT}: each must now be bounded (a rule or charge, and a named test that fails without it) or not applicable. Run the hostile input of every P, C1, C2 and C3 row. Report each row that is still partial or gap, or whose test does not fail without the bound.` },
  { key: 'audit-validate', title: 'audit re-check, validate rows', focus: `Re-check every row of section 3.2 (VAL) of ${AUDIT}: each must now be bounded with a named test that fails without the bound, or not applicable. Run the hostile input of every P, V1 and V2 row. Report each row that is still partial or gap.` },
  { key: 'audit-refs', title: 'audit re-check, reference rows', focus: `Re-check every row of section 3.3 (REFS) of ${AUDIT}: each must now be bounded with a named test that fails without the bound, or not applicable, for $ref, $dynamicRef and $recursiveRef alike. Report each row that is still partial or gap.` },
  { key: 'audit-values', title: 'audit re-check, values rows', focus: `Re-check every row of section 3.4 (VLU) of ${AUDIT}: each must now be bounded with a named test that fails without the bound, or not applicable. Report each row that is still partial or gap.` },
  { key: 'audit-cross', title: 'audit re-check, regexp, interaction and completeness rows', focus: `Re-check every row of sections 3.5 (CR, CX) and 3.6 (CC) of ${AUDIT}: each must now be bounded with a named test that fails without the bound, or not applicable. Run the hostile input of every regexp and interaction row. Report each row that is still partial or gap.` },
]

const reviewPrompt = (lens, round, history) => `${RULES}
You are an adversarial reviewer of WP-51 (round ${round}), lens: ${lens.title}. Default to skepticism; find real defects before this lands. Do NOT modify any repository file. Review every file in ${PKG}.
${lens.focus}
${history ? `Fixed in this run (do not re-report unless the fix is incomplete or broke something, and say so):\n${history.slice(0, 16000)}` : ''}
Severity: blocker = required behavior wrong or missing, a bound or contract broken, a panic, or a failing check; major = a likely bug or significant gap; minor = style, a comment, or a small improvement. Every finding must include the exact input, file:line or command and what you observed. verdict is "pass" only if there are no findings of any severity.`

const mergePrompt = (reviews) => `${RULES}
Nine reviewers looked at WP-51 through different lenses. Merge their findings: group those with the same root cause or fix into one, keeping the most precise input, location and fix and the highest severity, and list the lenses in sources. Put every blocker and major in findings and every minor in minors. Do not drop or invent findings and do not judge validity. Do not modify files.
Reviews:
${JSON.stringify(reviews, null, 1)}`

const verifyPrompt = (f) => `${RULES}
You independently verify one review finding on WP-51. Reproduce it on the current code with a concrete probe (go test -overlay with a scratch test file, or the package's own tests); do not modify repository files. Confirm it only if you reproduce the defect, or if it is a plain requirement gap or failing check you can point to (cite the requirement text). If the code behaves correctly, or the behavior is required by the spec or a decision, set confirmed=false and explain. Give your own severity.
Finding:
${JSON.stringify(f, null, 1)}`

const fixPrompt = (round, findings, minors, history) => `${RULES}
You are the engineer for WP-51, fixing review round ${round}'s findings. Each blocker and major was reproduced by an independent verifier (evidence included); the minors were not, so check each and fix it unless you show with evidence that it is wrong.
Fix every finding at its root cause: find every path that shares it (each keyword, input shape, code path or caller of the same kind), fix them together, and name the class members in summary. Give every finding and every class member its own test that fails without the fix, small and edge cases included, and prove it (go test -overlay against the code before your change, or a killed mutation); record the proof per member.
${history ? `Already fixed in this run (do not undo):\n${history.slice(0, 12000)}` : ''}
Findings:
${JSON.stringify(findings, null, 1)}
Minor findings:
${JSON.stringify(minors, null, 1)}
Run the scoped checks until green: ${CHECKS}. Return the structured report.`

// Phase 1: one group of classes at a time, each fixed and then checked independently;
// a group is retried with the check's open items until the check reports none.
let history = CARRY ? `\nGroups before ${groups[0].id} were fixed in an earlier run: read the "history" field of ${CARRY} (their fixes and deviations) and do not undo them.` : ''
const groupRecords = []
let carried = []
let stoppedAt = null
for (const g of groups) {
  const continued = g === groups[0] && START_ATTEMPT > 1
  let open = continued ? null : []
  let attempt = continued ? START_ATTEMPT - 1 : 0
  let check = null
  const record = { id: g.id, classes: g.classes, attempts: [] }
  while (attempt < MAX_ATTEMPTS) {
    attempt++
    const fix = await agent(classFixPrompt(g, attempt, open, history), { label: `fix:${g.id}:${attempt}`, phase: 'Class fix', schema: FIX_SCHEMA })
    if (stopped(fix)) { stoppedAt = `fix:${g.id}:${attempt}`; record.attempts.push({ fix, check: null }); break }
    check = await agent(classCheckPrompt(g, fix), { label: `check:${g.id}:${attempt}`, phase: 'Class check', schema: REVIEW_SCHEMA })
    record.attempts.push({ fix, check })
    open = check ? check.findings : [{ severity: 'major', where: g.id, issue: 'the class check returned no result', fix: 'check again' }]
    log(`${g.id} attempt ${attempt}: ${(fix.members || []).length} members reported, check ${check ? check.verdict : 'none'} with ${open.length} open`)
    if (check && check.findings.length === 0) break
  }
  groupRecords.push(record)
  if (stoppedAt) { log(`${g.id}: the engineer did no work; stopping`); break }
  const lastFix = record.attempts[record.attempts.length - 1].fix
  history += `\n${g.id} (${g.classes.join(', ')}): ${String(lastFix.summary).slice(0, 1500)}`
  if (lastFix.deviations && lastFix.deviations.length) history += `\n${g.id} deviations: ${JSON.stringify(lastFix.deviations).slice(0, 1500)}`
  if (open.length) {
    log(`${g.id}: ${open.length} items still open after ${attempt} attempts; carried into the review rounds`)
    carried = carried.concat(open.map(f => ({ ...f, where: `${g.id}: ${f.where}` })))
  }
}

// Phase 2: full review rounds until every lens reports zero findings.
const rounds = []
let clean = false
if (!stoppedAt) {
  let pendingSerious = carried.filter(f => f.severity !== 'minor')
  let pendingMinor = carried.filter(f => f.severity === 'minor')
  if (carried.length || CARRIED_IN_FILE) {
    const fromFile = CARRIED_IN_FILE ? `\nAlso fix the ${CARRIED_IN_FILE} items that groups of the earlier run left open, listed in the "carried" field of ${CARRY}; check each minor and fix it unless you show with evidence that it is wrong.` : ''
    const fix = await agent(fixPrompt(0, pendingSerious, pendingMinor, history) + fromFile, { label: 'fix:carried', phase: 'Fix', schema: FIX_SCHEMA })
    rounds.push({ round: 0, carried, fix })
    if (stopped(fix)) stoppedAt = 'fix:carried'
    else history += `\nCarried items fixed: ${brief(carried)}${CARRIED_IN_FILE ? `, and the "carried" items of ${CARRY}` : ''}`
  }
  for (let round = 1; !stoppedAt && round <= MAX_ROUNDS; round++) {
    const reviews = (await parallel(LENSES.map(lens => () =>
      agent(reviewPrompt(lens, round, history), { label: `review${round}:${lens.key}`, phase: 'Review', schema: REVIEW_SCHEMA })
        .then(r => r && { lens: lens.key, ...r })))).filter(Boolean)
    const all = reviews.flatMap(r => r.findings.map(f => ({ ...f, lens: r.lens })))
    log(`round ${round}: ${reviews.map(r => `${r.lens} ${r.findings.length}`).join(', ')}`)
    if (reviews.length < LENSES.length) log(`round ${round}: only ${reviews.length} of ${LENSES.length} lenses returned`)
    if (!all.length && reviews.length === LENSES.length) { rounds.push({ round, reviews, confirmed: [], minors: [] }); clean = true; break }
    const merged = await agent(mergePrompt(reviews), { label: `merge${round}`, phase: 'Merge', schema: MERGE_SCHEMA })
    const candidates = merged ? merged.findings : all.filter(f => f.severity !== 'minor')
    const minors = merged ? merged.minors : all.filter(f => f.severity === 'minor')
    const verdicts = (await parallel(candidates.map((f, i) => () =>
      agent(verifyPrompt(f), { label: `verify${round}:${i + 1}`, phase: 'Verify', schema: VERDICT_SCHEMA })
        .then(v => ({ finding: f, verdict: v }))))).filter(Boolean)
    const confirmed = verdicts.filter(x => x.verdict && x.verdict.confirmed)
    const serious = confirmed.filter(x => x.verdict.severity !== 'minor').map(x => ({ ...x.finding, evidence: x.verdict.evidence }))
    const allMinors = [...minors, ...confirmed.filter(x => x.verdict.severity === 'minor').map(x => x.finding)]
    log(`round ${round}: ${candidates.length} serious candidates, ${serious.length} confirmed, ${allMinors.length} minor`)
    if (!serious.length && !allMinors.length && reviews.length === LENSES.length) { rounds.push({ round, reviews, verdicts, confirmed: [], minors: [] }); clean = true; break }
    if (round === MAX_ROUNDS) { rounds.push({ round, reviews, verdicts, confirmed: serious, minors: allMinors }); break }
    const fix = await agent(fixPrompt(round, serious, allMinors, history), { label: `fix${round}`, phase: 'Fix', schema: FIX_SCHEMA })
    rounds.push({ round, reviews, verdicts, confirmed: serious, minors: allMinors, fix })
    if (stopped(fix)) { stoppedAt = `fix${round}`; break }
    history += `\nRound ${round} fixed: ${brief([...serious, ...allMinors])}`
    if (fix.deviations && fix.deviations.length) history += `\nRound ${round} deviations: ${JSON.stringify(fix.deviations).slice(0, 2000)}`
  }
}

const last = rounds[rounds.length - 1]
const fixes = [...groupRecords.flatMap(g => g.attempts.map(a => a.fix)), ...rounds.map(r => r.fix)].filter(Boolean)
return {
  id: 'WP-51',
  clean,
  finalVerdict: stoppedAt ? 'blocked' : clean ? 'pass' : 'fix',
  stoppedAt,
  // Continued runs: the earlier groups, their contract requests and paths are in this file.
  carryFile: CARRY,
  groups: groupRecords.map(g => ({
    id: g.id, classes: g.classes,
    attempts: g.attempts.map(a => ({ fix: a.fix && { status: a.fix.status, summary: a.fix.summary, members: a.fix.members, paths: a.fix.paths, deviations: a.fix.deviations, contractRequests: a.fix.contractRequests, checks: a.fix.checks }, check: a.check })),
  })),
  carried,
  rounds: rounds.map(r => ({
    round: r.round,
    lenses: (r.reviews || []).map(v => ({ lens: v.lens, verdict: v.verdict, counts: ['blocker', 'major', 'minor'].map(s => v.findings.filter(f => f.severity === s).length), summary: v.summary })),
    confirmed: r.confirmed || [], minors: r.minors || [],
    refuted: (r.verdicts || []).filter(v => !v.verdict || !v.verdict.confirmed).map(v => ({ where: v.finding.where, issue: String(v.finding.issue).slice(0, 400), why: v.verdict ? v.verdict.note : 'verifier failed' })),
    fix: r.fix && { status: r.fix.status, summary: r.fix.summary, members: r.fix.members, paths: r.fix.paths, deviations: r.fix.deviations, contractRequests: r.fix.contractRequests, checks: r.fix.checks },
  })),
  paths: [...new Set(fixes.flatMap(f => f.paths || []))],
  contractRequests: fixes.flatMap(f => f.contractRequests || []),
  remaining: clean ? [] : [...((last && last.confirmed) || []), ...((last && last.minors) || [])],
}
