#!/usr/bin/env python3
"""Real GPT function-tool round trips over all three public protocols."""
import argparse,json,time,os
from pathlib import Path
from full_acceptance import Run
from full_acceptance_cancel import audit

PARAMS={'type':'object','properties':{'city':{'type':'string'}},'required':['city'],'additionalProperties':False}
PROMPT='Call lookup_weather for Paris. After receiving its result, report the exact verification marker in the result.'
MARKER='VM_TOOL_RESULT_74b2'

def first_request(protocol,model,stream):
    b={'model':model,'stream':stream}
    if protocol=='responses': b.update(input=PROMPT,tools=[{'type':'function','name':'lookup_weather','description':'Look up weather','parameters':PARAMS,'strict':True}],tool_choice={'type':'function','name':'lookup_weather'})
    elif protocol=='chat':b.update(messages=[{'role':'user','content':PROMPT}],tools=[{'type':'function','function':{'name':'lookup_weather','description':'Look up weather','parameters':PARAMS,'strict':True}}],tool_choice={'type':'function','function':{'name':'lookup_weather'}})
    else:b.update(messages=[{'role':'user','content':PROMPT}],max_tokens=256,tools=[{'name':'lookup_weather','description':'Look up weather','input_schema':PARAMS}],tool_choice={'type':'tool','name':'lookup_weather'})
    return b

def decode(protocol,stream,data):
    if not stream:return json.loads(data)
    events=[json.loads(line[5:]) for line in data.decode().splitlines() if line.startswith('data:') and line[5:].strip()!='[DONE]']
    assert not any('error' in e or e.get('type')=='error' for e in events),'Tool stream error'
    if protocol=='responses':
        completed=[e['response'] for e in events if e.get('type')=='response.completed'];assert len(completed)==1;return completed[0]
    if protocol=='chat':
        assert b'[DONE]' in data;calls={};text='';finish=None
        for e in events:
            for choice in e.get('choices',[]):
                delta=choice.get('delta',{});text+=delta.get('content') or '';finish=choice.get('finish_reason') or finish
                for call in delta.get('tool_calls',[]):
                    target=calls.setdefault(call['index'],{'id':'','type':'function','function':{'name':'','arguments':''}})
                    target['id']+=call.get('id','')
                    for k in ('name','arguments'):target['function'][k]+=call.get('function',{}).get(k,'')
        return {'choices':[{'message':{'role':'assistant','content':text or None,'tool_calls':list(calls.values())},'finish_reason':finish}]}
    blocks={};arguments={};stop=None
    for e in events:
        if e.get('type')=='content_block_start':blocks[e['index']]=dict(e['content_block'])
        if e.get('type')=='content_block_delta':
            d=e['delta'];i=e['index']
            if d['type']=='input_json_delta':arguments[i]=arguments.get(i,'')+d['partial_json']
            if d['type']=='text_delta':blocks[i]['text']=blocks[i].get('text','')+d['text']
        if e.get('type')=='message_delta':stop=e.get('delta',{}).get('stop_reason') or stop
    assert any(e.get('type')=='message_stop' for e in events)
    for i,arg in arguments.items():blocks[i]['input']=json.loads(arg)
    return {'content':list(blocks.values()),'stop_reason':stop}

def continuation(protocol,first,result):
    b=dict(first);b['tool_choice']='auto' if protocol!='messages' else {'type':'auto'}
    if protocol=='responses':
        calls=[x for x in result.get('output',[]) if x.get('type')=='function_call'];assert len(calls)==1;c=calls[0];cid=c['call_id'];name=c['name'];args=json.loads(c['arguments'])
        b['input']=[{'role':'user','content':PROMPT}]+result['output']+[{'type':'function_call_output','call_id':cid,'output':MARKER}]
    elif protocol=='chat':
        message=result['choices'][0]['message'];calls=message.get('tool_calls',[]);assert len(calls)==1;c=calls[0];cid=c['id'];name=c['function']['name'];args=json.loads(c['function']['arguments'])
        b['messages']=first['messages']+[message,{'role':'tool','tool_call_id':cid,'content':MARKER}]
    else:
        calls=[x for x in result['content'] if x.get('type')=='tool_use'];assert len(calls)==1;c=calls[0];cid=c['id'];name=c['name'];args=c['input']
        b['messages']=first['messages']+[{'role':'assistant','content':result['content']},{'role':'user','content':[{'type':'tool_result','tool_use_id':cid,'content':MARKER}]}]
    assert cid and name=='lookup_weather' and str(args.get('city','')).lower()=='paris','Tool identity/arguments changed'
    return b

def main():
    p=argparse.ArgumentParser();p.add_argument('--config',required=True);p.add_argument('--output',required=True);a=p.parse_args();path=Path(a.config);assert path.stat().st_mode&0o077==0
    c=json.loads(path.read_text());assert c['isolated_database']==os.environ.get('PGDATABASE');c.setdefault('state_file',str(path.with_suffix('.state.json')));r=Run(c);assert r.state.get('seeded');assert not r.state.get('tool_attempt'),'Reconcile existing tool attempt before repeating';before=audit(r.state['ids'])
    r.state['tool_attempt']={'pending':True,'results':[]};r.save()
    for protocol,endpoint in [('responses','/v1/responses'),('chat','/v1/chat/completions'),('messages','/v1/messages')]:
        for stream in (False,True):
            first=first_request(protocol,c.get('model','gpt-6-luna'),stream);_,raw=r.call(endpoint,first,r.state['key'],True);result=decode(protocol,stream,raw);rid1=r.last_request_id
            second=continuation(protocol,first,result);_,raw=r.call(endpoint,second,r.state['key'],True);result=decode(protocol,stream,raw)
            assert MARKER in json.dumps(result),'Tool result was not consumed'
            r.state['tool_attempt']['results'].append({'protocol':protocol,'stream':stream,'tool_round_trip':True,'request_ids':[rid1,r.last_request_id]});r.save()
    for _ in range(60):
        after=audit(r.state['ids']);count=after['count']-before['count'];cost=float(after['cost'])-float(before['cost']);balance=float(before['balance'])-float(after['balance'])
        if count==12 and abs(cost-balance)<1e-8:break
        time.sleep(1)
    assert count==12 and after['count']==after['unique'] and cost>0 and abs(cost-balance)<1e-8,'Tool billing mismatch'
    r.state['tool_attempt'].update(pending=False,new_usage_rows=count,cost=cost,balance_delta=balance);r.save();Path(a.output).write_text(json.dumps(r.state['tool_attempt'],indent=2)+'\n');print('12 tool requests and balance accounting passed')
if __name__=='__main__':main()
