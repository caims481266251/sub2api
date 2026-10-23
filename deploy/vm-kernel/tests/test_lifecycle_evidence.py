import unittest
from unittest.mock import patch
import lifecycle_evidence as evidence

class LifecycleEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.before = dict(instance_id='fixture', docker_context='fixture', container_exists=True,
                           running=True, generation=2, guest_boot_id='boot1', image='sha256:fixture')

    def test_recovery_requires_new_generation_and_boot(self):
        after = dict(self.before, generation=3, guest_boot_id='boot2', kernel_ready=True)
        self.assertEqual(evidence.validate(self.before, after, 'recovered')['verified'], 'recovered')
        for changes in ({'generation':2}, {'guest_boot_id':'boot1'}, {'kernel_ready':False}, {'image':'other'}):
            with self.assertRaises(ValueError):
                evidence.validate(self.before, dict(after, **changes), 'recovered')

    def test_stopped_and_absent_are_distinct(self):
        stopped = dict(self.before, running=False, status='exited')
        absent = dict(instance_id='fixture', docker_context='fixture', container_exists=False)
        evidence.validate(self.before, stopped, 'stopped')
        evidence.validate(self.before, absent, 'absent')
        for snapshot, expectation in ((stopped,'absent'),(absent,'stopped'),(self.before,'stopped')):
            with self.assertRaises(ValueError): evidence.validate(self.before, snapshot, expectation)

    def test_wrong_identity_rejected(self):
        with self.assertRaisesRegex(ValueError, 'identity'):
            evidence.validate(self.before, dict(self.before, instance_id='other'), 'absent')

    def test_daemon_failure_is_not_absence(self):
        with patch.object(evidence, 'docker', side_effect=ValueError('docker_operation_failed')):
            with self.assertRaisesRegex(ValueError, 'docker_operation_failed'):
                evidence.snapshot('fixture','00000000-0000-0000-0000-000000000001')

    def test_confirmed_absence_requires_successful_inventory(self):
        with patch.object(evidence, 'docker', return_value='') as call:
            result=evidence.snapshot('fixture','00000000-0000-0000-0000-000000000001')
        self.assertFalse(result['container_exists'])
        self.assertEqual(call.call_count,1)
