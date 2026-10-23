"""Exercise service-group setup without root or a running container."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT=Path(__file__).resolve().parents[1]/'upstream/kin-kvm-entry'


class ServiceSocketTests(unittest.TestCase):
    def test_optional_group_changes_only_run_traversal_and_socket(self):
        source=SCRIPT.read_text();start=source.index('configure_service_socket() {')
        function=source[start:source.index('\nif [ -d /slot/claude ]',start)]
        with tempfile.TemporaryDirectory(prefix='sub2-socket-') as tmp:
            root=Path(tmp);harness=root/'test'
            harness.write_text('set -eu\ndie() { exit 1; }\nchown() { echo "chown $*"; }\nchmod() { echo "chmod $*"; }\n'+function+'\nprintf "socket=%s:%s\\n" "$SOCKET_GID" "$SOCKET_MODE"\n')
            for gid,ok in (('',True),('1234',True),('0',False),('-1',False),('bad',False),('2147483647',False)):
                with self.subTest(gid=gid):
                    result=subprocess.run(['bash',str(harness)],env=dict(os.environ,UID_NUM='10001',GID_NUM='10001',SLOT_RUN='/slot/run',SUB2_VM_SERVICE_GID=gid),text=True,capture_output=True,timeout=5)
                    self.assertEqual(result.returncode==0,ok)
                    if gid=='':self.assertEqual(result.stdout,'socket=10001:0600\n')
                    if gid=='1234':self.assertEqual(result.stdout,'chown 0:0 /slot/run\nchmod 0710 /slot/run\nchown 10001:1234 /slot/run\nsocket=1234:0660\n')


if __name__=='__main__':unittest.main()
