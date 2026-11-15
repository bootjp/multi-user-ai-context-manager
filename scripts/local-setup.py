#!/usr/bin/env python3
"""Create a private local demo project using the explicitly bootstrapped token."""
import json
import os
from pathlib import Path
import shlex
import subprocess
import urllib.request

root = Path(__file__).resolve().parent.parent
private = root / 'work/local'
address = subprocess.check_output(['docker', 'compose', 'port', 'app', '8080'], cwd=root, text=True).strip().splitlines()[0]
base = 'http://' + address


def request(path, token, body=None, key=None):
    headers = {'Authorization': 'Bearer ' + token}
    if key:
        headers['Idempotency-Key'] = key
    if body is not None:
        headers['Content-Type'] = 'application/json'
    data = None if body is None else json.dumps(body).encode()
    with urllib.request.urlopen(urllib.request.Request(base + path, data=data, headers=headers), timeout=30) as r:
        return json.load(r)


def store(path, value):
    with open(path, 'x', opener=lambda p, f: os.open(p, f, 0o600)) as file:
        json.dump(value, file)
        file.flush()
        os.fsync(file.fileno())


try:
    admin = (private / 'admin.token').read_text().strip()
    project_file = private / 'project.json'
    if project_file.exists():
        project = json.loads(project_file.read_text())
    else:
        project = request('/v1/projects', admin, {'name': 'local-demo'}, 'local-demo-project')
        store(project_file, project)
    session_file = private / 'session.json'
    if session_file.exists():
        session = json.loads(session_file.read_text())
    else:
        session = request('/v1/projects/' + project['id'] + '/tokens', project['token'], {'display_name': 'local/session', 'role': 'editor'}, 'local-demo-session')
        store(session_file, session)
    request('/v1/me', session['token'])
    config = private / 'multi-user-ai-context-manager.env'
    # Only this script's generated config is refreshed; source credentials are never overwritten.
    with open(config, 'w', opener=lambda p, f: os.open(p, f, 0o600)) as file:
        for key, value in {'MULTI_USER_AI_CONTEXT_MANAGER_URL': base, 'MULTI_USER_AI_CONTEXT_MANAGER_PROJECT': project['id'], 'MULTI_USER_AI_CONTEXT_MANAGER_TOKEN': session['token']}.items():
            file.write('export ' + key + '=' + shlex.quote(value) + '\n')
    config.chmod(0o600)
    print('Local console: ' + base + '/')
    print('Private session environment: ' + str(config))
    print('Administrator token: ' + str(private / 'admin.token') + ' (not printed)')
except (OSError, ValueError):
    raise SystemExit('Local setup could not finish. Existing private credentials were preserved. Check service availability and token expiration; recover credentials explicitly if needed.')
