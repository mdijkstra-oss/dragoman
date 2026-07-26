#!/usr/bin/env python3
"""The live half of the done criteria, run against all four real backends.

One openai-responses request carrying a get_weather tool and a prompt that needs
a step of reasoning before the call can be filled in, routed unmodified to each
backend in turn; then four independent two-turn conversations, each backend's
own turn-one response fed back to that same backend; then the same request
streamed.

Needs .env.local at the repository root and a built ./bin/dragoman. The offline
suite (go test ./...) covers everything that can be checked without keys.
"""
import json, subprocess, os, sys

env = dict(os.environ)
for line in open('.env.local'):
    line=line.strip()
    if line and not line.startswith('#') and '=' in line:
        k,v=line.split('=',1); env[k]=v

MATRIX = [("openai","gpt-5.4-mini"),("anthropic","claude-sonnet-5"),
          ("gemini","gemini-3.6-flash"),("deepseek","deepseek-v4-flash")]
BASE = json.load(open('testdata/weather.json')); BASE.pop('model',None)

def dragoman(backend, model, body, *extra):
    p = subprocess.run(['./bin/dragoman','--backend',backend,'--model',model,*extra],
                       input=json.dumps(body), capture_output=True, text=True, env=env)
    return p

def call_of(resp):
    c=[i for i in resp['output'] if i['type']=='function_call']
    return c[0] if c else None

def shape(resp):
    # Two separate live calls are two separate generations: a model may add or
    # omit a narration item between them. What must agree is the answer — the
    # call, its arguments, and whether provider state came back — not the exact
    # item sequence, which the offline same-source fold test covers exactly.
    call=[i for i in resp['output'] if i['type']=='function_call']
    return {
        'status': resp['status'],
        'call': (call[0]['name'], json.loads(call[0]['arguments'])) if call else None,
        'state': any(i.get('encrypted_content') for i in resp['output']),
    }

rows=[]
print("=== A. one request, four backends ===")
buffered={}
for b,m in MATRIX:
    p=dragoman(b,m,BASE)
    if p.returncode!=0: rows.append((b,'A','FAIL',p.stderr[:120])); print(f'{b}: FAIL {p.stderr[:200]}'); continue
    r=json.loads(p.stdout); buffered[b]=r
    c=call_of(r); a=json.loads(c['arguments']) if c else {}
    ok = c and c['name']=='get_weather' and 'rio' in a.get('location','').lower() and a.get('unit')=='celsius'
    print(f"  {b:10s} status={r['status']:9s} call={c['name'] if c else None} args={a} -> {'OK' if ok else 'FAIL'}")
    rows.append((b,'one-shot','OK' if ok else 'FAIL',''))

print("\n=== B. four independent two-turn conversations ===")
for b,m in MATRIX:
    if b not in buffered: rows.append((b,'two-turn','SKIP','')); continue
    r1=buffered[b]; c=call_of(r1)
    body=dict(BASE); body['input']=BASE['input']+r1['output']+[
        {"type":"function_call_output","call_id":c['call_id'],
         "output":json.dumps({"temp_c":27,"conditions":"sunny"})}]
    p=dragoman(b,m,body)
    if p.returncode!=0:
        print(f"  {b:10s} FAIL {p.stderr[:200]}"); rows.append((b,'two-turn','FAIL',p.stderr[:80])); continue
    r2=json.loads(p.stdout)
    text=''.join(pt['text'] for it in r2['output'] if it['type']=='message' for pt in it['content'])
    ok = '27' in text and 'rio' in text.lower()
    print(f"  {b:10s} -> {'OK' if ok else 'FAIL'} | {text[:90]!r}")
    rows.append((b,'two-turn','OK' if ok else 'FAIL',''))

print("\n=== C. stream:true, folded == buffered shape ===")
sbody=dict(BASE); sbody['stream']=True
for b,m in MATRIX:
    if b not in buffered: rows.append((b,'fold','SKIP','')); continue
    p=dragoman(b,m,sbody)
    if p.returncode!=0:
        print(f"  {b:10s} FAIL {p.stderr[:200]}"); rows.append((b,'fold','FAIL','')); continue
    ev=[json.loads(l) for l in p.stdout.splitlines() if l.strip()]
    seq=[e['sequence_number'] for e in ev]
    term=[e for e in ev if e['type'] in ('response.completed','response.incomplete','response.failed')]
    folded=term[-1]['response']
    same = shape(folded)==shape(buffered[b])
    dense = seq==list(range(1,len(seq)+1))
    print(f"  {b:10s} events={len(ev):3d} dense={dense} fold==buffered: {'OK' if same else 'DIFF'}")
    if not same:
        print(f"     stream : {shape(folded)}")
        print(f"     buffered: {shape(buffered[b])}")
    rows.append((b,'fold','OK' if (same and dense) else 'FAIL',''))

print("\n=== SUMMARY ===")
bad=[r for r in rows if r[2] not in ('OK',)]
for b,k,s,d in rows: print(f"  {b:10s} {k:10s} {s} {d}")
print(("ALL PASS" if not bad else f"{len(bad)} FAILURE(S)"))
sys.exit(1 if bad else 0)
