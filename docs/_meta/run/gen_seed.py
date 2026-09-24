#!/usr/bin/env python3
"""Build the label-keyed resume seed for docs/_meta/run/ruralz-continue.template.js.

  gen_seed.py init  <old_run_dir>      choose, per item, the latest consistent chain of stage results from
                                       the old run (exact prompt-lineage check) and write seed.json
  gen_seed.py merge <new_run_dir> ...  add results of later runs of ruralz-continue (labels are unique per run)
  gen_seed.py emit  <out.js>           slim the seed and write the runnable workflow script

A run dir holds journal.jsonl ({type: started|result|failed, key, agentId, label?, result?}) and agent-<id>.jsonl.
"""
import glob, json, os, re, sys

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, '..', '..', '..'))
SEED_PATH = os.path.join(HERE, 'seed.json')
TEMPLATE = os.path.join(HERE, 'ruralz-continue.template.js')


def dumps(x):  # byte-identical to JS JSON.stringify for these objects
    return json.dumps(x, ensure_ascii=False, separators=(',', ':'))


def items():
    import yaml
    m = yaml.safe_load(open(os.path.join(REPO, 'docs/_meta/manifest.yaml'), encoding='utf-8'))
    out = [{'s': d['slug'], 'p': d['path'], 'l': d['lenses'], 'kind': 'doc'} for d in m['docs'] if d.get('wave') in (2, 3)]
    out += [{'s': a['id'].lower(), 'p': a['path'], 'l': ['L1', 'L2'], 'kind': 'adr'} for a in m['adrs']]
    return out


def load_run(d):
    rows = [json.loads(l) for l in open(os.path.join(d, 'journal.jsonl'), encoding='utf-8') if l.strip()]
    k2l = {r['key']: r['label'] for r in rows if r['type'] == 'started' and 'label' in r}
    res = []
    for r in rows:
        if r['type'] != 'result':
            continue
        aid = r['agentId']
        end, prompt = '', None
        p = os.path.join(d, f'agent-{aid}.jsonl')
        if os.path.exists(p):
            for line in open(p, encoding='utf-8'):
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                if e.get('timestamp'):
                    end = e['timestamp']
                c = (e.get('message') or {}).get('content')
                txt = c if isinstance(c, str) else ' '.join(b.get('text', '') for b in (c or []) if isinstance(b, dict))
                if prompt is None and e.get('type') == 'user' and 'computed task' in txt[:200]:
                    prompt = txt
        res.append({'label': k2l.get(r['key'], '?'), 'end': end, 'result': r['result'], 'prompt': prompt or ''})
    return res


def init(old_dir):
    res = load_run(old_dir)
    by = {}
    for r in res:
        by.setdefault(r['label'], []).append(r)
    for v in by.values():
        v.sort(key=lambda r: r['end'])

    def latest(label, after='', before='~'):
        c = [r for r in by.get(label, []) if after < r['end'] < before]
        return c[-1] if c else None

    seed, report = {}, []
    for it in items():
        s, Ls = it['s'], it['l']
        if not os.path.exists(os.path.join(REPO, it['p'])):
            report.append((s, 'write'))
            continue
        w = latest(f'write:{s}')
        if not w:
            report.append((s, 'write (file exists but no result: will be rewritten)'))
            continue
        seed[f'write:{s}'] = w['result']
        rv = latest(f'revise:{s}')
        post = [latest(f'review:{s}:{L}', after=rv['end']) for L in Ls] if rv else [None]
        if rv and all(post):
            revs, rv = post, None                      # fresh review round of the revised file: revise again
        else:
            revs = [latest(f'review:{s}:{L}', before=rv['end'] if rv else '~') for L in Ls]
        if not all(revs):
            report.append((s, 'review'))
            continue
        for L, r in zip(Ls, revs):
            seed[f'review:{s}:{L}'] = r['result']
        blocking = [b for r in revs for b in r['result']['blocking']]
        if not blocking:
            report.append((s, 'finalize (no blocking)'))
            continue
        if not rv:
            report.append((s, 'revise'))
            continue
        assert dumps(blocking) in rv['prompt'], f'{s}: revise prompt does not quote the chosen review round'
        seed[f'revise:{s}'] = rv['result']
        bl = [L for L, r in zip(Ls, revs) if r['result']['blocking']]
        rr = {}
        for L in bl:
            prev = dumps(revs[Ls.index(L)]['result']['blocking'])
            c = [r for r in by.get(f'rereview:{s}:{L}', []) if r['end'] > rv['end'] and prev in r['prompt']]
            if c:
                rr[L] = c[-1]
        if len(rr) < len(bl):
            for L, r in rr.items():                     # a partial set is still a valid prefix per label
                seed[f'rereview:{s}:{L}'] = r['result']
            report.append((s, f'rereview {[L for L in bl if L not in rr]}'))
            continue
        for L, r in rr.items():
            seed[f'rereview:{s}:{L}'] = r['result']
        b2 = [b for L in bl for b in rr[L]['result']['blocking']]
        if not b2:
            report.append((s, 'finalize (rereview clean)'))
            continue
        t2 = max(r['end'] for r in rr.values())
        c = [r for r in by.get(f'revise2:{s}', []) if r['end'] > t2 and dumps(b2) in r['prompt']]
        if not c:
            report.append((s, 'revise2'))
            continue
        seed[f'revise2:{s}'] = c[-1]['result']
        report.append((s, f"finalize (revise2 disputed {[d['id'][:12] for d in c[-1]['result']['disputed']]})"))
    json.dump(seed, open(SEED_PATH, 'w', encoding='utf-8'), ensure_ascii=False, indent=0)
    for s, n in report:
        print(f'{s:42} next: {n}')
    print(f'seed labels: {len(seed)}')


def merge(dirs):
    seed = json.load(open(SEED_PATH, encoding='utf-8'))
    added = 0
    for d in dirs:
        for r in load_run(d):
            if r['label'] in seed:
                print(f"WARN {r['label']} already seeded; keeping the earlier result")
                continue
            seed[r['label']] = r['result']
            added += 1
    json.dump(seed, open(SEED_PATH, 'w', encoding='utf-8'), ensure_ascii=False, indent=0)
    print(f'added {added}; seed labels: {len(seed)}')


def slim(seed):
    """Keep full findings only where a stage that has not run yet will quote them."""
    out = {}
    for label, v in seed.items():
        parts = label.split(':')
        stage, s = parts[0], parts[1] if len(parts) > 1 else ''
        if isinstance(v, dict) and 'blocking' in v:
            L = parts[2]
            if stage == 'review':
                needed = f'revise:{s}' not in seed or (v['blocking'] and f'rereview:{s}:{L}' not in seed)
            else:  # rereview
                needed = f'revise2:{s}' not in seed
            keep = {'lens': v['lens'], 'verdict': v['verdict'], 'score': v['score']}
            if needed:
                keep['blocking'] = v['blocking']
                if stage == 'review' and f'revise:{s}' not in seed:
                    keep['non_blocking'] = v.get('non_blocking', [])
            else:
                keep['blocking'] = [{'id': b['id']} for b in v['blocking']]
            out[label] = keep
        elif stage == 'write':
            out[label] = {'path': v.get('path', ''), 'word_count': v.get('word_count', 0)}
        else:
            out[label] = v
    return out


def emit(out_js):
    seed = slim(json.load(open(SEED_PATH, encoding='utf-8')))
    t = open(TEMPLATE, encoding='utf-8').read()
    assert '/*__SEED__*/{}' in t
    js = t.replace('/*__SEED__*/{}', dumps(seed))
    assert '\r' not in js
    open(out_js, 'w', encoding='utf-8', newline='\n').write(js)
    print(f'wrote {out_js}: {len(js)} bytes, {len(seed)} seeded labels')


if __name__ == '__main__':
    cmd = sys.argv[1]
    if cmd == 'init':
        init(sys.argv[2])
    elif cmd == 'merge':
        merge(sys.argv[2:])
    elif cmd == 'emit':
        emit(sys.argv[2])
