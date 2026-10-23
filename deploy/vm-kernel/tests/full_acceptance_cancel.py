#!/usr/bin/env python3
"""Separate cancellation accounting probe; requires full_acceptance seed state."""
import argparse,json,os,subprocess,time,urllib.request
from pathlib import Path
from full_acceptance import Run,WS

def audit(ids):
    assert os.environ.get('PGDATABASE','').startswith('sub2api_vm_accept_')
    uid,kid,aid=(int(ids[k]) for k in ('user_id','api_key_id','account_id'))
    sql=f"SELECT json_build_object('count',count(*),'unique',count(DISTINCT request_id),'cost',coalesce(sum(actual_cost),0),'balance',(SELECT balance FROM users WHERE id={uid})) FROM usage_logs WHERE user_id={uid} AND api_key_id={kid} AND account_id={aid};"
    p=subprocess.run(['psql','-X','-A','-t','-v','ON_ERROR_STOP=1','-c',sql],capture_output=True,text=True)
    if p.returncode: raise RuntimeError('Isolated billing query failed')
    return json.loads(p.stdout)


def require_cancel_close(ws):
    try:
        ws.receive()
    except RuntimeError as exc:
        if str(exc) == 'WS closed before completion':
            assert ws.close_code == 1008, 'Cancellation requires policy close 1008'
            return
        raise
    raise RuntimeError('Server accepted another frame instead of closing cancelled connection')

def close_socket(ws):
    try:
        ws.close()
    except (OSError, RuntimeError):
        ws.f.close(); ws.s.close()

def wait_runtime_ready(r, admin, timeout=600):
    started=time.monotonic(); deadline=started+timeout
    while time.monotonic()<deadline:
        req=urllib.request.Request(r.root+'/api/v1/admin/accounts/'+str(int(r.state['ids']['account_id'])),headers={'Authorization':'Bearer '+admin})
        with urllib.request.urlopen(req,timeout=min(10,max(0.1,deadline-time.monotonic()))) as response:
            doc=json.loads(response.read())
        assert doc.get('code',0)==0, 'Runtime status API failed'
        account=doc.get('data',doc)
        if account.get('vm_kernel',{}).get('ready') is True:
            return round(time.monotonic()-started,3)
        time.sleep(min(5,max(0,deadline-time.monotonic())))
    raise RuntimeError('Runtime not ready within cancellation recovery budget')

def main():
    p=argparse.ArgumentParser();p.add_argument('--config',required=True);p.add_argument('--output',required=True);p.add_argument('--mode',choices=['disconnect','explicit'],default='disconnect');a=p.parse_args()
    config=Path(a.config);assert config.stat().st_mode&0o077==0
    c=json.loads(config.read_text());assert c['isolated_database']==os.environ.get('PGDATABASE');c.setdefault('state_file',str(config.with_suffix('.state.json')))
    r=Run(c);assert r.state.get('seeded');assert not r.state.get('pending');assert not r.state.get('cancel_'+a.mode), 'Reconcile existing cancel attempt before repeating'
    before=audit(r.state['ids']);r.state['cancel_'+a.mode]={'started':True};r.save()
    ws=WS(r.state['key']);response_id=None;delta_seen=False;recovery_wait=None;closed=False
    try:
        ws.send({'type':'response.create','model':c.get('model','gpt-6-luna'),'input':'Write a detailed 2000 word essay about the history of astronomy.'})
        for _ in range(500):
            event=ws.receive()
            if event.get('type')=='response.created':response_id=event.get('response',{}).get('id')
            if event.get('type')=='response.output_text.delta':delta_seen=True;break
            assert event.get('type') not in ('error','response.failed','response.completed'), 'No cancellation opportunity'
        assert delta_seen
        if a.mode=='explicit':
            ws.send({'type':'response.cancel'})
            for _ in range(500):
                event=ws.receive()
                if event.get('type')=='error':
                    assert event.get('error',{}).get('code')=='response_cancelled'
                    assert event.get('error',{}).get('reconnect_required') is True;break
                assert event.get('type')!='response.completed','Cancellation arrived after completion'
            else:raise RuntimeError('Cancellation acknowledgement missing')
            require_cancel_close(ws);closed=True;close_socket(ws)
            admin=r.login(c['admin_email'],c['admin_password'])
            recovery_wait=wait_runtime_ready(r,admin)
            ws=WS(r.state['key'])
            ws.send({'type':'response.create','model':c.get('model','gpt-6-luna'),'input':'Reply exactly RECOVERED.'})
            for _ in range(500):
                event=ws.receive();assert event.get('type') not in ('error','response.failed'),'New connection recovery failed'
                if event.get('type')=='response.completed':
                    assert 'RECOVERED' in json.dumps(event.get('response',{}).get('output',[]));break
            else:raise RuntimeError('Recovery completion missing')
    finally:close_socket(ws)
    # Cancellation can legitimately record zero usage when upstream sends none.
    # Check persisted billing consistency, never infer guest drain from socket close.
    time.sleep(5);after=audit(r.state['ids']);time.sleep(5);stable=audit(r.state['ids'])
    count=stable['count']-before['count'];cost=float(stable['cost'])-float(before['cost']);balance=float(before['balance'])-float(stable['balance'])
    assert after==stable,'Cancellation billing still changing'
    assert count in ((0,1) if a.mode=='disconnect' else (1,2)) and stable['count']==stable['unique'],'Duplicate cancellation billing'
    assert abs(balance-cost)<1e-8,'Cancellation balance mismatch'
    if a.mode=='explicit': assert cost>0,'Successful recovery missing billing'
    report={'mode':a.mode,'same_connection_recovered':False,'new_connection_recovered':a.mode=='explicit','cancelled_connection_closed':closed,'runtime_ready_wait_seconds':recovery_wait,'cancel_sent':a.mode=='explicit','delta_seen':True,'response_id':response_id,'new_usage_rows':count,'actual_cost_delta':cost,'balance_delta':balance,'billing_consistent_at_observation':True,'guest_drain_verified':False}
    r.state['cancel_'+a.mode]=report;r.save();Path(a.output).write_text(json.dumps(report,indent=2)+'\n');print('Cancellation probe complete; guest drain requires separate helper evidence')
if __name__=='__main__':main()
