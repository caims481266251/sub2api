import json
import os
import subprocess
import unittest

IMAGE = os.environ.get('SUB2_DOCKER_KERNEL_TEST_IMAGE', 'sub2api-docker-kernel:local')
CONTEXT = ['docker', '--context', 'colima-orderpilot-security-verify']


class DockerBridgeTest(unittest.TestCase):
    def test_50_parallel_socks_tunnels_recover_from_transient_resets(self):
        script = r'''import asyncio,importlib.util,json,sys
spec=importlib.util.spec_from_file_location('bridge','/usr/local/lib/sub2-container/proxy_bridge.py');m=importlib.util.module_from_spec(spec);sys.modules['bridge']=m;spec.loader.exec_module(m)
attempts=0;established=0
async def read(r,n):
 b=b''
 while len(b)<n:
  p=await r.read(n-len(b))
  if not p:return b
  b+=p
 return b
async def handle(r,w):
 global attempts,established
 attempts+=1;number=attempts
 try:
  if number<=30:w.close();await w.wait_closed();return
  head=await read(r,2);methods=await read(r,head[1]);w.write(b'\x05\x00');await w.drain()
  req=await read(r,4);atyp=req[3]
  if atyp==3:size=await read(r,1);await read(r,size[0])
  elif atyp==1:await read(r,4)
  else:await read(r,16)
  await read(r,2);established+=1;w.write(b'\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x50');await w.drain()
  while True:
   data=await r.read(65536)
   if not data:break
   w.write(data);await w.drain()
 except (OSError,asyncio.IncompleteReadError):pass
 finally:w.close();await w.wait_closed()
async def one(bridge,i):
 reader,writer,_=await asyncio.wait_for(bridge.upstream(b'\x03\x0bchatgpt.com\x01\xbb'),5);payload=('request-%d'%i).encode();writer.write(payload);await writer.drain();response=await asyncio.wait_for(reader.readexactly(len(payload)),3);writer.close();await writer.wait_closed();return response==payload
async def main():
 global attempts,established
 server=await asyncio.start_server(handle,'127.0.0.1',0);port=server.sockets[0].getsockname()[1];bridge=m.ProxyBridge('socks5://127.0.0.1:'+str(port),['127.0.0.1'],timeout=3,delays=(0.01,0.02))
 results=await asyncio.gather(*(one(bridge,i) for i in range(50)),return_exceptions=True);server.close();await server.wait_closed();ok=sum(x is True for x in results);print(json.dumps({'requests':50,'success':ok,'failed':50-ok,'proxy_connections':attempts,'tunnels_established':established}));assert ok==50 and established==50 and attempts==80
asyncio.run(main())'''
        subprocess.run(CONTEXT + ['run', '--rm', '--network=host', '--cap-drop=ALL', '--security-opt=no-new-privileges', '--entrypoint', 'python3', IMAGE, '-c', script], check=True, timeout=30)


if __name__ == '__main__':
    unittest.main()
