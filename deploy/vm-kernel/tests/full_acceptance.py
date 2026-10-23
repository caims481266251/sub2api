#!/usr/bin/env python3
"""Isolated acceptance. Secrets in mode-0600 config; never emit response bodies."""
import argparse, base64, hashlib, json, os, secrets, socket, struct, subprocess, time, urllib.request, urllib.error
from pathlib import Path

class WS:
    def __init__(self, key):
        self.s=socket.create_connection(('127.0.0.1',18081),timeout=180)
        nonce=base64.b64encode(os.urandom(16)).decode()
        self.s.sendall((f'GET /v1/responses HTTP/1.1\r\nHost: 127.0.0.1:18081\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {nonce}\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer {key}\r\nOpenAI-Beta: responses_websockets=2025-02-11\r\n\r\n').encode())
        self.f=self.s.makefile('rb'); status=self.f.readline(); headers={}
        while True:
            line=self.f.readline()
            if line==b'\r\n': break
            if not line: raise RuntimeError('WS handshake EOF')
            k,v=line.decode().split(':',1); headers[k.lower()]=v.strip()
        assert b' 101 ' in status, 'WS upgrade failed'
        assert headers.get('sec-websocket-accept')==base64.b64encode(hashlib.sha1((nonce+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
    def send(self, data, opcode=1):
        b=json.dumps(data).encode() if not isinstance(data,bytes) else data
        mask=os.urandom(4); n=len(b)
        length=bytes([n|128]) if n<126 else bytes([126|128])+struct.pack('!H',n)
        self.s.sendall(bytes([128|opcode])+length+mask+bytes(v^mask[i%4] for i,v in enumerate(b)))
    def receive(self):
        chunks=[]
        while True:
            h=self.f.read(2)
            if len(h)!=2: raise RuntimeError('WS EOF')
            n=h[1]&127
            if n==126: n=struct.unpack('!H',self.f.read(2))[0]
            if n==127: n=struct.unpack('!Q',self.f.read(8))[0]
            assert n<16*1024*1024
            b=self.f.read(n); op=h[0]&15
            if op==8:
                self.close_code=struct.unpack('!H',b[:2])[0] if len(b)>=2 else None
                raise RuntimeError('WS closed before completion')
            if op==9: self.send(b,10); continue
            if op==10: continue
            chunks.append(b)
            if h[0]&128: return json.loads(b''.join(chunks))
    def close(self):
        self.send(b'',8); self.f.close(); self.s.close()

class Run:
    def __init__(self,c):
        self.c=c; self.state_path=Path(c['state_file']); self.state=json.loads(self.state_path.read_text()) if self.state_path.exists() else {}; self.results=self.state.get('results',[]); self.root='http://127.0.0.1:18081'
    def save(self):
        self.state['results']=self.results
        fd=os.open(self.state_path,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
        with os.fdopen(fd,'w') as f: json.dump(self.state,f)
    def call(self,path,body=None,token=None,raw=False):
        headers={'Content-Type':'application/json','anthropic-version':'2023-06-01'}
        if token: headers['Authorization']='Bearer '+token
        req=urllib.request.Request(self.root+path,data=None if body is None else json.dumps(body).encode(),headers=headers)
        try:
            with urllib.request.urlopen(req,timeout=240) as r: data=r.read(); status=r.status; self.last_request_id=r.headers.get('x-request-id')
        except urllib.error.HTTPError as e: raise RuntimeError(f'{path}: HTTP {e.code}') from None
        if raw: return status,data
        obj=json.loads(data); assert obj.get('code',0)==0, f'{path}: API error'; return obj.get('data',obj)
    def login(self,email,password): return self.call('/api/v1/auth/login',{'email':email,'password':password})['access_token']
    def run(self):
        c=self.c; admin=self.login(c['admin_email'],c['admin_password']); label='vm-accept-'+secrets.token_hex(4)
        if not self.state.get('seeded'):
            assert not self.state.get('seed_pending'), 'Incomplete seed: reconcile created IDs before retry'
            self.state['seed_pending']=True; self.save()
            proxy=self.call('/api/v1/admin/proxies',dict(c['proxy'],name=label,fallback_mode='none'),admin)
            self.state['proxy_id']=proxy['id']; self.save()
            group=self.call('/api/v1/admin/groups',{'name':label,'platform':'openai','rate_multiplier':1,'subscription_type':'standard','is_exclusive':True,'allow_messages_dispatch':True},admin)
            self.state['group_id']=group['id']; self.save()
            account=self.call('/api/v1/admin/accounts',{'name':label,'execution_mode':'vm_kernel','platform':'openai','type':'oauth','credentials':c['credentials'],'proxy_id':proxy['id'],'group_ids':[group['id']],'concurrency':1,'priority':1,'rate_multiplier':1},admin)
            self.state['account_id']=account['id']; self.save()
            password=secrets.token_urlsafe(24); email=label+'@example.invalid'
            user=self.call('/api/v1/admin/users',{'email':email,'password':password,'balance':10,'concurrency':1,'allowed_groups':[group['id']],'restrict_public_groups':True},admin)
            self.state['user_id']=user['id']; self.save()
            ut=self.login(email,password); key=self.call('/api/v1/keys',{'name':label,'group_id':group['id'],'concurrency_limit':1},ut)
            self.ids={'user_id':user['id'],'account_id':account['id'],'api_key_id':key['id'],'group_id':group['id'],'proxy_id':proxy['id']}
            self.before=float(self.call('/api/v1/admin/users/'+str(user['id']),token=admin)['balance'])
            self.state.pop('seed_pending',None)
            self.state.update(seeded=True,ids=self.ids,before=self.before,key=key['key']); self.save()
        self.ids=self.state['ids']; self.before=self.state['before']; key={'key':self.state['key'],'id':self.ids['api_key_id']}; user={'id':self.ids['user_id']}; account={'id':self.ids['account_id']}
        if c['phase']=='seed': return
        model=c.get('model','gpt-6-luna')
        if self.state.get('pending'): raise RuntimeError('Previous request has uncertain result; reconcile pending operation with DB before clearing state')
        for protocol,path in [('responses','/v1/responses'),('chat','/v1/chat/completions'),('messages','/v1/messages')]:
            for stream in [False,True]:
                if c['phase'] not in ('all','http') or any(r.get('protocol')==protocol and r.get('stream')==stream for r in self.results): continue
                body={'model':model,'stream':stream}
                body.update({'input':'Reply exactly OK.'} if protocol=='responses' else {'messages':[{'role':'user','content':'Reply exactly OK.'}],'max_tokens':64})
                self.state['pending']={'protocol':protocol,'stream':stream}; self.save()
                status,data=self.call(path,body,key['key'],True)
                if stream:
                    events=[json.loads(line[5:]) for line in data.decode().splitlines() if line.startswith('data:') and line[5:].strip()!='[DONE]']
                    assert events, 'No SSE events'
                    assert not any(e.get('type')=='error' or 'error' in e for e in events), 'SSE error'
                    completed=any(e.get('type')=='response.completed' for e in events) if protocol=='responses' else (any(e.get('type')=='message_stop' for e in events) if protocol=='messages' else b'[DONE]' in data)
                    assert completed, 'Missing SSE terminal event'
                else:
                    obj=json.loads(data); assert 'error' not in obj
                    assert obj.get('status')=='completed' if protocol=='responses' else bool(obj.get('choices') if protocol=='chat' else obj.get('content'))
                self.results.append({'protocol':protocol,'stream':stream,'status':status,'completed':True,'request_id':self.last_request_id}); self.state.pop('pending',None); self.save()
        if c['phase']=='http': return
        if c['phase'] in ('all','ws') and not any(r.get('protocol')=='responses_ws' for r in self.results):
            self.run_ws(key['key'],model)
        if c['phase']=='ws': return
        self.billing(admin,user,account,key)
    def run_ws(self,key,model):
        ws=WS(key); previous=None
        self.state['pending']={'protocol':'responses_ws','turns':2}; self.save()
        try:
            for turn in range(2):
                frame={'type':'response.create','model':model,'input':[{'role':'user','content':'Remember the word cobalt and reply OK.' if turn==0 else 'What word did I ask you to remember?'}]}
                if previous: frame['previous_response_id']=previous
                ws.send(frame)
                while True:
                    event=ws.receive(); assert event.get('type') not in ('error','response.failed'), 'WS response failed'
                    if event.get('type')=='response.completed':
                        response=event['response']; previous=response['id']
                        if turn: assert 'cobalt' in json.dumps(response.get('output',[])).lower(), 'WS lost conversation context'
                        self.results.append({'protocol':'responses_ws','turn':turn+1,'completed':True,'response_id':previous}); self.save(); break
        finally: ws.close()
        self.state.pop('pending',None); self.save()
    def billing(self,admin,user,account,key):
        # psql reads connection credentials from inherited PG* variables, never argv.
        assert os.environ.get('PGDATABASE','').startswith('sub2api_vm_accept_'), 'Require isolated PGDATABASE prefix'
        uid=int(user['id']); aid=int(account['id']); kid=int(key['id'])
        sql=f"SELECT json_build_object('count',count(*),'distinct_requests',count(DISTINCT request_id),'actual_cost',coalesce(sum(actual_cost),0),'input_tokens',coalesce(sum(input_tokens),0),'output_tokens',coalesce(sum(output_tokens),0),'accounts',count(DISTINCT account_id)) FROM usage_logs WHERE user_id={uid} AND api_key_id={kid} AND account_id={aid};"
        for _ in range(60):
            p=subprocess.run(['psql','-X','-A','-t','-v','ON_ERROR_STOP=1','-c',sql],capture_output=True,text=True)
            if p.returncode: raise RuntimeError('Isolated database audit failed')
            audit=json.loads(p.stdout); after=float(self.call('/api/v1/admin/users/'+str(uid),token=admin)['balance'])
            if audit['count']==8 and abs((self.before-after)-float(audit['actual_cost']))<1e-8: break
            time.sleep(1)
        assert audit['count']==audit['distinct_requests']==8, 'Expected exactly eight unique billed requests'
        assert float(audit['actual_cost'])>0 and audit['input_tokens']>0 and audit['output_tokens']>0, 'Missing positive usage/billing'
        assert abs((self.before-after)-float(audit['actual_cost']))<1e-8, 'Balance does not match original billing'
        self.audit=dict(audit,balance_before=self.before,balance_after=after)

if __name__=='__main__':
    ap=argparse.ArgumentParser(); ap.add_argument('--config',required=True); ap.add_argument('--output',required=True); ap.add_argument('--phase',choices=['all','seed','http','ws','billing'],default='all'); a=ap.parse_args()
    p=Path(a.config); assert p.stat().st_mode&0o077==0, 'Config must be private mode 0600'
    c=json.loads(p.read_text()); c['phase']=a.phase; c.setdefault('state_file',str(p.with_suffix('.state.json'))); assert c.get('isolated_database')==os.environ.get('PGDATABASE') and c['isolated_database'].startswith('sub2api_vm_accept_')
    run=Run(c)
    try: run.run(); report={'passed':True,'scope':'real-openai-full-public-handler','ids':run.ids,'requests':run.results,'billing':getattr(run,'audit',None),'phase':a.phase}
    except Exception as e: report={'passed':False,'error_type':type(e).__name__,'error':str(e) if isinstance(e,(AssertionError,RuntimeError)) else 'Request failed; inspect redacted server diagnostics','ids':getattr(run,'ids',{}),'requests':run.results}
    Path(a.output).write_text(json.dumps(report,indent=2)+'\n'); print(json.dumps({'passed':report['passed'],'completed_requests':len(run.results)})); raise SystemExit(0 if report['passed'] else 1)
