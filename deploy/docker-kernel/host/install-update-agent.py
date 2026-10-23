#!/usr/bin/env python3
"""One-time root bootstrap. Existing kernel payload remains private on the host."""
import argparse,json,os,re,shutil,subprocess
from pathlib import Path
p=argparse.ArgumentParser()
p.add_argument('--base',required=True)
p.add_argument('--instances',required=True)
p.add_argument('--helper-socket',required=True)
p.add_argument('--templates',required=True)
p.add_argument('--service-uid',type=int,default=1000)
p.add_argument('--service-gid',type=int,default=20000)
p.add_argument('--helper-service',default='sub2api-vm-helper.service')
a=p.parse_args()
if os.geteuid()!=0:raise SystemExit('run as root')
for path in (a.base,a.instances,a.helper_socket,a.templates):
 if not path.startswith('/') or not re.fullmatch(r'[a-zA-Z0-9_./-]+',path) or '..' in Path(path).parts:raise SystemExit('invalid path')
if not re.fullmatch(r'[a-zA-Z0-9_-]+\.service',a.helper_service):raise SystemExit('invalid service')
base=Path(a.base);base.mkdir(parents=True,exist_ok=True,mode=0o700)
config={'releases':str(base/'releases'),'instances':a.instances,'helper_socket':a.helper_socket,'templates':a.templates,'service_uid':a.service_uid,'service_gid':a.service_gid,'helper_service':a.helper_service,'helper_unit':'/etc/systemd/system/'+a.helper_service}
path=base/'update-agent.json'
if path.exists() and json.loads(path.read_text())!=config:raise SystemExit('existing configuration differs; review before replacing')
path.write_text(json.dumps(config));path.chmod(0o600)
shutil.copyfile(Path(__file__).with_name('update_agent.py'),base/'update_agent.py')
(base/'update_agent.py').chmod(0o700)
unit=Path('/etc/systemd/system/sub2api-runtime-update.service')
unit.write_text('[Unit]\nDescription=Sub2API paired runtime updater\nAfter=docker.service\nRequires=docker.service\n[Service]\nExecStart=/usr/bin/python3 '+str(base/'update_agent.py')+' --config '+str(path)+'\nRestart=on-failure\nUMask=0077\n[Install]\nWantedBy=multi-user.target\n')
subprocess.run(['systemctl','daemon-reload'],check=True)
subprocess.run(['systemctl','enable','--now','sub2api-runtime-update.service'],check=True)
print('Update agent installed. Prepare the matching release before restarting Sub2API.')
