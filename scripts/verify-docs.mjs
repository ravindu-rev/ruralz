#!/usr/bin/env node
// verify-docs.mjs: documentation verification gate for the Ruralz repository.
//
// Usage (from anywhere; paths are resolved against --root):
//   node scripts/verify-docs.mjs [--only <check>[,<check>...]] [--skip <check>[,...]]
//                                [--json] [--fix-none] [--allow-missing]
//                                [--root <dir>] [--manifest <path>]
//
//   --only           run only the named checks (repeatable or comma-separated)
//   --skip           run every check except the named ones (repeatable or comma-separated)
//   --json           print a machine-readable JSON report instead of text
//   --fix-none       accepted for CI explicitness; the verifier never modifies files and never
//                    passes --fix to markdownlint-cli2
//   --allow-missing  report manifest documents that do not exist yet (and links, depends_on or
//                    related entries pointing at them) as warnings instead of errors
//   --root           repository root (default: the parent directory of this script)
//   --manifest       manifest path (default: <root>/docs/_meta/manifest.yaml)
//
// Exit codes: 0 = no error-severity findings, 1 = at least one error, 2 = usage or manifest error.
//
// Checks, in output order: missing-doc, markdownlint, links, mermaid, frontmatter, outline,
// machine-checks, adr, parity, glossary, scalability, numbers, milestones, placeholders,
// commands-kinds, citations, index, phases. Each check is one generator function below that
// yields findings {check, file, line, message, severity: 'error' | 'warn'}.
//
// Scope: content checks scan README.md plus every .md under docs/ except docs/_meta/**. The
// _meta folder holds the binding inputs (manifest, foundation pack, style guide) and research,
// drafts and review notes, which legitimately contain placeholders, forbidden aliases and bare
// URLs. _meta files remain valid link targets, and docs/_meta/research/ is the citation source.
// Missing manifest documents are reported once by `missing-doc` and skipped by content checks.
// Aggregate checks that need the whole doc set (glossary term usage, ADR back-references) are
// downgraded to warnings while any manifest document is still missing.
//
// Known limitations and heuristics (deliberate; reviewers spot-check what these cannot):
// - mermaid: blocks are parsed with the official `mermaid` package (optionalDependency in
//   scripts/package.json) through mermaid.parse(), with DOMPurify replaced by no-ops because
//   parse() never renders. `@mermaid-js/parser` is not used: it only covers the Langium-based
//   diagram types (pie, gitGraph, packet, ...), not flowchart or sequenceDiagram. If `mermaid`
//   cannot be loaded, a fallback syntax sanity check runs (balanced quotes and brackets, block
//   `end` balance) and every file with diagrams gets a `mermaid-parse-unverified` warning.
//   Node counts are heuristics: unique node ids (flowchart), participants (sequenceDiagram),
//   states (stateDiagram-v2) and classes (classDiagram); other diagram types are not counted.
// - Markdown parsing is line-based: ATX headings, GFM pipe tables, fenced code blocks, inline
//   links. Links, table rows or phase lists that wrap across lines are not recognized.
// - Forbidden aliases are matched in prose, inline code and mermaid blocks, not in other fenced
//   code: an HTML escape comment cannot be placed inside a code block (for example a KrakenD
//   JSON sample with "backend" keys) without changing the code.
// - Links from docs/README.md are counted for "linked exactly once" outside its `Reading paths`
//   section, because the manifest also requires the reading paths to link documents.
// - `kind:` values are checked only at resource level in yaml blocks whose apiVersion is absent
//   or starts with `ruralz` (Kubernetes manifests such as Deployments are skipped). The kind
//   list is manifest `kinds` when present, else the ten kinds of the foundation pack.
// - Citation URLs are accepted when they appear in docs/_meta/research/** or in the foundation
//   pack (the project's own URLs); localhost and example domains are ignored.
// - numbers ignores inline code spans (literal syntax such as `250ms` or `timeout: 3s`) in
//   addition to fenced code, table header rows and front matter.
// - phases treats a line with a run of 4+ phase names, or a list/table whose items start with a
//   phase, as a listing: names must be manifest phases in manifest order, flat listings must be
//   a contiguous slice (no skipped phase), and a full listing needs onChunk nearby. Tables that
//   group rows by phase (worked examples) only get the order check.
// - numbers, milestones, phases and glossary definition detection are regex heuristics; the
//   roadmap coverage part of `milestones` only warns because wording differs between docs.

import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import yaml from 'js-yaml';

const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const DOC_STATUS = ['draft', 'reviewed', 'approved', 'approved-with-escalations'];
const ADR_STATUS_RE = /^(proposed|accepted|deprecated|superseded-by ADR-\d{4})$/;
const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
const ADR_ID_RE = /^ADR-\d{4}$/;
const ADR_MENTION_RE = /\bADR-\d{4}\b/g;
const ADR_FILE_RE = /^docs\/adr\/(\d{4})-[^/]+\.md$/;
const ADR_INDEX_DEFAULT = 'docs/adr/README.md';
const ADR_FRONT_MATTER_DEFAULT = ['id', 'title', 'status', 'date', 'deciders', 'related'];
const DOC_FRONT_MATTER = ['title', 'status', 'owner', 'last_updated', 'depends_on', 'adrs', 'milestone_tags_used'];
const ALLOWED_DIAGRAMS = ['flowchart', 'sequenceDiagram', 'stateDiagram-v2', 'classDiagram', 'erDiagram', 'quadrantChart', 'gantt', 'mindmap'];
const MAX_DIAGRAM_NODES = 25;
const DEFAULT_KINDS = ['Gateway', 'Route', 'Upstream', 'Policy', 'Plugin', 'Consumer', 'AIProvider', 'AIModel', 'Environment', 'Cluster'];
const ADMIN_PATHS = ['/healthz', '/readyz', '/metrics', '/debug/', '/config/dump', '/tap'];
const ADMIN_PORT = '9901';
const NUMBER_DIRS = ['docs/architecture/', 'docs/operations/', 'docs/engineering/', 'docs/roadmap/', 'docs/reference/'];
const LINK_ONCE_EXEMPT_SECTIONS = ['Reading paths'];
const OPEN_QUESTION_COLUMNS = ['ID', 'Question', 'Options', 'Owner', 'Blocking?'];
const SUMMARY_MAX_WORDS = 120;
const DEFAULT_PARITY_STATUS = '^(Planned \\(M[0-5]\\)|Not planned|N/A)$';
const SHELL_LANGS = new Set(['bash', 'sh', 'shell', 'console', 'zsh', 'powershell', 'pwsh']);
const LOCAL_HOST_RE = /^(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\]|([\w-]+\.)*example\.(com|org|net)|[\w.-]+\.(example|local|internal|test))$/i;

// A number followed by a performance or scale unit (numbers check).
const NUM_UNIT_RE = /(?<![\p{L}\p{N}_.])\d+(?:[.,]\d+)*(?:\s?[kKM](?=\s?(?:rps|RPS|req\/s|qps|QPS|connections|conns)))?\s?(?:ms|µs|μs|us|s|rps|RPS|req\/s|qps|QPS|[KMGT]i?B|kB|%|connections|conns|cores|vCPUs?)(?![\p{L}\p{N}_-])/u;

const STOPWORDS = new Set(['the', 'a', 'an', 'and', 'or', 'of', 'for', 'to', 'in', 'on', 'with', 'via', 'by', 'at', 'as',
  'is', 'are', 'be', 'from', 'into', 'per', 'its', 'it', 'this', 'that', 'planned', 'not', 'using', 'use']);

// Special documents, located by manifest slug with a fallback path.
const SPECIAL = {
  glossary: ['glossary', 'docs/glossary.md'],
  parity: ['krakend-ee-parity-matrix', 'docs/comparison/01-krakend-ee-parity-matrix.md'],
  scalability: ['scalability-and-distributed-state', 'docs/architecture/11-scalability-and-distributed-state.md'],
  roadmap: ['roadmap-and-milestones', 'docs/roadmap/01-roadmap-and-milestones.md'],
  cli: ['cli-and-api-surface', 'docs/reference/01-cli-and-api-surface.md'],
  dataPlane: ['data-plane', 'docs/architecture/03-data-plane.md'],
  techStack: ['tech-stack-and-libraries', 'docs/engineering/01-tech-stack-and-libraries.md'],
  docsIndex: ['docs-index', 'docs/README.md'],
};

class UsageError extends Error {}

const E = (file, line, message) => ({ file, line, message, severity: 'error' });
const W = (file, line, message) => ({ file, line, message, severity: 'warn' });

// ---------------------------------------------------------------------------
// Generic string helpers
// ---------------------------------------------------------------------------

function toPosix(p) { return String(p).replace(/\\/g, '/'); }

function normRel(p) {
  const s = path.posix.normalize(toPosix(p).trim().replace(/^\.\//, ''));
  return s.replace(/\/+$/, '');
}

function escapeRegExp(s) { return String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }

function safeDecode(s) { try { return decodeURIComponent(s); } catch { return s; } }

function arraysEqual(a, b) { return a.length === b.length && a.every((x, i) => x === b[i]); }

function lastLine(s) {
  const lines = String(s || '').trim().split(/\r?\n/).filter(Boolean);
  return lines.length ? lines[lines.length - 1].slice(0, 300) : '(no output)';
}

function stripMarkdown(s) {
  return String(s ?? '')
    .replace(/<!--[\s\S]*?-->/g, ' ')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\[[^\]]*\]/g, '$1')
    .replace(/(`+)([\s\S]*?[^`])\1(?!`)/g, '$2')
    .replace(/<\/?[A-Za-z][^>]*>/g, ' ')
    .replace(/(\*\*|__)(.+?)\1/g, '$2')
    .replace(/(?<![\w*])\*(?!\s)(.+?)(?<!\s)\*(?![\w*])/g, '$1')
    .replace(/(?<!\w)_(?!\s)(.+?)(?<!\s)_(?!\w)/g, '$1')
    .replace(/~~(.+?)~~/g, '$1');
}

function normalizeText(s) { return stripMarkdown(s).toLowerCase().replace(/\s+/g, ' ').trim(); }

// Lowercase, alphanumerics only, single spaces: used for fuzzy comparisons.
function normLoose(s) { return normalizeText(s).replace(/[^\p{L}\p{N}]+/gu, ' ').trim(); }

function keyTokens(s) {
  return normLoose(String(s).replace(/Planned \(M\d+\)/g, ' '))
    .split(' ').filter((t) => t.length > 1 && !STOPWORDS.has(t));
}

function countWords(text) {
  let n = 0;
  for (const tok of String(text).split(/\s+/)) if (/[\p{L}\p{N}]/u.test(tok)) n++;
  return n;
}

function maskInlineCode(line) {
  return line.replace(/(`+)([\s\S]*?[^`])\1(?!`)/g, (m) => ' '.repeat(m.length));
}

function maskHtmlComments(line) {
  return line.replace(/<!--[\s\S]*?-->/g, (m) => ' '.repeat(m.length));
}

function inlineCodeSpans(line) {
  const out = [];
  for (const m of line.matchAll(/(`+)([\s\S]*?[^`])\1(?!`)/g)) out.push(m[2].trim());
  return out;
}

function cleanUrl(u) {
  let s = u;
  for (;;) {
    const last = s[s.length - 1];
    if ('.,;:!?*_\'"'.includes(last)) { s = s.slice(0, -1); continue; }
    if (last === ']' || last === '>') { s = s.slice(0, -1); continue; }
    if (last === ')' && s.split('(').length < s.split(')').length) { s = s.slice(0, -1); continue; }
    break;
  }
  return s;
}

function extractUrls(text) {
  const out = [];
  for (const m of String(text).matchAll(/https?:\/\/[^\s<>"'`]+/g)) out.push(cleanUrl(m[0]));
  return out;
}

function normalizeUrl(u) {
  try {
    const url = new URL(u);
    url.hash = '';
    return url.href.replace(/\/+$/, '');
  } catch {
    return u.replace(/#.*$/, '').replace(/\/+$/, '');
  }
}

function urlHost(u) { try { return new URL(u).hostname; } catch { return ''; } }

// GitHub-style heading slug (github-slugger compatible for ordinary headings).
function githubSlug(text) {
  return String(text).toLowerCase().replace(/[^\p{L}\p{M}\p{N}\p{Pc}\- ]/gu, '').replace(/ /g, '-');
}

// ---------------------------------------------------------------------------
// Markdown helpers: front matter, line classification, headings, tables, links
// ---------------------------------------------------------------------------

function parseFrontMatter(lines) {
  if (!lines.length || lines[0].trimEnd() !== '---') return { present: false, data: null, end: -1 };
  for (let i = 1; i < lines.length; i++) {
    const l = lines[i].trimEnd();
    if (l !== '---' && l !== '...') continue;
    const raw = lines.slice(1, i).join('\n');
    try {
      // CORE_SCHEMA keeps dates such as 2026-09-23 as strings.
      const data = yaml.load(raw, { schema: yaml.CORE_SCHEMA });
      if (data != null && (typeof data !== 'object' || Array.isArray(data))) {
        return { present: true, data: null, error: 'front matter is not a YAML mapping', end: i };
      }
      return { present: true, data: data ?? {}, end: i };
    } catch (e) {
      return { present: true, data: null, error: `invalid front matter YAML: ${String(e.message).split('\n')[0]}`, end: i };
    }
  }
  return { present: true, data: null, error: 'unterminated front matter (no closing ---)', end: -1 };
}

// Marks each line as front matter, fenced code (content or delimiter), or prose.
function classifyLines(lines, fmEnd) {
  const info = new Array(lines.length);
  const fences = [];
  let open = null;
  for (let i = 0; i < lines.length; i++) {
    if (i <= fmEnd) { info[i] = { fm: true, code: false, delim: false, fence: null }; continue; }
    const m = /^(\s*)(`{3,}|~{3,})(.*)$/.exec(lines[i]);
    if (!open) {
      if (m && !(m[2][0] === '`' && m[3].includes('`'))) {
        const lang = (m[3].trim().split(/\s+/)[0] || '').replace(/^\{\.?|\}$/g, '');
        open = { start: i, end: lines.length, char: m[2][0], len: m[2].length, lang: lang.toLowerCase() };
        fences.push(open);
        info[i] = { fm: false, code: true, delim: true, fence: open };
      } else {
        info[i] = { fm: false, code: false, delim: false, fence: null };
      }
    } else {
      info[i] = { fm: false, code: true, delim: false, fence: open };
      if (m && m[2][0] === open.char && m[2].length >= open.len && m[3].trim() === '') {
        info[i].delim = true;
        open.end = i;
        open = null;
      }
    }
  }
  return { info, fences };
}

function extractHeadings(lines, info) {
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    if (info[i].fm || info[i].code) continue;
    const m = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/.exec(lines[i]);
    if (!m) continue;
    const text = (m[2] || '').trim();
    out.push({ level: m[1].length, text, plain: stripMarkdown(text).replace(/\s+/g, ' ').trim(), line: i });
  }
  return out;
}

const TABLE_SEP_RE = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;

// Splits a GFM table row into trimmed cells; `\|` is a literal pipe; HTML comments are dropped.
function splitTableRow(line) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const cells = [];
  let cur = '';
  for (let k = 0; k < s.length; k++) {
    if (s[k] === '\\' && s[k + 1] === '|') { cur += '|'; k++; continue; }
    if (s[k] === '|') { cells.push(cur); cur = ''; continue; }
    cur += s[k];
  }
  cells.push(cur);
  return cells.map((c) => c.replace(/<!--[\s\S]*?-->/g, '').trim());
}

function parseTables(lines, info) {
  const tables = [];
  for (let i = 0; i < lines.length - 1; i++) {
    if (info[i].fm || info[i].code || info[i + 1].code) continue;
    const sep = lines[i + 1];
    if (!lines[i].includes('|') || !sep.includes('|') || !sep.includes('-') || !TABLE_SEP_RE.test(sep)) continue;
    const header = splitTableRow(lines[i]);
    const table = { headerLine: i, sepLine: i + 1, header, rows: [] };
    let j = i + 2;
    for (; j < lines.length; j++) {
      if (info[j].code || info[j].fm) break;
      const r = lines[j];
      if (!r.trim() || !r.includes('|')) break;
      const raw = splitTableRow(r);
      // GFM pads short rows with empty cells and drops extra cells.
      table.rows.push({ line: j, cells: header.map((_, k) => raw[k] ?? ''), raw });
    }
    tables.push(table);
    i = j - 1;
  }
  return tables;
}

function colIndex(table, name) {
  const want = normalizeText(name);
  return table.header.findIndex((h) => normalizeText(h) === want);
}

const INLINE_LINK_RE = /(!?)\[((?:\\.|[^[\]\\]|\[(?:\\.|[^[\]\\])*\])*)\]\(\s*(<[^<>\n]*>|(?:\\.|[^\s()\\]|\((?:\\.|[^\s()\\])*\))*)(?:\s+(?:"[^"]*"|'[^']*'|\([^()]*\)))?\s*\)/g;
const REF_DEF_RE = /^ {0,3}\[([^\]]+)\]:\s*(<[^>]*>|\S+)/;
const LIST_ITEM_RE = /^\s*(?:[-*+]|\d{1,9}[.)])\s+/;

// Resolves a Markdown link target relative to the linking file (repo-relative, posix).
function resolveTarget(fromRel, target) {
  let t = String(target).trim();
  if (t.startsWith('<') && t.endsWith('>')) t = t.slice(1, -1);
  if (/^[a-z][a-z0-9+.-]*:/i.test(t) || t.startsWith('//')) return { external: true };
  const hash = t.indexOf('#');
  let p = hash >= 0 ? t.slice(0, hash) : t;
  const frag = hash >= 0 ? t.slice(hash + 1) : null;
  const q = p.indexOf('?');
  if (q >= 0) p = p.slice(0, q);
  p = safeDecode(p);
  if (p === '') return { rel: fromRel, frag, sameFile: true };
  const base = p.startsWith('/') ? '' : path.posix.dirname(fromRel);
  const rel = normRel(path.posix.join(base, p.replace(/^\/+/, '')));
  if (rel === '..' || rel.startsWith('../')) return { rel, frag, outside: true };
  return { rel: rel === '' ? '.' : rel, frag };
}

class Doc {
  constructor(rel, text) {
    this.rel = rel;
    this.text = text.replace(/^﻿/, '');
    this.lines = this.text.split(/\r?\n/);
    if (this.lines.length > 1 && this.lines[this.lines.length - 1] === '') this.lines.pop();
    this.fm = parseFrontMatter(this.lines);
    const { info, fences } = classifyLines(this.lines, this.fm.end);
    this.info = info;
    this.fences = fences;
    this.headings = extractHeadings(this.lines, info);
    this._memo = {};
  }

  memo(key, fn) { if (!(key in this._memo)) this._memo[key] = fn(); return this._memo[key]; }

  get bodyStart() { return this.fm.end + 1; }

  isProse(i) { return !this.info[i].fm && !this.info[i].code; }

  get body() { return this.memo('body', () => this.lines.slice(this.bodyStart).join('\n')); }

  get tables() { return this.memo('tables', () => parseTables(this.lines, this.info)); }

  get h1s() { return this.headings.filter((h) => h.level === 1); }

  get anchors() {
    return this.memo('anchors', () => {
      const occurrences = new Map();
      const set = new Set();
      for (const h of this.headings) {
        const original = githubSlug(h.plain);
        let slug = original;
        while (occurrences.has(slug)) {
          occurrences.set(original, occurrences.get(original) + 1);
          slug = `${original}-${occurrences.get(original)}`;
        }
        occurrences.set(slug, 0);
        set.add(slug);
      }
      for (let i = 0; i < this.lines.length; i++) {
        if (!this.isProse(i)) continue;
        for (const m of this.lines[i].matchAll(/<a\s+(?:[^>]*?\s)?(?:id|name)\s*=\s*["']([^"']+)["']/gi)) set.add(m[1].toLowerCase());
      }
      return set;
    });
  }

  get links() {
    return this.memo('links', () => {
      const out = [];
      for (let i = 0; i < this.lines.length; i++) {
        if (!this.isProse(i)) continue;
        const orig = this.lines[i];
        const line = maskHtmlComments(maskInlineCode(orig));
        const def = REF_DEF_RE.exec(line);
        if (def) { out.push({ line: i, text: def[1], target: def[2], ref: true }); continue; }
        for (const m of line.matchAll(INLINE_LINK_RE)) {
          const textStart = m.index + m[1].length + 1;
          out.push({ line: i, image: m[1] === '!', text: orig.substr(textStart, m[2].length), target: m[3] });
        }
      }
      for (const l of out) l.resolved = resolveTarget(this.rel, l.target);
      return out;
    });
  }

  // Set of repo-relative paths this document links to (fragments ignored).
  get linkTargets() {
    return this.memo('linkTargets', () => new Set(this.links
      .filter((l) => !l.resolved.external && !l.resolved.sameFile && !l.resolved.outside)
      .map((l) => l.resolved.rel)));
  }

  linksTo(rel) { return this.linkTargets.has(rel); }

  // Range of the section under the heading `name` (case-insensitive) up to the next heading of
  // the same or a higher level: {start: heading line, end: exclusive line, heading}.
  section(name, level = 2) {
    const want = normalizeText(name);
    const idx = this.headings.findIndex((h) => h.level === level && normalizeText(h.text) === want);
    if (idx < 0) return null;
    const h = this.headings[idx];
    let end = this.lines.length;
    for (let k = idx + 1; k < this.headings.length; k++) {
      if (this.headings[k].level <= level) { end = this.headings[k].line; break; }
    }
    return { start: h.line, end, heading: h };
  }

  tablesIn(range) { return this.tables.filter((t) => t.headerLine > range.start && t.headerLine < range.end); }

  headingsIn(range, level) {
    return this.headings.filter((h) => h.line > range.start && h.line < range.end && (level == null || h.level === level));
  }

  // Word count per the manifest word_count_rule: body after front matter, excluding fenced code
  // (mermaid included) and HTML comments; table cell text counts.
  wordCount(range) {
    const start = range ? range.start + 1 : this.bodyStart;
    const end = range ? range.end : this.lines.length;
    const kept = [];
    for (let i = start; i < end; i++) if (this.isProse(i)) kept.push(this.lines[i]);
    return countWords(kept.join('\n').replace(/<!--[\s\S]*?-->/g, ' '));
  }

  proseText(range) {
    const kept = [];
    for (let i = range.start + 1; i < range.end; i++) if (this.isProse(i)) kept.push(this.lines[i]);
    return kept.join('\n');
  }

  get tableRowLines() {
    return this.memo('tableRowLines', () => {
      const m = new Map();
      for (const t of this.tables) for (const r of t.rows) m.set(r.line, { table: t, row: r });
      return m;
    });
  }
}

// ---------------------------------------------------------------------------
// Context: manifest, file discovery, document cache
// ---------------------------------------------------------------------------

const dirCache = new Map();

function listDir(abs) {
  if (!dirCache.has(abs)) {
    try { dirCache.set(abs, new Set(fs.readdirSync(abs))); } catch { dirCache.set(abs, null); }
  }
  return dirCache.get(abs);
}

// Case-exact existence check (GitHub is case-sensitive even when the local filesystem is not).
function existsExact(root, rel) {
  if (!rel || rel === '.') return true;
  let cur = root;
  for (const part of rel.split('/')) {
    if (!part) continue;
    const names = listDir(cur);
    if (!names || !names.has(part)) return false;
    cur = path.join(cur, part);
  }
  return true;
}

function listScanned(root) {
  const out = [];
  if (existsExact(root, 'README.md')) out.push('README.md');
  const walk = (relDir) => {
    let entries;
    try { entries = fs.readdirSync(path.join(root, relDir), { withFileTypes: true }); } catch { return; }
    for (const ent of entries) {
      const rel = `${relDir}/${ent.name}`;
      if (ent.isDirectory()) {
        if (rel === 'docs/_meta' || ent.name === 'node_modules' || ent.name.startsWith('.')) continue;
        walk(rel);
      } else if (ent.isFile() && ent.name.toLowerCase().endsWith('.md')) {
        out.push(rel);
      }
    }
  };
  walk('docs');
  return out.sort();
}

function loadContext(opts) {
  const root = path.resolve(opts.root);
  const manifestPath = opts.manifest ? path.resolve(opts.manifest) : path.join(root, 'docs', '_meta', 'manifest.yaml');
  let manifest;
  try {
    manifest = yaml.load(fs.readFileSync(manifestPath, 'utf8'));
  } catch (e) {
    throw new UsageError(`cannot read manifest ${manifestPath}: ${String(e.message).split('\n')[0]}`);
  }
  if (!manifest || typeof manifest !== 'object') throw new UsageError(`manifest ${manifestPath} is empty or not a mapping`);

  const cache = new Map();
  const ctx = { root, opts, manifest, manifestPath };
  ctx.exists = (rel) => existsExact(root, normRel(rel));
  ctx.getDoc = (relIn) => {
    const rel = normRel(relIn);
    if (cache.has(rel)) return cache.get(rel);
    let doc = null;
    if (existsExact(root, rel)) {
      try {
        const abs = path.join(root, rel);
        if (fs.statSync(abs).isFile()) doc = new Doc(rel, fs.readFileSync(abs, 'utf8'));
      } catch { doc = null; }
    }
    cache.set(rel, doc);
    return doc;
  };

  ctx.scanned = listScanned(root).map((rel) => ctx.getDoc(rel)).filter(Boolean);
  ctx.manifestDocs = (manifest.docs || []).map((spec) => ({ spec, rel: normRel(spec.path) }));
  for (const e of ctx.manifestDocs) e.doc = ctx.getDoc(e.rel);
  ctx.specByRel = new Map(ctx.manifestDocs.map((e) => [e.rel, e.spec]));
  ctx.adrSpecs = (manifest.adrs || []).map((a) => ({ ...a, path: normRel(a.path) }));
  ctx.adrById = new Map(ctx.adrSpecs.map((a) => [a.id, a]));
  ctx.adrIndexRel = normRel(manifest.adr_index?.path ?? ADR_INDEX_DEFAULT);
  ctx.milestones = (manifest.milestones || []).map(String);
  ctx.kinds = Array.isArray(manifest.kinds) && manifest.kinds.length ? manifest.kinds.map(String) : DEFAULT_KINDS;

  ctx.missing = [];
  for (const e of ctx.manifestDocs) if (!e.doc) ctx.missing.push({ rel: e.rel, what: `document #${e.spec.id} (${e.spec.slug})` });
  for (const a of ctx.adrSpecs) if (!ctx.getDoc(a.path)) ctx.missing.push({ rel: a.path, what: a.id });
  if ((ctx.adrSpecs.length || manifest.adr_index) && !ctx.getDoc(ctx.adrIndexRel)) {
    ctx.missing.push({ rel: ctx.adrIndexRel, what: 'ADR index (adr_index.path)' });
  }
  ctx.plannedMissing = new Set(ctx.missing.map((m) => m.rel));
  ctx.missingSev = opts.allowMissing ? W : E;
  ctx.aggregateSev = ctx.missing.length ? W : E;
  ctx.isAdr = (rel) => ADR_FILE_RE.test(rel);
  ctx.special = (key) => {
    const [slug, fallback] = SPECIAL[key];
    const e = ctx.manifestDocs.find((d) => d.spec.slug === slug);
    return e ? e.rel : fallback;
  };
  // Severity for a reference to a path that does not exist: planned-but-unwritten manifest files
  // follow missing-doc severity; anything else is an error.
  ctx.refSev = (rel) => (ctx.plannedMissing.has(rel) ? ctx.missingSev : E);
  return ctx;
}

// ---------------------------------------------------------------------------
// Check: missing-doc
// ---------------------------------------------------------------------------

function* checkMissingDocs(ctx) {
  for (const m of ctx.missing) yield ctx.missingSev(m.rel, 0, `${m.what} is listed in the manifest but does not exist yet`);
}

// ---------------------------------------------------------------------------
// Check: markdownlint
// ---------------------------------------------------------------------------

function parseMarkdownlintOutput(out) {
  const re = /^(.+?):(\d+)(?::(\d+))?\s+(?:(?:error|warning)\s+)?(MD\d{3}(?:\/[\w-]+)*)\s+(.*)$/;
  const issues = [];
  for (const raw of String(out).split(/\r?\n/)) {
    const m = re.exec(raw.trim());
    if (m) issues.push({ file: toPosix(m[1]), line: Number(m[2]), column: m[3] ? Number(m[3]) : null, rule: m[4], message: m[5] });
  }
  return issues;
}

function* checkMarkdownlint(ctx) {
  const cmd = 'npx --yes markdownlint-cli2 "docs/**/*.md" "README.md"';
  const res = spawnSync(cmd, { cwd: ctx.root, shell: true, encoding: 'utf8', timeout: 300000, maxBuffer: 64 * 1024 * 1024 });
  if (res.error) {
    yield W('-', 0, `markdownlint-cli2 could not be run (${res.error.code || res.error.message}); check skipped`);
    return;
  }
  const out = `${res.stdout || ''}\n${res.stderr || ''}`;
  if (!/markdownlint-cli2 v\d/.test(out)) {
    yield W('-', 0, `markdownlint-cli2 unavailable (npx exit ${res.status}): ${lastLine(out)}`);
    return;
  }
  const issues = parseMarkdownlintOutput(out);
  for (const is of issues) yield E(is.file, is.line, `${is.rule} ${is.message}`);
  if (res.status !== 0 && issues.length === 0) yield E('-', 0, `markdownlint-cli2 exited with status ${res.status}: ${lastLine(out)}`);
}

// ---------------------------------------------------------------------------
// Check: links
// ---------------------------------------------------------------------------

const BARE_PATH_RE = /(?<![\w/.-])(?:\.{1,2}\/)*(?:[\w.-]+\/)+[\w.-]+\.md\b/g;

function* checkLinks(ctx) {
  for (const doc of ctx.scanned) {
    for (const link of doc.links) {
      const r = link.resolved;
      if (r.external) continue;
      const where = link.line + 1;
      if (r.outside) { yield E(doc.rel, where, `link '${link.target}' escapes the repository root`); continue; }
      if (!r.sameFile && !ctx.exists(r.rel)) {
        yield ctx.refSev(r.rel)(doc.rel, where, `broken link '${link.target}': ${r.rel} does not exist`);
        continue;
      }
      if (r.frag && r.rel.toLowerCase().endsWith('.md')) {
        const target = r.sameFile ? doc : ctx.getDoc(r.rel);
        if (target && !target.anchors.has(safeDecode(r.frag).toLowerCase())) {
          yield E(doc.rel, where, `broken anchor '#${r.frag}' in ${r.sameFile ? 'this document' : r.rel}`);
        }
      }
    }
    // Bare path references in prose (outside link syntax), inline code included.
    for (let i = 0; i < doc.lines.length; i++) {
      if (!doc.isProse(i)) continue;
      const line = maskHtmlComments(doc.lines[i]);
      if (REF_DEF_RE.test(line)) continue;
      const stripped = line.replace(INLINE_LINK_RE, (m) => ' '.repeat(m.length)).replace(/https?:\/\/\S+/g, ' ');
      for (const m of stripped.matchAll(BARE_PATH_RE)) {
        yield W(doc.rel, i + 1, `bare path '${m[0]}' in prose; use a relative Markdown link`);
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Check: mermaid
// ---------------------------------------------------------------------------

let mermaidPromise = null;

async function loadMermaid() {
  if (!mermaidPromise) {
    mermaidPromise = (async () => {
      try {
        // mermaid.parse() only needs DOMPurify when rendering; in Node there is no window, so the
        // DOMPurify instance lacks its API. Replace it with no-ops before mermaid uses it.
        const dp = await import('dompurify').catch(() => null);
        const DP = dp?.default;
        if (DP && typeof DP.addHook !== 'function') {
          for (const k of ['addHook', 'removeHook', 'removeHooks', 'removeAllHooks', 'setConfig', 'clearConfig']) DP[k] = () => {};
          DP.sanitize = (s) => String(s);
          DP.isValidAttribute = () => true;
        }
        const mod = await import('mermaid');
        const mermaid = mod.default ?? mod;
        await mermaid.parse('flowchart LR\n  A --> B');
        return mermaid;
      } catch {
        return null;
      }
    })();
  }
  return mermaidPromise;
}

// First meaningful line of a mermaid block (skips blanks, %% comments and a --- config block).
function mermaidHeader(contentLines) {
  let k = 0;
  const skipBlank = () => { while (k < contentLines.length && (!contentLines[k].trim() || contentLines[k].trim().startsWith('%%'))) k++; };
  skipBlank();
  if (k < contentLines.length && contentLines[k].trim() === '---') {
    k++;
    while (k < contentLines.length && contentLines[k].trim() !== '---') k++;
    k++;
    skipBlank();
  }
  if (k >= contentLines.length) return { type: '', index: -1 };
  return { type: contentLines[k].trim().split(/\s+/)[0], index: k };
}

function flowchartNodes(lines) {
  const ids = new Set();
  const skipFirst = new Set(['classDef', 'class', 'style', 'linkStyle', 'click', 'direction', 'end', 'subgraph']);
  const kw = new Set(['TB', 'TD', 'BT', 'RL', 'LR', 'end', 'subgraph', 'flowchart', 'graph']);
  for (const raw of lines) {
    let l = raw.trim();
    if (!l || skipFirst.has(l.split(/\s+/)[0])) continue;
    l = l.replace(/"[^"]*"/g, '""')
      .replace(/\|[^|]*\|/g, ' ')
      .replace(/(--|==|-\.)\s+[^\n]*?\s+(-->|---|==>|===|\.->|\.-)/g, ' --> ')
      .replace(/:::[\w-]+/g, ' ')
      .replace(/(\w)>[^\]\n]*\]/g, '$1 ');
    let prev;
    do { prev = l; l = l.replace(/\[[^[\]]*\]|\([^()]*\)|\{[^{}]*\}/g, ' '); } while (l !== prev);
    l = l.replace(/[<ox]?(?:-{2,}|={2,}|-\.+-?|~{3,})[>ox]?/g, ' ').replace(/[&;]/g, ' ');
    for (const t of l.split(/\s+/)) if (/^[\w][\w-]*$/.test(t) && !kw.has(t)) ids.add(t);
  }
  return ids.size;
}

function sequenceParticipants(lines) {
  const ids = new Set();
  const kw = /^(note|loop|alt|else|opt|par|and|end|rect|critical|break|box|autonumber|activate|deactivate|title|destroy|option)\b/;
  for (const raw of lines) {
    const l = raw.trim();
    const p = /^(?:create\s+)?(?:participant|actor)\s+(.+?)(?:\s+as\s+.*)?$/.exec(l);
    if (p) { ids.add(p[1].trim()); continue; }
    if (kw.test(l)) continue;
    const m = /^(.+?)\s*(?:<<)?(-{1,2})(>>|>|x|\))\s*[+-]?\s*(.+?)\s*:/.exec(l);
    if (m) { ids.add(m[1].trim()); ids.add(m[4].trim()); }
  }
  return ids.size;
}

function stateNodes(lines) {
  const ids = new Set();
  for (const raw of lines) {
    const l = raw.trim();
    if (!l || /^(note|direction|classDef|class|style|\}|--)/.test(l)) continue;
    const s = /^state\s+(?:"[^"]*"\s+as\s+)?([\w-]+)/.exec(l);
    if (s) { ids.add(s[1]); continue; }
    if (l.includes('-->')) {
      for (const part of l.split(/\s*-->\s*/)) {
        const tok = part.replace(/:.*$/, '').trim().split(/\s+/)[0];
        if (tok && tok !== '[*]' && /^[\w-]+$/.test(tok)) ids.add(tok);
      }
      continue;
    }
    const d = /^([\w-]+)\s*:/.exec(l);
    if (d) ids.add(d[1]);
  }
  return ids.size;
}

function classNodes(lines) {
  const ids = new Set();
  const rel = /^([\w~-]+)\s*(?:"[^"]*"\s*)?(<\|--|\*--|o--|-->|<--|--\*|--o|--\|>|\.\.>|<\.\.|\.\.\|>|<\|\.\.|--|\.\.)\s*(?:"[^"]*"\s*)?([\w~-]+)/;
  for (const raw of lines) {
    const l = raw.trim();
    if (!l || /^(note|direction|classDef|style|cssClass|click|callback|link|namespace|\}|<<)/.test(l)) continue;
    const c = /^class\s+([\w-]+)/.exec(l);
    if (c) { ids.add(c[1]); continue; }
    const r = rel.exec(l);
    if (r) { ids.add(r[1].replace(/~.*$/, '')); ids.add(r[3].replace(/~.*$/, '')); continue; }
    const m = /^([\w-]+)\s*:/.exec(l);
    if (m) ids.add(m[1]);
  }
  return ids.size;
}

function countMermaidNodes(type, lines) {
  const body = lines.map((l) => l.replace(/%%.*$/, ''));
  switch (type) {
    case 'flowchart': return flowchartNodes(body);
    case 'sequenceDiagram': return sequenceParticipants(body);
    case 'stateDiagram-v2': return stateNodes(body);
    case 'classDiagram': return classNodes(body);
    default: return null;
  }
}

// Fallback when the official parser is unavailable: quotes, brackets and block `end` balance.
function mermaidSanity(type, lines) {
  const problems = [];
  let opens = 0;
  let ends = 0;
  lines.forEach((raw, k) => {
    const l = raw.replace(/%%.*$/, '');
    if ((l.match(/"/g) || []).length % 2) problems.push({ k, msg: 'unbalanced double quote' });
    if (type === 'flowchart') {
      const s = l.replace(/"[^"]*"/g, '').replace(/\|[^|]*\|/g, '');
      const bal = { '[': 0, '(': 0, '{': 0 };
      for (const ch of s) {
        if (ch === '[') bal['[']++; else if (ch === ']') bal['[']--;
        else if (ch === '(') bal['(']++; else if (ch === ')') bal['(']--;
        else if (ch === '{') bal['{']++; else if (ch === '}') bal['{']--;
      }
      if (Object.values(bal).some((v) => v !== 0)) problems.push({ k, msg: 'unbalanced brackets' });
      if (/^\s*subgraph\b/.test(l)) opens++;
    }
    if (type === 'sequenceDiagram' && /^\s*(loop|alt|opt|par|critical|break|rect|box)\b/.test(l)) opens++;
    if (/^\s*end\s*$/.test(l)) ends++;
  });
  if ((type === 'flowchart' || type === 'sequenceDiagram') && opens !== ends) {
    problems.push({ k: 0, msg: `${opens} block opener(s) but ${ends} 'end' line(s)` });
  }
  return problems;
}

const CAPTION_RE = /^\s*(?:\*(?!\*)(.+?)\*|_(?!_)(.+?)_)\s*$/;

async function* checkMermaid(ctx) {
  const mermaid = await loadMermaid();
  for (const doc of ctx.scanned) {
    const blocks = doc.fences.filter((f) => f.lang === 'mermaid');
    const found = new Map();
    let figure = 0;
    for (const f of blocks) {
      const content = doc.lines.slice(f.start + 1, f.end);
      const at = f.start + 1;
      const { type, index } = mermaidHeader(content);
      if (!ALLOWED_DIAGRAMS.includes(type)) {
        yield E(doc.rel, at, `diagram type '${type || '(empty)'}' is not allowed (allowed: ${ALLOWED_DIAGRAMS.join(', ')})`);
      } else {
        found.set(type, (found.get(type) || 0) + 1);
        const n = countMermaidNodes(type, content.slice(index + 1));
        if (n != null && n > MAX_DIAGRAM_NODES) yield E(doc.rel, at, `${type} has ${n} nodes (heuristic count); at most ${MAX_DIAGRAM_NODES} allowed, split the diagram`);
      }
      // Caption: an italic `*Figure N: ...*` line immediately above (blank lines allowed).
      let c = f.start - 1;
      while (c >= 0 && !doc.lines[c].trim()) c--;
      const cm = c >= 0 && doc.isProse(c) ? CAPTION_RE.exec(doc.lines[c]) : null;
      const caption = cm ? (cm[1] ?? cm[2]).trim() : '';
      const fig = /^Figure (\d+): \S/.exec(caption);
      if (!fig) {
        yield E(doc.rel, at, 'missing caption: put an italic line `*Figure N: ...*` immediately above the mermaid fence');
      } else {
        figure++;
        if (Number(fig[1]) !== figure) yield W(doc.rel, c + 1, `figure numbered ${fig[1]}, expected ${figure} (number figures in order)`);
      }
      if (f.end >= doc.lines.length) yield E(doc.rel, at, 'unclosed mermaid fence');
      // Parse with the official parser, or fall back to the sanity check.
      const text = content.join('\n');
      if (mermaid) {
        try {
          await mermaid.parse(text);
        } catch (e) {
          const msg = String(e?.message ?? e).split('\n');
          const lm = /line (\d+)/i.exec(msg[0]);
          const detail = msg.find((m) => /^Expecting|^Unexpected|got /.test(m));
          yield E(doc.rel, lm ? at + Number(lm[1]) - 1 : at, `mermaid parse error: ${msg[0].slice(0, 160)}${detail ? ` (${detail.slice(0, 160)})` : ''}`);
        }
      } else if (ALLOWED_DIAGRAMS.includes(type)) {
        for (const p of mermaidSanity(type, content)) yield E(doc.rel, at + p.k, `mermaid syntax (fallback check): ${p.msg}`);
      }
    }
    if (blocks.length && !mermaid) {
      yield W(doc.rel, blocks[0].start + 1, `mermaid-parse-unverified: ${blocks.length} diagram(s) checked with the fallback sanity check only (run npm install in scripts/ to enable the official mermaid parser)`);
    }
    // Diagrams required by the manifest.
    const spec = ctx.specByRel.get(doc.rel);
    if (spec && Array.isArray(spec.diagrams)) {
      const need = new Map();
      for (const d of spec.diagrams) need.set(d.type, (need.get(d.type) || 0) + 1);
      for (const [type, n] of need) {
        const have = found.get(type) || 0;
        if (have < n) {
          const subjects = spec.diagrams.filter((d) => d.type === type).map((d) => d.subject).join('; ');
          yield E(doc.rel, 0, `manifest requires ${n} ${type} diagram(s), found ${have} (subjects: ${subjects})`);
        }
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Check: frontmatter
// ---------------------------------------------------------------------------

function usedMilestoneTags(doc) {
  const tags = new Set();
  for (let i = doc.bodyStart; i < doc.lines.length; i++) {
    for (const m of doc.lines[i].matchAll(/\bM(\d+)\b/g)) tags.add(`M${m[1]}`);
  }
  return tags;
}

function adrMentions(doc) {
  const ids = new Map();
  for (let i = doc.bodyStart; i < doc.lines.length; i++) {
    for (const m of doc.lines[i].matchAll(ADR_MENTION_RE)) if (!ids.has(m[0])) ids.set(m[0], i);
  }
  return ids;
}

// Non-ADR documents under docs/ plus manifest documents elsewhere, unless front_matter: false.
function frontMatterTargets(ctx) {
  return ctx.scanned.filter((d) => {
    if (ctx.isAdr(d.rel) || d.rel === ctx.adrIndexRel) return false;
    const spec = ctx.specByRel.get(d.rel);
    if (spec && spec.front_matter === false) return false;
    return d.rel.startsWith('docs/') || Boolean(spec);
  });
}

function* checkFrontmatter(ctx) {
  const milestones = new Set(ctx.milestones);
  for (const doc of frontMatterTargets(ctx)) {
    const spec = ctx.specByRel.get(doc.rel);
    const fm = doc.fm;
    if (!fm.present) { yield E(doc.rel, 1, 'missing YAML front matter'); continue; }
    if (fm.error) { yield E(doc.rel, 1, fm.error); continue; }
    const d = fm.data;
    for (const k of DOC_FRONT_MATTER) if (!(k in d)) yield E(doc.rel, 1, `front matter lacks '${k}'`);
    if ('title' in d && (typeof d.title !== 'string' || !d.title.trim())) yield E(doc.rel, 1, 'title must be a non-empty string');
    if (spec && typeof d.title === 'string' && spec.title && d.title !== spec.title) yield W(doc.rel, 1, `title '${d.title}' differs from the manifest title '${spec.title}'`);
    if ('status' in d && !DOC_STATUS.includes(d.status)) yield E(doc.rel, 1, `status '${d.status}' must be one of ${DOC_STATUS.join(', ')}`);
    if ('owner' in d && (typeof d.owner !== 'string' || !d.owner.trim())) yield E(doc.rel, 1, 'owner must be a non-empty string');
    if ('last_updated' in d) {
      const v = String(d.last_updated);
      if (!ISO_DATE_RE.test(v) || Number.isNaN(Date.parse(v))) yield E(doc.rel, 1, `last_updated '${v}' is not an ISO date (YYYY-MM-DD)`);
    }
    if ('depends_on' in d) {
      if (!Array.isArray(d.depends_on)) yield E(doc.rel, 1, 'depends_on must be a list');
      else for (const p of d.depends_on) {
        const rel = normRel(String(p));
        if (!ctx.exists(rel)) yield ctx.refSev(rel)(doc.rel, 1, `depends_on path '${p}' does not exist`);
      }
    }
    let adrs = [];
    if ('adrs' in d) {
      if (!Array.isArray(d.adrs)) yield E(doc.rel, 1, 'adrs must be a list');
      else {
        adrs = d.adrs.map(String);
        for (const a of adrs) {
          if (!ADR_ID_RE.test(a)) yield E(doc.rel, 1, `adrs entry '${a}' does not match ADR-NNNN`);
          else if (!ctx.adrById.has(a)) yield E(doc.rel, 1, `adrs entry '${a}' is not an ADR in the manifest`);
        }
      }
    }
    if ('milestone_tags_used' in d) {
      if (!Array.isArray(d.milestone_tags_used)) yield E(doc.rel, 1, 'milestone_tags_used must be a list');
      else {
        const listed = new Set(d.milestone_tags_used.map(String));
        for (const t of listed) if (!milestones.has(t)) yield E(doc.rel, 1, `milestone_tags_used entry '${t}' is not one of ${ctx.milestones.join(', ')}`);
        const used = usedMilestoneTags(doc);
        for (const t of used) if (!listed.has(t)) yield E(doc.rel, 1, `milestone tag ${t} is used in the body but missing from milestone_tags_used`);
        for (const t of listed) if (!used.has(t)) yield E(doc.rel, 1, `milestone_tags_used lists ${t} but the body never uses it`);
      }
    }
    const h1 = doc.h1s[0];
    if (h1 && typeof d.title === 'string' && h1.text !== d.title) yield E(doc.rel, h1.line + 1, `H1 '${h1.text}' does not equal the front matter title '${d.title}'`);
    const mentions = adrMentions(doc);
    if (Array.isArray(d.adrs)) {
      for (const [id, i] of mentions) if (!adrs.includes(id)) yield E(doc.rel, i + 1, `${id} is mentioned in the body but missing from front matter adrs`);
      for (const a of adrs) if (!mentions.has(a)) yield W(doc.rel, 1, `front matter adrs lists ${a} but the body never cites it`);
    }
    if (spec && Array.isArray(spec.adrs)) {
      for (const a of spec.adrs) if (!mentions.has(a)) yield W(doc.rel, 1, `the manifest expects this document to cite ${a}`);
    }
  }
}

// ---------------------------------------------------------------------------
// Check: outline
// ---------------------------------------------------------------------------

// Compares a document's H2 list with the expected list (exact spelling and order).
function* outlineFindings(doc, expected) {
  const actual = doc.headings.filter((h) => h.level === 2);
  const texts = actual.map((h) => h.text);
  if (arraysEqual(texts, expected)) return;
  const expSet = new Set(expected);
  const actSet = new Set(texts);
  const nearExpected = new Map(expected.map((e) => [normalizeText(e), e]));
  let reported = false;
  const seen = new Set();
  for (const h of actual) {
    if (seen.has(h.text)) { yield E(doc.rel, h.line + 1, `duplicate H2 '${h.text}'`); reported = true; }
    seen.add(h.text);
  }
  const misspelled = new Set();
  for (const h of actual) {
    if (expSet.has(h.text)) continue;
    const near = nearExpected.get(normalizeText(h.text));
    if (near && !actSet.has(near)) { misspelled.add(near); yield E(doc.rel, h.line + 1, `H2 '${h.text}' must be spelled exactly '${near}'`); }
    else yield E(doc.rel, h.line + 1, `unexpected H2 '${h.text}' (not in the manifest outline)`);
    reported = true;
  }
  for (const e of expected) {
    if (!actSet.has(e) && !misspelled.has(e)) { yield E(doc.rel, 0, `missing H2 '${e}'`); reported = true; }
  }
  const common = [...new Set(texts.filter((t) => expSet.has(t)))];
  const expCommon = expected.filter((e) => actSet.has(e));
  if (!arraysEqual(common, expCommon)) {
    const k = common.findIndex((t, i) => t !== expCommon[i]);
    const h = actual.find((x) => x.text === common[k]);
    yield E(doc.rel, h ? h.line + 1 : 0, `H2 out of order: expected '${expCommon[k]}' but found '${common[k]}'. Expected order: ${expected.join(' | ')}`);
  } else if (!reported) {
    yield E(doc.rel, 0, `H2 list differs from the manifest outline: ${expected.join(' | ')}`);
  }
}

function expectedOutline(ctx, spec) {
  const outline = (spec.outline_h2 || []).map(String);
  if (spec.mandatory_h2 === false) return outline;
  const mand = ctx.manifest.mandatory_h2 || {};
  return [...(mand.first || []), ...outline, ...(mand.last || [])].map(String);
}

function* checkOutline(ctx) {
  for (const { spec, rel, doc } of ctx.manifestDocs) {
    if (!doc) continue;
    const h1s = doc.h1s;
    if (h1s.length === 0) yield E(rel, 0, 'document has no H1');
    for (const h of h1s.slice(1)) yield E(rel, h.line + 1, `more than one H1 ('${h.text}')`);
    yield* outlineFindings(doc, expectedOutline(ctx, spec));
    if (spec.mandatory_h2 === false) continue;
    const summary = doc.section('Summary');
    if (summary) {
      const n = doc.wordCount(summary);
      if (n > SUMMARY_MAX_WORDS) yield E(rel, summary.start + 1, `Summary has ${n} words; at most ${SUMMARY_MAX_WORDS} allowed`);
    }
    const oq = doc.section('Open questions');
    if (oq) {
      const tables = doc.tablesIn(oq);
      if (!tables.length && !/^\s*None\.\s*$/m.test(doc.proseText(oq))) {
        yield E(rel, oq.start + 1, `Open questions must be a table (${OPEN_QUESTION_COLUMNS.join(' | ')}) or the line 'None.'`);
      }
      for (const t of tables) {
        const header = t.header.map((h) => stripMarkdown(h).trim());
        if (!arraysEqual(header.map((h) => h.toLowerCase()), OPEN_QUESTION_COLUMNS.map((h) => h.toLowerCase()))) {
          yield E(rel, t.headerLine + 1, `Open questions table columns must be ${OPEN_QUESTION_COLUMNS.join(' | ')} (found ${header.join(' | ')})`);
          continue;
        }
        const idRe = new RegExp(`^OQ-${escapeRegExp(spec.slug)}-\\d+$`);
        for (const r of t.rows) {
          const id = stripMarkdown(r.cells[0]).trim();
          if (!idRe.test(id)) yield E(rel, r.line + 1, `open question ID '${id}' must match OQ-${spec.slug}-<n>`);
        }
      }
    }
  }
  const known = new Set(ctx.manifestDocs.map((e) => e.rel));
  for (const doc of ctx.scanned) {
    if (known.has(doc.rel) || ctx.isAdr(doc.rel) || doc.rel === ctx.adrIndexRel) continue;
    yield W(doc.rel, 0, 'document is not listed in the manifest; outline not checked');
  }
}

// ---------------------------------------------------------------------------
// Check: machine-checks (manifest machine_check_kinds)
// ---------------------------------------------------------------------------

function sectionsFor(doc, names, label) {
  const sections = [];
  const missing = [];
  for (const n of names) {
    const s = doc.section(n);
    if (s) sections.push(s); else missing.push(E(doc.rel, 0, `[${label}] section '${n}' not found`));
  }
  return { sections, missing };
}

function headingNames(mc) {
  if (Array.isArray(mc.table_headings)) return mc.table_headings;
  if (mc.table_heading != null) return [mc.table_heading];
  return [];
}

// Counts of each value of `column` per section in tableHeadings (keys normalized).
function computeStatusCounts(doc, column, tableHeadings) {
  const perCat = [];
  const perStatus = new Map();
  let total = 0;
  for (const h of tableHeadings) {
    const sec = doc.section(h);
    const counts = new Map();
    let catTotal = 0;
    if (sec) {
      for (const t of doc.tablesIn(sec)) {
        const ci = colIndex(t, column);
        if (ci < 0) continue;
        for (const r of t.rows) {
          const key = normalizeText(r.cells[ci]);
          counts.set(key, (counts.get(key) || 0) + 1);
          perStatus.set(key, (perStatus.get(key) || 0) + 1);
          catTotal++;
          total++;
        }
      }
    }
    perCat.push({ name: h, loose: normLoose(h), counts, total: catTotal });
  }
  return { perCat, perStatus, total };
}

function parseCount(cell) {
  const m = /^\s*(\d[\d,]*)(?![\d,]*\s*%)/.exec(stripMarkdown(cell ?? ''));
  return m ? Number(m[1].replace(/,/g, '')) : null;
}

// Compares the tables under mc.section with computed counts. Recognized layouts: status rows
// (`| Planned (M1) | 12 |`, `| Total | 153 |`) and category rows with status and/or total columns.
function* summaryCountFindings(doc, mc, label, unrecognizedSev) {
  const sec = doc.section(mc.section);
  if (!sec) { yield E(doc.rel, 0, `[${label}] section '${mc.section}' not found`); return; }
  const { perCat, perStatus, total } = computeStatusCounts(doc, mc.column, headingNames(mc));
  const isStatus = (s) => {
    const n = normalizeText(s);
    return perStatus.has(n) || /^(planned \(m\d+\)|not planned|n\/a|yes|no|partial)$/.test(n);
  };
  const isTotal = (s) => /^(total|all|sum|overall)\b/.test(normalizeText(s));
  const findCat = (s) => perCat.find((c) => c.loose === normLoose(s));
  const totalColRe = /^(total|count|rows|features|number|n)$/;
  let recognized = 0;
  const cmp = (line, what, n, expected) => {
    recognized++;
    return n !== expected ? E(doc.rel, line + 1, `[${label}] ${what}: table says ${n}, computed ${expected}`) : null;
  };
  for (const t of doc.tablesIn(sec)) {
    const hdr = t.header.map((h) => normalizeText(h));
    const statusCols = hdr.map((h, i) => (i > 0 && isStatus(h) ? i : -1)).filter((i) => i >= 0);
    const totalCol = hdr.findIndex((h, i) => i > 0 && totalColRe.test(h));
    for (const r of t.rows) {
      const labelCell = r.cells[0] ?? '';
      const name = stripMarkdown(labelCell).trim();
      const cat = findCat(labelCell);
      if (!cat && (isStatus(labelCell) || (isTotal(labelCell) && statusCols.length === 0))) {
        const ci = totalCol >= 0 ? totalCol : r.cells.findIndex((c, i) => i > 0 && parseCount(c) != null);
        const n = ci >= 0 ? parseCount(r.cells[ci]) : null;
        if (n == null) continue;
        const f = cmp(r.line, `'${name}'`, n, isTotal(labelCell) ? total : (perStatus.get(normalizeText(labelCell)) || 0));
        if (f) yield f;
      } else if (cat || isTotal(labelCell)) {
        for (const ci of statusCols) {
          const n = parseCount(r.cells[ci]);
          if (n == null) continue;
          const f = cmp(r.line, `'${name}' / '${t.header[ci]}'`, n, cat ? (cat.counts.get(hdr[ci]) || 0) : (perStatus.get(hdr[ci]) || 0));
          if (f) yield f;
        }
        if (totalCol >= 0) {
          const n = parseCount(r.cells[totalCol]);
          if (n != null) {
            const f = cmp(r.line, `'${name}' total`, n, cat ? cat.total : total);
            if (f) yield f;
          }
        }
      }
    }
  }
  if (!recognized) {
    yield unrecognizedSev(doc.rel, sec.start + 1, `[${label}] no count rows recognized in '${mc.section}'; expected rows like '| Planned (M1) | <n> |' or a category x status table (computed total ${total})`);
  }
}

function countLinksOutside(doc, exemptSections) {
  const exempt = exemptSections.map((n) => doc.section(n)).filter(Boolean);
  const counts = new Map();
  for (const l of doc.links) {
    if (l.resolved.external || l.resolved.sameFile || l.resolved.outside) continue;
    if (exempt.some((s) => l.line > s.start && l.line < s.end)) continue;
    counts.set(l.resolved.rel, (counts.get(l.resolved.rel) || 0) + 1);
  }
  return counts;
}

const MACHINE_CHECKS = {
  *max_words(doc, mc) {
    const n = doc.wordCount();
    if (n > mc.value) yield E(doc.rel, 0, `[max_words] body has ${n} words; at most ${mc.value} allowed`);
  },
  *min_words(doc, mc) {
    const n = doc.wordCount();
    if (n < mc.value) yield E(doc.rel, 0, `[min_words] body has ${n} words; at least ${mc.value} required`);
  },
  *max_lines(doc, mc) {
    if (doc.lines.length > mc.value) yield E(doc.rel, 0, `[max_lines] file has ${doc.lines.length} lines; at most ${mc.value} allowed`);
  },
  *required_terms(doc, mc) {
    for (const term of mc.value || []) if (!doc.body.includes(String(term))) yield E(doc.rel, 0, `[required_terms] missing required term '${term}'`);
  },
  *links_to(doc, mc) {
    for (const p of mc.value || []) if (!doc.linksTo(normRel(p))) yield E(doc.rel, 0, `[links_to] no relative Markdown link to ${p}`);
  },
  *has_table(doc, mc) {
    const { sections, missing } = sectionsFor(doc, [mc.section], 'has_table');
    yield* missing;
    for (const s of sections) if (!doc.tablesIn(s).length) yield E(doc.rel, s.start + 1, `[has_table] section '${mc.section}' contains no table`);
  },
  *min_table_rows(doc, mc) {
    const { sections, missing } = sectionsFor(doc, headingNames(mc), 'min_table_rows');
    yield* missing;
    for (const s of sections) {
      const best = Math.max(0, ...doc.tablesIn(s).map((t) => t.rows.length));
      if (best < mc.value) yield E(doc.rel, s.start + 1, `[min_table_rows] largest table in '${s.heading.text}' has ${best} data rows; at least ${mc.value} required`);
    }
  },
  *min_table_rows_total(doc, mc) {
    const { sections, missing } = sectionsFor(doc, headingNames(mc), 'min_table_rows_total');
    yield* missing;
    const total = sections.reduce((acc, s) => acc + doc.tablesIn(s).reduce((a, t) => a + t.rows.length, 0), 0);
    if (total < mc.value) yield E(doc.rel, 0, `[min_table_rows_total] ${total} data rows across the listed sections; at least ${mc.value} required`);
  },
  *no_blank_cells(doc, mc) {
    const { sections, missing } = sectionsFor(doc, headingNames(mc), 'no_blank_cells');
    yield* missing;
    for (const s of sections) {
      for (const t of doc.tablesIn(s)) {
        for (const r of t.rows) {
          const blanks = r.cells.map((c, k) => (!c.trim() ? t.header[k] || `column ${k + 1}` : null)).filter(Boolean);
          if (blanks.length) yield E(doc.rel, r.line + 1, `[no_blank_cells] blank cell(s) in '${s.heading.text}': ${blanks.join(', ')}`);
        }
      }
    }
  },
  *cell_values_in(doc, mc) {
    const { sections, missing } = sectionsFor(doc, headingNames(mc), 'cell_values_in');
    yield* missing;
    let re;
    try { re = new RegExp(mc.allowed_pattern); } catch (e) { yield W(doc.rel, 0, `[cell_values_in] invalid allowed_pattern: ${e.message}`); return; }
    for (const s of sections) {
      const tables = doc.tablesIn(s);
      if (!tables.length) yield E(doc.rel, s.start + 1, `[cell_values_in] section '${s.heading.text}' contains no table`);
      for (const t of tables) {
        let cols = [];
        if (mc.columns === 'all_but_first') cols = t.header.map((_, i) => i).slice(1);
        else {
          for (const name of [].concat(mc.columns || [])) {
            const ci = colIndex(t, name);
            if (ci < 0) yield E(doc.rel, t.headerLine + 1, `[cell_values_in] table in '${s.heading.text}' has no column '${name}'`);
            else cols.push(ci);
          }
        }
        for (const r of t.rows) {
          for (const ci of cols) {
            const v = (r.cells[ci] ?? '').trim();
            if (!re.test(v)) yield E(doc.rel, r.line + 1, `[cell_values_in] '${t.header[ci]}' value '${v}' does not match ${mc.allowed_pattern}`);
          }
        }
      }
    }
  },
  *min_h3(doc, mc) {
    const { sections, missing } = sectionsFor(doc, [mc.section], 'min_h3');
    yield* missing;
    for (const s of sections) {
      const n = doc.headingsIn(s, 3).length;
      if (n < mc.value) yield E(doc.rel, s.start + 1, `[min_h3] section '${mc.section}' has ${n} H3 headings; at least ${mc.value} required`);
    }
  },
  *min_list_items(doc, mc) {
    const { sections, missing } = sectionsFor(doc, [mc.section], 'min_list_items');
    yield* missing;
    for (const s of sections) {
      let items = 0;
      for (let i = s.start + 1; i < s.end; i++) if (doc.isProse(i) && /^ ?(?:[-*+]|\d{1,9}[.)])\s+\S/.test(doc.lines[i])) items++;
      const rows = doc.tablesIn(s).reduce((a, t) => a + t.rows.length, 0);
      if (items < mc.value && rows < mc.value) yield E(doc.rel, s.start + 1, `[min_list_items] section '${mc.section}' has ${items} top-level list items and ${rows} table rows; at least ${mc.value} required`);
    }
  },
  *min_urls_per_h3(doc, mc) {
    const { sections, missing } = sectionsFor(doc, [mc.section], 'min_urls_per_h3');
    yield* missing;
    for (const s of sections) {
      for (const h of doc.headingsIn(s, 3)) {
        const next = doc.headings.find((x) => x.line > h.line && x.level <= 3);
        const end = Math.min(next ? next.line : doc.lines.length, s.end);
        const urls = new Set();
        for (let i = h.line + 1; i < end; i++) if (doc.isProse(i)) for (const u of extractUrls(doc.lines[i])) urls.add(normalizeUrl(u));
        if (urls.size < mc.value) yield E(doc.rel, h.line + 1, `[min_urls_per_h3] H3 '${h.text}' cites ${urls.size} distinct URLs; at least ${mc.value} required`);
      }
    }
  },
  *min_distinct_matches(doc, mc) {
    let re;
    try { re = new RegExp(mc.pattern, 'g'); } catch (e) { yield W(doc.rel, 0, `[min_distinct_matches] invalid pattern: ${e.message}`); return; }
    const found = new Set(doc.body.match(re) || []);
    if (found.size < mc.value) yield E(doc.rel, 0, `[min_distinct_matches] ${found.size} distinct matches of /${mc.pattern}/; at least ${mc.value} required`);
  },
  *covers_items(doc, mc) {
    const norm = (s) => normalizeText(s).replace(/[.:;,]+$/, '');
    const texts = new Set([
      ...doc.headings.filter((h) => h.level === 2 || h.level === 3).map((h) => norm(h.plain)),
      ...doc.links.map((l) => norm(l.text)),
    ]);
    for (const item of mc.value || []) if (!texts.has(norm(item))) yield E(doc.rel, 0, `[covers_items] '${item}' is not the text of an H2/H3 heading or of a Markdown link`);
  },
  *links_all_docs_once(doc, mc, ctx) {
    const counts = countLinksOutside(doc, LINK_ONCE_EXEMPT_SECTIONS);
    const expected = [
      ...ctx.manifestDocs.filter((e) => e.spec.id !== 1 && e.spec.id !== 2).map((e) => e.rel),
      ...ctx.adrSpecs.map((a) => a.path),
    ];
    for (const rel of expected) {
      const n = counts.get(rel) || 0;
      if (n !== 1) yield E(doc.rel, 0, `[links_all_docs_once] ${rel} is linked ${n} times (outside Reading paths); expected exactly once`);
    }
  },
  *covers_source_rows(doc, mc, ctx) {
    const source = ctx.getDoc(mc.source);
    if (!source) { yield E(doc.rel, 0, `[covers_source_rows] source ${mc.source} not found`); return; }
    const column = mc.column || 'Feature';
    const { sections, missing } = sectionsFor(doc, headingNames(mc), 'covers_source_rows');
    yield* missing;
    const have = new Set();
    for (const s of sections) {
      for (const t of doc.tablesIn(s)) {
        const ci = colIndex(t, column);
        if (ci >= 0) for (const r of t.rows) have.add(normalizeText(r.cells[ci]));
      }
    }
    for (const t of source.tables) {
      const ci = colIndex(t, column);
      if (ci < 0) continue;
      for (const r of t.rows) {
        const v = normalizeText(r.cells[ci]);
        if (v && !have.has(v)) yield E(doc.rel, 0, `[covers_source_rows] '${stripMarkdown(r.cells[ci]).trim()}' (${mc.source}:${r.line + 1}) is not a ${column} cell in the listed sections`);
      }
    }
  },
  *summary_counts_match(doc, mc) {
    yield* summaryCountFindings(doc, mc, 'summary_counts_match', W);
  },
};

function* checkMachineChecks(ctx) {
  for (const kind of Object.keys(ctx.manifest.machine_check_kinds || {})) {
    if (!MACHINE_CHECKS[kind]) yield W(normRel(path.relative(ctx.root, ctx.manifestPath)), 0, `machine_check_kinds defines '${kind}', which verify-docs does not implement`);
  }
  for (const { spec, doc } of ctx.manifestDocs) {
    if (!doc) continue;
    for (const mc of spec.machine_checks || []) {
      const fn = MACHINE_CHECKS[mc.kind];
      if (!fn) { yield W(doc.rel, 0, `unknown machine check kind '${mc.kind}'`); continue; }
      yield* fn(doc, mc, ctx);
    }
    // Doc-level required_terms are checked like the required_terms kind.
    if (Array.isArray(spec.required_terms)) yield* MACHINE_CHECKS.required_terms(doc, { value: spec.required_terms });
    const band = spec.length_words;
    if (band && typeof band === 'object') {
      const n = doc.wordCount();
      if ((band.min != null && n < band.min) || (band.max != null && n > band.max)) {
        yield E(doc.rel, 0, `[length_words] body has ${n} words; the manifest band is ${band.min ?? 0} to ${band.max ?? 'unbounded'}`);
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Check: adr
// ---------------------------------------------------------------------------

function* checkAdr(ctx) {
  const common = ctx.manifest.adr_common || {};
  const fmKeys = common.front_matter || ADR_FRONT_MATTER_DEFAULT;
  const index = ctx.getDoc(ctx.adrIndexRel);
  const docsIndexRel = ctx.special('docsIndex');
  // Index files link every ADR by construction, so they do not count as a back-reference.
  const contentDocs = ctx.scanned.filter((d) => !ctx.isAdr(d.rel) && d.rel !== ctx.adrIndexRel && d.rel !== docsIndexRel);
  const statusById = new Map();

  for (const doc of ctx.scanned.filter((d) => ctx.isAdr(d.rel))) {
    const id = `ADR-${ADR_FILE_RE.exec(doc.rel)[1]}`;
    const spec = ctx.adrById.get(id);
    if (!spec) yield E(doc.rel, 0, `${id} is not listed in the manifest adrs`);
    else if (spec.path !== doc.rel) yield E(doc.rel, 0, `file name differs from the manifest path ${spec.path}`);

    const fm = doc.fm;
    if (!fm.present) yield E(doc.rel, 1, 'missing ADR front matter');
    else if (fm.error) yield E(doc.rel, 1, fm.error);
    else {
      const d = fm.data;
      for (const k of fmKeys) if (!(k in d)) yield E(doc.rel, 1, `ADR front matter lacks '${k}'`);
      if ('id' in d && String(d.id) !== id) yield E(doc.rel, 1, `front matter id '${d.id}' does not match the file name (${id})`);
      if ('status' in d) {
        statusById.set(id, String(d.status));
        if (!ADR_STATUS_RE.test(String(d.status))) yield E(doc.rel, 1, `ADR status '${d.status}' must be proposed, accepted, deprecated or superseded-by ADR-NNNN`);
        else if (spec?.status && String(d.status) !== spec.status) yield E(doc.rel, 1, `ADR status '${d.status}' differs from the manifest status '${spec.status}'`);
      }
      if ('date' in d && !ISO_DATE_RE.test(String(d.date))) yield E(doc.rel, 1, `date '${d.date}' is not an ISO date (YYYY-MM-DD)`);
      if ('deciders' in d && (!Array.isArray(d.deciders) || !d.deciders.length)) yield E(doc.rel, 1, 'deciders must be a non-empty list');
      if ('related' in d) {
        if (!Array.isArray(d.related)) yield E(doc.rel, 1, 'related must be a list');
        else for (const p of d.related) {
          const rel = normRel(String(p));
          if (!ctx.exists(rel)) yield ctx.refSev(rel)(doc.rel, 1, `related path '${p}' does not exist`);
        }
      }
      if (spec?.title && d.title && String(d.title) !== spec.title) yield W(doc.rel, 1, `title '${d.title}' differs from the manifest title '${spec.title}'`);
    }

    const h1s = doc.h1s;
    if (h1s.length !== 1) yield E(doc.rel, h1s[1] ? h1s[1].line + 1 : 0, `ADR must have exactly one H1 (found ${h1s.length})`);
    else if (!h1s[0].text.startsWith(`${id}: `)) yield E(doc.rel, h1s[0].line + 1, `H1 must read '${id}: <title>'`);
    else if (fm.data?.title && h1s[0].text !== `${id}: ${fm.data.title}`) yield W(doc.rel, h1s[0].line + 1, `H1 title differs from the front matter title '${fm.data.title}'`);

    if (Array.isArray(common.outline_h2)) yield* outlineFindings(doc, common.outline_h2.map(String));
    const outcome = doc.section('Decision outcome');
    if (outcome) {
      const h3 = new Set(doc.headingsIn(outcome, 3).map((h) => h.text));
      for (const want of common.required_h3_under_decision_outcome || []) {
        if (!h3.has(want)) yield E(doc.rel, outcome.start + 1, `Decision outcome lacks the H3 '${want}'`);
      }
    }
    const band = common.length_words;
    if (band) {
      const n = doc.wordCount();
      if ((band.min != null && n < band.min) || (band.max != null && n > band.max)) yield E(doc.rel, 0, `ADR body has ${n} words; the band is ${band.min} to ${band.max}`);
    }
    if (index && !index.linksTo(doc.rel)) yield E(doc.rel, 0, `not linked from ${ctx.adrIndexRel}`);
    const referenced = contentDocs.some((d) => d.body.includes(id) || d.linksTo(doc.rel));
    if (!referenced) yield ctx.aggregateSev(doc.rel, 0, `${id} is not referenced from any non-ADR document`);
    if (spec?.owning_doc) {
      const owner = ctx.getDoc(spec.owning_doc);
      if (owner && !owner.linksTo(doc.rel)) yield W(doc.rel, 0, `owning document ${spec.owning_doc} does not link this ADR`);
    }
  }

  // Every ADR-NNNN mention in any document must resolve to a manifest ADR.
  for (const doc of ctx.scanned) {
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      for (const m of doc.lines[i].matchAll(ADR_MENTION_RE)) {
        if (!ctx.adrById.has(m[0])) yield E(doc.rel, i + 1, `${m[0]} does not resolve to an ADR in the manifest`);
      }
    }
  }

  // ADR index table: required columns, one row per ADR, statuses match front matter.
  if (index) {
    const cols = (ctx.manifest.adr_index?.table_columns || ['ID', 'Title', 'Status', 'Date', 'Owning document']).map(String);
    const table = index.tables.find((t) => arraysEqual(t.header.map((h) => normalizeText(h)), cols.map((c) => normalizeText(c))));
    if (!table) yield E(ctx.adrIndexRel, 0, `no table with columns ${cols.join(' | ')}`);
    else {
      const si = colIndex(table, 'Status');
      const listed = new Set();
      for (const r of table.rows) {
        const id = (stripMarkdown(r.cells[0]).match(/ADR-\d{4}/) || [])[0];
        if (!id) { yield E(ctx.adrIndexRel, r.line + 1, 'row does not start with an ADR ID'); continue; }
        listed.add(id);
        const st = stripMarkdown(r.cells[si] ?? '').trim();
        if (si >= 0 && statusById.has(id) && st !== statusById.get(id)) yield E(ctx.adrIndexRel, r.line + 1, `${id} status '${st}' differs from its front matter status '${statusById.get(id)}'`);
      }
      for (const a of ctx.adrSpecs) if (!listed.has(a.id)) yield E(ctx.adrIndexRel, table.headerLine + 1, `${a.id} has no row in the ADR index table`);
    }
  }
}

// ---------------------------------------------------------------------------
// Check: parity
// ---------------------------------------------------------------------------

function fuzzyContains(src, candidates) {
  const n = normLoose(src);
  if (!n) return true;
  const st = n.split(' ');
  for (const c of candidates) {
    if (c.loose === n || c.loose.includes(n)) return true;
    if (st.filter((t) => c.tokens.has(t)).length / st.length >= 0.8) return true;
  }
  return false;
}

function* checkParity(ctx) {
  const rel = ctx.special('parity');
  const doc = ctx.getDoc(rel);
  if (!doc) return;
  const spec = ctx.specByRel.get(rel) || {};
  const mcs = spec.machine_checks || [];
  const statusMc = mcs.find((m) => m.kind === 'cell_values_in' && [].concat(m.columns || []).some((c) => normalizeText(c) === 'ruralz status'));
  const allowed = new RegExp(statusMc?.allowed_pattern ?? DEFAULT_PARITY_STATUS);
  const totalMc = mcs.find((m) => m.kind === 'min_table_rows_total');
  const floor = totalMc?.value ?? 120;
  const categories = totalMc?.table_headings ?? spec.outline_h2 ?? [];
  const coverMc = mcs.find((m) => m.kind === 'covers_source_rows');
  const summaryMc = mcs.find((m) => m.kind === 'summary_counts_match');

  // No empty cells in any table; Ruralz status cells of the category tables in the allowed set
  // (the Summary counts table also has a status column, with a Total row).
  const catSections = categories.map((h) => doc.section(h)).filter(Boolean);
  const inCategory = (t) => (catSections.length ? catSections.some((s) => t.headerLine > s.start && t.headerLine < s.end) : true);
  for (const t of doc.tables) {
    const si = inCategory(t) ? colIndex(t, 'Ruralz status') : -1;
    for (const r of t.rows) {
      const blanks = r.cells.map((c, k) => (!c.trim() ? t.header[k] || `column ${k + 1}` : null)).filter(Boolean);
      if (blanks.length) yield E(rel, r.line + 1, `empty cell(s): ${blanks.join(', ')}`);
      if (si >= 0) {
        const v = r.cells[si].trim();
        if (v && !allowed.test(v)) yield E(rel, r.line + 1, `Ruralz status '${v}' is not allowed (${allowed.source})`);
      }
    }
  }
  // Row floor across the category tables.
  let rows = 0;
  for (const h of categories) {
    const sec = doc.section(h);
    if (sec) rows += doc.tablesIn(sec).reduce((a, t) => a + t.rows.length, 0);
  }
  if (rows < floor) yield E(rel, 0, `${rows} feature rows across the category tables; at least ${floor} required`);

  // Every research feature row appears (fuzzy, case-insensitive).
  const sourceRel = coverMc?.source ?? 'docs/_meta/research/krakend-parity.md';
  const source = ctx.getDoc(sourceRel);
  if (!source) yield W(rel, 0, `research source ${sourceRel} not found; feature coverage not checked`);
  else {
    const column = coverMc?.column ?? 'Feature';
    const candidates = [];
    for (const t of doc.tables) {
      const ci = colIndex(t, column);
      if (ci < 0) continue;
      for (const r of t.rows) { const loose = normLoose(r.cells[ci]); candidates.push({ loose, tokens: new Set(loose.split(' ')) }); }
    }
    for (const t of source.tables) {
      const ci = colIndex(t, column);
      if (ci < 0) continue;
      for (const r of t.rows) {
        if (!fuzzyContains(r.cells[ci], candidates)) yield E(rel, 0, `KrakenD feature '${stripMarkdown(r.cells[ci]).trim()}' (${sourceRel}:${r.line + 1}) does not appear in the matrix`);
      }
    }
  }
  // Summary counts equal computed counts.
  const smc = summaryMc ?? { section: 'Summary counts', column: 'Ruralz status', table_headings: categories };
  if (doc.section(smc.section)) yield* summaryCountFindings(doc, smc, 'summary counts', W);
  else yield W(rel, 0, `no '${smc.section}' section to compare with computed counts`);
}

// ---------------------------------------------------------------------------
// Check: glossary
// ---------------------------------------------------------------------------

function termRegex(term) {
  return new RegExp(`(?<![\\p{L}\\p{N}_])${escapeRegExp(term)}(?:s|es)?(?![\\p{L}\\p{N}_])`, 'iu');
}

function* checkGlossary(ctx) {
  const terms = (ctx.manifest.glossary_terms || []).map(String);
  const glossRel = ctx.special('glossary');
  const gloss = ctx.getDoc(glossRel);
  const others = ctx.scanned.filter((d) => d.rel !== glossRel);

  // 1. Each term is used in at least one other document.
  const unused = terms.filter((t) => {
    const re = termRegex(t);
    return !others.some((d) => re.test(d.body));
  });
  if (unused.length && ctx.missing.length) {
    yield W(glossRel, 0, `${unused.length} glossary term(s) not yet used outside the glossary (doc set incomplete): ${unused.join(', ')}`);
  } else {
    for (const t of unused) yield E(glossRel, 0, `glossary term '${t}' is not used in any document other than the glossary`);
  }

  // 2. Each term is defined in the glossary's Terms table.
  if (gloss) {
    const sec = gloss.section('Terms');
    const firstCells = [];
    for (const t of sec ? gloss.tablesIn(sec) : gloss.tables) for (const r of t.rows) firstCells.push(normalizeText(r.cells[0]));
    for (const t of terms) {
      const n = normalizeText(t);
      if (!firstCells.some((c) => c === n || c.startsWith(`${n} `) || c.startsWith(`${n}(`))) {
        yield E(glossRel, sec ? sec.start + 1 : 0, `glossary term '${t}' is not defined in the Terms table`);
      }
    }
  }

  // 3. Forbidden aliases: prose, inline code and mermaid; other fenced code is skipped.
  const am = ctx.manifest.alias_match || {};
  const escape = am.escape_comment || '<!-- alias-ok -->';
  const flags = am.case_insensitive === false ? 'g' : 'gi';
  const patterns = [];
  for (const fa of ctx.manifest.forbidden_aliases || []) {
    try { patterns.push({ re: new RegExp(fa.pattern, flags), use: fa.use }); } catch (e) { yield W(normRel(path.relative(ctx.root, ctx.manifestPath)), 0, `invalid forbidden_aliases pattern ${fa.pattern}: ${e.message}`); }
  }
  for (const doc of ctx.scanned) {
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      const info = doc.info[i];
      if (info.code && (info.delim || info.fence?.lang !== 'mermaid')) continue;
      const raw = doc.lines[i];
      if (raw.includes(escape)) continue;
      const line = raw.replace(/\]\([^)]*\)/g, ']').replace(/https?:\/\/\S+/g, ' ');
      for (const p of patterns) {
        for (const m of line.matchAll(p.re)) yield E(doc.rel, i + 1, `forbidden alias '${m[0]}' (use ${p.use}, or add ${escape} on the line for an unavoidable quotation)`);
      }
    }
  }

  // 4. Local one-line definitions of glossary terms in other documents (human review).
  const termSet = new Map(terms.map((t) => [t.toLowerCase(), t]));
  const defRes = [
    /^\s*(?:[-*+]\s+|\d+[.)]\s+)?\*\*([^*]+?)\*\*\s*(?::|—|–|\s-\s)\s*\S/,
    /^\s*(?:[-*+]\s+|\d+[.)]\s+)?\*\*([^*]+?):\*\*\s*\S/,
    /^\s*(?:[-*+]\s+|\d+[.)]\s+)?([A-Z][\w-]*(?:\s[A-Z][\w-]*){0,3})\s+[—–]\s+\S/,
  ];
  for (const doc of others) {
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      if (!doc.isProse(i)) continue;
      for (const re of defRes) {
        const m = re.exec(doc.lines[i]);
        if (!m) continue;
        const key = m[1].trim().toLowerCase();
        const term = termSet.get(key) || termSet.get(key.replace(/(?:es|s)$/, ''));
        if (term) { yield W(doc.rel, i + 1, `local definition of glossary term '${term}'; make sure it agrees with ${glossRel}`); break; }
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Check: scalability
// ---------------------------------------------------------------------------

function* checkScalability(ctx) {
  const rel = ctx.special('scalability');
  const doc = ctx.getDoc(rel);
  if (!doc) return;
  const norm = (s) => normalizeText(s).replace(/[-‐‑–—]/g, ' ').replace(/\s+/g, ' ').trim();
  const heads = doc.headings.filter((h) => h.level === 2 || h.level === 3).map((h) => norm(h.plain));
  const linkLines = new Set(doc.links.filter((l) => !l.ref).map((l) => l.line));
  for (const item of ctx.manifest.scalability_items || []) {
    const it = norm(item);
    if (heads.some((h) => h.includes(it))) continue;
    let pointer = false;
    for (const i of linkLines) if (norm(doc.lines[i]).includes(it)) { pointer = true; break; }
    if (!pointer) yield E(rel, 0, `scalability item '${item}' is neither an H2/H3 heading nor a pointer sentence with a link`);
  }
}

// ---------------------------------------------------------------------------
// Check: numbers
// ---------------------------------------------------------------------------

function* checkNumbers(ctx) {
  for (const doc of ctx.scanned) {
    if (!NUMBER_DIRS.some((d) => doc.rel.startsWith(d))) continue;
    const skip = new Set();
    for (const t of doc.tables) { skip.add(t.headerLine); skip.add(t.sepLine); }
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      if (!doc.isProse(i) || skip.has(i)) continue;
      const line = doc.lines[i];
      if (line.includes('(target)') || line.includes('(hypothesis)') || /https?:\/\//.test(line)) continue;
      // Inline code holds literal syntax (`250ms` durations, `timeout: 3s`), not claims.
      const m = NUM_UNIT_RE.exec(maskInlineCode(maskHtmlComments(line)));
      if (m) yield E(doc.rel, i + 1, `figure '${m[0].trim()}' needs (target), (hypothesis) or a measurement URL on the same line`);
    }
  }
}

// ---------------------------------------------------------------------------
// Check: milestones
// ---------------------------------------------------------------------------

function* checkMilestones(ctx) {
  const valid = new Set(ctx.milestones);
  const used = new Set();
  const range = `${ctx.milestones[0] ?? 'M0'}..${ctx.milestones[ctx.milestones.length - 1] ?? 'M5'}`;
  for (const doc of ctx.scanned) {
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      const line = doc.lines[i];
      for (const m of line.matchAll(/\bM(\d+)\b/g)) {
        const tag = `M${m[1]}`;
        if (valid.has(tag)) used.add(tag); else yield E(doc.rel, i + 1, `milestone tag ${tag} is not one of ${range}`);
      }
      for (const m of line.matchAll(/Planned \(([^)]*)\)/g)) {
        const inner = m[1].trim();
        if (!valid.has(inner) && inner !== 'Mx' && !/^M\d+$/.test(inner)) yield E(doc.rel, i + 1, `'Planned (${inner})' must name a milestone ${range}`);
      }
    }
  }

  const roadmapRel = ctx.special('roadmap');
  const roadmap = ctx.getDoc(roadmapRel);
  if (!roadmap) return;
  const sections = new Map();
  for (const h of roadmap.headings) {
    const m = h.level === 2 && /^(M\d+)\b/.exec(h.text);
    if (m && !sections.has(m[1])) sections.set(m[1], roadmap.section(h.text));
  }
  for (const ms of [...used].sort()) if (!sections.has(ms)) yield E(roadmapRel, 0, `roadmap has no H2 for milestone ${ms}, which the documents use`);

  const sectionTokens = new Map();
  for (const [ms, sec] of sections) {
    const lines = [];
    for (let i = sec.start; i < sec.end; i++) lines.push(new Set(keyTokens(roadmap.lines[i])));
    sectionTokens.set(ms, lines);
  }
  const misses = new Map();
  for (const doc of ctx.scanned) {
    if (doc.rel === roadmapRel) continue;
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      if (!doc.isProse(i)) continue;
      const line = doc.lines[i];
      const pm = /Planned \((M\d+)\)/.exec(line);
      if (!pm || !sectionTokens.has(pm[1])) continue;
      let key = null;
      const tr = doc.tableRowLines.get(i);
      if (tr) key = tr.row.cells.find((c) => c.trim() && !/Planned \(/.test(c)) ?? null;
      else if (LIST_ITEM_RE.test(line)) key = stripMarkdown(line.replace(LIST_ITEM_RE, '').replace(/Planned \(M\d+\)/g, ' ')).split(/\s+/).filter(Boolean).slice(0, 6).join(' ');
      if (!key) continue;
      const tokens = [...new Set(keyTokens(key))];
      if (!tokens.length) continue;
      const hit = sectionTokens.get(pm[1]).some((set) => tokens.filter((t) => set.has(t)).length / tokens.length >= 0.6);
      if (hit) continue;
      const id = `${pm[1]}|${tokens.join(' ')}`;
      if (misses.has(id)) misses.get(id).count++;
      else misses.set(id, { ms: pm[1], key: stripMarkdown(key).trim(), rel: doc.rel, line: i + 1, count: 1 });
    }
  }
  for (const m of misses.values()) {
    yield W(m.rel, m.line, `'${m.key}' is Planned (${m.ms}) but has no matching item under the ${m.ms} section of ${roadmapRel}${m.count > 1 ? ` (${m.count} occurrences)` : ''}`);
  }
}

// ---------------------------------------------------------------------------
// Check: placeholders
// ---------------------------------------------------------------------------

function* checkPlaceholders(ctx) {
  const res = [/\b(?:TODO|TBD|XXX)\b/g, /lorem/gi, /\?\?\?/g];
  for (const doc of ctx.scanned) {
    for (let i = 0; i < doc.lines.length; i++) {
      for (const re of res) for (const m of doc.lines[i].matchAll(re)) yield E(doc.rel, i + 1, `placeholder '${m[0]}'; move unknowns to Open questions`);
    }
  }
}

// ---------------------------------------------------------------------------
// Check: commands-kinds
// ---------------------------------------------------------------------------

// Resource-level `kind:` keys of a yaml block whose resource has no apiVersion or a ruralz/* one.
function yamlResourceKinds(lines) {
  const out = [];
  const groups = [[]];
  for (let k = 0; k < lines.length; k++) {
    if (/^---\s*$/.test(lines[k])) { groups.push([]); continue; }
    groups[groups.length - 1].push(k);
  }
  for (const g of groups) {
    const resources = [];
    let cur = null;
    for (const k of g) {
      const l = lines[k];
      if (/^\s*(#.*)?$/.test(l)) continue;
      if (/^- \S/.test(l)) { cur = { indent: 2, keys: [] }; resources.push(cur); }
      else if (/^\S/.test(l) && (!cur || cur.indent !== 0)) { cur = { indent: 0, keys: [] }; resources.push(cur); }
      if (!cur) continue;
      const m = cur.indent === 2 ? /^(?:- | {2})([A-Za-z]\w*):\s*(.*)$/.exec(l) : /^([A-Za-z]\w*):\s*(.*)$/.exec(l);
      if (m) cur.keys.push({ k, key: m[1], value: m[2].replace(/\s+#.*$/, '').replace(/^["']|["']$/g, '').trim() });
    }
    for (const r of resources) {
      const api = r.keys.find((x) => x.key === 'apiVersion');
      if (api && !/^ruralz/.test(api.value)) continue;
      for (const x of r.keys) if (x.key === 'kind') out.push({ k: x.k, kind: x.value });
    }
  }
  return out;
}

function* checkCommandsKinds(ctx) {
  const cliRel = ctx.special('cli');
  const cli = ctx.getDoc(cliRel);
  const cliText = cli ? cli.body.replace(/\s+/g, ' ') : null;
  const kinds = new Set(ctx.kinds);
  const cmdRe = /(?<![\w./-])ruralz\s+([a-z][a-z0-9-]*)\s+([a-z][a-z0-9-]*)/g;
  const dpRel = ctx.special('dataPlane');
  const dp = ctx.getDoc(dpRel);
  const adminSec = dp?.section('Admin endpoints');
  const adminText = adminSec ? dp.lines.slice(adminSec.start, adminSec.end).join('\n') : null;
  const adminSeen = new Set();
  const portPathRe = new RegExp(`:${ADMIN_PORT}(/[\\w\\-./*{}]*)`, 'g');

  for (const doc of ctx.scanned) {
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      const info = doc.info[i];
      const line = doc.lines[i];
      // CLI commands in inline code spans and in shell code blocks.
      if (cliText && doc.rel !== cliRel) {
        const sources = [];
        if (!info.code) sources.push(...inlineCodeSpans(line));
        else if (!info.delim && SHELL_LANGS.has(info.fence.lang)) sources.push(line.replace(/^\s*(?:\$|>|PS>)\s+/, ''));
        for (const s of sources) {
          for (const m of s.matchAll(cmdRe)) {
            const cmd = `ruralz ${m[1]} ${m[2]}`;
            if (!cliText.includes(cmd)) yield E(doc.rel, i + 1, `command '${cmd}' is not documented in ${cliRel}`);
          }
        }
      }
      // ruralzd admin paths: the known ones anywhere, plus any /path tied to port 9901.
      if (adminText && doc.rel !== dpRel) {
        const found = [];
        for (const p of ADMIN_PATHS) {
          const re = new RegExp(`(?<![\\w/])${escapeRegExp(p)}${p.endsWith('/') ? '[\\w*{}\\-./]*' : '(?![\\w/-])'}`, 'g');
          for (const m of line.matchAll(re)) found.push(m[0]);
        }
        if (line.includes(ADMIN_PORT)) {
          for (const m of line.matchAll(portPathRe)) if (m[1].length > 1) found.push(m[1]);
          if (!info.code) for (const s of inlineCodeSpans(line)) if (/^\/\w[\w\-./*{}]*$/.test(s)) found.push(s);
        }
        for (const raw of found) {
          const p = raw.replace(/[.,;:]+$/, '');
          const covered = adminText.includes(p) || (p.startsWith('/debug/') && adminText.includes('/debug/'));
          if (!covered && !adminSeen.has(p)) {
            adminSeen.add(p);
            yield W(doc.rel, i + 1, `admin path '${p}' is not listed in the Admin endpoints section of ${dpRel}`);
          }
        }
      }
    }
    // kind: values in yaml code blocks.
    for (const f of doc.fences) {
      if (f.lang !== 'yaml' && f.lang !== 'yml') continue;
      for (const { k, kind } of yamlResourceKinds(doc.lines.slice(f.start + 1, f.end))) {
        if (!kinds.has(kind)) yield E(doc.rel, f.start + 2 + k, `kind '${kind}' is not a Ruralz kind (${[...kinds].join(', ')})`);
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Check: citations
// ---------------------------------------------------------------------------

function* checkCitations(ctx) {
  const allowed = new Set();
  const addFrom = (text) => { for (const u of extractUrls(text)) allowed.add(normalizeUrl(u)); };
  const walk = (dir) => {
    let entries;
    try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
    for (const ent of entries) {
      const abs = path.join(dir, ent.name);
      if (ent.isDirectory()) walk(abs);
      else if (ent.isFile()) { try { addFrom(fs.readFileSync(abs, 'utf8')); } catch { /* unreadable */ } }
    }
  };
  walk(path.join(ctx.root, 'docs', '_meta', 'research'));
  const foundation = ctx.getDoc('docs/_meta/foundation-pack.md');
  if (foundation) addFrom(foundation.text);

  const techRel = ctx.special('techStack');
  for (const doc of ctx.scanned) {
    if (!doc.rel.startsWith('docs/comparison/') && doc.rel !== techRel) continue;
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      if (!doc.isProse(i)) continue;
      for (const u of extractUrls(doc.lines[i])) {
        if (LOCAL_HOST_RE.test(urlHost(u))) continue;
        if (!allowed.has(normalizeUrl(u))) yield E(doc.rel, i + 1, `URL ${u} does not appear in any file under docs/_meta/research/`);
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Check: index
// ---------------------------------------------------------------------------

function* checkIndex(ctx) {
  const indexRel = ctx.special('docsIndex');
  const index = ctx.getDoc(indexRel);
  if (!index) return;
  const expected = ctx.scanned.map((d) => d.rel).filter((rel) => rel.startsWith('docs/') && rel !== indexRel && !ctx.isAdr(rel));
  const counts = countLinksOutside(index, LINK_ONCE_EXEMPT_SECTIONS);
  for (const rel of expected) {
    const n = counts.get(rel) || 0;
    if (n !== 1) yield E(indexRel, 0, `${rel} is linked ${n} times (outside Reading paths); expected exactly once`);
  }

  // Status table: every document with front matter appears with its current status.
  const sec = index.section('Document status');
  const candidates = (sec ? index.tablesIn(sec) : index.tables).filter((t) => colIndex(t, 'Status') >= 0);
  if (!candidates.length) { yield E(indexRel, sec ? sec.start + 1 : 0, 'no status table (a table with a Status column) found'); return; }
  const rows = [];
  for (const t of candidates) {
    const si = colIndex(t, 'Status');
    for (const r of t.rows) {
      const targets = index.links.filter((l) => l.line === r.line && !l.resolved.external).map((l) => l.resolved.rel);
      rows.push({ r, status: stripMarkdown(r.cells[si]).trim(), targets, cells: r.cells.map((c) => normalizeText(c)) });
    }
  }
  for (const doc of ctx.scanned) {
    if (!doc.rel.startsWith('docs/') || ctx.isAdr(doc.rel) || doc.rel === ctx.adrIndexRel || doc.rel === indexRel) continue;
    const st = doc.fm.data?.status;
    if (!st) continue;
    const title = doc.fm.data?.title ? normalizeText(doc.fm.data.title) : null;
    const row = rows.find((x) => x.targets.includes(doc.rel) || x.cells.includes(doc.rel.toLowerCase()) || (title && x.cells.includes(title)));
    if (!row) yield E(indexRel, sec ? sec.start + 1 : 0, `${doc.rel} has no row in the status table`);
    else if (row.status !== String(st)) yield E(indexRel, row.r.line + 1, `status for ${doc.rel} is '${row.status}' but its front matter says '${st}'`);
  }
}

// ---------------------------------------------------------------------------
// Check: phases
// ---------------------------------------------------------------------------

const PHASE_TOKEN_RE = /\bon[A-Z][A-Za-z]+\b/g;

// Longest run of phase tokens separated only by list separators (arrows, commas, pipes, "and").
function longestPhaseRun(line) {
  const toks = [...line.matchAll(PHASE_TOKEN_RE)];
  let best = [];
  let cur = [];
  toks.forEach((t, k) => {
    const between = k ? line.slice(toks[k - 1].index + toks[k - 1][0].length, t.index) : '';
    if (k && /^(?:\s|[`*_,;|/→>=-]|and|then|or)*$/.test(between)) cur.push(t[0]); else cur = [t[0]];
    if (cur.length > best.length) best = [...cur];
  });
  return best;
}

function leadingPhase(line) {
  const s = line.replace(LIST_ITEM_RE, '').replace(/^\s*\|\s*/, '')
    .replace(/^[\s`*_[(]+/, '').replace(/^\d+[.)]\s*/, '').replace(/^[\s`*_[(]+/, '');
  const m = /^on[A-Z][A-Za-z]+/.exec(s);
  return m ? m[0] : null;
}

// A listing is either one line with a run of at least four phase tokens joined by separators
// (arrows, commas) or a list/table block whose items start with a phase. Prose mentions of a
// few phases and fenced code (diagrams, plugin manifests with partial phase lists) are ignored.
// Rules for a listing: every token is a manifest phase; phases follow the manifest order; a flat
// listing is a contiguous slice of the manifest list (a partial slice such as "the request
// phases" is fine, skipping a phase is not); a listing of all core phases must name onChunk in
// the listing or within a few lines. Tables grouped by phase (consecutive rows repeating a
// phase, as in worked examples) only get the order check.
function* checkPhases(ctx) {
  const expected = (ctx.manifest.filter_chain_phases || []).map(String);
  if (!expected.length) return;
  const pos = new Map(expected.map((p, k) => [p, k]));
  const core = expected.filter((p) => p !== 'onChunk');
  const trigger = expected[0];
  for (const doc of ctx.scanned) {
    for (let i = doc.bodyStart; i < doc.lines.length; i++) {
      if (!doc.isProse(i) || !doc.lines[i].includes(trigger)) continue;
      const line = doc.lines[i];
      const tokens = longestPhaseRun(line);
      let seq = null;
      let end = i;
      if (tokens.length >= 4) seq = tokens;
      else if (leadingPhase(line) === trigger && (LIST_ITEM_RE.test(line) || /^\s*\|/.test(line))) {
        seq = [];
        for (let j = i; j < doc.lines.length && doc.isProse(j) && doc.lines[j].trim(); j++) {
          const l = doc.lines[j];
          if (!(LIST_ITEM_RE.test(l) || /^\s*\|/.test(l))) break;
          const p = leadingPhase(l);
          if (p) seq.push(p);
          end = j;
        }
        if (seq.length < 4) seq = null;
      }
      if (!seq) continue;
      const at = i + 1;
      i = end;
      const unknown = [...new Set(seq.filter((t) => !pos.has(t)))];
      if (unknown.length) { yield E(doc.rel, at, `unknown Filter Chain phase(s) ${unknown.join(', ')}; the phases are ${expected.join(' → ')}`); continue; }
      const collapsed = seq.filter((t, k) => k === 0 || t !== seq[k - 1]);
      const idx = collapsed.map((t) => pos.get(t));
      if (!idx.every((v, k) => k === 0 || v > idx[k - 1])) {
        yield E(doc.rel, at, `Filter Chain phases out of order: ${collapsed.join(' → ')}; expected ${expected.join(' → ')}`);
        continue;
      }
      if (collapsed.length < seq.length) continue; // grouped by phase: only the order is checkable
      if (!idx.every((v, k) => v === idx[0] + k)) {
        const skipped = expected.slice(idx[0], idx[idx.length - 1] + 1).filter((p) => !collapsed.includes(p));
        yield E(doc.rel, at, `Filter Chain phase listing skips ${skipped.join(', ')}; expected ${expected.join(' → ')}`);
        continue;
      }
      if (pos.has('onChunk') && core.every((p) => collapsed.includes(p)) && !collapsed.includes('onChunk')) {
        const near = doc.lines.slice(at - 1, Math.min(doc.lines.length, end + 6)).some((l) => l.includes('onChunk'));
        if (!near) yield E(doc.rel, at, 'Filter Chain phases listed without the streaming hook onChunk nearby');
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Runner and reporting
// ---------------------------------------------------------------------------

const CHECKS = [
  ['missing-doc', checkMissingDocs],
  ['markdownlint', checkMarkdownlint],
  ['links', checkLinks],
  ['mermaid', checkMermaid],
  ['frontmatter', checkFrontmatter],
  ['outline', checkOutline],
  ['machine-checks', checkMachineChecks],
  ['adr', checkAdr],
  ['parity', checkParity],
  ['glossary', checkGlossary],
  ['scalability', checkScalability],
  ['numbers', checkNumbers],
  ['milestones', checkMilestones],
  ['placeholders', checkPlaceholders],
  ['commands-kinds', checkCommandsKinds],
  ['citations', checkCitations],
  ['index', checkIndex],
  ['phases', checkPhases],
];
const CHECK_NAMES = CHECKS.map(([n]) => n);

function parseArgs(argv) {
  const opts = { only: [], skip: [], json: false, fixNone: false, allowMissing: false, root: path.resolve(SCRIPT_DIR, '..'), manifest: null, help: false };
  for (let i = 0; i < argv.length; i++) {
    let a = argv[i];
    let val = null;
    const eq = a.indexOf('=');
    if (a.startsWith('--') && eq > 0) { val = a.slice(eq + 1); a = a.slice(0, eq); }
    const next = () => {
      if (val != null) return val;
      if (i + 1 >= argv.length) throw new UsageError(`${a} needs a value`);
      return argv[++i];
    };
    switch (a) {
      case '--only': opts.only.push(...next().split(',').map((s) => s.trim()).filter(Boolean)); break;
      case '--skip': opts.skip.push(...next().split(',').map((s) => s.trim()).filter(Boolean)); break;
      case '--json': opts.json = true; break;
      case '--fix-none': opts.fixNone = true; break;
      case '--allow-missing': opts.allowMissing = true; break;
      case '--root': opts.root = path.resolve(next()); break;
      case '--manifest': opts.manifest = path.resolve(next()); break;
      case '-h': case '--help': opts.help = true; break;
      default: throw new UsageError(`unknown argument '${argv[i]}'`);
    }
  }
  for (const n of [...opts.only, ...opts.skip]) if (!CHECK_NAMES.includes(n)) throw new UsageError(`unknown check '${n}' (known: ${CHECK_NAMES.join(', ')})`);
  return opts;
}

async function runChecks(ctx) {
  const only = new Set(ctx.opts.only);
  const skip = new Set(ctx.opts.skip);
  const results = [];
  for (const [name, fn] of CHECKS) {
    if ((only.size && !only.has(name)) || skip.has(name)) { results.push({ check: name, skipped: true, findings: [] }); continue; }
    const findings = [];
    try {
      for await (const f of fn(ctx)) if (f) findings.push({ check: name, file: f.file, line: f.line, message: f.message, severity: f.severity });
    } catch (e) {
      findings.push({ check: name, file: '-', line: 0, message: `internal error: ${String(e?.stack ?? e).split('\n').slice(0, 3).join(' | ')}`, severity: 'error' });
    }
    results.push({ check: name, skipped: false, findings });
  }
  return results;
}

function summarize(results) {
  return results.map((r) => {
    const errors = r.findings.filter((f) => f.severity === 'error').length;
    return { check: r.check, status: r.skipped ? 'SKIP' : errors ? 'FAIL' : 'PASS', errors, warnings: r.findings.length - errors };
  });
}

function formatText(results, summary, ctx) {
  const out = [];
  out.push(`verify-docs: root ${ctx.root}`);
  out.push(`manifest ${ctx.manifestPath}; ${ctx.scanned.length} Markdown file(s) scanned, ${ctx.missing.length} manifest file(s) missing`);
  out.push('');
  for (const r of results) {
    if (r.skipped) continue;
    const errs = r.findings.filter((f) => f.severity === 'error');
    const warns = r.findings.filter((f) => f.severity === 'warn');
    if (!errs.length) out.push(`PASS ${r.check}`);
    for (const f of errs) out.push(`FAIL ${r.check} ${f.file}:${f.line} ${f.message}`);
    for (const f of warns) out.push(`WARN ${r.check} ${f.file}:${f.line} ${f.message}`);
    out.push('');
  }
  const w = Math.max(5, ...summary.map((s) => s.check.length));
  const rule = `${'-'.repeat(w)}  ------  ------  --------`;
  out.push('Summary');
  out.push(`${'check'.padEnd(w)}  result  errors  warnings`);
  out.push(rule);
  for (const s of summary) out.push(`${s.check.padEnd(w)}  ${s.status.padEnd(6)}  ${String(s.errors).padStart(6)}  ${String(s.warnings).padStart(8)}`);
  const te = summary.reduce((a, s) => a + s.errors, 0);
  const tw = summary.reduce((a, s) => a + s.warnings, 0);
  out.push(rule);
  out.push(`${'TOTAL'.padEnd(w)}  ${(te ? 'FAIL' : 'PASS').padEnd(6)}  ${String(te).padStart(6)}  ${String(tw).padStart(8)}`);
  return out.join('\n');
}

const HELP = `Usage: node scripts/verify-docs.mjs [--only <check>[,...]] [--skip <check>[,...]] [--json]
       [--fix-none] [--allow-missing] [--root <dir>] [--manifest <path>]
Checks: ${CHECK_NAMES.join(', ')}`;

export async function main(argv = process.argv.slice(2)) {
  let opts;
  let ctx;
  try {
    opts = parseArgs(argv);
    if (opts.help) { console.log(HELP); return 0; }
    ctx = loadContext(opts);
  } catch (e) {
    if (e instanceof UsageError) { console.error(`verify-docs: ${e.message}\n${HELP}`); return 2; }
    throw e;
  }
  const results = await runChecks(ctx);
  const summary = summarize(results);
  const errors = summary.reduce((a, s) => a + s.errors, 0);
  if (opts.json) {
    const findings = results.flatMap((r) => r.findings);
    console.log(JSON.stringify({ root: ctx.root, manifest: ctx.manifestPath, ok: errors === 0, summary, findings }, null, 2));
  } else {
    console.log(formatText(results, summary, ctx));
  }
  return errors ? 1 : 0;
}

export { CHECK_NAMES, parseMarkdownlintOutput, githubSlug, countWords, splitTableRow, resolveTarget, NUM_UNIT_RE };

const invokedDirectly = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invokedDirectly) {
  main().then((code) => { process.exitCode = code; }, (e) => { console.error(e?.stack ?? e); process.exitCode = 2; });
}
