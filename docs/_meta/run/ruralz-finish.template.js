export const meta = {
  name: 'ruralz-finish',
  description: 'Finish the Ruralz doc set: per-document fact ledgers, cross-document critics and fixers (loop until dry), research patch for open escalations, then glossary, docs index, ADR index and root README (label-seeded resume)',
  phases: [
    { title: 'Ledger', detail: 'one compact fact ledger per document' },
    { title: 'Critique', detail: '5 cross-document critics per round, up to 3 rounds' },
    { title: 'Fix', detail: 'one fixer per affected document' },
    { title: 'Research', detail: 'research-file patch for escalated source gaps' },
    { title: 'Index', detail: 'glossary, docs/README.md, docs/adr/README.md, README.md, each reviewed' },
  ],
}

const SEED = /*__SEED__*/{}

const { today, repo, docs, adrs, maxConcurrent = 2, dryRun = false } = args
const M = 'opus'

// ---------- limiter: seed replay, priority queue, halt after the first failed agent (same as ruralz-continue)
class DryStop extends Error { constructor(label) { super(`dry-run stop at ${label}`); this.label = label } }
let HALT = false
let active = 0
let seq = 0
const queue = []
const requested = new Set()
function pump() {
  while (active < maxConcurrent && queue.length) {
    queue.sort((a, b) => b.prio - a.prio || a.seq - b.seq)
    const job = queue.shift()
    active++
    job.run()
  }
}
function limited(prompt, opts, prio = 0) {
  const label = opts.label
  if (requested.has(label)) throw new Error(`label requested twice: ${label}`)
  requested.add(label)
  if (Object.prototype.hasOwnProperty.call(SEED, label)) return Promise.resolve(SEED[label])
  if (dryRun) return Promise.reject(new DryStop(label))
  return new Promise((resolve) => {
    queue.push({ prio, seq: seq++, run: async () => {
      let r = null
      try { if (!HALT) r = await agent(prompt, opts) } catch (e) { r = null }
      active--
      if (r == null && !HALT) { HALT = true; log(`HALT: ${label} returned no result; no new agents will start`) }
      resolve(r)
      pump()
    } })
    pump()
  })
}
const must = (r, what) => { if (r == null) throw new Error(`${what} returned no result`); return r }

const ALL = [...docs, ...adrs]
const PATHS = new Set(ALL.map(d => d.p))
const bySlugPath = Object.fromEntries(ALL.map(d => [d.p, d.s]))
const RULES = `Run rules: do not install packages, do not run git, and do not create or leave any file other than the one(s) this task names (put scratch work under /tmp). Never change a document's front matter status; ADRs keep their date. Documents must stay within their manifest length_words band, and never delete text that resolved an earlier blocking review finding just to save words.`
const BASE = `Repository ${repo}; today ${today}. Binding: docs/_meta/foundation-pack.md (v1), docs/_meta/style-guide.md, docs/_meta/manifest.yaml. docs/architecture/02-configuration-model.md is the only source of kinds, fields and Policy types; foundation pack sections 9-11 hold the CLI command registry, Policy type registry and glossary definitions. ${RULES}`
const VERIFY_FILE = (p) => `Before returning, run \`node scripts/verify-docs.mjs --allow-missing --skip markdownlint 2>&1 | grep -F "${p}"\` from ${repo} and fix every FAIL line for this file, then \`npx --yes markdownlint-cli2 "${p}"\` and fix errors.`
const ledgerPath = (s) => `docs/_meta/run/ledgers/${s}.json`

// ---------- schemas
const LEDGER = { type: 'object', required: ['path', 'facts'], properties: { path: { type: 'string' }, facts: { type: 'integer' } } }
const FINDINGS = { type: 'object', required: ['findings'], properties: { findings: { type: 'array', items: {
  type: 'object', required: ['id', 'tag', 'fix_in', 'section', 'issue', 'evidence', 'fix', 'severity'], properties: {
    id: { type: 'string' }, tag: { type: 'string', description: 'new | regression | still_open:<earlier id>' },
    fix_in: { type: 'string' }, section: { type: 'string' }, issue: { type: 'string' },
    evidence: { type: 'string' }, fix: { type: 'string' }, related_docs: { type: 'array', items: { type: 'string' } },
    severity: { type: 'string', enum: ['blocking', 'minor'] } } } } } }
const FIXED = { type: 'object', required: ['path', 'fixed', 'disputed', 'verifier_clean'], properties: {
  path: { type: 'string' }, fixed: { type: 'array', items: { type: 'string' } },
  disputed: { type: 'array', items: { type: 'object', required: ['id', 'reason'], properties: { id: { type: 'string' }, reason: { type: 'string' } } } },
  verifier_clean: { type: 'boolean' } } }
const RESEARCH = { type: 'object', required: ['resolved', 'files_changed', 'urls_added', 'notes'], properties: {
  resolved: { type: 'boolean' }, files_changed: { type: 'array', items: { type: 'string' } },
  urls_added: { type: 'array', items: { type: 'string' } }, notes: { type: 'string' } } }
const WRITE = { type: 'object', required: ['path', 'verifier_clean'], properties: { path: { type: 'string' }, verifier_clean: { type: 'boolean' }, notes: { type: 'array', items: { type: 'string' } } } }
const VERDICT = { type: 'object', required: ['lens', 'verdict', 'blocking'], properties: {
  lens: { type: 'string' }, verdict: { type: 'string', enum: ['accept', 'revise', 'reject'] },
  blocking: { type: 'array', items: { type: 'object', required: ['id', 'issue', 'fix'], properties: { id: { type: 'string' }, issue: { type: 'string' }, fix: { type: 'string' } } } } } }

// ================= Ledger: compact facts per document so critics can compare the whole set within one context
phase('Ledger')
const ledgerPrompt = (d) => `${BASE}\n\nRead ${repo}/${d.p} completely and write a compact fact ledger to ${repo}/${ledgerPath(d.s)} as JSON: {"path": "${d.p}", "status": <front matter status>, "facts": [{"section": <H2/H3 heading>, "kind": one of port|default|limit|timeout|number|milestone|kind_or_field|policy_type|cli_command|api_path|metric|error_code|library|decision|failure_mode|claim|term, "value": <exact value or quoted phrase, <= 200 chars>}]}. Include EVERY port, default, limit, timeout, performance or capacity number (with its (target)/(hypothesis) tag), Planned (Mx) assignment (feature + milestone), kind/field/Policy type used in examples, ruralz CLI command, admin or Control API path, metric name, RZ- error code, library choice, ADR decision, fail-open/closed rule and competitor claim the document states. Values must be copied exactly. Do not edit the document. Return the path and the number of facts.`
await Promise.all(ALL.map(d => limited(ledgerPrompt(d), { label: `ledger:${d.s}`, phase: 'Ledger', model: M, effort: 'medium', schema: LEDGER }, 1)
  .then(r => must(r, `ledger:${d.s}`))))
log(`ledgers: ${ALL.length} written`)

// ================= Critique -> Fix, loop until no new or regressed blocking findings (cap 3 rounds)
const CRITICS = [
  { key: 'terms', prompt: 'Terminology critic. Across ALL documents find: forbidden aliases; non-canonical names; kinds, fields or Policy type names not defined in the configuration model or pack registry; CLI commands not in the pack CLI registry; glossary terms used with a meaning different from pack section 11; inconsistent spelling or capitalization of canonical terms.' },
  { key: 'contradictions', prompt: 'Contradiction critic. Across ALL documents (including ADRs) find statements that contradict each other or the pack: ports, defaults, limits, timeouts, failure modes (fail-open/closed), Rollout states, Last-Known-Good rules, State Store usage, token accounting, milestone assignments for the same feature, performance or capacity targets, library choices, ADR decisions vs owning documents. For each, decide which document is authoritative (the owning document per the manifest, else the pack) and set fix_in to the NON-authoritative document.' },
  { key: 'completeness', prompt: 'Completeness critic. Check every manifest acceptance criterion for every doc and ADR; the 11 scalability_items coverage in docs/architecture/11-scalability-and-distributed-state.md; that every KrakenD EE-only feature in docs/_meta/research/krakend-parity.md appears in docs/comparison/01-krakend-ee-parity-matrix.md with a milestone or Not planned plus reason, and that the mechanism named exists in the architecture docs; that each of the four differentiators is specified in depth, not just asserted; that operations docs give concrete runbook steps; that Open questions are real questions, not deferred work the doc should have decided.' },
  { key: 'examples', prompt: 'Examples critic. Validate every YAML/JSON config example in every doc against the configuration model (kinds, required fields, field names, Policy types, attachment rules, CEL placement, secretRef usage); every CLI invocation against the pack CLI registry and docs/reference/01-cli-and-api-surface.md; every admin or Control API path against the data plane and CLI/API reference docs; every metric name against the observability doc catalog and naming convention; every error code against pack section 8 areas.' },
  { key: 'roadmap', prompt: 'Roadmap critic. Every "Planned (Mx)" tag across all documents must be consistent for the same feature, appear in docs/roadmap/01-roadmap-and-milestones.md under Mx, and respect dependencies (nothing in an earlier milestone depends on something planned later). The parity matrix milestones must match the component documents. Milestone exit criteria must be measurable and reference the performance budgets where relevant.' },
]
const history = []          // {id, fix_in, issue, outcome}
const escalated = []        // findings that cannot be fixed inside a document of the set
const allFixed = []
let round = 0
let dry = false
while (!dry && round < 3) {
  round++
  phase('Critique')
  const critics = round === 1 ? CRITICS : CRITICS.filter(c => ['contradictions', 'examples', 'terms', 'completeness'].includes(c.key))
  const prior = history.length ? ` Findings from earlier rounds and their outcomes (do not re-report fixed items; tag an earlier item that is still wrong as still_open:<its id>, and anything a fix broke as regression): ${JSON.stringify(history.slice(-150))}.` : ''
  const found = (await Promise.all(critics.map(c => limited(
    `${BASE}\n\nYou are the cross-document ${c.prompt} The documentation set is ${ALL.length} files: ${JSON.stringify(ALL.map(d => d.p))}. The whole set does not fit in one context, so work from the compact fact ledgers in ${repo}/docs/_meta/run/ledgers/*.json (one per document; read all of them), the pack and the manifest, then open the specific document sections you need and quote them before reporting anything. Report only real, specific problems with quoted evidence from the document itself; no style nits.${prior} id format: ${c.key}-r${round}-<n>. tag: new, regression or still_open:<earlier id>. fix_in is the single repo-relative path of the document to change and must be one of the files listed above; if the fix belongs in the pack, manifest or a research file, still report it with fix_in set to that path. severity blocking = wrong, contradictory, or missing required content; minor = clarity. Do not edit files.`,
    { label: `critic:${c.key}:r${round}`, phase: 'Critique', model: M, effort: 'xhigh', schema: FINDINGS }, 3)
    .then(r => must(r, `critic:${c.key}:r${round}`).findings)))).flat()
  const blocking = found.filter(f => f.severity === 'blocking')
  const outside = blocking.filter(f => !PATHS.has(f.fix_in))
  escalated.push(...outside)
  const actionable = blocking.filter(f => PATHS.has(f.fix_in))
  const newOrRegressed = actionable.filter(f => f.tag === 'new' || f.tag === 'regression')
  log(`round ${round}: ${found.length} findings, ${blocking.length} blocking (${newOrRegressed.length} new or regressed, ${actionable.length - newOrRegressed.length} still open, ${outside.length} outside the doc set)`)
  if (!actionable.length) { dry = true; break }
  phase('Fix')
  const byDoc = {}
  for (const f of [...actionable, ...found.filter(f => f.severity === 'minor' && PATHS.has(f.fix_in))]) (byDoc[f.fix_in] = byDoc[f.fix_in] || []).push(f)
  const targets = Object.keys(byDoc).filter(p => byDoc[p].some(f => f.severity === 'blocking'))
  const fixes = await Promise.all(targets.map(p => limited(
    `${BASE}\n\nFix ${repo}/${p} in place. Blocking cross-document findings you MUST resolve (or dispute with a precise reason): ${JSON.stringify(byDoc[p].filter(f => f.severity === 'blocking'))}. Optional minor findings: ${JSON.stringify(byDoc[p].filter(f => f.severity === 'minor').slice(0, 15))}. Read the related documents named in each finding so your fix agrees with them. Keep the mandated H2s.${p.startsWith('docs/adr/') ? '' : ` Update last_updated to ${today}.`} ${VERIFY_FILE(p)} Then update the fact ledger ${repo}/${ledgerPath(bySlugPath[p])} so it matches the fixed document. Edit only these two files.`,
    { label: `fix:r${round}:${p}`, phase: 'Fix', model: M, effort: 'high', schema: FIXED }, 2)
    .then(r => must(r, `fix:r${round}:${p}`))))
  allFixed.push(...fixes)
  for (const x of fixes) {
    for (const id of x.fixed) history.push({ id, fix_in: x.path, outcome: 'fixed' })
    for (const d of x.disputed) history.push({ id: d.id, fix_in: x.path, outcome: `disputed: ${d.reason}` })
  }
  for (const f of actionable) if (!history.some(h => h.id === f.id)) history.push({ id: f.id, fix_in: f.fix_in, issue: f.issue.slice(0, 160), outcome: 'unreported by fixer' })
}
if (!dry) log(`critique loop stopped at the round cap (${round}); the last round's fixes were not re-checked by critics`)

// ================= Research patch for escalations that need sources
phase('Research')
const research = must(await limited(
  `${BASE}\n\nResearch patch. Open escalations that need sources: (1) docs/architecture/07-multi-protocol.md finding L1-2 and OQ-multi-protocol-10: the NATS JetStream consumer pre-buffer setting and in-progress acknowledgments are marked unresearched because docs/_meta/research/go-libraries-protocols.md lacks nats.go sources. ${escalated.length ? `(2) Cross-document findings whose fix belongs outside the documents: ${JSON.stringify(escalated.slice(0, 20))}. Fix those only if they are research-file source gaps; list the rest in notes.` : ''} Use WebFetch/WebSearch to find authoritative sources (pkg.go.dev for github.com/nats-io/nats.go/jetstream, docs.nats.io, the nats.go repository), verify each URL loads and supports the claim, and add them to the NATS section of docs/_meta/research/go-libraries-protocols.md in that file's existing citation style. Then update docs/architecture/07-multi-protocol.md: replace the "unresearched" wording with the sourced facts, cite the URLs, and remove OQ-multi-protocol-10 if it is fully answered (keep it, narrowed, if not). ${VERIFY_FILE('docs/architecture/07-multi-protocol.md')} Edit only those two files. Return resolved true only if L1-2 is fully answered with verified sources.`,
  { label: 'research:multi-protocol-nats', phase: 'Research', model: M, effort: 'high', schema: RESEARCH }, 2), 'research:multi-protocol-nats')
log(`research patch: resolved ${research.resolved}; urls ${research.urls_added.length}`)

// ================= Index documents: write -> L2 + L3 review -> revise (sequential: each reads the previous)
phase('Index')
async function indexDoc(p, task) {
  must(await limited(`${BASE}\n\n${task} Read the manifest entry for ${p} (outline_h2, acceptance, machine_checks) and follow it exactly. ${VERIFY_FILE(p)}`,
    { label: `index-write:${p}`, phase: 'Index', model: M, effort: 'high', schema: WRITE }, 1), `index-write:${p}`)
  const vs = await Promise.all(['L2', 'L3'].map(L => limited(`${BASE}\n\nReviewer lens ${L} (read lenses.${L} in the manifest). Review ${repo}/${p} against its manifest entry and against the actual documents (check every link target and every status it copies). Also run \`node scripts/verify-docs.mjs --skip markdownlint 2>&1 | grep -F " ${p}:"\`; each FAIL line is blocking. Blocking items need id ${L}-<n>, issue and fix. Default to revise when uncertain. Do not edit.`,
    { label: `index-review:${p}:${L}`, phase: 'Index', model: M, effort: 'high', schema: VERDICT }, 1).then(r => must(r, `index-review:${p}:${L}`))))
  const blocking = vs.flatMap(v => v.blocking)
  if (blocking.length) must(await limited(`${BASE}\n\nRevise ${repo}/${p}: resolve ${JSON.stringify(blocking)}. ${VERIFY_FILE(p)} Edit only this file.`,
    { label: `index-revise:${p}`, phase: 'Index', model: M, effort: 'high', schema: WRITE }, 1), `index-revise:${p}`)
  return { p, blocking: blocking.length }
}
const idx = []
idx.push(await indexDoc('docs/glossary.md', `Write ${repo}/docs/glossary.md: every term in manifest glossary_terms, alphabetical, each with definition (40 words max, consistent with pack section 11 and the documents), canonical spelling, forbidden aliases, and a "Defined in" relative link to the section that owns it (verify the anchor exists). Front matter per style guide, status reviewed.`))
idx.push(await indexDoc('docs/README.md', `Write ${repo}/docs/README.md, the documentation index: organization, five reading paths, a status table with every document under docs/ (excluding docs/_meta) linked exactly once outside the reading paths and its status copied from its front matter, a link to the ADR index, and conventions. Front matter per style guide, status reviewed.`))
idx.push(await indexDoc('docs/adr/README.md', `Write ${repo}/docs/adr/README.md, the ADR index per manifest adr_index: a table ID | Title | Status | Date | Owning document with one row per ADR file in docs/adr/, values copied from each ADR's front matter, each ID linked to its file and each owning document linked.`))
idx.push(await indexDoc('README.md', `Write the repository root ${repo}/README.md (150 lines max, no front matter): what Ruralz is, why (vs KrakenD Enterprise, everything Apache-2.0, no feature gating), the four differentiators verbatim from the vision doc, status (design phase, nothing implemented), a documentation map linking docs/README.md and key docs, license and governance (Apache-2.0, Copyright 2026 Revington, trademark note, DCO), and a system-context Mermaid diagram with caption. Make no claim absent from the parity matrix or roadmap.`))

return { rounds: round, dry, fixed: allFixed.map(f => ({ path: f.path, fixed: f.fixed.length, disputed: f.disputed })), escalated, research, index: idx, halted: HALT }
