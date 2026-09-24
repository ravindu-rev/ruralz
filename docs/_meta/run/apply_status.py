#!/usr/bin/env python3
"""Set front-matter status (and last_updated) of docs from a {path: status} JSON map on stdin."""
import json, re, sys, os
REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..', '..'))
today = sys.argv[1]
for path, status in json.load(sys.stdin).items():
    p = os.path.join(REPO, path); t = open(p, encoding='utf-8').read()
    head, sep, body = t[4:].partition('\n---\n')
    head = re.sub(r'(?m)^status: .*$', f'status: {status}', head)
    head = re.sub(r'(?m)^last_updated: .*$', f'last_updated: {today}', head)
    open(p, 'w', encoding='utf-8', newline='\n').write(t[:4] + head + sep + body)
    print(path, status)
