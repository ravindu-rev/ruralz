// Builds the continuation inputs of the S2 minors run from run wf_7860476f-71c, which the lead
// stopped on 2026-10-17 at 17:23 UTC at a point where no fix agent was editing:
//   runs/w3-s2-minors-continue3.json  open findings per package whose next step is a fix
//   reports/WP-35.json                 the fix0 report of WP-35, whose next step is a review
// Usage: node build-minors-continue3.js <journal.jsonl> <plan dir m1> [--write]
// Kept as the record of how continue3 was built and as a template for the next stop: change RUN,
// STOPPED, and each package's block to match where that run stopped (merged and verified, merged
// only, lenses only, or a finished fix whose next step is a review).
const fs = require('fs')
const path = require('path')
const [journal, m1, flag] = process.argv.slice(2)
const RUN = 'wf_7860476f-71c'
const STOPPED = '2026-10-17 at 17:23 UTC'

const labels = new Map()
const results = new Map()
for (const l of fs.readFileSync(journal, 'utf8').split('\n')) {
  let e
  try { e = JSON.parse(l) } catch { continue }
  if (e.type === 'started') labels.set(e.agentId, e.label)
  else if (e.type === 'result') results.set(labels.get(e.agentId), e.result)
}
const started = new Set(labels.values())
const has = (label) => results.has(label) && results.get(label) !== null
const stoppedLabels = [...started].filter(l => !has(l))

// Mirrors m1-section.js harden(): merged serious findings go to independent verifiers in order
// (verify<round>:<id>:<i+1>); confirmed serious ones carry the verifier's evidence, confirmed
// minors join the merge's minors, refuted ones are dropped.
const fromMerge = (id, round) => {
  const merged = results.get(`merge${round}:${id}`)
  const findings = []
  const refuted = []
  let unverified = 0
  merged.findings.forEach((f, i) => {
    const label = `verify${round}:${id}:${i + 1}`
    const v = results.get(label)
    if (!started.has(label) || v === undefined || v === null) {
      unverified++
      findings.push({ ...f, verified: false })
    } else if (!v.confirmed) {
      refuted.push({ where: f.where, issue: String(f.issue).slice(0, 300), why: v.note })
    } else {
      findings.push({ ...f, severity: v.severity, verifiedSeverity: v.severity, evidence: v.evidence, verified: true })
    }
  })
  for (const m of merged.minors) findings.push({ ...m, verified: false })
  return { findings, refuted, unverified }
}

const fromLenses = (id, round) => {
  const findings = []
  const lenses = []
  const missing = []
  for (const lens of ['spec', 'adversarial', 'integration', 'code']) {
    const label = `review${round}:${lens}:${id}`
    const r = results.get(label)
    if (r && Array.isArray(r.findings)) {
      lenses.push(`${lens} ${r.verdict} (${r.findings.length})`)
      for (const f of r.findings) findings.push({ ...f, sources: [lens], verified: false })
    } else if (started.has(label)) missing.push(lens)
  }
  return { findings, lenses, missing }
}

const counts = (fs_) => fs_.reduce((a, f) => ((a[f.severity] = (a[f.severity] || 0) + 1), a), {})
const out = {}

// WP-39: round 1 merged and every serious finding verified.
{
  const r = fromMerge('WP-39', 1)
  out['WP-39'] = {
    from: `run ${RUN} (stopped by the lead on ${STOPPED}, no fix in flight): round 1 merged; every serious finding was verified independently (verified: true, with evidence); the merge's minors and verified-minor findings were not reproduced (verified: false). fix1 had not started, so no edits of this run's round 1 are on disk.`,
    findings: r.findings,
    refuted: r.refuted,
  }
}
// WP-36: round 1 merged; the verifiers were stopped before any reported.
{
  const r = fromMerge('WP-36', 1)
  out['WP-36'] = {
    from: `run ${RUN} (stopped by the lead on ${STOPPED}, no fix in flight): fix0 done, then round 1 merged; the lead stopped the run while the first verifier ran, so ${r.unverified} serious finding(s) are not reproduced yet (verified: false): reproduce each before changing code. No edits of round 1 are on disk.`,
    findings: r.findings,
    refuted: r.refuted,
  }
}
// WP-38: round 1 not merged: three lenses reported, the adversarial lens was stopped.
{
  const r = fromLenses('WP-38', 1)
  out['WP-38'] = {
    from: `run ${RUN} (stopped by the lead on ${STOPPED}, no fix in flight): fix0 done, then round 1: ${r.lenses.join(', ')}; ${r.missing.length ? `the ${r.missing.join(' and ')} lens was stopped before it reported, so ` : ''}these findings were not merged or reproduced (verified: false): reproduce each before changing code. The next review round covers every lens again. No edits of round 1 are on disk.`,
    findings: r.findings,
  }
}
// WP-37: its fix0 died on an API error before changing a file; the open findings are unchanged.
{
  const prev = JSON.parse(fs.readFileSync(path.join(m1, 'runs/w3-s2-minors-continue2.json'), 'utf8'))['WP-37']
  out['WP-37'] = {
    from: `${prev.from} Then fix0 of run ${RUN} died at 14:58 UTC on an API error ("No response from API") before it changed a file, so these findings are still open and no partial edits of that run are on disk.`,
    findings: prev.findings,
  }
}

// WP-35: fix0 finished; its round-1 reviewers were stopped, so its next step is a fresh review.
const fix0 = results.get('fix0:WP-35')
const report35 = {
  id: 'WP-35',
  note: `Lead summary for a continuation run (2026-10-17): fix0 of run ${RUN} closed the 11 open findings of runs/w3-s2-minors-continue2.json; its four round-1 reviewers were stopped on ${STOPPED} before they reported, so the next step is a fresh review. The uncommitted fixes of runs wf_9c24f909-7ca, wf_c0fb6de8-9cc, wf_2dcc9b57-933 and ${RUN} are in the working tree.`,
  lastFix: `fix0 (run ${RUN})`,
  report: { status: fix0.status, summary: fix0.summary, paths: fix0.paths, checks: fix0.checks, contractRequests: fix0.contractRequests, deviations: fix0.deviations },
}

// Contract requests filed by this run's fix agents, for the lead's triage at the end of S2.
const contractRequests = {}
for (const id of ['WP-35', 'WP-36', 'WP-38']) {
  const f = results.get(`fix0:${id}`)
  if (f && f.contractRequests && f.contractRequests.length) contractRequests[id] = f.contractRequests
}

console.log('agents started:', started.size, 'with result:', [...results.values()].filter(v => v !== null).length)
console.log('stopped or died without result:', stoppedLabels.join(', '))
for (const [id, e] of Object.entries(out)) console.log(id, JSON.stringify(counts(e.findings)), 'unverified:', e.findings.filter(f => f.verified === false).length, e.refuted ? `refuted: ${e.refuted.length}` : '')
console.log('WP-35 fix0:', fix0.status, fix0.paths.length, 'paths;', (fix0.contractRequests || []).length, 'contract requests')
console.log('contract requests by package:', JSON.stringify(Object.fromEntries(Object.entries(contractRequests).map(([k, v]) => [k, v.length]))))
if (flag === '--write') {
  out._meta = { run: RUN, stopped: STOPPED, contractRequests, stoppedAgents: stoppedLabels }
  fs.writeFileSync(path.join(m1, 'runs/w3-s2-minors-continue3.json'), JSON.stringify(out, null, 1) + '\n')
  fs.writeFileSync(path.join(m1, 'reports/WP-35.json'), JSON.stringify(report35, null, 1) + '\n')
  console.log('written: runs/w3-s2-minors-continue3.json, reports/WP-35.json')
}
