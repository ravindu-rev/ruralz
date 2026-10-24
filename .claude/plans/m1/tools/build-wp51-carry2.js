// Builds runs/w3-s2-wp51-carry2.json: the earlier carry file plus the record of a G04 attempt 3
// that was stopped before it returned a report (run wf_721727d8-351, stopped on 2026-10-17 at
// 18:41 UTC during the engineer's final checks). The next run continues G04 attempt 3 and reads
// attempt3Progress first (m1-wp51-close.js tells it to).
// Usage: node build-wp51-carry2.js <plan dir m1> <engineer transcript agent-<id>.jsonl> <scratch dir of the attempt> [--write]
// Kept as the record of how carry2 was built and as a template for the next stop: change the
// run, the stop time, the attempt and the changed-files line (from git diff against the last WIP commit).
const fs = require('fs')
const path = require('path')
const [m1, transcript, scratch, flag] = process.argv.slice(2)

const prev = JSON.parse(fs.readFileSync(path.join(m1, 'runs/w3-s2-wp51-carry.json'), 'utf8'))
const texts = []
for (const l of fs.readFileSync(transcript, 'utf8').split('\n')) {
  let e
  try { e = JSON.parse(l) } catch { continue }
  if (e.type !== 'assistant') continue
  for (const c of e.message.content || []) {
    if (c.type === 'text' && c.text.trim()) texts.push(`${(e.timestamp || '').slice(0, 16)}Z ${c.text.replace(/\s+/g, ' ').slice(0, 600)}`)
  }
}
const mutFile = path.join(scratch, 'mut2/progress.txt')
const mut = fs.existsSync(mutFile) ? fs.readFileSync(mutFile, 'utf8').split('\n').filter(Boolean) : []
const killed = mut.filter(l => / KILLED /.test(l)).length
const survived = mut.filter(l => /SURVIV/i.test(l))

const out = {
  ...prev,
  from: `Two stops of G04 attempt 3. (1) ${prev.from} (2) Run wf_721727d8-351 of m1-wp51-close.js (2026-10-17, on the user's Windows machine) continued G04 attempt 3 from the first stop; the lead stopped it at the user's request on 2026-10-17 at 18:41 UTC after about 4 hours, while the engineer ran its final checks (stress and load runs of the changed tests), before it returned a report and before its independent check. Its edits are in the working tree: read attempt3Progress first, then git diff for internal/filter/validation/jsonschema/.`,
  evidenceNote: `${prev.evidenceNote} Run wf_721727d8-351 kept its probes, edit scripts and mutation battery under ${scratch} (session scratchpad; it may be gone).`,
  startGroup: 'G04',
  startAttempt: 3,
  attempt3Progress: {
    run: 'wf_721727d8-351',
    stopped: "2026-10-17T18:41Z, during the engineer's final checks",
    changedFiles: '9 files, +2005 -547 against the wip/s2-handoff commit 451a729: doc.go (266), fit_test.go (122), meter_test.go (1328, the heap meter rewritten), patternsize_test.go (1), regexcost_test.go (1), states_test.go (1), work_limit_test.go (146), working.go (109), working_test.go (578)',
    mutationBattery: `${killed} mutants killed, ${survived.length} survived in ${scratch}/mut2/progress.txt${survived.length ? `: ${survived.join(' | ')}` : ''}`,
    scratchArchive: 'runs/scratch/wp51-g04a3-scratch.tar.gz: the stopped attempt\'s scratch directory without binaries (probes, edit scripts, pre-fix copies under base/, the mutation batteries mut/, mut2/ and mut3/ with their progress.txt, and the stress and load scripts). Unpack it into a scratch directory, never into the repository. A record only: never commit it to develop.',
    engineerMessages: texts,
  },
}

console.log(`messages: ${texts.length}; mutants killed: ${killed}, survived: ${survived.length}; carried: ${out.carried.length}; open: ${out.open.length}`)
if (flag === '--write') {
  fs.writeFileSync(path.join(m1, 'runs/w3-s2-wp51-carry2.json'), JSON.stringify(out, null, 1) + '\n')
  console.log('written: runs/w3-s2-wp51-carry2.json')
}
