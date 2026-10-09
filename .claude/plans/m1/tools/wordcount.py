import re,sys
def count(text):
    lines=text.split('\n')
    # strip front matter
    if lines and lines[0].strip()=='---':
        for i in range(1,len(lines)):
            if lines[i].strip()=='---':
                lines=lines[i+1:];break
    body='\n'.join(lines)
    body=re.sub(r'<!--.*?-->','',body,flags=re.S)
    out=[];inf=False;fence=None
    for l in body.split('\n'):
        m=re.match(r'^\s*(```+|~~~+)',l)
        if m:
            if not inf: inf=True;fence=m.group(1)[0]*3;continue
            elif l.strip().startswith(fence): inf=False;continue
        if not inf: out.append(l)
    toks=' '.join(out).split()
    return sum(1 for t in toks if re.search(r'[A-Za-z0-9]',t))
for p in sys.argv[1:]:
    print(p, count(open(p,encoding='utf-8').read()))
