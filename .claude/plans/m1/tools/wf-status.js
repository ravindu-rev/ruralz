// Live status of workflow runs, for IDE sessions where /workflows is unavailable.
// Usage: node wf-status.js [--watch [seconds]] [--done N] <runDir> [runDir ...]
// A run directory is ~/.claude/projects/<project slug>/<session id>/subagents/workflows/wf_<run id>;
// it holds journal.jsonl and one agent-<id>.jsonl transcript per agent. --watch redraws every
// `seconds` (default 20); --done N shows the last N finished agents per run (default 8).
// Running agents show their action count, idle time and last action; an agent whose transcript
// ends in an API error is marked as died, because the journal never records a result for it.
const fs = require('fs')
const path = require('path')

const argv = process.argv.slice(2)
let watch = 0
let doneShown = 8
const runs = []
for (let i = 0; i < argv.length; i++) {
  if (argv[i] === '--watch') watch = Number(argv[i + 1]) > 0 ? Number(argv[++i]) : 20
  else if (argv[i] === '--done') doneShown = Number(argv[++i]) || 0
  else runs.push({ dir: argv[i], name: path.basename(argv[i]) })
}
if (!runs.length) {
  console.error('usage: node wf-status.js [--watch [seconds]] [--done N] <runDir> [runDir ...]')
  process.exit(2)
}

const width = () => Math.max(80, (process.stdout.columns || 120) - 1)
const clip = (s, n) => (s.length > n ? s.slice(0, n - 1) + '…' : s)
const mins = (ms) => {
  const m = Math.floor(ms / 60000)
  return m >= 60 ? `${Math.floor(m / 60)}h${String(m % 60).padStart(2, '0')}m` : `${m}m`
}

const readLines = (f) => {
  try { return fs.readFileSync(f, 'utf8').split('\n').filter(Boolean) } catch { return [] }
}

// Short text for a finished agent's structured result (fix, review, merge or verdict).
const describe = (r) => {
  if (r === null || r === undefined) return 'NO RESULT (failed or skipped)'
  if (typeof r === 'string') return `text: ${r.replace(/\s+/g, ' ')}`
  if ('verdict' in r && Array.isArray(r.findings)) {
    const c = { blocker: 0, major: 0, minor: 0 }
    for (const f of r.findings) c[f.severity] = (c[f.severity] || 0) + 1
    return `review ${r.verdict}: ${c.blocker} blocker, ${c.major} major, ${c.minor} minor`
  }
  if ('confirmed' in r) return `verify: ${r.confirmed ? 'CONFIRMED' : 'refuted'} (${r.severity})`
  if (Array.isArray(r.findings) && Array.isArray(r.minors)) return `merge: ${r.findings.length} serious, ${r.minors.length} minor`
  if ('status' in r) {
    const members = Array.isArray(r.members) ? `, ${r.members.length} members` : ''
    const paths = Array.isArray(r.paths) ? `, ${r.paths.length} paths` : ''
    return `fix ${r.status}${members}${paths}`
  }
  return JSON.stringify(r).slice(0, 120)
}

// Last tool call or text of a running agent, from its transcript.
const lastAction = (file) => {
  const lines = readLines(file)
  let actions = 0
  let last = ''
  let dead = false
  for (const l of lines) {
    let e
    try { e = JSON.parse(l) } catch { continue }
    const content = e.type === 'assistant' && e.message && Array.isArray(e.message.content) ? e.message.content : []
    // An assistant entry flagged as an API error ends the agent; any later entry revives it.
    if (e.type === 'assistant') dead = !!(e.isApiErrorMessage || e.error)
    for (const c of content) {
      if (c.type === 'tool_use') {
        actions++
        const inp = c.input || {}
        last = `${c.name}: ${String(inp.command || inp.file_path || inp.pattern || inp.description || JSON.stringify(inp)).replace(/\s+/g, ' ')}`
      } else if (c.type === 'text' && c.text.trim()) {
        last = `“${c.text.replace(/\s+/g, ' ')}”`
      }
    }
  }
  return { actions, last, dead }
}

const render = () => {
  const now = Date.now()
  const out = []
  const w = width()
  out.push(`Workflow status  ${new Date(now).toISOString().replace('T', ' ').slice(0, 19)} UTC${watch ? `  (refresh ${watch}s, Ctrl+C to stop)` : ''}`)
  for (const run of runs) {
    const journal = path.join(run.dir, 'journal.jsonl')
    const started = new Map()
    const results = new Map()
    for (const l of readLines(journal)) {
      let e
      try { e = JSON.parse(l) } catch { continue }
      if (e.type === 'started') started.set(e.agentId, e)
      else if (e.type === 'result') results.set(e.agentId, e.result)
    }
    let launchedAt = null
    try { launchedAt = fs.statSync(journal).birthtimeMs } catch { /* no journal yet */ }
    const running = [...started.values()].filter(s => !results.has(s.agentId))
    const done = [...started.values()].filter(s => results.has(s.agentId))
    out.push('')
    out.push(`■ ${run.name}  [${path.basename(run.dir)}]  ${launchedAt ? `up ${mins(now - launchedAt)}` : 'no journal'}  agents: ${started.size} started, ${done.length} done, ${running.length} running`)
    for (const s of running) {
      const file = path.join(run.dir, `agent-${s.agentId}.jsonl`)
      let idle = ''
      try { idle = mins(now - fs.statSync(file).mtimeMs) } catch { idle = '?' }
      const { actions, last, dead } = lastAction(file)
      out.push(clip(`  ${dead ? '✗' : '▶'} ${s.label.padEnd(28)} ${String(actions).padStart(4)} acts  idle ${idle.padStart(5)}  ${dead ? 'DIED (API error): ' : ''}${last}`, w))
    }
    const shown = doneShown ? done.slice(-doneShown) : []
    if (done.length > shown.length) out.push(`  … ${done.length - shown.length} earlier finished agents not shown (--done N)`)
    for (const s of shown) out.push(clip(`  ✓ ${s.label.padEnd(28)} ${describe(results.get(s.agentId))}`, w))
    if (!started.size) out.push('  (no agents started yet)')
  }
  return out.join('\n')
}

if (watch) {
  const tick = () => { process.stdout.write('\x1b[2J\x1b[H' + render() + '\n') }
  tick()
  setInterval(tick, watch * 1000)
} else {
  console.log(render())
}
