import json,os,sys,glob,collections
W=sys.argv[1]
K=('input_tokens','output_tokens','cache_creation_input_tokens','cache_read_input_tokens')
def agent_usage(path):
    best={}
    for line in open(path,errors='ignore'):
        try: o=json.loads(line)
        except: continue
        m=o.get('message') or {}
        u=m.get('usage') if isinstance(m,dict) else None
        if not u: continue
        mid=m.get('id') or o.get('uuid')
        cur=best.setdefault(mid,collections.Counter())
        for k in K: cur[k]=max(cur[k],u.get(k) or 0)
    return sum(best.values(),collections.Counter())
def run_stats(run):
    labels={}
    for line in open(os.path.join(W,run,'journal.jsonl')):
        try:o=json.loads(line)
        except: continue
        if o.get('type')=='started': labels[o.get('agentId')]=o.get('label')
    per=collections.defaultdict(collections.Counter)
    for f in glob.glob(os.path.join(W,run,'agent-*.jsonl')):
        aid=os.path.basename(f)[6:-6]
        lab=labels.get(aid) or labels.get('a'+aid) or '?'
        kind=lab.split(':')[0].rstrip('0123456789')
        u=agent_usage(f); u['n']=1; u['size_'+lab]=0
        per[kind]+=u
    return per
if __name__=='__main__':
    for run in sys.argv[2:]:
        per=run_stats(run); T=sum(per.values(),collections.Counter())
        tot=lambda c: c['input_tokens']+c['output_tokens']+c['cache_creation_input_tokens']
        print(run,'agents',T['n'],'in+out+create %.2fM'%(tot(T)/1e6),'out %.2fM'%(T['output_tokens']/1e6),'cache_read %.1fM'%(T['cache_read_input_tokens']/1e6))
        for k,c in sorted(per.items()): print('   %-10s n=%-3d in+out+create %.2fM  out %.2fM  cache_read %.1fM'%(k,c['n'],tot(c)/1e6,c['output_tokens']/1e6,c['cache_read_input_tokens']/1e6))
