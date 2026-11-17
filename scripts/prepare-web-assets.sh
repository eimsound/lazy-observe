#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source_dir="$root/web/dist"
target_dir="$root/internal/webui/dist"

[[ -f "$source_dir/index.html" ]] || { printf 'web build is missing %s\n' "$source_dir/index.html" >&2; exit 1; }
mkdir -p "$target_dir"
find "$target_dir" -mindepth 1 ! -name .gitkeep -delete
cp -R "$source_dir"/. "$target_dir"/
printf 'prepared embedded web assets in %s\n' "$target_dir"
