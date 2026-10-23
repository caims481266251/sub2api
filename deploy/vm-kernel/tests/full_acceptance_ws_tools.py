#!/usr/bin/env python3
"""Real GPT WS tool continuation and connection-owned history acceptance."""
import argparse
import json
import os
import time
from pathlib import Path

from full_acceptance import Run, WS
from full_acceptance_cancel import audit, close_socket
from full_acceptance_tools import MARKER, PARAMS


def complete(ws):
    for _ in range(1000):
        event = ws.receive()
        assert event.get('type') not in ('error', 'response.failed'), 'WS tool response failed'
        if event.get('type') == 'response.completed':
            return event['response']
    raise RuntimeError('Missing WS tool completion')


def reject_foreign_history(key, model, previous):
    ws = WS(key)
    error_seen = False
    try:
        ws.send({'type': 'response.create', 'model': model,
                 'previous_response_id': previous, 'input': 'Reply OK.'})
        for _ in range(10):
            try:
                event = ws.receive()
            except RuntimeError as exc:
                assert str(exc) == 'WS closed before completion', 'Expected explicit WS rejection'
                assert ws.close_code in (1008, 1011), 'Unexpected rejection close code'
                return {'rejected': True, 'close_code': ws.close_code, 'error_frame': error_seen}
            assert event.get('type') == 'error', 'Foreign history reached inference'
            error_seen = True
        raise RuntimeError('Foreign history connection stayed open')
    finally:
        close_socket(ws)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    path = Path(args.config)
    assert path.stat().st_mode & 0o077 == 0
    config = json.loads(path.read_text())
    assert config['isolated_database'] == os.environ.get('PGDATABASE')
    config.setdefault('state_file', str(path.with_suffix('.state.json')))
    run = Run(config)
    assert run.state.get('seeded') and not run.state.get('pending')
    assert not run.state.get('ws_tools_attempt'), 'Reconcile existing WS tool attempt before retry'
    before = audit(run.state['ids'])
    run.state['ws_tools_attempt'] = {'pending': True}
    run.save()
    model = config.get('model', 'gpt-6-luna')
    ws = WS(run.state['key'])
    try:
        ws.send({'type': 'response.create', 'model': model,
                 'input': 'Call lookup_weather for Paris exactly once. After its result arrives, report the exact verification marker from that result. Do not call the tool again.',
                 'tools': [{'type': 'function', 'name': 'lookup_weather',
                            'description': 'Look up weather for the requested city',
                            'parameters': PARAMS, 'strict': True}],
                 'tool_choice': 'auto'})
        first = complete(ws)
        calls = [x for x in first.get('output', []) if x.get('type') == 'function_call']
        assert len(calls) == 1, 'Expected one WS tool call'
        call = calls[0]
        assert call['name'] == 'lookup_weather' and call['call_id']
        assert json.loads(call['arguments']).get('city', '').lower() == 'paris'
        run.state['ws_tools_attempt']['first_response_id'] = first['id']
        run.save()
        ws.send({'type': 'response.create', 'model': model,
                 'previous_response_id': first['id'],
                 'input': [{'type': 'function_call_output', 'call_id': call['call_id'], 'output': MARKER}]})
        second = complete(ws)
        assert MARKER in json.dumps(second.get('output', [])), 'WS tool result not consumed'
        assert not any(x.get('type') == 'function_call' for x in second.get('output', []))
    finally:
        close_socket(ws)
    boundaries = {
        'unknown_history': reject_foreign_history(run.state['key'], model, 'resp_vm_accept_unknown'),
        'cross_connection_history': reject_foreign_history(run.state['key'], model, second['id']),
    }
    for _ in range(60):
        after = audit(run.state['ids'])
        count = after['count'] - before['count']
        cost = float(after['cost']) - float(before['cost'])
        balance = float(before['balance']) - float(after['balance'])
        if count == 2 and abs(cost-balance) < 1e-8:
            break
        time.sleep(1)
    assert count == 2 and after['count'] == after['unique'], 'WS tool/history billing mismatch'
    assert cost > 0 and abs(cost-balance) < 1e-8
    report = {'passed': True, 'pending': False, 'tool_round_trip': True,
              'response_ids': [first['id'], second['id']], 'boundaries': boundaries,
              'new_usage_rows': count, 'cost': cost, 'balance_delta': balance}
    run.state['ws_tools_attempt'] = report
    run.save()
    Path(args.output).write_text(json.dumps(report, indent=2)+'\n')
    print('WS tool continuation and two foreign-history rejections passed')


if __name__ == '__main__':
    main()
