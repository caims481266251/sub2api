"""Bridge receipts and health policy, with fake filesystem/services and no VM."""
import contextlib
import hashlib
import importlib.machinery
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1] / 'upstream'
loader = importlib.machinery.SourceFileLoader('guest_control', str(ROOT/'guest/sub2-vm-control'))
spec = importlib.util.spec_from_loader(loader.name,loader)
control = importlib.util.module_from_spec(spec); loader.exec_module(control)


class ReceiptTests(unittest.TestCase):
    def setUp(self):
        tmp=tempfile.TemporaryDirectory();self.addCleanup(tmp.cleanup);self.root=Path(tmp.name)
        self.proc={'pid':42,'start_ticks':'100','executable':'/usr/local/bin/kin-codex-kernel'}
        self.write('/run/kin/instance.json',{'generation':1,'desired_credential_revision':'c1','spec':{'platform':'codex','proxy_revision':'p1'}})
        credential=b'{"accounts":[]}'
        self.write('/run/sub2-credentials/credential.json',credential)
        self.write('/run/sub2-credentials/revision.json',{'revision':'c1','digest':hashlib.sha256(credential).hexdigest()})
        self.write('/run/sub2-credentials/proxy.json',{'revision':'p1'})
        self.write('/proc/sys/kernel/random/boot_id',b'boot-one')
        self.write('/run/kin/internal.token',b'test-only')
        realPath=Path
        def mapped(*parts):
            value=realPath(*parts)
            return self.root/str(value).lstrip('/') if value.is_absolute() else value
        self.patches=[patch.object(control,'Path',mapped),patch.object(control,'process_identity',side_effect=lambda:dict(self.proc)),patch.object(control.subprocess,'run')]
        for item in self.patches:item.start();self.addCleanup(item.stop)
        class HTTP:
            status=200
            def __init__(self,*a,**k):pass
            def request(self,*a,**k):pass
            def getresponse(self):return self
            def read(self,*a):return b'{"healthy":true}'
            def close(self):pass
        item=patch.object(control,'UnixHTTP',HTTP);item.start();self.addCleanup(item.stop)

    def write(self,path,value):
        p=self.root/path.lstrip('/');p.parent.mkdir(parents=True,exist_ok=True)
        p.write_bytes(value if isinstance(value,bytes) else json.dumps(value).encode())

    def call(self,operation):
        with patch.object(control.sys,'argv',['control',operation]),contextlib.redirect_stdout(io.StringIO()) as out:
            control.main()
        return json.loads(out.getvalue())

    def apply(self):
        with self.assertRaisesRegex(ValueError,'restart_pending'):self.call('apply')
        with self.assertRaisesRegex(ValueError,'restart_pending'):self.call('apply')
        self.proc['pid']=43;self.proc['start_ticks']='101'
        return self.call('apply')

    def test_receipt_requires_restart_and_live_process(self):
        with self.assertRaisesRegex(ValueError,'application_unconfirmed'):self.call('observe')
        result=self.apply();self.assertTrue(result['kernel_ready'])
        self.assertEqual(self.call('observe'),result)
        self.proc['start_ticks']='102'
        with self.assertRaisesRegex(ValueError,'application_unconfirmed'):self.call('observe')
        with self.assertRaisesRegex(ValueError,'unfenced_kernel_restart'):self.call('apply')

    def test_changed_boot_or_projection_cannot_reuse_receipt(self):
        self.apply()
        self.write('/proc/sys/kernel/random/boot_id',b'boot-two')
        with self.assertRaisesRegex(ValueError,'application_unconfirmed'):self.call('observe')
        self.write('/run/sub2-credentials/credential.json',b'{"accounts":[{}]}')
        with self.assertRaisesRegex(ValueError,'projection_mismatch'):self.call('observe')

    def test_wrong_kernel_executable_is_not_ready(self):
        self.apply()
        self.proc['executable'] = '/usr/local/bin/unrelated-worker'
        with self.assertRaisesRegex(ValueError, 'wrong_kernel_process'):
            self.call('observe')

    def test_unhealthy_kernel_cannot_reuse_receipt(self):
        self.apply()
        with patch.object(control.UnixHTTP, 'read', return_value=b'{"healthy":false}'):
            with self.assertRaisesRegex(ValueError, 'kernel_not_ready'):
                self.call('observe')

    def test_proxy_revision_drift_invalidates_ready_receipt(self):
        self.apply()
        self.write('/run/sub2-credentials/proxy.json', {'revision': 'p2'})
        with self.assertRaisesRegex(ValueError, 'projection_mismatch'):
            self.call('observe')

    def test_process_change_during_health_check_is_not_ready(self):
        self.apply()
        current = dict(self.proc)
        replacement = dict(current, start_ticks='999')
        with patch.object(control, 'process_identity', side_effect=[current, replacement]):
            with self.assertRaisesRegex(ValueError, 'kernel_not_ready'):
                self.call('observe')

    def test_network_projection_precedes_restart_and_observe_is_read_only(self):
        with self.assertRaisesRegex(ValueError, 'restart_pending'):
            self.call('apply')
        calls = control.subprocess.run.call_args_list
        self.assertEqual(calls[0].args[0], ['/usr/local/sbin/sub2-vm-network'])
        self.assertEqual(calls[0].kwargs['env']['KIN_GUEST_ENGINE'], 'codex')
        self.assertEqual(calls[1].args[0], ['systemctl', '--no-block', 'restart', 'kin-kernel.service'])
        control.subprocess.run.reset_mock()
        with self.assertRaisesRegex(ValueError, 'application_unconfirmed'):
            self.call('observe')
        self.assertEqual(control.subprocess.run.call_args.args[0], ['/usr/local/sbin/sub2-vm-network', '--check'])

    def test_missing_effective_network_cannot_issue_ready_receipt(self):
        self.apply()
        with patch.object(control.subprocess, 'run', side_effect=RuntimeError('network invalid')):
            with self.assertRaisesRegex(RuntimeError, 'network invalid'):
                self.call('observe')

    def test_guest_network_is_bound_to_receipt_and_checked_again_after_health(self):
        observed = {'guest_subnet': '10.241.0.16/28', 'guest_address': '10.241.0.20',
                    'guest_gateway': '10.241.0.18', 'guest_dns': '10.241.0.19',
                    'mac': '02:10:20:30:40:50', 'interface': 'enp0s3'}
        with patch.object(control, 'guest_network_observation', return_value=observed):
            self.apply()
            receipt = json.loads((self.root / 'run/sub2-vm-control/applied.json').read_text())
            self.assertEqual(receipt['projection']['guest_network'], observed)
            self.assertTrue(self.call('observe')['kernel_ready'])
        with patch.object(control, 'guest_network_observation', side_effect=[observed, dict(observed, guest_address='10.241.0.21')]):
            with self.assertRaisesRegex(ValueError, 'guest_network_changed'):
                self.call('observe')
        with patch.object(control, 'guest_network_observation', side_effect=ValueError('guest_route_unconfirmed')):
            with self.assertRaisesRegex(ValueError, 'guest_route_unconfirmed'):
                self.call('observe')

    def test_device_identity_requires_matching_config_and_receipt(self):
        device = '824b3d27-8fbd-4cc0-bf5e-ad2650602a74'
        manifest_path = self.root / 'run/kin/instance.json'
        manifest = json.loads(manifest_path.read_text())
        manifest['client_identity'] = {'device_id': device}
        self.write('/run/kin/instance.json', manifest)
        self.write('/run/kin/codex-kernel.json', {'device_id': device})
        self.apply()
        receipt = json.loads((self.root / 'run/sub2-vm-control/applied.json').read_text())
        self.assertEqual(receipt['projection']['device_id'], device)
        self.assertTrue(self.call('observe')['kernel_ready'])
        self.write('/run/kin/codex-kernel.json', {'device_id': '814b3d27-8fbd-4cc0-bf5e-ad2650602a74'})
        with self.assertRaisesRegex(ValueError, 'client_identity_unconfirmed'):
            self.call('observe')
        self.write('/run/kin/codex-kernel.json', {'device_id': device})
        with patch.object(control, 'client_device_observation', side_effect=[device, 'changed']):
            with self.assertRaisesRegex(ValueError, 'client_identity_changed'):
                self.call('observe')

    def test_claude_device_identity_uses_its_kernel_config(self):
        device = '824b3d27-8fbd-4cc0-bf5e-ad2650602a74'
        self.write('/run/kin/kernel.json', {'device_id': device})
        manifest = {'client_identity': {'device_id': device}, 'spec': {'platform': 'claude'}}
        self.assertEqual(control.client_device_observation(manifest), device)

    def test_refresh_token_rejected_recursively(self):
        for value in ({'accounts':[{'refresh_token':'fixture'}]}, {'nested':{'refreshToken':'fixture'}}):
            with self.assertRaisesRegex(ValueError,'refresh_credential_forbidden'):control.reject_refresh(value)


if __name__=='__main__':unittest.main()
