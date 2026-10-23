import importlib.util,json,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).parent))
from full_acceptance_tools import first_request,continuation,decode
class ToolTests(unittest.TestCase):
    def test_responses_tool_continuation(self):
        r={'output':[{'type':'function_call','call_id':'call_1','name':'lookup_weather','arguments':'{"city":"Paris"}'}]}
        b=continuation('responses',first_request('responses','fixture',False),r)
        self.assertEqual(b['input'][-1]['call_id'],'call_1')
    def test_chat_fragment_arguments(self):
        events=[{'choices':[{'delta':{'tool_calls':[{'index':0,'id':'c','function':{'name':'lookup_weather','arguments':'{"city":'}}]}}]},{'choices':[{'delta':{'tool_calls':[{'index':0,'function':{'arguments':'"Paris"}'}}]},'finish_reason':'tool_calls'}]}]
        raw=('\n'.join('data: '+json.dumps(e) for e in events)+'\ndata: [DONE]\n').encode()
        r=decode('chat',True,raw);b=continuation('chat',first_request('chat','fixture',True),r)
        self.assertEqual(b['messages'][-1]['tool_call_id'],'c')
    def test_wrong_arguments_rejected(self):
        r={'content':[{'type':'tool_use','id':'t','name':'wrong','input':{'city':'Paris'}}]}
        with self.assertRaises(AssertionError):continuation('messages',first_request('messages','fixture',False),r)
if __name__=='__main__':unittest.main()
