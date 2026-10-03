export const meta = {
  name: 'm1-apply-decisions',
  description: 'Apply the triaged wave-2 contract change decisions per unit, then review and fix each unit',
  phases: [
    { title: 'Apply', detail: 'one engineer per unit, owned paths only' },
    { title: 'Review', detail: 'adversarial review of each unit' },
    { title: 'Fix', detail: 'apply review findings' },
    { title: 'Re-review', detail: 'second pass after fixes' },
  ],
}

const REPO = '/home/user/ruralz'
const M1 = '/home/user/ruralz/.claude/plans/m1'
const units = args.units

const CONTEXT = `
Repository: ${REPO} (Go module github.com/ravindu-rev/ruralz, branch develop). Ruralz is an API gateway; milestone M1 waves 1 and 2 are committed. Binding architecture: ${M1}/specs/00-architecture.md; work packages: ${M1}/arch/wps.json; area specs: ${M1}/specs/01-*.md ... 11-*.md; design documents under docs/.
At the wave-2 boundary, engineers raised contract change requests. The lead triaged them; the decisions are in ${M1}/triage.json (one entry per request: n, action, targets, duplicateOf, summary, change). The original request text is in ${M1}/crs-all.json (by n). The "change" text of each triage entry is the lead's decision; apply it as written unless doing so would break a spec requirement or an existing test, in which case do not apply it and explain in deviations.
Rules:
1. Write ONLY inside your unit's owned paths. Other units are editing other paths in this same working tree right now. Exception: you may add an allow entry to a depguard rule in .golangci.yml when an import your change needs is refused; make one minimal edit and re-read the file first, because another unit may edit another line.
2. Never run git commands that change state (no add, commit, stash, checkout, reset, restore, clean). Read-only git is fine.
3. Never run whole-tree mutating commands (no go mod tidy, no gofmt -w ., no golangci-lint fmt ./...). Scope checks to your packages. If a package you do not own fails to compile because another unit is mid-edit, ignore it.
4. Conventions: AGENTS.md hard rules and .claude/rules/*.md; architecture section 0. Keep each change minimal and keep committed behavior and exported APIs stable except where a decision changes them; update every caller inside your owned paths, and report callers outside them.
`

const IMPL_SCHEMA = {
  type: 'object',
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    summary: { type: 'string', description: 'what changed, 3-8 sentences' },
    applied: { type: 'array', items: { type: 'integer' }, description: 'request numbers applied (fully or the part in your paths)' },
    skipped: { type: 'array', items: { type: 'string' }, description: '"<n>: reason" for each assigned request not applied, or applied only in part' },
    paths: { type: 'array', items: { type: 'string' }, description: 'repository paths created or modified' },
    checks: { type: 'string', description: 'exact commands run and their results' },
    followups: { type: 'array', items: { type: 'string' }, description: 'changes needed outside your paths (callers, docs, other units), each naming the path' },
    deviations: { type: 'array', items: { type: 'string' } },
  },
  required: ['status', 'summary', 'applied', 'skipped', 'paths', 'checks', 'followups', 'deviations'],
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

const applyPrompt = (u) => `${CONTEXT}
You are the engineer for unit ${u.id}: ${u.title}.
Owned paths: ${u.owns.join(', ')}.
Your requests (triage numbers): ${u.items.join(', ')}.
${u.note || ''}
For each request: read its triage entry (and, when you need detail, the original request), read the code or document it concerns, and apply the part of the decision that falls inside your owned paths. Parts outside your paths belong to other units (architecture and wps.json text: unit A; design documents: unit D; other packages: the unit owning them); skip those parts and list them in followups only when no other unit obviously covers them. An entry with duplicateOf follows the entry it duplicates.
${u.checks}
Return the structured report.`

const reviewPrompt = (u, impl) => `${CONTEXT}
You are an adversarial reviewer for unit ${u.id}: ${u.title} (owned paths: ${u.owns.join(', ')}; requests ${u.items.join(', ')}). Default to skepticism.
The engineer reported:
${JSON.stringify(impl, null, 1).slice(0, 7000)}
Check, by reading the triage entries, the changed files (git diff and git status; untracked files are new) and the surrounding code or text:
- each assigned request is applied correctly where it falls in the owned paths, or skipped for a sound reason; a decision applied wrongly or a sound decision skipped is a major finding;
- no regression: behavior, exported API, tests, coverage, conventions (AGENTS.md, .claude/rules, architecture section 0); nothing written outside the owned paths (except a minimal .golangci.yml allow entry);
- every exported API change has all callers inside the owned paths updated.
${u.checks}
Do NOT modify any file. verdict is "pass" only if there are no blocker or major findings.`

const fixPrompt = (u, review) => `${CONTEXT}
You are the engineer for unit ${u.id}: ${u.title} (owned paths: ${u.owns.join(', ')}), fixing review findings:
${JSON.stringify(review.findings, null, 1).slice(0, 10000)}
Fix every blocker and major finding and every minor finding that is plainly correct; explain any finding you reject in deviations. Stay inside the owned paths.
${u.checks}
Return the structured report (paths = files changed in this pass).`

const rereviewPrompt = (u, review, fix) => `${CONTEXT}
You are an adversarial re-reviewer for unit ${u.id}: ${u.title} (owned paths: ${u.owns.join(', ')}). A previous review raised:
${JSON.stringify(review.findings, null, 1).slice(0, 7000)}
The engineer reported:
${JSON.stringify(fix, null, 1).slice(0, 4000)}
Verify each finding is fixed, judge any rebuttals, and look for regressions.
${u.checks}
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
    if (u.after && gates[u.after]) await gates[u.after].p
    try {
      return await agent(applyPrompt(u), { label: `apply:${u.id}`, phase: 'Apply', schema: IMPL_SCHEMA })
    } finally {
      gates[u.id].resolve()
    }
  },
  (impl, u) => agent(reviewPrompt(u, impl), { label: `review:${u.id}`, phase: 'Review', schema: REVIEW_SCHEMA })
    .then(review => ({ impl, review })),
  async (prev, u) => {
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
  })

return results.map((r, i) => {
  const u = units[i]
  if (!r) return { id: u.id, error: 'pipeline failed' }
  const fixes = r.rounds.map(x => x.fix).filter(Boolean)
  return {
    id: u.id,
    status: r.impl?.status,
    summary: r.impl?.summary,
    applied: r.impl?.applied,
    skipped: r.impl?.skipped,
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
