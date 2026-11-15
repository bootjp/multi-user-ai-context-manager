#!/usr/bin/env python3
"""Preview, then explicitly migrate the legacy local Compose stack without deleting it."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

NAME = 'multi-user-ai-context-manager'
LEGACY = 'agent-context'
ROOT = Path(__file__).resolve().parent.parent


def run(args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs).stdout


def container(project, service):
    ids = run(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + project, '--filter', 'label=com.docker.compose.service=' + service]).decode().split()
    if len(ids) != 1:
        raise RuntimeError('Expected exactly one ' + project + '/' + service + ' container')
    return json.loads(run(['docker', 'inspect', ids[0]]))[0]


def database_info(info):
    env = dict(item.split('=', 1) for item in info['Config']['Env'] if '=' in item)
    return info['Id'], env['POSTGRES_USER'], env['POSTGRES_DB']


def sql(info, statement):
    cid, user, database = database_info(info)
    return run(['docker', 'exec', '-e', 'PGTZ=UTC', cid, 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-U', user, '-d', database, '-c', statement]).decode().strip()


def fingerprint(info, table, audit_max=None):
    quoted = '"' + table.replace('"', '""') + '"'
    expression = 'to_jsonb(t)'
    if table == 'instance_metadata':
        expression += " - 'epoch'"
    if table in ('tasks', 'resource_leases'):
        expression += " - ARRAY['version','lease_generation','lease_hash','lease_expires_at']"
    where = '' if audit_max is None else ' WHERE id <= ' + str(int(audit_max))
    return json.loads(sql(info, "SELECT jsonb_build_object('count',count(*),'digest',md5(COALESCE(jsonb_agg(doc ORDER BY doc::text),'[]'::jsonb)::text)) FROM (SELECT " + expression + ' AS doc FROM public.' + quoted + ' t' + where + ') rows'))


def private_files():
    files = [ROOT / '.env'] + [p for p in (ROOT / 'work/local').rglob('*') if p.is_file()]
    return {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest() for p in files}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='stop the old app, back up, restore to a new volume, and verify')
    args = parser.parse_args()
    old_db, old_app = container(LEGACY, 'db'), container(LEGACY, 'app')
    for info in (old_db, old_app):
        directory = info['Config']['Labels'].get('com.docker.compose.project.working_dir')
        if not directory or Path(directory).resolve() != ROOT:
            raise RuntimeError('Legacy containers belong to a different directory')
    if not old_db['State']['Running']:
        raise RuntimeError('Start the legacy database before migration')
    existing = subprocess.run(['docker', 'volume', 'inspect', NAME + '_pgdata'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if existing.returncode == 0:
        raise RuntimeError('New data volume already exists; inspect the previous attempt instead of overwriting it')
    tables = sql(old_db, "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename").splitlines()
    preview = {table: int(sql(old_db, 'SELECT count(*) FROM public."' + table.replace('"', '""') + '"')) for table in tables}
    print('Legacy data volume will be retained. New volume: ' + NAME + '_pgdata', flush=True)
    print('Tables and row counts: ' + json.dumps(preview, sort_keys=True), flush=True)
    if not args.apply:
        print('Preview only: no containers or files changed. Rerun with --apply after reviewing this plan.')
        return
    os.umask(0o077)
    backup = ROOT / 'work/backups' / ('name-migration-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ'))
    backup.mkdir(parents=True, mode=0o700)
    original_files = private_files()
    for relative in original_files:
        target = backup / 'config' / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(ROOT / relative, target)
    # Build first, so compilation failures do not interrupt the running legacy app.
    run(['docker', 'compose', 'build', 'app'])
    run(['docker', 'stop', old_app['Id']])
    new_app_started = False
    try:
        audit_max = int(sql(old_db, 'SELECT COALESCE(max(id),0) FROM audit_entries'))
        before = {table: fingerprint(old_db, table) for table in tables if table != 'idempotency_keys'}
        epoch = sql(old_db, 'SELECT epoch FROM instance_metadata')
        cid, user, database = database_info(old_db)
        dump = backup / 'database.dump'
        with dump.open('xb') as output:
            subprocess.run(['docker', 'exec', cid, 'pg_dump', '-U', user, '-d', database, '-Fc', '--no-owner', '--no-acl'], cwd=ROOT, check=True, stdout=output, stderr=subprocess.PIPE)
        with dump.open('rb') as source:
            listing = run(['docker', 'exec', '-i', cid, 'pg_restore', '--list'], stdin=source)
        (backup / 'archive-list.txt').write_bytes(listing)
        (backup / 'manifest.json').write_text(json.dumps({'private_file_hashes': original_files, 'tables': before, 'old_epoch': epoch, 'old_audit_max': audit_max}, indent=2) + '\n')
        print('Backup created and archive verified: ' + str(backup), flush=True)
        run(['docker', 'compose', 'up', '-d', '--wait', 'db'])
        new_db = container(NAME, 'db')
        ncid, nuser, ndatabase = database_info(new_db)
        if sql(new_db, "SELECT count(*) FROM pg_tables WHERE schemaname='public'") != '0':
            raise RuntimeError('Destination database is not empty')
        with dump.open('rb') as source:
            run(['docker', 'exec', '-i', ncid, 'pg_restore', '-U', nuser, '-d', ndatabase, '--exit-on-error', '--no-owner', '--no-acl'], stdin=source)
        run(['docker', 'compose', 'run', '--rm', 'app', 'migrate'])
        run(['docker', 'compose', 'run', '--rm', 'app', 'restore-epoch', '--reason', 'local project namespace migration'])
        for table, expected in before.items():
            actual = fingerprint(new_db, table, audit_max if table == 'audit_entries' else None)
            if actual != expected:
                raise RuntimeError('Restored table differs: ' + table)
        if sql(new_db, 'SELECT epoch FROM instance_metadata') == epoch:
            raise RuntimeError('Restore epoch did not change')
        if sql(new_db, 'SELECT count(*) FROM idempotency_keys') != '0':
            raise RuntimeError('Old idempotent responses survived restoration')
        # Treat attempted exposure as live: never silently roll back a stack that may have accepted writes.
        new_app_started = True
        run(['docker', 'compose', 'up', '-d', '--wait', 'app'])
        run([sys.executable, 'scripts/local-setup.py'])
        for relative, digest in original_files.items():
            if hashlib.sha256((ROOT / relative).read_bytes()).hexdigest() != digest:
                raise RuntimeError('Original private connection file changed: ' + relative)
        marker = ROOT / 'work/local/name-migration.json'
        marker.write_text(json.dumps({'project': NAME, 'backup': str(backup), 'legacy_db': old_db['Id'], 'legacy_app': old_app['Id'], 'verified_tables': len(before)}, indent=2) + '\n')
        run(['docker', 'stop', old_db['Id']])
        print('Migration verified. Existing tokens and records retained; original private connection files unchanged.', flush=True)
        print('Old cursor/lease state requires resynchronization after restore. Old containers and volume are retained, stopped.', flush=True)
        print('New connection environment: work/local/' + NAME + '.env', flush=True)
    except Exception:
        # Keep both data volumes. Do not hide failed restores or reapply them automatically.
        subprocess.run(['docker', 'compose', 'stop', 'app'], cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if not new_app_started:
            subprocess.run(['docker', 'start', old_app['Id']], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        print('Migration stopped. Both data volumes and backup were preserved; inspect the attempt before retrying.', file=sys.stderr)
        raise


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, subprocess.CalledProcessError, OSError, ValueError) as error:
        # Do not print captured SQL/data or Docker environment values.
        raise SystemExit('Migration could not complete: ' + (str(error) if isinstance(error, RuntimeError) else type(error).__name__))
