#!/usr/bin/env python3
"""Collect copyright/license notices for the actually linked host modules."""
import json
import pathlib
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[1]
out = pathlib.Path(sys.argv[1])
out.mkdir(parents=True, exist_ok=True)
stream = subprocess.check_output(['go', 'list', '-deps', '-json', './cmd/p2pvideod'], cwd=root / 'host', text=True)
decoder = json.JSONDecoder()
position = 0
modules = {}
while position < len(stream):
    while position < len(stream) and stream[position].isspace():
        position += 1
    if position == len(stream):
        break
    package, position = decoder.raw_decode(stream, position)
    module = package.get('Module', {})
    if module and not module.get('Main'):
        modules[module['Path']] = module.get('Replace', module)
index = ['Third-party notices for p2p.video host', '']
for module_name, module in sorted(modules.items()):
    source = pathlib.Path(module['Dir'])
    notices = [p for p in source.iterdir() if p.is_file() and p.name.lower().startswith(('license', 'copying', 'notice', 'copyright'))]
    if not notices:
        raise SystemExit(f'Missing license notice for {module_name}; review before distribution')
    target = out / (module_name.replace('/', '__') + '@' + module.get('Version', 'local'))
    target.mkdir(exist_ok=True)
    for notice in notices:
        (target / notice.name).write_bytes(notice.read_bytes())
    index.append(module_name + ' ' + module.get('Version', 'local'))
golang = pathlib.Path(subprocess.check_output(['go', 'env', 'GOROOT'], text=True).strip())
runtime_license = next((p for p in [golang / 'LICENSE', golang.parent / 'LICENSE'] if p.is_file()), None)
if runtime_license is None:
    raise SystemExit('Go runtime license missing; review before distribution')
(out / 'Go-LICENSE.txt').write_bytes(runtime_license.read_bytes())
(out / 'README.txt').write_text('\n'.join(index) + '\n')
print(f'Collected {len(modules)} module notices and Go runtime license')
