#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

migration_root=internal/meta/migrations
sqlite_dir=$migration_root
postgres_dir=$migration_root/postgres

shopt -s nullglob
sqlite_files=("$sqlite_dir"/*.sql)
postgres_files=("$postgres_dir"/*.sql)

if ((${#sqlite_files[@]} == 0 || ${#postgres_files[@]} == 0)); then
  printf 'SQLite and PostgreSQL migration sets must both be non-empty\n' >&2
  exit 1
fi

validate_set() {
  local label=$1
  shift
  if (($# != 1)); then
    printf '%s must contain exactly one pre-release baseline migration, found %d\n' "$label" "$#" >&2
    exit 1
  fi
  local file name
  for file in "$@"; do
    name=${file##*/}
    if [[ $name != 001_baseline.sql ]]; then
      printf '%s pre-release migration must be 001_baseline.sql, found %s\n' "$label" "$name" >&2
      exit 1
    fi
  done
}

validate_set SQLite "${sqlite_files[@]}"
validate_set PostgreSQL "${postgres_files[@]}"

sqlite_names=$(printf '%s\n' "${sqlite_files[@]##*/}")
postgres_names=$(printf '%s\n' "${postgres_files[@]##*/}")
if [[ $sqlite_names != "$postgres_names" ]]; then
  printf 'SQLite and PostgreSQL migration filenames differ\n' >&2
  diff -u <(printf '%s\n' "$sqlite_names") <(printf '%s\n' "$postgres_names") || true
  exit 1
fi

printf 'pre-release single-baseline migration check passed\n'
