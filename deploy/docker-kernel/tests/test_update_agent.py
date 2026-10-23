import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('update_agent',Path(__file__).parents[1]/'host/update_agent.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class AgentTest(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.root=Path(self.temp.name)
        self.config={'releases':str(self.root/'releases'),'templates':str(self.root/'templates.json'), 'helper_socket':str(self.root/'helper.sock'), 'helper_unit':str(self.root/'helper.service'),'helper_service':'helper.service','instances':str(self.root/'instances'),'service_uid':1000,'service_gid':20000}
        with patch.object(m.platform,"machine",return_value="x86_64"):
            self.agent=m.Installer(self.config)
        Path(self.config['templates']).write_text(json.dumps({'default':{'runtime':'container-v1','image':'sha256:'+'1'*64,'payload_dir':'/private/payload','artifacts':{}}}))
        Path(self.config['helper_unit']).write_text('old unit')
        Path(self.config['helper_socket']+'.coordinator.lock').touch()
        stage=self.agent.root/'2.15.4';stage.mkdir()
        for f in ('vm-runtime-helper','kernel-image.tar'):(stage/f).write_text('fixture')
        self.manifest={'version':'2.15.4','arch':self.agent.arch,'protocol':1,'image':'sha256:'+'2'*64,'files':{f:m.digest(stage/f) for f in ('vm-runtime-helper','kernel-image.tar')}}
        (stage/'runtime-release.json').write_text(json.dumps(self.manifest));self.stage=stage
    def tearDown(self):self.temp.cleanup()
    def test_checksum_and_version_mismatch(self):
        self.assertEqual(self.agent.validate(self.stage,'2.15.4'),self.manifest)
        with self.assertRaisesRegex(ValueError,'incompatible'):self.agent.validate(self.stage,'2.15.5')
        (self.stage/'kernel-image.tar').write_text('tampered')
        with self.assertRaisesRegex(ValueError,'checksum'):self.agent.validate(self.stage,'2.15.4')
    def test_rejects_unsafe_archive(self):
        for name,kind in [('../escape',tarfile.REGTYPE),('/absolute',tarfile.REGTYPE),('link',tarfile.SYMTYPE)]:
            archive=self.root/'bad.tar.gz'
            with tarfile.open(archive,'w:gz') as t:
                info=tarfile.TarInfo(name);info.type=kind;t.addfile(info)
            with self.assertRaisesRegex(ValueError,'unsafe'):m.extract(archive,self.root/'out')
    def test_active_application_blocks_activation(self):
        with open(self.config['helper_socket']+'.coordinator.lock','r+') as fence,patch.object(self.agent,'health',return_value=False),patch.object(self.agent,'prepare',return_value=self.manifest),patch.object(m,'run') as run:
            m.fcntl.flock(fence,m.fcntl.LOCK_EX|m.fcntl.LOCK_NB)
            with self.assertRaisesRegex(ValueError,'application_still_running'):self.agent.activate('2.15.4')
            run.assert_not_called()
    def test_health_failure_restores_previous_unit_and_template(self):
        previous=Path(self.config['templates']).read_bytes()
        with patch.object(self.agent,'health',return_value=False),patch.object(self.agent,'prepare',return_value=self.manifest),patch.object(m,'run',return_value=''),patch.object(m.time,'sleep'):
            with self.assertRaisesRegex(ValueError,'helper_health_failed'):self.agent.activate('2.15.4')
        self.assertEqual(Path(self.config['templates']).read_bytes(),previous)
        self.assertEqual(Path(self.config['helper_unit']).read_text(),'old unit')
        self.assertFalse((self.agent.root/'activation.json').exists())
    def test_success_updates_pair_and_preserves_instances(self):
        with patch.object(self.agent,'health',side_effect=[False,True]),patch.object(self.agent,'prepare',return_value=self.manifest),patch.object(m,'run',return_value=''):
            self.agent.activate('2.15.4')
        self.assertEqual(json.loads(Path(self.config['templates']).read_text())['default']['image'],self.manifest['image'])
        self.assertIn(str(self.stage/'vm-runtime-helper'),Path(self.config['helper_unit']).read_text())
        self.assertEqual(json.loads((self.agent.root/'active.json').read_text())['version'],'2.15.4')
    def test_interrupted_activation_recovers(self):
        m.atomic_json(self.agent.root/'activation.json',{'unit':'restored unit','templates':'{}'})
        with patch.object(m,'run',return_value=''):self.agent.recover()
        self.assertEqual(Path(self.config['helper_unit']).read_text(),'restored unit')
        self.assertFalse((self.agent.root/'activation.json').exists())

if __name__=='__main__':unittest.main()
