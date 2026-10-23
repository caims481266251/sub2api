import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('full_acceptance',Path(__file__).with_name('full_acceptance.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class Socket:
    def __init__(self): self.sent=[]
    def sendall(self,b): self.sent.append(b)

class AcceptanceTest(unittest.TestCase):
    def test_client_masks_payload(self):
        w=m.WS.__new__(m.WS);w.s=Socket();w.send({'type':'response.create'})
        b=w.s.sent[0];mask=b[2:6];payload=bytes(v^mask[i%4] for i,v in enumerate(b[6:]))
        self.assertTrue(b[1]&128);self.assertEqual(json.loads(payload),{'type':'response.create'})
    def test_fragment_and_ping(self):
        w=m.WS.__new__(m.WS);w.s=Socket()
        w.f=io.BytesIO(b'\x01\x05{"x":'+b'\x89\x01p'+b'\x80\x021}')
        self.assertEqual(w.receive(),{'x':1});self.assertEqual(w.s.sent[0][0],0x8a)
    def test_close_is_not_completion(self):
        w=m.WS.__new__(m.WS);w.f=io.BytesIO(b'\x88\x00')
        with self.assertRaises(RuntimeError):w.receive()
        self.assertIsNone(w.close_code)
        w.f=io.BytesIO(b'\x88\x02\x03\xf0')
        with self.assertRaises(RuntimeError):w.receive()
        self.assertEqual(w.close_code,1008)
    def test_state_is_private_and_reused(self):
        with tempfile.TemporaryDirectory() as d:
            path=str(Path(d)/'state.json');r=m.Run({'state_file':path});r.state['key']='fixture';r.save()
            self.assertEqual(Path(path).stat().st_mode&0o077,0)
            self.assertEqual(m.Run({'state_file':path}).state['key'],'fixture')

if __name__=='__main__':unittest.main()
