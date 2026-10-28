import importlib.util
import argparse
import copy
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import threading
import subprocess
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('update_agent',Path(__file__).parents[1]/'host/update_agent.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
installer_spec=importlib.util.spec_from_file_location('install_update_agent',Path(__file__).parents[1]/'host/install-update-agent.py')
bootstrap=importlib.util.module_from_spec(installer_spec);installer_spec.loader.exec_module(bootstrap)

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
    def test_engine_specific_image_id_is_bound_to_archive_content(self):
        config={'config':{'Env':['PATH=/usr/bin']},'rootfs':{'diff_ids':['sha256:'+'3'*64]}}
        self.manifest['image']='sha256:'+hashlib.sha256(json.dumps(config).encode()).hexdigest()
        with tarfile.open(self.stage/'kernel-image.tar','w') as t:
            for name,value in [('manifest.json',[{'Config':'config.json','RepoTags':['sub2api-docker-kernel:release-2.15.4-amd64']}]),('config.json',config)]:
                data=json.dumps(value).encode();info=tarfile.TarInfo(name);info.size=len(data);t.addfile(info,io.BytesIO(data))
        self.manifest['files']['kernel-image.tar']=m.digest(self.stage/'kernel-image.tar')
        (self.stage/'runtime-release.json').write_text(json.dumps(self.manifest))
        payload=self.root/'payload';kernel=payload/'usr/local/bin/kin-codex-kernel';kernel.parent.mkdir(parents=True);kernel.write_text('private fixture')
        Path(self.config['templates']).write_text(json.dumps({'default':m.payload_template(payload)}))
        actual={'Id':'sha256:'+'4'*64,'Architecture':'amd64','Os':'linux','Config':config['config'],'RootFS':{'Layers':config['rootfs']['diff_ids']}}
        def command(*args,**kwargs):
            if args[:2]==('docker','info'):return 'x86_64' if args[-1]=='{{.Architecture}}' else str(self.root)
            if args[:3]==('docker','image','inspect'):return json.dumps([actual])
            if args[-1]=='--version':return '2.15.4'
            return ''
        with patch.object(m,'run',side_effect=command):
            self.assertEqual(self.agent.prepare('2.15.4')['image'],actual['Id'])
            actual['Config'] = dict(config['config'], Hostname='', Domainname='', User='', Image='', WorkingDir='',
                                    AttachStdin=False, AttachStdout=False, AttachStderr=False, Tty=False,
                                    OpenStdin=False, StdinOnce=False, Cmd=None, OnBuild=None, Volumes=None)
            self.assertEqual(self.agent.prepare('2.15.4')['image'],actual['Id'])
            for key,value in [('User','10001'),('Cmd',['unexpected']),('Env',['PATH=/evil']),('UnknownField',False)]:
                previous=copy.deepcopy(actual['Config'])
                actual['Config'][key]=value
                with self.subTest(key=key),self.assertRaisesRegex(ValueError,'loaded_image_content_mismatch'):self.agent.prepare('2.15.4')
                actual['Config']=previous
            actual['RootFS']['Layers']=['unexpected layer']
            with self.assertRaisesRegex(ValueError,'loaded_image_content_mismatch'):self.agent.prepare('2.15.4')

    def test_interrupted_activation_recovers(self):
        m.atomic_json(self.agent.root/'activation.json',{'unit':'restored unit','templates':'{}'})
        with patch.object(m,'run',return_value=''):self.agent.recover()
        self.assertEqual(Path(self.config['helper_unit']).read_text(),'restored unit')
        self.assertFalse((self.agent.root/'activation.json').exists())

    def test_fresh_activation_starts_helper_when_unit_is_confirmed_missing(self):
        unit=Path(self.config['helper_unit']);unit.unlink()
        stop=('systemctl','stop',self.config['helper_service'])
        show=('systemctl','show','--property=LoadState',self.config['helper_service'])
        def command(*args,**kwargs):
            if args==stop and not unit.exists():raise subprocess.CalledProcessError(5,args)
            if args==show:return 'LoadState=not-found'
            if args[:2]==('systemctl','start'):self.assertTrue(unit.is_file())
            return ''
        with patch.object(self.agent,'health',side_effect=[False,True]),patch.object(self.agent,'prepare',return_value=self.manifest),patch.object(m,'run',side_effect=command) as commands:
            self.agent.activate('2.15.4')
        self.assertIn(show,[call.args for call in commands.call_args_list])
        self.assertIn(('systemctl','start','helper.service'),[call.args for call in commands.call_args_list])
        self.assertIn(str(self.stage/'vm-runtime-helper'),unit.read_text())
        self.assertEqual(json.loads((self.agent.root/'active.json').read_text())['version'],'2.15.4')
        self.assertFalse((self.agent.root/'activation.json').exists())

    def test_fresh_interrupted_activation_recovers_without_helper_unit(self):
        unit=Path(self.config['helper_unit']);unit.unlink()
        templates=Path(self.config['templates']);previous=templates.read_text()
        journal=self.agent.root/'activation.json'
        m.atomic_json(journal,{'unit':None,'templates':previous,'active':None})
        templates.write_text('{}');(self.agent.root/'active.json').write_text('{}')
        def command(*args,**kwargs):
            if args[:2]==('systemctl','stop'):raise subprocess.CalledProcessError(5,args)
            if args[:2]==('systemctl','show'):return 'LoadState=not-found'
            return ''
        with patch.object(m,'run',side_effect=command) as commands:self.agent.recover()
        self.assertEqual(templates.read_text(),previous)
        self.assertFalse(unit.exists());self.assertFalse((self.agent.root/'active.json').exists())
        self.assertFalse(journal.exists())
        self.assertNotIn(('systemctl','start','helper.service'),[call.args for call in commands.call_args_list])

    def test_fresh_activation_start_failure_restores_absent_unit(self):
        unit=Path(self.config['helper_unit']);unit.unlink()
        templates=Path(self.config['templates']);previous=templates.read_bytes()
        start_error=subprocess.CalledProcessError(1,('systemctl','start','helper.service'))
        def command(*args,**kwargs):
            if args[:2]==('systemctl','stop') and not unit.exists():raise subprocess.CalledProcessError(5,args)
            if args[:2]==('systemctl','show'):return 'LoadState=not-found'
            if args[:2]==('systemctl','start'):raise start_error
            return ''
        with patch.object(self.agent,'health',return_value=False),patch.object(self.agent,'prepare',return_value=self.manifest),patch.object(m,'run',side_effect=command):
            with self.assertRaises(subprocess.CalledProcessError) as error:self.agent.activate('2.15.4')
        self.assertIs(error.exception,start_error)
        self.assertEqual(templates.read_bytes(),previous)
        self.assertFalse(unit.exists());self.assertFalse((self.agent.root/'active.json').exists())
        self.assertFalse((self.agent.root/'activation.json').exists())

    def test_unconfirmed_stop_failure_blocks_activation_and_recovery(self):
        unit=Path(self.config['helper_unit']);unit.unlink()
        templates=Path(self.config['templates']);previous=templates.read_bytes()
        stop_error=subprocess.CalledProcessError(5,('systemctl','stop','helper.service'))
        def command(*args,**kwargs):
            if args[:2]==('systemctl','stop'):raise stop_error
            if args[:2]==('systemctl','show'):return 'LoadState=loaded'
            self.fail('unexpected command after failed stop: '+str(args))
        with patch.object(self.agent,'health',return_value=False),patch.object(self.agent,'prepare',return_value=self.manifest),patch.object(m,'run',side_effect=command):
            with self.assertRaises(subprocess.CalledProcessError) as error:self.agent.activate('2.15.4')
            self.assertIs(error.exception,stop_error)
            self.assertTrue((self.agent.root/'activation.json').exists())
            with self.assertRaises(subprocess.CalledProcessError) as error:self.agent.recover()
            self.assertIs(error.exception,stop_error)
        self.assertEqual(templates.read_bytes(),previous)
        self.assertFalse(unit.exists());self.assertFalse((self.agent.root/'active.json').exists())
        self.assertTrue((self.agent.root/'activation.json').exists())

    def test_failed_stop_requires_exact_successful_not_found_state(self):
        stop_error=subprocess.CalledProcessError(5,('systemctl','stop','helper.service'))
        for state in ('LoadState=loaded','LoadState=masked','LoadState=error','',
                      'LoadState=not-found\nActiveState=active','not-found',
                      subprocess.CalledProcessError(1,('systemctl','show')),
                      subprocess.TimeoutExpired(('systemctl','show'),180),OSError('unavailable')):
            with self.subTest(state=repr(state)),patch.object(m,'run',side_effect=[stop_error,state]):
                with self.assertRaises(subprocess.CalledProcessError) as error:self.agent.stop_helper()
                self.assertIs(error.exception,stop_error)
        with patch.object(m,'run',return_value='') as commands:
            self.agent.stop_helper()
            commands.assert_called_once_with('systemctl','stop','helper.service')
        timeout=subprocess.TimeoutExpired(('systemctl','stop'),180)
        with patch.object(m,'run',side_effect=timeout) as commands:
            with self.assertRaises(subprocess.TimeoutExpired) as error:self.agent.stop_helper()
            self.assertIs(error.exception,timeout)
            commands.assert_called_once_with('systemctl','stop','helper.service')

    def pool_manifest(self):
        manifest=copy.deepcopy(self.manifest)
        manifest.update(manifest_version=2,minimum_updater=2)
        manifest['images']={profile:{'image':'sha256:'+str(index+2)*64,'tag':m.image_tag('2.15.4','amd64',profile)} for index,profile in enumerate(('default',*m.PROFILES))}
        manifest['image']=manifest['images']['default']['image']
        (self.stage/'runtime-release.json').write_text(json.dumps(manifest))
        return manifest

    def owned_fixture(self, profile, number):
        instance='instance-'+str(number)
        directory=Path(self.config['instances'])/instance
        directory.mkdir(parents=True)
        state={'instance_id':instance,'account_id':number,'spec':{'template':profile}}
        (directory/'manifest.json').write_text(json.dumps(state))
        return {'Id':'container-'+str(number),'Image':'sha256:'+'1'*64,'Config':{'Image':'sha256:'+'1'*64,'Labels':{'sub2.vm.runtime':'container-v1','sub2.vm.instance':instance,'sub2.vm.account':str(number)}}}

    def test_multi_image_manifest_validates_arch_catalog_and_tags(self):
        manifest=self.pool_manifest()
        self.assertEqual(self.agent.validate(self.stage,'2.15.4'),manifest)
        for edit,error in [(lambda item:item['images'].pop('fedora-44'),'catalog'),(lambda item:item.update(minimum_updater=3),'bootstrap'),(lambda item:item['images']['ubuntu-24.04'].update(tag='another/image:latest'),'tag')]:
            altered=copy.deepcopy(manifest);edit(altered)
            (self.stage/'runtime-release.json').write_text(json.dumps(altered))
            with self.assertRaisesRegex(ValueError,error):self.agent.validate(self.stage,'2.15.4')
        manifest['arch']='arm64';self.agent.arch='arm64'
        manifest['images']={key:dict(value,tag=m.image_tag('2.15.4','arm64',key)) for key,value in manifest['images'].items() if key in ('default','ubuntu-24.04','debian-12')}
        (self.stage/'runtime-release.json').write_text(json.dumps(manifest))
        self.agent.validate(self.stage,'2.15.4')

    def test_first_pool_activation_keeps_default_and_unknown_containers(self):
        manifest=self.pool_manifest()
        templates=json.loads(Path(self.config['templates']).read_text())
        templates['custom']=copy.deepcopy(templates['default'])
        Path(self.config['templates']).write_text(json.dumps(templates))
        self.owned_fixture('default',1);self.owned_fixture('custom',2)
        with patch.object(self.agent,'health',side_effect=[False,True]),patch.object(self.agent,'prepare',return_value=manifest),patch.object(self.agent,'cleanup'),patch.object(m,'run',return_value='') as command:
            self.agent.activate('2.15.4')
        updated=json.loads(Path(self.config['templates']).read_text())
        self.assertEqual(updated['default'],templates['default'])
        self.assertEqual(updated['custom'],templates['custom'])
        for profile in m.PROFILES:self.assertEqual(updated[profile]['image'],manifest['images'][profile]['image'])
        self.assertFalse(any(call.args[0]=='docker' for call in command.call_args_list))

    def test_only_changed_profile_containers_are_removed(self):
        manifest=self.pool_manifest()
        templates=self.agent.next_templates(json.loads(Path(self.config['templates']).read_text()),manifest)
        templates['ubuntu-24.04']['image']='sha256:'+'a'*64
        Path(self.config['templates']).write_text(json.dumps(templates))
        for index,profile in enumerate(('default','ubuntu-24.04','debian-12')):
            item=self.owned_fixture(profile,index+1)
            if profile=='ubuntu-24.04':affected=item
        def command(*args):
            if args[:2]==('docker','ps'):return affected['Id']
            if args[:2]==('docker','inspect'):return json.dumps([affected])
            return ''
        with patch.object(self.agent,'health',side_effect=[False,True]),patch.object(self.agent,'prepare',return_value=manifest),patch.object(self.agent,'cleanup'),patch.object(m,'run',side_effect=command) as commands:
            self.agent.activate('2.15.4')
        stopped=[call.args[-1] for call in commands.call_args_list if call.args[:2]==('docker','stop')]
        removed=[call.args[-1] for call in commands.call_args_list if call.args[:2]==('docker','rm')]
        self.assertEqual(stopped,[affected['Id']]);self.assertEqual(removed,stopped)

    def test_same_helper_version_does_not_skip_missing_profile_templates(self):
        manifest=self.pool_manifest()
        m.atomic_json(self.agent.root/'active.json',{'version':'2.15.4','image':manifest['image']})
        with patch.object(self.agent,'health',return_value=True),patch.object(self.agent,'prepare',return_value=manifest) as prepare,patch.object(self.agent,'cleanup'),patch.object(m,'run',return_value=''):
            self.agent.activate('2.15.4')
        prepare.assert_called_once()
        self.assertIn('fedora-44',json.loads(Path(self.config['templates']).read_text()))

    def test_pool_health_failure_restores_entire_map_and_active_metadata(self):
        manifest=self.pool_manifest()
        original=self.agent.next_templates(json.loads(Path(self.config['templates']).read_text()),manifest)
        original['ubuntu-24.04']['image']='sha256:'+'a'*64
        Path(self.config['templates']).write_text(json.dumps(original))
        active={'version':'2.15.3','images':{key:value['image'] for key,value in original.items()}}
        m.atomic_json(self.agent.root/'active.json',active)
        with patch.object(self.agent,'health',return_value=False),patch.object(self.agent,'prepare',return_value=manifest),patch.object(m,'run',return_value=''),patch.object(m.time,'sleep'):
            with self.assertRaisesRegex(ValueError,'helper_health_failed'):self.agent.activate('2.15.4')
        self.assertEqual(json.loads(Path(self.config['templates']).read_text()),original)
        self.assertEqual(json.loads((self.agent.root/'active.json').read_text()),active)

    def test_recovery_restores_full_map_and_active_record(self):
        original=json.loads(Path(self.config['templates']).read_text())
        original['ubuntu-24.04']=copy.deepcopy(original['default'])
        old={'version':'2.15.3','images':{'default':'sha256:'+'1'*64}}
        m.atomic_json(self.agent.root/'activation.json',{'unit':'restored unit','templates':json.dumps(original),'active':json.dumps(old)})
        with patch.object(m,'run',return_value=''):self.agent.recover()
        self.assertEqual(json.loads(Path(self.config['templates']).read_text()),original)
        self.assertEqual(json.loads((self.agent.root/'active.json').read_text()),old)

    def test_multi_image_preload_checks_each_archive_config_and_payload(self):
        manifest=self.pool_manifest()
        payload=self.root/'payload';kernel=payload/'usr/local/bin/kin-codex-kernel';kernel.parent.mkdir(parents=True);kernel.write_text('private fixture')
        templates={'default':dict(m.payload_template(payload),image='sha256:'+'1'*64)}
        Path(self.config['templates']).write_text(json.dumps(templates))
        entries=[];configs={};actual={}
        for index,(profile,image) in enumerate(manifest['images'].items()):
            name=profile+'.json'
            config={'config':{'Env':['PROFILE='+profile]},'rootfs':{'diff_ids':['sha256:'+str(index+1)*64]}}
            image['image']='sha256:'+hashlib.sha256(json.dumps(config).encode()).hexdigest()
            configs[name]=config
            entries.append({'Config':name,'RepoTags':[image['tag']]})
            actual[image['tag']]={'Id':'sha256:'+str(index+3)*64,'Architecture':'amd64','Os':'linux','Config':config['config'],'RootFS':{'Layers':config['rootfs']['diff_ids']}}
        with tarfile.open(self.stage/'kernel-image.tar','w') as archive:
            for name,value in [('manifest.json',entries),*configs.items()]:
                raw=json.dumps(value).encode();info=tarfile.TarInfo(name);info.size=len(raw);archive.addfile(info,io.BytesIO(raw))
        manifest['files']['kernel-image.tar']=m.digest(self.stage/'kernel-image.tar')
        manifest['image']=manifest['images']['default']['image']
        (self.stage/'runtime-release.json').write_text(json.dumps(manifest))
        def command(*args,**kwargs):
            if args[:2]==('docker','info'):return 'x86_64' if args[-1]=='{{.Architecture}}' else str(self.root)
            if args[:3]==('docker','image','inspect'):
                return json.dumps([actual.get(args[3],next(iter(actual.values())))])
            if args[-1]=='--version':return '2.15.4'
            if '--payload' in args:return json.dumps({'ok':True,'profile_id':args[args.index('--profile')+1]})
            if '-c' in args:return json.dumps(self.fingerprint())
            return ''
        with patch.object(m,'run',side_effect=command) as commands:
            resolved=self.agent.prepare('2.15.4')
        self.assertEqual(resolved['images']['ubuntu-24.04']['image'],actual[manifest['images']['ubuntu-24.04']['tag']]['Id'])
        probes=[call.args for call in commands.call_args_list if '--profile' in call.args]
        self.assertEqual(len(probes),10)
        for probe in probes:
            if '--payload' in probe:
                self.assertIn('--user=10001:10001',probe);self.assertNotIn('--network-check',probe)
            else:self.assertIn('--cap-add=NET_ADMIN',probe);self.assertIn('--network-check',probe)
        cleanup=[call.args for call in commands.call_args_list if call.args[:3]==('docker','rm','-f')]
        self.assertEqual(len(cleanup),13)
        actual[manifest['images']['fedora-44']['tag']]['Architecture']='arm64'
        with patch.object(m,'run',side_effect=command):
            with self.assertRaisesRegex(ValueError,'architecture'):self.agent.prepare('2.15.4')

    def test_capabilities_endpoint(self):
        socket=str(self.root/'api.sock')
        with m.Server(socket,m.Handler) as server:
            thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
            conn=m.LocalHTTP(socket)
            try:
                conn.request('GET','/v1/capabilities');response=conn.getresponse()
                self.assertEqual(response.status,200)
                self.assertEqual(json.loads(response.read()),{'updater_version':2,'multi_image':True})
            finally:conn.close();server.shutdown();thread.join()

    def test_disk_and_expanded_archive_limits(self):
        with patch.object(m.shutil,'disk_usage',return_value=argparse.Namespace(free=m.DISK_RESERVE)):
            with self.assertRaisesRegex(ValueError,'insufficient'):m.require_space(self.root,1)
        archive=self.root/'large.tar.gz'
        with tarfile.open(archive,'w:gz') as stream:
            info=tarfile.TarInfo('file');info.size=4;stream.addfile(info,io.BytesIO(b'data'))
        with patch.object(m,'MAX_EXPANDED',3):
            with self.assertRaisesRegex(ValueError,'expanded'):m.extract(archive,self.root)

    def test_cleanup_preserves_running_images_active_and_rollback(self):
        templates=json.loads(Path(self.config['templates']).read_text())
        old=self.agent.root/'2.15.1';old.mkdir()
        referenced='sha256:'+'a'*64;unused='sha256:'+'b'*64
        images={'default':{'image':referenced,'tag':m.image_tag('2.15.1','amd64')},'ubuntu-24.04':{'image':unused,'tag':m.image_tag('2.15.1','amd64','ubuntu-24.04')}}
        m.atomic_json(old/'.prepared.json',{'managed_by':'sub2api-runtime-updater-v2','version':'2.15.1','images':images})
        m.atomic_json(old/'.release-files.json',{'files':[],'directories':[]})
        rollback=self.agent.root/'2.15.3';rollback.mkdir()
        unknown=self.agent.root/'1.0.0';unknown.mkdir();(unknown/'business-record').write_text('preserve')
        active={'version':'2.15.4','rollback_version':'2.15.3','images':{'default':templates['default']['image']},'rollback_images':{}}
        def command(*args):
            if args[:2]==('docker','ps'):return 'business-container'
            if args[:2]==('docker','inspect'):return json.dumps([{'Image':referenced,'Config':{'Image':referenced}}])
            if args[:3]==('docker','image','inspect'):return json.dumps([{'Id':unused}])
            return ''
        with patch.object(m,'run',side_effect=command) as commands:self.agent.cleanup(active)
        self.assertFalse(old.exists());self.assertTrue(rollback.exists());self.assertTrue(unknown.exists());self.assertTrue(self.stage.exists())
        removals=[call.args for call in commands.call_args_list if call.args[:3]==('docker','image','rm')]
        self.assertEqual(removals,[('docker','image','rm',images['ubuntu-24.04']['tag'])])

    def test_bootstrap_atomically_upgrades_and_restarts_existing_agent(self):
        unit_root=self.root/'units';unit_root.mkdir()
        args=argparse.Namespace(base=str(self.root/'host'),instances=self.config['instances'],helper_socket=self.config['helper_socket'],templates=self.config['templates'],service_uid=1000,service_gid=20000,helper_service='helper.service',version=None,activate=False,payload_dir=None)
        inode=Path(self.config['helper_socket']+'.coordinator.lock').stat().st_ino
        with patch.object(bootstrap.os,'geteuid',return_value=0),patch.object(bootstrap.os,'chown'),patch.object(bootstrap.os,'fchown'),patch.object(bootstrap.subprocess,'run') as command:
            bootstrap.install(args,unit_root)
            bootstrap.install(args,unit_root)
        self.assertEqual(Path(self.config['helper_socket']+'.coordinator.lock').stat().st_ino,inode)
        self.assertEqual((Path(args.base)/'update_agent.py').read_bytes(),Path(m.__file__).read_bytes())
        self.assertEqual(sum(call.args[0]==['systemctl','restart','sub2api-runtime-update.service'] for call in command.call_args_list),2)

    def test_probe_timeout_removes_only_its_unique_container(self):
        def command(*args,**kwargs):
            if args[:2]==('docker','run'):raise subprocess.TimeoutExpired('docker run',35)
            return ''
        with patch.object(m,'run',side_effect=command) as commands:
            with self.assertRaises(subprocess.TimeoutExpired):m.image_probe('--entrypoint=/bin/true','sha256:'+'a'*64)
        created=commands.call_args_list[0].args
        removed=commands.call_args_list[1].args
        self.assertEqual(removed,('docker','rm','-f',created[created.index('--name')+1]))
        self.assertTrue(removed[-1].startswith('sub2-update-check-'))

    def test_payload_symlinks_are_rejected(self):
        payload=self.root/'payload';kernel=payload/'usr/local/bin/kin-codex-kernel';kernel.parent.mkdir(parents=True);kernel.write_text('private fixture')
        link=payload/'alias';link.symlink_to(kernel)
        with self.assertRaisesRegex(ValueError,'symlink'):m.payload_template(payload)
        link.unlink();link.symlink_to(kernel.parent,target_is_directory=True)
        with self.assertRaisesRegex(ValueError,'symlink'):m.payload_template(payload)

    def test_cross_process_operation_is_exclusive(self):
        with patch.object(m.platform,'machine',return_value='x86_64'):other=m.Installer(self.config)
        with self.agent.operation():
            with self.assertRaisesRegex(ValueError,'in_progress'):
                with other.operation():self.fail('overlapping mutation')
        with other.operation():pass

    def test_fresh_bootstrap_initializes_template_then_activates(self):
        Path(self.config['templates']).unlink()
        unit_root=self.root/'units';unit_root.mkdir()
        payload=self.root/'payload';kernel=payload/'usr/local/bin/kin-codex-kernel';kernel.parent.mkdir(parents=True);kernel.write_text('private fixture')
        args=argparse.Namespace(base=str(self.root/'host'),instances=self.config['instances'],helper_socket=self.config['helper_socket'],templates=self.config['templates'],service_uid=1000,service_gid=20000,helper_service='helper.service',version='2.15.4',activate=True,payload_dir=str(payload))
        real_exec=bootstrap.importlib.machinery.SourceFileLoader.exec_module
        def execute(loader,module):
            real_exec(loader,module)
            if module.__name__=='sub2api_update_agent_bootstrap':
                module.Installer=m.Installer
        with patch.object(bootstrap.os,'geteuid',return_value=0),patch.object(bootstrap.os,'chown'),patch.object(bootstrap.os,'fchown'),patch.object(bootstrap.subprocess,'run') as commands,patch.object(bootstrap.importlib.machinery.SourceFileLoader,'exec_module',execute),patch.object(m.platform,'machine',return_value='x86_64'),patch.object(m.Installer,'prepare',return_value=self.manifest) as prepare,patch.object(m.Installer,'activate') as activate:
            bootstrap.install(args,unit_root)
        prepare.assert_called_once_with('2.15.4',initial_payload=str(payload))
        activate.assert_called_once_with('2.15.4')
        template=json.loads(Path(self.config['templates']).read_text())['default']
        self.assertEqual(template['image'],self.manifest['image']);self.assertEqual(template['artifacts'],m.payload_template(payload)['artifacts'])
        self.assertIn(['systemctl','enable','helper.service'],[call.args[0] for call in commands.call_args_list])

    def test_cleanup_skips_unrecorded_files_inside_old_release(self):
        old=self.agent.root/'2.15.1';old.mkdir()
        image={'image':'sha256:'+'b'*64,'tag':m.image_tag('2.15.1','amd64')}
        m.atomic_json(old/'.prepared.json',{'managed_by':'sub2api-runtime-updater-v2','version':'2.15.1','images':{'default':image}})
        m.atomic_json(old/'.release-files.json',{'files':[],'directories':[]})
        (old/'business-record').write_text('preserve')
        active={'version':'2.15.4','images':{'default':'sha256:'+'1'*64}}
        with patch.object(m,'run',return_value='') as commands:self.agent.cleanup(active)
        self.assertTrue((old/'business-record').exists())
        self.assertFalse(any(call.args[:3]==('docker','image','rm') for call in commands.call_args_list))

    def test_pool_network_probe_failure_preserves_installed_templates(self):
        manifest=self.pool_manifest()
        original=Path(self.config['templates']).read_bytes()
        with patch.object(self.agent,'health',return_value=False),patch.object(self.agent,'prepare',side_effect=ValueError('network_probe_failed')),patch.object(m,'run') as command:
            with self.assertRaisesRegex(ValueError,'network_probe_failed'):self.agent.activate('2.15.4')
        self.assertEqual(Path(self.config['templates']).read_bytes(),original)
        command.assert_not_called()

    def test_declared_image_digest_requires_archive_proof(self):
        payload=self.root/'payload';kernel=payload/'usr/local/bin/kin-codex-kernel';kernel.parent.mkdir(parents=True);kernel.write_text('fixture')
        Path(self.config['templates']).write_text(json.dumps({'default':m.payload_template(payload)}))
        config={'config':{'Env':['PATH=/usr/bin']},'rootfs':{'diff_ids':['sha256:'+'3'*64]}}
        config_raw=json.dumps(config).encode();config_digest='sha256:'+hashlib.sha256(config_raw).hexdigest()
        descriptor={'schemaVersion':2,'mediaType':'application/vnd.oci.image.manifest.v1+json','config':{'digest':config_digest}}
        descriptor_raw=json.dumps(descriptor).encode();descriptor_digest='sha256:'+hashlib.sha256(descriptor_raw).hexdigest()
        index={'schemaVersion':2,'mediaType':'application/vnd.oci.image.index.v1+json','manifests':[{'digest':descriptor_digest,'platform':{'os':'linux','architecture':'amd64'}}]}
        index_raw=json.dumps(index).encode();index_digest='sha256:'+hashlib.sha256(index_raw).hexdigest()
        actual={'Id':'sha256:'+'4'*64,'Architecture':'amd64','Os':'linux','Config':config['config'],'RootFS':{'Layers':config['rootfs']['diff_ids']}}
        entries=[{'Config':'config.json','RepoTags':[m.image_tag('2.15.4','amd64')]}]
        def archive_write(descriptor_content=descriptor_raw,index_content=index_raw):
            with tarfile.open(self.stage/'kernel-image.tar','w') as archive:
                for name,raw in [('manifest.json',json.dumps(entries).encode()),('config.json',config_raw),('blobs/sha256/'+descriptor_digest[7:],descriptor_content),('blobs/sha256/'+index_digest[7:],index_content)]:
                    info=tarfile.TarInfo(name);info.size=len(raw);archive.addfile(info,io.BytesIO(raw))
            self.manifest['files']['kernel-image.tar']=m.digest(self.stage/'kernel-image.tar')
            (self.stage/'runtime-release.json').write_text(json.dumps(self.manifest))
        def command(*args,**kwargs):
            if args[:2]==('docker','info'):return 'x86_64' if args[-1]=='{{.Architecture}}' else str(self.root)
            if args[:3]==('docker','image','inspect'):return json.dumps([actual])
            if args[-1]=='--version':return '2.15.4'
            return ''
        for digest in (config_digest,descriptor_digest,index_digest):
            with self.subTest(digest=digest):
                self.manifest['image']=digest;archive_write()
                with patch.object(m,'run',side_effect=command):self.assertEqual(self.agent.prepare('2.15.4')['image'],actual['Id'])
        self.manifest['image']='sha256:'+'9'*64;archive_write()
        with patch.object(m,'run') as commands:
            with self.assertRaisesRegex(ValueError,'image_digest_mismatch'):self.agent.prepare('2.15.4')
        commands.assert_not_called()
        self.manifest['image']=descriptor_digest;archive_write(descriptor_content=b'{}')
        with patch.object(m,'run') as commands:
            with self.assertRaisesRegex(ValueError,'image_digest_mismatch'):self.agent.prepare('2.15.4')
        commands.assert_not_called()

    def fingerprint(self,version='12',script='a'):
        return {'os_release':{'ID':'debian','VERSION_ID':version},'runtime_scripts':{name:script*64 for name in m.DEFAULT_SCRIPTS}}

    def test_default_pool_only_preserves_image_and_overrides_forged_refresh(self):
        manifest=self.pool_manifest();manifest['refresh_default']=True
        templates=json.loads(Path(self.config['templates']).read_text())
        with patch.object(self.agent,'default_fingerprint',return_value=self.fingerprint()):self.agent.assess_default_refresh(manifest,templates)
        self.assertFalse(manifest['refresh_default'])
        self.assertEqual(self.agent.next_templates(templates,manifest)['default'],templates['default'])

    def test_same_system_script_change_refreshes_default_under_existing_fence(self):
        manifest=self.pool_manifest()
        templates=json.loads(Path(self.config['templates']).read_text())
        with patch.object(self.agent,'default_fingerprint',side_effect=[self.fingerprint(script='a'),self.fingerprint(script='b')]):self.agent.assess_default_refresh(manifest,templates)
        self.assertTrue(manifest['refresh_default'])
        affected=self.owned_fixture('default',1)
        def command(*args):
            if args[:2]==('docker','ps'):return affected['Id']
            if args[:2]==('docker','inspect'):return json.dumps([affected])
            return ''
        with patch.object(self.agent,'health',side_effect=[False,True]),patch.object(self.agent,'prepare',return_value=manifest),patch.object(self.agent,'cleanup'),patch.object(m,'run',side_effect=command) as commands:self.agent.activate('2.15.4')
        self.assertEqual(json.loads(Path(self.config['templates']).read_text())['default']['image'],manifest['image'])
        self.assertIn(('docker','stop','--time=40',affected['Id']),[call.args for call in commands.call_args_list])
        self.assertIn(('docker','rm',affected['Id']),[call.args for call in commands.call_args_list])

    def test_default_script_change_cannot_migrate_system(self):
        manifest=self.pool_manifest()
        templates=json.loads(Path(self.config['templates']).read_text())
        previous=templates['default']['image']
        with patch.object(self.agent,'default_fingerprint',side_effect=lambda image:self.fingerprint('12','a') if image==previous else self.fingerprint('13','b')):
            with self.assertRaisesRegex(ValueError,'system_migration_forbidden'):self.agent.assess_default_refresh(manifest,templates)
        self.assertFalse(manifest['refresh_default'])

    def test_default_alias_refresh_uses_verified_same_system_pool_image(self):
        manifest=self.pool_manifest()
        templates=json.loads(Path(self.config['templates']).read_text())
        previous=templates['default']['image'];same=manifest['images']['debian-12']['image']
        def fingerprint(image):
            return self.fingerprint('12','a') if image==previous else self.fingerprint('12','b') if image==same else self.fingerprint('13','b')
        with patch.object(self.agent,'default_fingerprint',side_effect=fingerprint):
            self.agent.assess_default_refresh(manifest,templates)
        self.assertTrue(manifest['refresh_default'])
        self.assertEqual(manifest['image'],same)
        self.assertEqual(self.agent.next_templates(templates,manifest)['default']['image'],same)
        self.assertEqual(templates['default']['image'],previous)

    def test_default_alias_rejects_same_system_with_stale_runtime(self):
        manifest=self.pool_manifest()
        templates=json.loads(Path(self.config['templates']).read_text())
        previous=templates['default']['image'];same=manifest['images']['debian-12']['image']
        def fingerprint(image):
            return self.fingerprint('12','a') if image in (previous,same) else self.fingerprint('13','b')
        with patch.object(self.agent,'default_fingerprint',side_effect=fingerprint):
            with self.assertRaisesRegex(ValueError,'system_migration_forbidden'):
                self.agent.assess_default_refresh(manifest,templates)
        self.assertFalse(manifest['refresh_default'])

    def test_missing_default_image_is_not_silently_replaced(self):
        with patch.object(m,'run',side_effect=subprocess.CalledProcessError(1,['docker','image','inspect'])):
            with self.assertRaisesRegex(ValueError,'default_image_unavailable'):self.agent.default_fingerprint('sha256:'+'1'*64)

if __name__=='__main__':unittest.main()
