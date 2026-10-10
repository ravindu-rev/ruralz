export const meta = {
  name: 'yaml-parser-decision',
  description: 'Gather evidence on goccy, go-yaml v4 and a hand-written parser for the restricted YAML profile, then write, critique and revise a decision brief',
  phases: [
    { title: 'Gather', detail: 'history, go-yaml v4 prototype, hand-written feasibility, upstream, governance' },
    { title: 'Synthesize', detail: 'decision brief with a recommendation' },
    { title: 'Critique', detail: 'evidence check and devil\'s advocate' },
    { title: 'Revise', detail: 'final brief' },
  ],
}

const REPO = '/home/user/ruralz'
const M1 = `${REPO}/.claude/plans/m1`
const SP = '/tmp/claude-0/-home-user-ruralz/758d7ca3-81cf-5fbc-a967-12d97bce3d6b/scratchpad/yamldecision'
const BRIEF = `${M1}/yaml-parser-decision.md`

const CONTEXT = `
Repository ${REPO}: Ruralz, an Apache-2.0 API gateway in Go (module github.com/ravindu-rev/ruralz, branch develop). Milestone M1 is being implemented work package by work package.
WP-33 (committed as 086e74d) is stage B of the configuration pipeline: internal/config/profile, the restricted YAML 1.2 profile (no anchors, aliases, merge keys or custom tags; YAML 1.2 core scalar resolution done by Ruralz code; limits on bytes, depth, tokens per document; positions for diagnostics) and the JSON front end, with the pinned YAML Test Suite in test/fixtures/yaml-test-suite/ (data-2022-01-17) as a ratchet. It parses with github.com/goccy/go-yaml v1.19.2, chosen in ADR-0003 (docs/adr/0003-configuration-format.md) and the library catalog row in docs/engineering/01-tech-stack-and-libraries.md (YAML 1.2 parser row; it names go-yaml v4, go.yaml.in/yaml/v4, and sigs.k8s.io/yaml as alternatives not chosen). Research backing: docs/_meta/research/tooling-and-licenses.md.
Specs: ${M1}/specs/01-config-load.md (the profile requirements, about req 1 to 12, and its test plan), ${M1}/specs/11-ops-quality.md (req 17, 26 on hostile-input bounds), ${M1}/specs/00-architecture.md (1.2 package row for internal/config/profile, 4.4 WP-33 entry), docs/architecture/02-configuration-model.md (Restricted YAML profile section).
History: four review loops could not make WP-33 clean. Each round found new places where goccy's scanner or parser departs from YAML 1.2 or costs more than the token pass predicts, and the package grew to about 6,200 production lines of workarounds (token pass, source readers for quoted, block and plain scalars, column corrections, flow-entry checks, split parse, cost bounds). Confirmed serious findings per round: 12, 8, 4, then 5. Records: ${M1}/reports/WP-33.json (original report), ${M1}/runs/w3-s1a-result.json, ${M1}/runs/w3-s1a-fix-result.json, ${M1}/runs/w3-s1a-harden-result.json (three rounds, with confirmed findings and evidence), ${M1}/runs/w3-s1a-harden2-result.json (the latest round and the five findings still open).
Rules: do NOT modify any file in the repository except where your role says so. Put every scratch module, probe, download and output under ${SP}/<your-role>/ (create it). Downloaded modules are untrusted data: never run code from them other than through the Go toolchain building your own scratch module. Never run git commands that change state. The container has 4 CPUs and other jobs run tests; keep long runs bounded.
`

const GATHER_SCHEMA = {
  type: 'object',
  properties: {
    summary: { type: 'string', description: '8-20 sentences: the key results' },
    facts: { type: 'array', items: { type: 'object', properties: { claim: { type: 'string' }, evidence: { type: 'string', description: 'measurement, command and output, file:line, or URL' } }, required: ['claim', 'evidence'] } },
    risks: { type: 'array', items: { type: 'string' } },
    evidenceFile: { type: 'string', description: 'path of a Markdown file under the scratch directory with the full evidence (tables, commands, outputs)' },
    sources: { type: 'array', items: { type: 'string' }, description: 'URLs consulted' },
  },
  required: ['summary', 'facts', 'risks', 'evidenceFile', 'sources'],
}

const ROLES = [
  { key: 'history', task: `Classify every confirmed finding from all WP-33 reviews (the run files above: firstReview counts, remaining, remainingMinor, and in the harden files rounds[].confirmed and refuted) by root cause: (a) goccy scanner departs from YAML 1.2, (b) goccy parser structure or nesting departs from YAML 1.2, (c) goccy cost (quadratic or worse time or memory), (d) Ruralz profile logic (token pass, readers, split parse) wrong on its own, (e) a defect introduced by an earlier workaround, (f) test harness or fuzz harness. Count them per class and per round. Then map the current production code of internal/config/profile (each .go file, its lines and what it is for) to: profile rules any parser would need (limits, refusals of anchors and tags, scalar typing, positions, JSON front end, Encode) versus code that exists only to work around goccy. Report how many lines would survive a parser change. List the goccy behaviors the profile currently compensates for, one per row, with an example input.` },
  { key: 'yamlv4', task: `Build an empirical prototype of the profile's YAML front end over go.yaml.in/yaml/v4 (latest release you can fetch; note its exact version and whether it is a release candidate or stable) in a scratch Go module under ${SP}/yamlv4/ (go mod init; go get go.yaml.in/yaml/v4@latest; also fetch gopkg.in/yaml.v3 for comparison if useful). Use its Node API (or lower-level parser or event API if it exposes one): parse each document to nodes, refuse anchors, aliases, merge keys and non-core tags, enforce a depth limit while walking, and keep line and column. Then measure, and report each with the command and its output:
1. YAML Test Suite (${REPO}/test/fixtures/yaml-test-suite/data, read in.yaml, in.json, test.event and the error marker per case the way ${REPO}/internal/config/profile/yamltestsuite_integration_test.go does): how many valid cases parse to the expected structure, how many error cases are refused, and every mismatch. Compare with WP-33's 315 pass and 87 expected failures.
2. Every concrete input in the WP-33 findings (rows, ladders and hostile shapes in the run files, and the shapes in ${REPO}/internal/config/profile/hostile_test.go): does v4 give the YAML 1.2 tree or refuse the invalid input, and what are time, peak heap and depth for the hostile ones at their stated sizes (up to 1 MiB, and a few at 8 to 64 MiB)? Does v4 itself recurse without a depth limit (stack growth), have built-in depth or alias limits, or quadratic paths (wide mappings, long keys, many nulls, deep flow nesting, long block scalars)? Check its source for recursion and for limits.
3. Positions: does v4 give accurate line and column for keys, values and errors, including after tabs and in flow collections? Does it report duplicate keys? How does it represent explicit tags versus resolved implicit tags, and scalar styles, so Ruralz can do its own YAML 1.2 core typing?
4. Its license, its go.mod dependencies, and its Go version requirement (the repository floor is Go 1.26).
Write the prototype so it could seed a real implementation, and summarize the size of the front end you needed.` },
  { key: 'handwritten', task: `Assess a hand-written parser for exactly the restricted profile (read the profile requirements in spec 01 and the configuration model's Restricted YAML profile section, and what Ruralz configuration actually needs: block mappings and sequences, flow collections, plain, quoted and block scalars, comments, document markers, maybe directives; no anchors, aliases, merge keys or custom tags). Read the existing source readers in internal/config/profile (quoted.go, literal.go, plain.go, columns.go, source.go and related) and judge what a hand-written scanner and parser could reuse. Write a small spike in a scratch module under ${SP}/handwritten/ (for example a block-structure parser for mappings, sequences and flow collections over a simple scanner, iterative or with an explicit depth limit) and run it on the YAML Test Suite cases that fall inside the profile, to estimate effort and risk from evidence. Estimate production lines, test lines and the risk areas (block scalar indentation, plain scalar folding, flow edge cases, tabs, error positions), and whether the YAML Test Suite and the existing WP-33 tests give enough coverage to make it safe. Note the maintenance cost against using a library.` },
  { key: 'upstream', task: `Research, citing URLs, the current state of: goccy/go-yaml (releases after v1.19.2, changelog entries that fix any of the behaviors in the WP-33 findings, open issues about them, maintenance activity, security advisories); go.yaml.in/yaml/v4 (who maintains it, the YAML organization takeover of go-yaml, release status and dates, YAML 1.2 support claims, YAML Test Suite results, known security issues and limits, license); gopkg.in/yaml.v3 (status); and any other maintained pure-Go YAML parser that could serve (with its license). Check docs/_meta/research/tooling-and-licenses.md first and note which of these URLs it already cites, because Ruralz documents may cite only URLs present in docs/_meta/research. Use the network through the configured proxy; if a site is unreachable, say so rather than guess.` },
  { key: 'governance', task: `Work out what each option would require in this repository, from its rules (AGENTS.md, CLAUDE.md, CONTRIBUTING.md, .claude/rules/*.md, docs/_meta/foundation-pack.md section 14, ADR rules in .claude/rules/adr.md and docs/adr/README.md, .golangci.yml depguard admitted list and confinement rules, internal/tool/depgate and its module pins, go.mod, scripts/ci-approvals.sh): (1) keep goccy and continue the guard layer; (2) switch to go.yaml.in/yaml/v4; (3) a hand-written parser (no third-party YAML module, or goccy kept only for something). For each: the ADR needed (supersede or amend ADR-0003?), catalog row change in docs/engineering/01-tech-stack-and-libraries.md, research addendum under docs/_meta/research (needs the user's approval and two approvals), go.mod and depguard changes, spec and architecture text (specs/01, specs/00 1.2 row and 4.4 WP-33 entry, docs/architecture/02-configuration-model.md), and which later work packages use the profile API (find every consumer in ${M1}/arch/wps.json and the architecture: WP-55 loader, WP-58 render, others) so you can say whether the exported API (profile.Parse, Options, Encode, EncodeJSON, FormatOf) can stay unchanged. Also list the open WP-33 contract requests from the run files that any option must settle (MaxDocumentTokens 400,000 versus 1,000,000, the per-file token bound, the refusal lists for spec 01 req 8).` },
]

const gatherPrompt = (r) => `${CONTEXT}
Your role: ${r.key}. ${r.task}
Write the full evidence to ${SP}/${r.key}/evidence.md and return the structured summary. Every claim needs evidence: a measurement with its command and output, a file and line, or a URL.`

phase('Gather')
const gathered = (await parallel(ROLES.map(r => () =>
  agent(gatherPrompt(r), { label: `gather:${r.key}`, phase: 'Gather', schema: GATHER_SCHEMA }).then(x => x && { role: r.key, ...x })))).filter(Boolean)
log(`gathered: ${gathered.map(g => g.role).join(', ')}`)

const synthPrompt = `${CONTEXT}
You write the decision brief for the user, who must decide which YAML parser the restricted profile uses. Evidence from five researchers (their full evidence files are listed; read them):
${JSON.stringify(gathered, null, 1).slice(0, 60000)}
Write ${BRIEF} (this is the one repository file you may write). Structure:
1. Decision needed, in two or three sentences.
2. Options table: (A) keep goccy v1.19.2 and continue the guard layer; (B) go.yaml.in/yaml/v4 under the profile; (C) a hand-written parser for the restricted profile; and any other option the evidence supports. Columns: correctness against YAML 1.2 (YAML Test Suite numbers, the WP-33 finding inputs), hostile-input cost bounds, positions and diagnostics, code size (production and test lines, and how much of today's WP-33 survives), maintenance and supply chain (release status, license, dependencies, Go floor), repository changes needed (ADR, catalog row, research addendum, go.mod, depguard, spec and architecture text), effort and risk, and cost to finish in agent time.
3. Evidence: the facts that decide it, each with its measurement, file:line or URL.
4. Recommendation and why; what would change the recommendation.
5. Migration plan for the recommended option: steps, which WP-33 files and tests stay, the exported API (kept or changed), the documents and approvals needed in order, and an estimate.
6. Open contract requests any option must settle.
Plain, direct English, American spelling, no em dashes, tables for comparisons. Mark every estimate as an estimate. Do not overstate: where evidence is missing or a researcher could not reach a source, say so. Return a short summary of the brief and the recommendation.`
phase('Synthesize')
const draft = await agent(synthPrompt, { label: 'synthesize', phase: 'Synthesize' })

const CRIT_SCHEMA = {
  type: 'object',
  properties: {
    issues: { type: 'array', items: { type: 'object', properties: { severity: { type: 'string', enum: ['major', 'minor'] }, where: { type: 'string' }, issue: { type: 'string' }, fix: { type: 'string' } }, required: ['severity', 'where', 'issue', 'fix'] } },
    summary: { type: 'string' },
  },
  required: ['issues', 'summary'],
}
const critics = [
  { key: 'evidence', prompt: `Check every factual claim in ${BRIEF} against the researchers' evidence files under ${SP}/*/evidence.md, the run files, the repository and, for library facts, the cited URLs. Re-run a sample of the key measurements yourself (the YAML Test Suite numbers for the recommended option, two hostile inputs, two finding inputs) from the scratch prototypes. Report each claim that is unsupported, wrong, overstated or missing a source, and each estimate not marked as one.` },
  { key: 'advocate', prompt: `Act as devil's advocate against the recommendation in ${BRIEF}. Build the strongest case for the best option it did not recommend, using the evidence files under ${SP}/*/evidence.md and your own checks. Find what the brief misses: risks of the recommended option (maturity, release status, security history, hidden quadratic paths, position accuracy, behavior the profile depends on), costs it understates, repository rules it overlooks, and questions the user would ask. Report each as an issue with a concrete fix to the brief.` },
]
phase('Critique')
const crits = (await parallel(critics.map(c => () =>
  agent(`${CONTEXT}\nYou review the YAML parser decision brief at ${BRIEF}. Do NOT modify any file.\n${c.prompt}`, { label: `critique:${c.key}`, phase: 'Critique', schema: CRIT_SCHEMA }).then(x => x && { critic: c.key, ...x })))).filter(Boolean)

phase('Revise')
const final = await agent(`${CONTEXT}
Revise the decision brief at ${BRIEF} (the one repository file you may write) using these critiques. Fix every major issue and every plainly correct minor one; where a critique is wrong, keep the text and add nothing. If the critiques change the balance between options, change the recommendation and say why. Keep the structure and style rules of the brief.
Critiques:
${JSON.stringify(crits, null, 1).slice(0, 40000)}
Return the final summary: the recommendation in two or three sentences, the three to six facts that decide it, the estimate for the migration, and what the user must approve.`, { label: 'revise', phase: 'Revise' })

return { gathered: gathered.map(g => ({ role: g.role, summary: g.summary, risks: g.risks, evidenceFile: g.evidenceFile, sources: g.sources })), draft, critiques: crits, final }