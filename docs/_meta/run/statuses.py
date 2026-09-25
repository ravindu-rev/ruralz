#!/usr/bin/env python3
"""Print {path: status} for draft docs whose review chain is complete in seed.json (pipe into apply_status.py)."""
import json, os, re, yaml
R = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..', '..'))
seed = json.load(open(os.path.join(R, 'docs/_meta/run/seed.json'), encoding='utf-8'))
m = yaml.safe_load(open(os.path.join(R, 'docs/_meta/manifest.yaml'), encoding='utf-8'))
ESC = re.compile(r'^L\d[SXC]?-\d+'); out = {}
for d in m['docs']:
    s = d['slug']
    if d.get('wave') not in (2, 3) or not seed.get(f'write:{s}') or not os.path.exists(os.path.join(R, d['path'])): continue
    if 'status: draft' not in open(os.path.join(R, d['path']), encoding='utf-8').read().split('\n---', 1)[0]: continue
    revs = [seed.get(f'review:{s}:{L}') for L in d['lenses']]
    if not all(revs): continue
    bl = [L for L, v in zip(d['lenses'], revs) if v['blocking']]
    rr = [seed.get(f'rereview:{s}:{L}') for L in bl]
    if bl and (f'revise:{s}' not in seed or not all(rr)): continue
    ids = {b['id'] for v in rr for b in v['blocking']}
    if ids and f'revise2:{s}' not in seed: continue
    esc = [x for x in (seed.get(f'revise2:{s}') or {'disputed': []})['disputed'] if ESC.match(str(x['id'])) and ESC.match(str(x['id']))[0] in ids]
    out[d['path']] = 'approved-with-escalations' if esc else 'reviewed'
print(json.dumps(out))
