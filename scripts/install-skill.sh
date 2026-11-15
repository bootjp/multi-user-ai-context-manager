#!/bin/sh
set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_dir="$script_dir/../skills/multi-user-ai-context-manager"
skill_root=${1:-${CODEX_HOME:-$HOME/.codex}/skills}
destination="$skill_root/multi-user-ai-context-manager"
mkdir -p "$skill_root"
if [ -e "$destination" ] || [ -L "$destination" ]; then
  echo "Existing skill left unchanged: $destination" >&2
  exit 1
fi
# mkdir is atomic: simultaneous installers cannot overwrite one another.
mkdir "$destination"
cp -R "$source_dir/." "$destination/"
printf 'Installed multi-user-ai-context-manager skill: %s\n' "$destination"
