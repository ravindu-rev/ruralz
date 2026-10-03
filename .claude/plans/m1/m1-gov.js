export const meta = {
  name: 'm1-governance-docs',
  description: 'Write the user-requested ADRs, research addendum and WP-32 doc fixes, then review and fix each unit',
  phases: [
    { title: 'Write', detail: 'one writer per unit, owned files only' },
    { title: 'Review', detail: 'adversarial review of each unit' },
    { title: 'Fix', detail: 'apply review findings' },
    { title: 'Re-review', detail: 'second pass after fixes' },
  ],
}

const REPO = '/home/user/ruralz'
const M1 = '/home/user/ruralz/.claude/plans/m1'
const units = args.units

const CONTEXT = `
Repository: ${REPO} (Ruralz, a Go API gateway; branch develop). The design documents under docs/ are the specification. Milestone M1 waves 1 and 2 are committed. The user's decisions at the wave-2 boundary are recorded in ${M1}/user-decisions.md: read it first; it is the authority for this work. Supporting material: ${M1}/triage.json (request decisions by number n, with ${M1}/crs-all.json holding the original request text) and the binding architecture ${M1}/specs/00-architecture.md (section 2.16 R-rows).
Rules:
1. Write ONLY the files your unit owns (listed below). Other units edit other files in this same working tree.
2. Never run git commands that change state; read-only git (status, diff, log, show) is fine.
3. Follow .claude/rules/docs.md, .claude/rules/adr.md and docs/_meta/style-guide.md exactly (no em dashes; Planned (Mx) tags; word counts inside each document's docs/_meta/manifest.yaml band, counted by its word_count_rule; citations only from docs/_meta/research/; OQ row format; last_updated bumps to 2026-10-03). Several documents sit at their maximum word count: keep edits there word-neutral by tightening nearby text you own.
`

const IMPL_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string', description: 'what changed, 3-8 sentences' },
    paths: { type: 'array', items: { type: 'string' } },
    checks: { type: 'string', description: 'exact commands run and their results, word counts per changed document' },
    followups: { type: 'array', items: { type: 'string' }, description: 'changes needed outside your files, each naming the file' },
    deviations: { type: 'array', items: { type: 'string' } },
  },
  required: ['status', 'summary', 'paths', 'checks', 'followups', 'deviations'],
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
          where: { type: 'string' },
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

const CHECKS = `Checks: go test -count=1 -run 'TestMatchesDocs|TestRegistry' ./internal/errcode/; go test -count=1 -run 'TestDocSlugs' ./internal/tool/commitcheck/; go run ./internal/tool/repocheck; for each changed Markdown file: no em dashes (grep for the U+2014 character), relative links resolve, front matter valid YAML, and the word count inside the manifest band (compute it with the manifest's word_count_rule and report the numbers).`

const writePrompt = (u) => `${CONTEXT}
You are the writer for unit ${u.id}: ${u.title}.
Owned files: ${u.owns.join(', ')}.
Task:
${u.task}
${CHECKS}
Return the structured report.`

const reviewPrompt = (u, impl) => `${CONTEXT}
You are an adversarial reviewer for unit ${u.id}: ${u.title} (owned files: ${u.owns.join(', ')}). Default to skepticism.
The task was:
${u.task}
The writer reported:
${JSON.stringify(impl, null, 1).slice(0, 6000)}
Check, by reading the diff (git diff; untracked files are new) and the surrounding documents: every part of the task is done correctly and consistently with ${M1}/user-decisions.md, the ADR and docs rules and the style guide; every count, index row, manifest entry and cross-reference is updated consistently; no claim lacks a docs/_meta/research citation where one is required; nothing outside the owned files changed; word bands hold.
${CHECKS}
Do NOT modify any file. verdict is "pass" only if there are no blocker or major findings.`

const fixPrompt = (u, review) => `${CONTEXT}
You are the writer for unit ${u.id}: ${u.title} (owned files: ${u.owns.join(', ')}), fixing review findings:
${JSON.stringify(review.findings, null, 1).slice(0, 10000)}
Fix every blocker and major finding and every minor finding that is plainly correct; explain any finding you reject in deviations. Stay inside the owned files.
${CHECKS}
Return the structured report (paths = files changed in this pass).`

const rereviewPrompt = (u, review, fix) => `${CONTEXT}
You are an adversarial re-reviewer for unit ${u.id}: ${u.title} (owned files: ${u.owns.join(', ')}). A previous review raised:
${JSON.stringify(review.findings, null, 1).slice(0, 7000)}
The writer reported:
${JSON.stringify(fix, null, 1).slice(0, 4000)}
Verify each finding is fixed, judge any rebuttals, and look for regressions.
${CHECKS}
Do NOT modify files. verdict is "pass" only if no blocker or major finding remains.`

const blocking = (r) => !r || r.verdict !== 'pass' || r.findings.some(f => f.severity !== 'minor')

const gates = {}
for (const u of units) {
  let resolve
  const p = new Promise(r => { resolve = r })
  gates[u.id] = { p, resolve }
}

const results = await pipeline(units,
  async (u) => {
    for (const dep of (u.after || [])) if (gates[dep]) await gates[dep].p
    return agent(writePrompt(u), { label: `write:${u.id}`, phase: 'Write', schema: IMPL_SCHEMA })
      .catch(e => { gates[u.id].resolve(); throw e })
  },
  (impl, u) => agent(reviewPrompt(u, impl), { label: `review:${u.id}`, phase: 'Review', schema: REVIEW_SCHEMA })
    .then(review => ({ impl, review }))
    .catch(e => { gates[u.id].resolve(); throw e }),
  async (prev, u) => {
    try {
      const { impl, review } = prev
      const rounds = []
      let current = review
      for (let round = 1; round <= 2 && current && (blocking(current) || current.findings.length > 0); round++) {
        const fix = await agent(fixPrompt(u, current), { label: `fix${round}:${u.id}`, phase: 'Fix', schema: IMPL_SCHEMA })
        const again = await agent(rereviewPrompt(u, current, fix), { label: `rereview${round}:${u.id}`, phase: 'Re-review', schema: REVIEW_SCHEMA })
        rounds.push({ fix, review: again })
        current = again
        if (again && !blocking(again)) break
      }
      return { impl, review, rounds, final: current }
    } finally {
      gates[u.id].resolve()
    }
  })


return results.map((r, i) => {
  const u = units[i]
  if (!r) return { id: u.id, error: 'pipeline failed' }
  const fixes = r.rounds.map(x => x.fix).filter(Boolean)
  return {
    id: u.id,
    status: r.impl?.status,
    summary: r.impl?.summary,
    paths: [...new Set([...(r.impl?.paths || []), ...fixes.flatMap(f => f.paths || [])])],
    followups: [...(r.impl?.followups || []), ...fixes.flatMap(f => f.followups || [])],
    deviations: [...(r.impl?.deviations || []), ...fixes.flatMap(f => f.deviations || [])],
    checks: fixes.length ? fixes[fixes.length - 1].checks : r.impl?.checks,
    firstReview: r.review ? { verdict: r.review.verdict, counts: ['blocker', 'major', 'minor'].map(s => r.review.findings.filter(f => f.severity === s).length) } : null,
    finalVerdict: r.final?.verdict,
    remaining: (r.final?.findings || []).filter(f => f.severity !== 'minor'),
    remainingMinor: (r.final?.findings || []).filter(f => f.severity === 'minor'),
  }
})
