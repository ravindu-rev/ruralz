// Usage: node wf-watch.js <journal.jsonl> <stopLabelRegex> [pollSeconds]
// Prints one line per new started or result event (label, and a short result summary),
// and exits with "STOP-POINT <label>" when an agent whose label matches stopLabelRegex starts.
// Run it as a Monitor to stop a run at a clean point: for example '^(?!(fix|check):G04:3$)'
// fires when the agent after G04 attempt 3's fix and check starts, before it edits anything.
// On Windows wrap it as `timeout 1790 node wf-watch.js ...`: an expired Monitor there leaves
// its command running.
const fs = require('fs')
const [file, stopPattern, pollArg] = process.argv.slice(2)
const stop = new RegExp(stopPattern)
const poll = (Number(pollArg) || 10) * 1000
const labels = new Map()
let seen = 0

const summary = (r) => {
  if (r === null || r === undefined) return 'no result'
  if (typeof r !== 'object') return 'text'
  if ('verdict' in r && Array.isArray(r.findings)) return `${r.verdict}, ${r.findings.length} findings`
  if ('status' in r) return `${r.status}, ${(r.paths || []).length} paths`
  return 'object'
}

const tick = () => {
  let lines
  try { lines = fs.readFileSync(file, 'utf8').split('\n') } catch { return }
  // The last element is empty or a partial line still being written: leave it for the next tick.
  const complete = lines.slice(0, -1)
  for (; seen < complete.length; seen++) {
    let e
    try { e = JSON.parse(complete[seen]) } catch { continue }
    if (e.type === 'started') {
      labels.set(e.agentId, e.label)
      if (stop.test(e.label)) {
        console.log(`STOP-POINT ${e.label} started`)
        process.exit(0)
      }
      console.log(`started ${e.label}`)
    } else if (e.type === 'result') {
      console.log(`result ${labels.get(e.agentId) || e.agentId}: ${summary(e.result)}`)
    }
  }
}

// Skip the events that already exist, so only new ones are reported.
try {
  const lines = fs.readFileSync(file, 'utf8').split('\n').slice(0, -1)
  for (const l of lines) {
    try { const e = JSON.parse(l); if (e.type === 'started') labels.set(e.agentId, e.label) } catch { /* partial */ }
  }
  seen = lines.length
} catch { /* journal not there yet */ }
setInterval(tick, poll)
