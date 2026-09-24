// verify-docs.test.mjs: fixture-based self-test for scripts/verify-docs.mjs.
//
// Run: node scripts/verify-docs.test.mjs   (or `npm test` inside scripts/)
//
// Builds throwaway doc trees under os.tmpdir(), writes a trimmed manifest (global rules copied
// from the real docs/_meta/manifest.yaml, documents replaced by fixtures), runs the verifier as a
// subprocess with --root/--manifest/--json, and asserts on the reported findings. markdownlint is
// skipped because it needs npx and network access. Set KEEP_FIXTURE=1 to keep the temp trees.

import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import yaml from 'js-yaml';
import { githubSlug, parseMarkdownlintOutput, countWords, splitTableRow } from './verify-docs.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const VERIFY = path.join(HERE, 'verify-docs.mjs');
const REAL_MANIFEST = path.join(HERE, '..', 'docs', '_meta', 'manifest.yaml');
const PHASES = ['onRequestHeaders', 'onRequestBody', 'onRoute', 'onUpstreamRequest', 'onUpstreamResponseHeaders', 'onUpstreamResponseBody', 'onResponse', 'onLog', 'onChunk'];
const FENCE = '```';
const tempRoots = [];

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

function write(root, rel, content) {
  const abs = path.join(root, ...rel.split('/'));
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content);
}

function mkRoot(name) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), `verify-docs-${name}-`));
  tempRoots.push(root);
  return root;
}

const DEFAULT_DESIGN = 'Each Route forwards a matched request to one Upstream after the Filter Chain runs.';
const DEFAULT_CATALOG = [
  '| Item | Description |',
  '|---|---|',
  '| Route | Matches requests by host and path. |',
  '| Upstream | Receives the forwarded request. |',
].join('\n');

function mkDoc({ title, adrs = [], milestones = [], design = DEFAULT_DESIGN, catalog = DEFAULT_CATALOG, order = ['Design', 'Catalog'] }) {
  const sections = { Design: design, Catalog: catalog };
  return [
    '---',
    `title: ${title}`,
    'status: draft',
    'owner: ruralz-core',
    'last_updated: 2026-09-23',
    'depends_on: []',
    `adrs: [${adrs.join(', ')}]`,
    `milestone_tags_used: [${milestones.join(', ')}]`,
    '---',
    '',
    `# ${title}`,
    '',
    '## Summary',
    '',
    'This document explains how a Route selects an Upstream. Architects should read it before changing routing.',
    '',
    '## Scope and non-goals',
    '',
    'It covers Route matching only. It does not cover State Store internals.',
    '',
    ...order.flatMap((h) => [`## ${h}`, '', sections[h], '']),
    '## Open questions',
    '',
    'None.',
    '',
  ].join('\n');
}

const GOOD_DESIGN = [
  'The p99 overhead of Route matching is 2 ms (target). The Filter Chain runs',
  `\`${PHASES.slice(0, 8).join(' → ')}\` plus the streaming hook \`onChunk\`.`,
  '',
  'The request phases alone are `onRequestHeaders`, `onRequestBody`, `onRoute` and `onUpstreamRequest`. Durations use Go syntax such as `250ms`.',
  '',
  'Weighted selection is recorded in [ADR-0001](../adr/0001-example-decision.md), and the State Store backend stays pluggable. See the [catalog](#catalog) for the resource summary.',
  '',
  '- Weighted Upstream selection, Planned (M1).',
  '',
  '*Figure 1: a Route forwarding a request to an Upstream.*',
  '',
  `${FENCE}mermaid`,
  'flowchart LR',
  '  C[Client] --> G["Ruralz Gateway (ruralzd)"]',
  '  G --> U[Upstream]',
  FENCE,
  '',
  `${FENCE}yaml`,
  'apiVersion: ruralz/v1alpha1',
  'kind: Route',
  'metadata:',
  '  name: orders',
  FENCE,
  '',
  'Kubernetes manifests are not Ruralz resources and are skipped by the kind check:',
  '',
  `${FENCE}yaml`,
  'apiVersion: apps/v1',
  'kind: Deployment',
  'metadata:',
  '  name: ruralzd',
  FENCE,
].join('\n');

const GLOSSARY = [
  '---',
  'title: Glossary',
  'status: draft',
  'owner: ruralz-core',
  'last_updated: 2026-09-23',
  'depends_on: []',
  'adrs: []',
  'milestone_tags_used: []',
  '---',
  '',
  '# Glossary',
  '',
  '## Summary',
  '',
  'Canonical definitions of Ruralz terms. Every writer and reviewer should read it.',
  '',
  '## Scope and non-goals',
  '',
  'It defines terms only.',
  '',
  '## Terms',
  '',
  '| Term | Definition |',
  '|---|---|',
  '| Filter Chain | The ordered filters that run for each request. |',
  '| Route | Match rules that select one or more Upstreams. |',
  '| Upstream | A target service that receives forwarded traffic. |',
  '',
  '## Forbidden aliases',
  '',
  '| Forbidden | Use instead |',
  '|---|---|',
  '| backend <!-- alias-ok --> | Upstream |',
  '| dashboard <!-- alias-ok --> | Ruralz Console |',
  '',
  '## Open questions',
  '',
  'None.',
  '',
].join('\n');

const ADR = [
  '---',
  'id: ADR-0001',
  'title: Example decision',
  'status: accepted',
  'date: 2026-09-23',
  'deciders: [ruralz-core]',
  'related:',
  '  - docs/architecture/01-good.md',
  '---',
  '',
  '# ADR-0001: Example decision',
  '',
  '## Context and problem statement',
  '',
  'The gateway needs one predictable way to pick an Upstream for each Route.',
  '',
  '## Decision drivers',
  '',
  '- Predictable behavior under load.',
  '',
  '## Considered options',
  '',
  '- Option A: a static weighted table.',
  '- Option B: dynamic discovery for every request.',
  '',
  '## Decision outcome',
  '',
  'Chosen option: "Option A", because it is predictable and simple to test.',
  '',
  '### Consequences',
  '',
  '- Good, because selection is deterministic.',
  '- Bad, because weights change only with a new Revision.',
  '',
  '### Confirmation',
  '',
  'A unit test covers the weighted selection table.',
  '',
  '## Pros and cons of the options',
  '',
  '### Option A',
  '',
  'Simple and deterministic, but static.',
  '',
  '### Option B',
  '',
  'Flexible, but adds a lookup to the request path.',
  '',
  '## More information',
  '',
  'See the owning document.',
  '',
].join('\n');

const ADR_INDEX = [
  '# Architecture Decision Records',
  '',
  '| ID | Title | Status | Date | Owning document |',
  '|---|---|---|---|---|',
  '| [ADR-0001](0001-example-decision.md) | Example decision | accepted | 2026-09-23 | [Good Doc](../architecture/01-good.md) |',
  '',
].join('\n');

function docSpec(id, slug, rel, title, extra = {}) {
  return { id, slug, path: rel, title, outline_h2: ['Design', 'Catalog'], diagrams: [], machine_checks: [], required_terms: [], adrs: [], length_words: null, ...extra };
}

const GOOD_SPEC = docSpec(10, 'good', 'docs/architecture/01-good.md', 'Good Doc', {
  diagrams: [{ type: 'flowchart', subject: 'a Route forwarding to an Upstream' }],
  machine_checks: [
    { kind: 'required_terms', value: ['Filter Chain', 'onChunk'] },
    { kind: 'has_table', section: 'Catalog' },
    { kind: 'min_table_rows', table_heading: 'Catalog', value: 2 },
    { kind: 'no_blank_cells', table_heading: 'Catalog' },
    { kind: 'max_words', value: 2000 },
    { kind: 'links_to', value: ['docs/adr/0001-example-decision.md'] },
  ],
  required_terms: ['Route', 'Upstream'],
  adrs: ['ADR-0001'],
  length_words: { min: 40, max: 3000 },
});

const GLOSSARY_SPEC = {
  ...docSpec(3, 'glossary', 'docs/glossary.md', 'Glossary'),
  outline_h2: ['Terms', 'Forbidden aliases'],
  machine_checks: [
    { kind: 'min_table_rows', table_heading: 'Terms', value: 3 },
    { kind: 'no_blank_cells', table_heading: 'Terms' },
  ],
};

// One failing document per defect: [slug, file, title, mkDoc options, expected check, message fragment].
const FAILING = [
  ['bad-order', 'docs/architecture/02-bad-order.md', 'Bad Order', { order: ['Catalog', 'Design'] }, 'outline', 'out of order'],
  ['untagged', 'docs/architecture/03-untagged.md', 'Untagged Number', { design: 'Route matching adds 5 ms of latency under load.' }, 'numbers', "'5 ms'"],
  ['alias', 'docs/architecture/04-alias.md', 'Forbidden Alias', { design: 'Each Route forwards requests to a backend service.' }, 'glossary', "forbidden alias 'backend'"],
  ['no-caption', 'docs/architecture/05-no-caption.md', 'Missing Caption', { design: `A Route forwards to an Upstream.\n\n${FENCE}mermaid\nflowchart LR\n  R[Route] --> U[Upstream]\n${FENCE}` }, 'mermaid', 'missing caption'],
  ['broken-link', 'docs/architecture/06-broken-link.md', 'Broken Link', { design: 'See [the missing page](does-not-exist.md) and [a bad anchor](01-good.md#no-such-heading).' }, 'links', 'does-not-exist.md'],
  ['placeholder', 'docs/architecture/07-placeholder.md', 'Placeholder', { design: 'The retry budget for each Upstream is TBD.' }, 'placeholders', "'TBD'"],
  ['bad-kind', 'docs/architecture/08-bad-kind.md', 'Bad Kind', { design: `A Route example:\n\n${FENCE}yaml\napiVersion: ruralz/v1alpha1\nkind: Service\nmetadata:\n  name: orders\n${FENCE}` }, 'commands-kinds', "kind 'Service'"],
  ['bad-mermaid', 'docs/architecture/10-bad-mermaid.md', 'Bad Mermaid', { design: `A Route forwards to an Upstream.\n\n*Figure 1: a broken diagram.*\n\n${FENCE}mermaid\nflowchart LR\n  R[Route --> U[Upstream\n${FENCE}` }, 'mermaid', 'mermaid'],
  ['bad-phases', 'docs/architecture/12-bad-phases.md', 'Bad Phases', { design: ['The Filter Chain phases are:', '', ...['onRequestHeaders', 'onRoute', 'onRequestBody', 'onUpstreamRequest', 'onUpstreamResponseHeaders', 'onUpstreamResponseBody', 'onResponse', 'onLog', 'onChunk'].map((p, k) => `${k + 1}. \`${p}\``)].join('\n') }, 'phases', 'out of order'],
  ['big-diagram', 'docs/architecture/11-big-diagram.md', 'Big Diagram', {
    design: `A Route chain.\n\n*Figure 1: twenty-six nodes in a row.*\n\n${FENCE}mermaid\nflowchart LR\n${Array.from({ length: 25 }, (_, k) => `  N${k + 1} --> N${k + 2}`).join('\n')}\n${FENCE}`,
  }, 'mermaid', '26 nodes'],
];
const MISSING_REL = 'docs/architecture/09-missing.md';

function buildManifest(docs) {
  const real = yaml.load(fs.readFileSync(REAL_MANIFEST, 'utf8'));
  const globals = {};
  for (const k of ['mandatory_h2', 'milestones', 'forbidden_aliases', 'alias_match', 'filter_chain_phases', 'word_count_rule', 'machine_check_kinds', 'adr_common']) globals[k] = real[k];
  return {
    version: 1,
    ...globals,
    scalability_items: ['stateless data plane'],
    glossary_terms: ['Route', 'Upstream', 'Filter Chain'],
    adr_common: { ...real.adr_common, length_words: { min: 20, max: 1200 } },
    adr_index: { path: 'docs/adr/README.md', table_columns: ['ID', 'Title', 'Status', 'Date', 'Owning document'] },
    docs,
    adrs: [{ id: 'ADR-0001', path: 'docs/adr/0001-example-decision.md', title: 'Example decision', status: 'accepted', owning_doc: 'docs/architecture/01-good.md' }],
  };
}

function buildFixture(name, { withFailing }) {
  const root = mkRoot(name);
  const docs = [GLOSSARY_SPEC, GOOD_SPEC];
  write(root, 'docs/glossary.md', GLOSSARY);
  write(root, GOOD_SPEC.path, mkDoc({ title: 'Good Doc', adrs: ['ADR-0001'], milestones: ['M1'], design: GOOD_DESIGN }));
  write(root, 'docs/adr/0001-example-decision.md', ADR);
  write(root, 'docs/adr/README.md', ADR_INDEX);
  if (withFailing) {
    FAILING.forEach(([slug, rel, title, opts], k) => {
      const diagrams = /mermaid/.test(opts.design || '') ? [{ type: 'flowchart', subject: 'fixture' }] : [];
      docs.push(docSpec(20 + k, slug, rel, title, { diagrams }));
      write(root, rel, mkDoc({ title, ...opts }));
    });
    docs.push(docSpec(90, 'missing', MISSING_REL, 'Missing'));
  }
  write(root, 'manifest.yaml', yaml.dump(buildManifest(docs)));
  return root;
}

function run(root, extra = [], { json = true } = {}) {
  const args = [VERIFY, '--root', root, '--manifest', path.join(root, 'manifest.yaml'), '--skip', 'markdownlint', ...extra];
  if (json) args.push('--json');
  const r = spawnSync(process.execPath, args, { encoding: 'utf8', timeout: 180000 });
  if (r.error) throw r.error;
  const out = { status: r.status, stdout: r.stdout, stderr: r.stderr };
  if (json) {
    try { out.report = JSON.parse(r.stdout); } catch { throw new Error(`verifier did not print JSON (exit ${r.status}):\n${r.stdout}\n${r.stderr}`); }
  }
  return out;
}

const errorsOf = (report, file) => report.findings.filter((f) => f.severity === 'error' && (!file || f.file === file));
const show = (fs_) => fs_.map((f) => `${f.check} ${f.file}:${f.line} ${f.message}`).join('\n');

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test('helpers: GitHub slugs, markdownlint output, word counts, table cells', () => {
  assert.equal(githubSlug('CI/CD, GitOps and development tools'), 'cicd-gitops-and-development-tools');
  assert.equal(githubSlug('Filter Chain execution'), 'filter-chain-execution');
  const parsed = parseMarkdownlintOutput('docs/x.md:12:3 error MD009/no-trailing-spaces Trailing spaces [Expected: 0 or 2; Actual: 1]\ndocs/y.md:4 MD041/first-line-heading First line');
  assert.deepEqual(parsed.map((p) => [p.file, p.line, p.rule]), [['docs/x.md', 12, 'MD009/no-trailing-spaces'], ['docs/y.md', 4, 'MD041/first-line-heading']]);
  assert.equal(countWords('| Route | Matches requests |\n|---|---|'), 3);
  assert.deepEqual(splitTableRow('| a | b \\| c | <!-- alias-ok --> |'), ['a', 'b | c', '']);
});

test('fixture with failing documents: each defect is reported and the good documents pass', () => {
  const root = buildFixture('full', { withFailing: true });
  const { status, report } = run(root);
  assert.equal(status, 1, 'exit code must be 1 when errors exist');

  // Every failing document is flagged by exactly its expected check, with the expected message.
  for (const [, rel, , , check, fragment] of FAILING) {
    const errs = errorsOf(report, rel);
    assert.ok(errs.some((f) => f.check === check && f.message.includes(fragment)), `${rel}: expected a ${check} error containing "${fragment}", got:\n${show(errs) || '(none)'}`);
    const others = errs.filter((f) => f.check !== check);
    assert.deepEqual(others, [], `${rel}: unexpected errors from other checks:\n${show(others)}`);
  }
  // Broken anchors are reported too.
  assert.ok(errorsOf(report, 'docs/architecture/06-broken-link.md').some((f) => f.message.includes('#no-such-heading')), 'broken anchor not reported');
  // The missing document is reported once and only by missing-doc.
  const missing = report.findings.filter((f) => f.file === MISSING_REL);
  assert.deepEqual(missing.map((f) => [f.check, f.severity]), [['missing-doc', 'error']], `missing doc findings:\n${show(missing)}`);

  // Good documents: no errors at all.
  for (const rel of [GOOD_SPEC.path, 'docs/glossary.md', 'docs/adr/0001-example-decision.md', 'docs/adr/README.md']) {
    assert.deepEqual(errorsOf(report, rel), [], `${rel} should pass, got:\n${show(errorsOf(report, rel))}`);
  }
  // No internal errors anywhere.
  assert.deepEqual(report.findings.filter((f) => f.message.startsWith('internal error')), []);
});

test('clean fixture: every check passes and the exit code is 0', () => {
  const root = buildFixture('clean', { withFailing: false });
  const { status, report } = run(root);
  assert.deepEqual(errorsOf(report), [], `unexpected errors:\n${show(errorsOf(report))}`);
  assert.equal(status, 0);
  for (const s of report.summary) assert.ok(s.check === 'markdownlint' ? s.status === 'SKIP' : s.status === 'PASS', `${s.check} is ${s.status}`);
});

test('--only runs a single check and text output uses PASS/FAIL lines with a summary table', () => {
  const root = buildFixture('only', { withFailing: true });
  const { status, report } = run(root, ['--only', 'placeholders']);
  assert.equal(status, 1);
  assert.deepEqual(report.summary.filter((s) => s.status !== 'SKIP').map((s) => s.check), ['placeholders']);

  const text = run(root, ['--only', 'outline,citations'], { json: false });
  assert.equal(text.status, 1);
  assert.match(text.stdout, /^FAIL outline docs\/architecture\/02-bad-order\.md:\d+ H2 out of order/m);
  assert.match(text.stdout, /^PASS citations$/m);
  assert.match(text.stdout, /^Summary$/m);
  assert.match(text.stdout, /^outline\s+FAIL\s+\d+\s+\d+$/m);
});

test('--allow-missing downgrades missing documents to warnings', () => {
  const root = buildFixture('allow', { withFailing: true });
  const { status, report } = run(root, ['--only', 'missing-doc', '--allow-missing']);
  assert.equal(status, 0);
  assert.deepEqual(report.findings.map((f) => [f.file, f.severity]), [[MISSING_REL, 'warn']]);
});

test.after(() => {
  if (process.env.KEEP_FIXTURE) { console.log(`fixtures kept: ${tempRoots.join(', ')}`); return; }
  for (const r of tempRoots) fs.rmSync(r, { recursive: true, force: true });
});
