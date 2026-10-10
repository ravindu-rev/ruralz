export const meta = {
  name: 'wp33-harden',
  description: 'Finish the interrupted WP-33 fix, then a multi-lens adversarial review, verification and fix loop until no confirmed blocker or major finding remains',
  phases: [
    { title: 'Finish', detail: 'complete the interrupted fix pass and run the checks' },
    { title: 'Review', detail: 'four lenses: depth and cost, differential, spec and valid YAML, tests and code' },
    { title: 'Merge', detail: 'deduplicate blocker and major findings' },
    { title: 'Verify', detail: 'independent reproduction of each finding' },
    { title: 'Fix', detail: 'fix confirmed findings' },
  ],
}

const REPO = '/home/user/ruralz'
const M1 = '/home/user/ruralz/.claude/plans/m1'
const ARCH = `${M1}/specs/00-architecture.md`
const WPS = `${M1}/arch/wps.json`
// args (all optional): findingsFile (a JSON file), or findings and minors, start the loop with a fix of already verified
// findings instead of the Finish step; history lists findings fixed in earlier runs;
// maxRounds caps the review rounds (default 3). A round that confirms findings is fixed
// only when another round follows, so the last round reviews and verifies only.
const A = args || {}
const MAX_ROUNDS = A.maxRounds || 3

const RULES = `
Repository: ${REPO} (Go module github.com/ravindu-rev/ruralz, branch develop). Ruralz is an API gateway; milestone M1 is being implemented from a reviewed design:
- Integration architecture (binding): ${ARCH} (section 0 conventions, 1.2 package table and allowed imports, 2 shared contracts, 2.16 resolved conflicts, 4.4 work packages).
- Work packages: ${WPS} (find WP-33 by id). Area specs: ${M1}/specs/01-config-load.md ... 11-ops-quality.md; "01 req 9" means spec 01 requirement 9. Design documents under docs/ are binding.
Work package WP-33 builds the restricted YAML and JSON profile: owned paths internal/config/profile/ and test/fixtures/yaml-test-suite/. It parses with goccy/go-yaml v1.19.2, guarded by a token pass (tokens.go) that rejects forbidden constructs and bounds depth and token count before goccy parses, and a split parse (split.go) for block mappings of 256 or more entries because goccy is quadratic on wide mappings.
History: the engineer's original report is ${M1}/reports/WP-33.json; the earlier review rounds are in ${M1}/runs/w3-s1a-result.json and ${M1}/runs/w3-s1a-fix-result.json. Three reviews in a row found a new input shape that goccy nests deeper than the column-based token pass counts (tagged empty nodes, then explicit '?' entries), which let accepted trees exceed Options.MaxDepth (01 req 9; 11 req 17, 26), and shapes that parse differently on the narrow and the split parse.
Hard rules:
1. Write ONLY inside internal/config/profile/ and test/fixtures/yaml-test-suite/ (and only when your role says you may write). Committed packages elsewhere must not change.
2. Never run git commands that change state (no add, commit, stash, checkout, reset, restore, clean). Read-only git is fine.
3. Never edit go.mod, go.sum, the Makefile, CI workflows or any other package. Never run whole-tree mutating commands (no go mod tidy, no gofmt -w ., no golangci-lint fmt ./...).
4. Probes and experiments that are not part of the package go in a scratch directory outside the repository and reach the package with go test -overlay (or a temporary _test.go file you delete before you finish, only if your role allows writing). Leave no stray files or binaries in the repository (build with -o into the scratch directory).
5. Conventions: AGENTS.md hard rules and .claude/rules/*.md; license header "// Copyright 2026 Revington" + "// SPDX-License-Identifier: Apache-2.0"; RZ codes only from internal/errcode; no init(); no mutable package-level variables; documented exported identifiers; American spelling; tests cite the requirement numbers they cover; 90%+ statement coverage for this package.
6. The container has 4 CPUs and another agent may be running tests at the same time; use -count=1 and give long test runs time.
`

const IMPL_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string', description: 'what changed in this pass, 3-8 sentences' },
    paths: { type: 'array', items: { type: 'string' } },
    checks: { type: 'string', description: 'exact commands run and their results, with coverage' },
    contractRequests: { type: 'array', items: { type: 'string' } },
    deviations: { type: 'array', items: { type: 'string' }, description: 'findings rejected or handled differently, with reasons' },
  },
  required: ['status', 'summary', 'paths', 'checks', 'contractRequests', 'deviations'],
}
const FINDING = {
  type: 'object',
  properties: {
    severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
    where: { type: 'string', description: 'file:line and requirement ids' },
    issue: { type: 'string', description: 'the defect, with the exact input that shows it and the observed versus expected result' },
    fix: { type: 'string' },
  },
  required: ['severity', 'where', 'issue', 'fix'],
}
const REVIEW_SCHEMA = {
  type: 'object',
  properties: {
    verdict: { type: 'string', enum: ['pass', 'fix'] },
    findings: { type: 'array', items: FINDING },
    summary: { type: 'string' },
  },
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
  properties: {
    confirmed: { type: 'boolean', description: 'true only if you reproduced the defect on the current code, or it is a plain requirement or check failure' },
    severity: { type: 'string', enum: ['blocker', 'major', 'minor'], description: 'your own severity judgment if confirmed' },
    evidence: { type: 'string', description: 'the exact probe or command you ran and its output' },
    note: { type: 'string' },
  },
  required: ['confirmed', 'severity', 'evidence', 'note'],
}

const LENSES = [
  { key: 'depth', title: 'depth and cost bounds', focus: `Try to break the resource bounds. Construct inputs that (a) are accepted with a tree deeper than Options.MaxDepth (default 64), on the narrow parse or the split parse; (b) make Parse exceed the hostile-input bounds (01 req 6 to 9, 01 test plan FuzzProfileYAML: heap growth at most 256 MiB for 1 MiB inputs; 11 req 17, 26) in heap or time; (c) slip past MaxDocumentTokens or MaxBytes. Think about every way goccy's nesting can differ from the token pass: tags, anchors, aliases, explicit keys, multi-line implicit keys, flow collections spanning lines, block scalars, comments, document markers, compact sequences, mixed indentation, and the post-parse depth guard that was just added (does it run on every path, before goccy's quadratic work or after it, and is the cost before it bounded?). Measure, do not guess: run probes with go test -overlay and report bytes, depth, heap and time.` },
  { key: 'diff', title: 'narrow versus split differential', focus: `The result of Parse must not depend on a block mapping's width: the same content must give the same tree or the same findings whether the mapping takes the narrow parse (fewer than 256 entries) or the split parse (use the package's internal split threshold option or pad with sibling keys). Build a differential probe (random and hand-written shapes: tags, explicit keys, empty values, compact sequences, comments, block scalars with chomping, flow collections, anchors, document markers, nested mappings at several depths) and compare: narrow parse, split parse at thresholds 0, 1 and default, and the YAML 1.2 meaning. Also compare JSON and YAML twins. Report every divergence with its exact input.` },
  { key: 'spec', title: 'spec conformance and valid YAML', focus: `Check every spec requirement in WP-33's scope (its wps.json entry, architecture 4.4 and spec 01 requirements it cites, and the configuration model's Restricted YAML profile section in docs/architecture/02-configuration-model.md) is implemented and tested, with the right RZ codes and positions. Hunt for false rejections: valid YAML 1.2 inside the profile that is now refused because of the recent token-pass tightening (tagged empty values, explicit '?' keys, multi-line keys, column corrections), and check each refusal is either required by the profile or recorded as a documented deviation. Run the YAML Test Suite runner (go test -tags integration) and check the expected-failures ratchet and each reason. Check the JSON front end and Encode/EncodeJSON round trips.` },
  { key: 'code', title: 'tests and code quality', focus: `Review the code and the tests as a senior Go reviewer: correctness, error handling, context cancellation, goroutine ownership, allocation and complexity of the token pass and the split parse, clarity, dead code, comments that no longer match the code, package docs. For the tests: would each test fail if its behavior broke (mutate mentally or with an overlay)? Are hostile-input tests deterministic and not flaky under -race on 4 CPUs? Do the fuzz targets cover the new shapes? Run go build, go vet (plain and -tags integration), go test -race -count=1 -cover -shuffle=on, CGO_ENABLED=0 go test -count=1, bin/golangci-lint run (plain and --build-tags integration), bin/golangci-lint fmt --diff, and go run ./internal/tool/repocheck, scoped to the package.` },
]

const finishPrompt = () => `${RULES}
You are the engineer for WP-33. A fix pass was interrupted by a container restart while it ran its final checks; its edits are on disk (it changed tokens.go, profile.go, yaml.go, split.go, explicit.go, doc.go, hostile_test.go, fuzz_test.go and edge_test.go). It was fixing these findings:
1. (blocker, lead) Add a post-parse depth guard where goccy's AST becomes the tree: any node deeper than Options.MaxDepth fails the document with RZ-CFG-001, on both the narrow and the split parse, so accepted trees are bounded whatever the token pass predicts; test it directly with a shape the token pass undercounts.
2. (blocker) A ':' that completes an open explicit '?' entry is accepted even when goccy groups it with a later, deeper node (a block sequence, a second scalar or a tag after the '?' key), so the token pass undercounts depth; ladders of 100 KB and 515 KB reached depth 1,056 and 2,139. Let the ':' complete the '?' entry only when no block frame opened after the '?' was closed by it and at most one node started after the '?'; otherwise RZ-CFG-001 at the ':'. Rows that must parse: ? "x"\\n: y, ? |\\n  k\\n: v, ? >\\n: v, ? !!str\\n: v, ? !!str "a"\\n: v, ? a\\n  b\\n: v.
3. (minor) A '?' entry whose line holds a second node after its key, and an implicit-key ':' on the '?' line inside an open '?' entry (a complex key), must be RZ-CFG-001 on both parses.
Read the current code and tests, finish anything the interrupted pass left incomplete, and make sure all three are fixed with tests (rows at split thresholds -1, 0 and 1; the ladders in TestHostileInputsBounded and as FuzzLoadYAML seeds). Then run the full scoped checks until green: go build and go vet (plain and -tags integration), go test -race -count=1 -cover -shuffle=on ./internal/config/profile/..., CGO_ENABLED=0 go test -count=1 -tags integration ./internal/config/profile/... (YAML Test Suite ratchet), a FuzzLoadYAML run of at least 60s, bin/golangci-lint run (plain and --build-tags integration), bin/golangci-lint fmt ./internal/config/profile/..., and go run ./internal/tool/repocheck. Return the structured report.`

const reviewPrompt = (lens, round, history) => `${RULES}
You are an adversarial reviewer of WP-33 (round ${round}), lens: ${lens.title}. Default to skepticism; your job is to find real defects before this lands. Do NOT modify any repository file.
${lens.focus}
${history ? `Findings already raised and fixed in this loop (do not re-report them unless the fix is incomplete or broke something; say so explicitly if so):\n${history.slice(0, 12000)}` : ''}
Severity: blocker = a required behavior is wrong or missing, a bound can be broken, an accepted tree differs from YAML 1.2 meaning or between narrow and split parse, or a check fails; major = a likely bug, a false rejection of valid in-profile YAML without a documented reason, or a significant test gap; minor = style or small improvement. Every blocker and major finding must include the exact input or command that shows it and what you observed. verdict is "pass" only if there are no blocker or major findings.`

const mergePrompt = (reviews) => `${RULES}
Four reviewers looked at WP-33 through different lenses. Merge their findings: group findings that describe the same defect (same root cause or same fix) into one, keeping the most precise input, location and fix text and the highest severity, and list the lenses in sources. Put every blocker and major finding in findings, and every minor finding in minors (merged the same way). Do not drop or invent findings and do not judge validity; another stage verifies them. Do not modify files.
Reviews:
${JSON.stringify(reviews, null, 1).slice(0, 60000)}`

const verifyPrompt = (f) => `${RULES}
You independently verify one review finding on WP-33. Try to reproduce it on the current code with a concrete probe (go test -overlay with a scratch test file outside the repository, or the package's own tests); do not modify repository files. Confirm it only if you reproduce the defect, or if it is a plain requirement gap or failing check you can point to. If the input behaves correctly, or the behavior is required by the spec or a documented deviation, set confirmed=false and explain. Give your own severity.
Finding:
${JSON.stringify(f, null, 1)}`

const fixPrompt = (findings, minors) => `${RULES}
You are the engineer for WP-33, fixing verified review findings. Each was reproduced by an independent verifier (evidence included). Fix every one at its root cause, with a test that fails without the fix. Also fix each minor finding below that is plainly correct; explain any you reject in deviations.
Verified findings:
${JSON.stringify(findings, null, 1).slice(0, 30000)}
Minor findings:
${JSON.stringify(minors, null, 1).slice(0, 8000)}
Stay inside internal/config/profile/ and test/fixtures/yaml-test-suite/. Then run the full scoped checks until green: go build and go vet (plain and -tags integration), go test -race -count=1 -cover -shuffle=on ./internal/config/profile/..., CGO_ENABLED=0 go test -count=1 -tags integration ./internal/config/profile/..., a FuzzLoadYAML run of at least 60s, bin/golangci-lint run (plain and --build-tags integration), bin/golangci-lint fmt ./internal/config/profile/..., and go run ./internal/tool/repocheck. Return the structured report.`

let finish = null
let history = A.history || ''
let lastMinors = []
if (A.findingsFile) {
  // findingsFile: a JSON file { findings, minors, history } of findings verified in an earlier run.
  phase('Fix')
  finish = await agent(`${RULES}
You are the engineer for WP-33, fixing verified review findings. Read ${A.findingsFile}: "findings" were each reproduced by an independent verifier (their evidence field shows how); fix every one at its root cause, with a test that fails without the fix. "minors" are minor findings: fix each one that is plainly correct and explain any you reject in deviations. "history" lists findings fixed in earlier rounds; do not regress them.
Stay inside internal/config/profile/ and test/fixtures/yaml-test-suite/. Then run the full scoped checks until green: go build and go vet (plain and -tags integration), go test -race -count=1 -cover -shuffle=on ./internal/config/profile/..., CGO_ENABLED=0 go test -count=1 -tags integration ./internal/config/profile/..., a FuzzLoadYAML run of at least 120s with -parallel 3 (it must pass), bin/golangci-lint run (plain and --build-tags integration), bin/golangci-lint fmt ./internal/config/profile/..., and go run ./internal/tool/repocheck. Return the structured report.`, { label: 'fix0:WP-33', phase: 'Fix', schema: IMPL_SCHEMA })
  history += `\nThe findings in ${A.findingsFile} ("findings" and "minors") were fixed just before this review; its "history" lists earlier rounds. Read that file before reviewing.`
  if (finish && finish.deviations && finish.deviations.length) history += `\nEngineer deviations:\n` + JSON.stringify(finish.deviations, null, 1).slice(0, 4000)
} else if (A.findings && A.findings.length) {
  phase('Fix')
  finish = await agent(fixPrompt(A.findings, A.minors || []), { label: 'fix0:WP-33', phase: 'Fix', schema: IMPL_SCHEMA })
  history += `\nFixed just before this review:\n` + JSON.stringify(A.findings.map(f => ({ severity: f.severity, where: f.where, issue: String(f.issue).slice(0, 600) })), null, 1)
  if (finish && finish.deviations && finish.deviations.length) history += `\nEngineer deviations:\n` + JSON.stringify(finish.deviations, null, 1).slice(0, 2500)
} else {
  phase('Finish')
  finish = await agent(finishPrompt(), { label: 'finish:WP-33', phase: 'Finish', schema: IMPL_SCHEMA })
}

const rounds = []
let clean = false
for (let round = 1; round <= MAX_ROUNDS; round++) {
  const reviews = (await parallel(LENSES.map(lens => () =>
    agent(reviewPrompt(lens, round, history), { label: `review${round}:${lens.key}`, phase: 'Review', schema: REVIEW_SCHEMA })
      .then(r => r && { lens: lens.key, ...r })))).filter(Boolean)
  log(`round ${round}: ${reviews.map(r => `${r.lens} ${r.verdict} ${r.findings.length}`).join(', ')}`)
  const all = reviews.flatMap(r => r.findings.map(f => ({ ...f, lens: r.lens })))
  if (!all.length) { rounds.push({ round, reviews, confirmed: [], minors: [] }); clean = true; break }
  const merged = await agent(mergePrompt(reviews), { label: `merge${round}`, phase: 'Merge', schema: MERGE_SCHEMA })
  const candidates = merged ? merged.findings : all.filter(f => f.severity !== 'minor')
  lastMinors = merged ? merged.minors : all.filter(f => f.severity === 'minor')
  const verdicts = await parallel(candidates.map((f, i) => () =>
    agent(verifyPrompt(f), { label: `verify${round}:${i + 1}`, phase: 'Verify', schema: VERDICT_SCHEMA })
      .then(v => ({ finding: f, verdict: v }))))
  const confirmed = verdicts.filter(Boolean).filter(x => x.verdict && x.verdict.confirmed && x.verdict.severity !== 'minor')
  const demoted = verdicts.filter(Boolean).filter(x => x.verdict && x.verdict.confirmed && x.verdict.severity === 'minor').map(x => x.finding)
  lastMinors = [...lastMinors, ...demoted]
  const refuted = verdicts.filter(Boolean).filter(x => !x.verdict || !x.verdict.confirmed)
  log(`round ${round}: ${candidates.length} blocker/major candidates, ${confirmed.length} confirmed, ${refuted.length} refuted, ${lastMinors.length} minor`)
  if (!confirmed.length) { rounds.push({ round, reviews, merged, verdicts, confirmed: [], minors: lastMinors }); clean = true; break }
  if (round === MAX_ROUNDS) { rounds.push({ round, reviews, merged, verdicts, confirmed, minors: lastMinors }); break }
  const fix = await agent(fixPrompt(confirmed.map(x => ({ ...x.finding, evidence: x.verdict.evidence })), lastMinors), { label: `fix${round}:WP-33`, phase: 'Fix', schema: IMPL_SCHEMA })
  rounds.push({ round, reviews, merged, verdicts, confirmed, minors: lastMinors, fix })
  history += `\nRound ${round} fixed:\n` + JSON.stringify(confirmed.map(x => ({ severity: x.verdict.severity, where: x.finding.where, issue: String(x.finding.issue).slice(0, 600) })), null, 1)
  if (fix && fix.deviations && fix.deviations.length) history += `\nEngineer deviations in round ${round}:\n` + JSON.stringify(fix.deviations, null, 1).slice(0, 2000)
}

const last = rounds[rounds.length - 1]
return {
  id: 'WP-33',
  clean,
  finish,
  rounds: rounds.map(r => ({
    round: r.round,
    lensVerdicts: r.reviews.map(x => ({ lens: x.lens, verdict: x.verdict, counts: ['blocker', 'major', 'minor'].map(s => x.findings.filter(f => f.severity === s).length), summary: x.summary })),
    confirmed: (r.confirmed || []).map(x => ({ ...x.finding, verifiedSeverity: x.verdict.severity, evidence: x.verdict.evidence })),
    refuted: (r.verdicts || []).filter(x => x && (!x.verdict || !x.verdict.confirmed)).map(x => ({ where: x.finding.where, issue: String(x.finding.issue).slice(0, 400), why: x.verdict ? x.verdict.note : 'verifier failed' })),
    fix: r.fix || null,
  })),
  remainingConfirmed: clean ? [] : (last.confirmed || []).map(x => ({ ...x.finding, evidence: x.verdict.evidence })),
  remainingMinor: last.minors || [],
}