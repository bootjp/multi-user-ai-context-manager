#!/bin/sh
# Start only this Compose project and keep local credentials private.
set -eu
project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"
umask 077
if [ ! -f .env ]; then
  python3 - <<'PY'
import os,secrets
fd=os.open('.env',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'w') as f:f.write('POSTGRES_PASSWORD='+secrets.token_hex(32)+'\nAPP_PORT=8080\n')
PY
fi
mkdir -p work/local
chmod 700 work/local
if ! docker info >/dev/null 2>&1; then
  echo 'Start a local Docker engine, then rerun this command.' >&2
  exit 1
fi
if docker volume inspect agent-context_pgdata >/dev/null 2>&1 && [ ! -f work/local/name-migration.json ]; then
  echo 'Legacy local data detected. Preview scripts/migrate-local-name.py, then run it with --apply.' >&2
  exit 1
fi
docker compose up -d --wait db
docker compose build app
docker compose run --rm app migrate
if [ ! -f work/local/admin.token ]; then
  docker compose run --rm --user "$(id -u):$(id -g)" \
    --volume "$project_dir/work/local:/secrets" \
    app bootstrap --output-file /secrets/admin.token
fi
docker compose up -d --wait app
python3 scripts/local-setup.py
