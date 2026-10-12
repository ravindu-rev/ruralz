export const meta = {
  name: 'wp51-library-audit',
  description: 'Audit every keyword and mechanism jsonschema/v6 acts on against WP-51 bounds and tests: coverage matrix, gap classes, completeness check',
  phases: [
    { title: 'Audit', detail: 'five library areas, each audited then verified' },
    { title: 'Merge', detail: 'one matrix and a gap list grouped by root-cause class' },
    { title: 'Completeness', detail: 'every library function covered' },
    { title: 'Report', detail: 'final audit document and gap classes' },
  ],
}

const REPO = '/home/user/ruralz'
const M1 = `${REPO}/.claude/plans/m1`
const LIB = '/root/go/pkg/mod/github.com/santhosh-tekuri/jsonschema/v6@v6.0.3'
const PKG = 'internal/filter/validation/jsonschema'
const SP = '/tmp/claude-0/-home-user-ruralz/0aabfe6e-34a6-51d2-9b9b-23886b703760/scratchpad/wp51audit'
const DOC = `${M1}/wp51-library-audit.md`

const CONTEXT = `
Repository ${REPO} (Ruralz API gateway, Go, branch develop). Work package WP-51 is the validation.json-schema Policy in ${REPO}/${PKG}: it compiles an operator's JSON Schema (draft 2020-12) with the third-party library github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 (source: ${LIB}) and validates JSON request bodies with it. Because the library exposes neither its memory nor its work and cannot be stopped, WP-51 bounds it from outside: compile-time rules (RZ-CFG-005 refusals), a pre-validation working-set and work model with reservations and caps (413 RZ-RT-003, 503), number bounds, a regex program measure, a meta-schema re-check, a two-pass validation and more. The working tree holds WP-51's latest state (the committed package plus the post-fix8 changes, uncommitted).
Binding contract: spec 07 (${M1}/specs/07-transform.md) reqs 74 to 84 and 90 and section 9 items 21 to 24; architecture R-63 and R-77 (${M1}/specs/00-architecture.md); the user's decisions in ${M1}/user-decisions.md items 12 to 15 (also the "decisions" field of ${M1}/runs/w3-s2-wp51-r9.json). The user decided on 2026-10-12: NO restricted profile. Every keyword and mechanism the library acts on when compiling and validating a draft 2020-12 schema must be bounded and tested, including older-draft keywords the library still enforces under 2020-12 (its compile runs the draft 4, 6, 7 and 2019-09 layers cumulatively: for example dependencies and $recursiveRef/$recursiveAnchor).
History: nine review rounds kept finding new paths through the library one at a time (serious findings per round 5, 9, 6, 3, 2, 2, 2, 4, 4). Records: ${M1}/runs/w3-s2-wp51-fix-result.json, fix2-, fix3-, fix4-result.json, and the open round-9 findings in ${M1}/runs/w3-s2-wp51-r9.json ($recursiveRef targets escape the rule walk; long member names make the library's compile-queue search quadratic; the dynamic-scope work charge ignores $dynamicAnchor name length; a load-dependent TestWorkingSetCoversPeak case). This audit replaces one-at-a-time discovery with a complete inventory.
Cost dimensions for every row: C1 compile CPU; C2 compile peak memory; C3 memory the compiled Schema keeps; V1 validation CPU (decision and location passes); V2 validation memory (working set, error trees, transient buffers); P panic, crash, hang or unbounded recursion; W wrong result (invalid accepted, valid refused, wrong keyword location or detail); S security (file or network access, loader, URL handling); D nondeterminism.
Status for every row: bounded (a WP-51 rule or charge bounds it AND a named test fails without that bound), partial (bounded in some shapes only, or bounded but untested), gap (not bounded), not-applicable (the cost cannot arise; say why).
Rules: do NOT modify any repository file, except the one file your role names. Probes and scratch tests go under ${SP}/<your-label>/ and reach the package with go test -overlay; build binaries with -o into that directory. Never run git commands that change state. The container has 4 CPUs and other agents run tests; keep runs bounded (-count=1, timeouts).
Be exhaustive within your area: list every keyword, mechanism and code path, not examples. Cite library code as file:line and WP-51 code as file:line, and tests by name.`

const ROW = {
  type: 'object',
  properties: {
    id: { type: 'string', description: 'stable id, e.g. A1-17' },
    feature: { type: 'string', description: 'keyword, mechanism, or interaction class (keyword x references, compositions, nesting, names...)' },
    libraryPath: { type: 'string', description: 'library file:line of the code path, and what it does' },
    cost: { type: 'string', enum: ['C1', 'C2', 'C3', 'V1', 'V2', 'P', 'W', 'S', 'D'] },
    status: { type: 'string', enum: ['bounded', 'partial', 'gap', 'not-applicable'] },
    wp51: { type: 'string', description: 'WP-51 rule or charge that bounds it (file:line), or none' },
    tests: { type: 'string', description: 'WP-51 test names proving the bound, or none' },
    evidence: { type: 'string', description: 'probe command and measurement, or code reasoning' },
    gapFix: { type: 'string', description: 'for partial or gap: the fix for the whole class, and the tests it needs' },
    severity: { type: 'string', enum: ['blocker', 'major', 'minor', 'none'] },
  },
  required: ['id', 'feature', 'libraryPath', 'cost', 'status', 'wp51', 'tests', 'evidence', 'gapFix', 'severity'],
}
const AUDIT_SCHEMA = {
  type: 'object',
  properties: {
    rows: { type: 'array', items: ROW },
    examined: { type: 'array', items: { type: 'string' }, description: 'every library file and function examined' },
    notExamined: { type: 'array', items: { type: 'string' }, description: 'anything in the area not examined, with the reason' },
    summary: { type: 'string' },
  },
  required: ['rows', 'examined', 'notExamined', 'summary'],
}

const AREAS = [
  { key: 'compile', files: 'objcompiler.go, draft.go, compiler.go, vocab.go, content.go, metaschemas/', focus: 'Every keyword each compile layer (draft 4, 6, 7, 2019-09, 2020-12) parses under a draft 2020-12 schema: what it reads, allocates, parses (numbers via math/big, regexes via regexp), enqueues and keeps. The compile queue and its search (enqueue*, the queue lookups), the meta-schema check (draft.go: which values it parses and how), vocabularies, $schema and dialect handling, format and content keywords. For each keyword: compile-time cost and kept memory, and how WP-51 bounds it.' },
  { key: 'validate', files: 'validator.go', focus: 'Runtime behavior of every keyword in validation: CPU per evaluation and per value, memory (unevaluated-tracking maps, error trees, contains index lists, uniqueItems structures, dynamic-scope walks, $recursiveRef resolution), short-circuiting in boolean and full-output modes, recursion depth, and how WP-51 charges each (working set, work units, nesting limit, reservation) in both passes.' },
  { key: 'refs', files: 'roots.go, root.go, loader.go, position.go, util.go (URL, pointer and escaping helpers)', focus: 'References and resources: $ref, $anchor, $dynamicAnchor, $dynamicRef, $recursiveRef, $recursiveAnchor, $id resources, JSON Pointer fragments, percent-encoding and ~0/~1 escapes, URL resolution and the loader (must never reach file: or network), resource collection, subschema positions the library compiles that the meta-schema check does not cover, and every way a keyword can be reached only through a reference. Map each to the WP-51 rule walk (does the walker follow it, count it, apply every rule to its target?).' },
  { key: 'values', files: 'kind/kind.go, util.go (equality, hashing, number handling), output.go, format.go', focus: 'Numbers (json.Number, math/big Rat and Float, Quo, Cmp, multipleOf, min/max, const, enum, uniqueItems hashing and equality), strings and member names (length, non-ASCII, escaping), error kinds and the output and keyword-location building used by the location pass, format and content (must stay annotations), and how WP-51 bounds each (number literal bounds, name and location costs, detail caps).' },
  { key: 'cross', files: 'Go regexp and regexp/syntax as the library uses them, plus interactions across all files', focus: 'Regex compile and match costs (syntax parse, simplify, compile, one-pass tables, backtracker and its job stack and visited set, NFA queues, case folding, Unicode classes), and interaction classes across keywords: every keyword combined with references, with compositions (allOf/anyOf/oneOf/not/if-then-else/dependentSchemas), with nesting depth, with unevaluated*, with long or numerous member names, and with meta-schema re-checks. Find combinations whose cost exceeds the sum WP-51 charges for the parts.' },
]

const auditPrompt = (a) => `${CONTEXT}
Your role: auditor for the library area "${a.key}" (files: ${a.files}). ${a.focus}
Read the library code in full for your files, and the WP-51 code that handles each path. For each (feature, cost) pair that applies, write one row. Measure with probes where a row is uncertain or high-risk. List every function you examined in "examined".`

const verifyPrompt = (a, audit) => `${CONTEXT}
Your role: verifier for the library area "${a.key}" (files: ${a.files}). An auditor produced the rows below. Check them adversarially:
- For every row marked bounded: confirm the WP-51 rule or charge exists at the cited place and covers the whole class, and that the named test would fail without it (mutate with -overlay where cheap). Reclassify to partial or gap when not proven.
- For high-risk rows (P, C1, V1, V2 with hostile inputs): run a probe.
- Add every row the auditor missed in this area (read the library files yourself).
Return the full, corrected row list for the area.
Auditor rows:
${JSON.stringify(audit, null, 1).slice(0, 70000)}`

phase('Audit')
const areaResults = await pipeline(AREAS,
  (a) => agent(auditPrompt(a), { label: `audit:${a.key}`, phase: 'Audit', schema: AUDIT_SCHEMA }),
  (audit, a) => audit
    ? agent(verifyPrompt(a, audit), { label: `verify:${a.key}`, phase: 'Audit', schema: AUDIT_SCHEMA }).then(v => ({ area: a.key, audit, verified: v || audit }))
    : { area: a.key, audit: null, verified: null })

const missingAreas = areaResults.filter(r => !r || !r.verified).map((r, i) => AREAS[i].key)
if (missingAreas.length) log(`areas without results: ${missingAreas.join(', ')}`)
const rows = areaResults.filter(r => r && r.verified).flatMap(r => r.verified.rows.map(x => ({ area: r.area, ...x })))
log(`rows: ${rows.length}; gaps ${rows.filter(r => r.status === 'gap').length}, partial ${rows.filter(r => r.status === 'partial').length}`)

const GAPS_SCHEMA = {
  type: 'object',
  properties: {
    gapClasses: { type: 'array', items: { type: 'object', properties: {
      class: { type: 'string', description: 'root-cause class' },
      rows: { type: 'array', items: { type: 'string' } },
      severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
      fix: { type: 'string', description: 'one fix for the whole class' },
      tests: { type: 'string', description: 'the tests the class needs, one per member and per small edge case' },
    }, required: ['class', 'rows', 'severity', 'fix', 'tests'] } },
    summary: { type: 'string' },
  },
  required: ['gapClasses', 'summary'],
}

phase('Merge')
const merged = await agent(`${CONTEXT}
Your role: merge the five areas' verified rows into ${DOC} (the one repository file you may write). Structure:
1. Method and scope (library version, files, cost dimensions, status meanings).
2. Inventory: every keyword the library acts on under draft 2020-12, by compile layer, and every internal mechanism.
3. The matrix: one table per area, columns ID, feature, cost, status, WP-51 bound (file:line), tests, gap fix.
4. Gap classes: group every partial and gap row by root cause; for each class give the single fix for the whole class and the tests it needs (one per member and per small edge case), and its severity.
5. How the open round-9 findings in ${M1}/runs/w3-s2-wp51-r9.json map to rows and classes.
Plain, direct English; tables for the matrix. Return the gap classes.
Rows (${rows.length}):
${JSON.stringify(rows, null, 1).slice(0, 180000)}
Area notes: ${JSON.stringify(areaResults.filter(Boolean).map(r => ({ area: r.area, summary: r.verified && r.verified.summary, notExamined: r.verified && r.verified.notExamined })), null, 1).slice(0, 12000)}`, { label: 'merge', phase: 'Merge', schema: GAPS_SCHEMA })

const COMPLETE_SCHEMA = {
  type: 'object',
  properties: {
    uncoveredFunctions: { type: 'array', items: { type: 'string' } },
    addedRows: { type: 'array', items: ROW },
    summary: { type: 'string' },
  },
  required: ['uncoveredFunctions', 'addedRows', 'summary'],
}
phase('Completeness')
const critic = await agent(`${CONTEXT}
Your role: completeness check of ${DOC}. List every function and method in the library's non-test files (${LIB}/*.go without _test.go, and ${LIB}/kind/kind.go), every keyword string in objcompiler.go and draft.go, and every Go regexp entry point the library or WP-51 calls. For each, find the matrix row that covers it. For each one no row covers, judge whether it can cost anything under a draft 2020-12 schema or body; if so add a classified row (probe where uncertain). Do NOT modify any file.`, { label: 'completeness', phase: 'Completeness', schema: COMPLETE_SCHEMA })

phase('Report')
const final = await agent(`${CONTEXT}
Your role: finalize ${DOC} (the one repository file you may write). Add the completeness check's rows to the matrix and to the gap classes (merging into existing classes where the root cause is the same), add a short "Coverage" section listing what the completeness check verified, and make sure every partial and gap row belongs to exactly one gap class. Return the final gap classes.
Completeness check: ${JSON.stringify(critic, null, 1).slice(0, 40000)}
Current gap classes: ${JSON.stringify(merged, null, 1).slice(0, 40000)}`, { label: 'finalize', phase: 'Report', schema: GAPS_SCHEMA })

return { doc: DOC, rows: rows.length, byStatus: ['bounded', 'partial', 'gap', 'not-applicable'].map(s => [s, rows.filter(r => r.status === s).length]), missingAreas, critic: critic && { uncovered: critic.uncoveredFunctions.length, added: critic.addedRows.length }, gapClasses: (final || merged) }
