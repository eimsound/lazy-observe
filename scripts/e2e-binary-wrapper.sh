#!/bin/sh
set -eu

# OpenStore keeps accepting plain SQLite paths, while the public configuration
# boundary requires an explicit scheme. E2E fixtures initialize the database
# through OpenStore and pass that same path to the server, so normalize it here.
case ${LO_META_DB_URL:-} in
  /*) LO_META_DB_URL="sqlite://$LO_META_DB_URL"; export LO_META_DB_URL ;;
esac

if [ "${1:-}" = server ]; then
  exec "${LO_E2E_REAL_BINARY:?LO_E2E_REAL_BINARY is required}" \
    "$@" --component-port-offset "${LO_E2E_COMPONENT_PORT_OFFSET:-5000}"
fi

exec "${LO_E2E_REAL_BINARY:?LO_E2E_REAL_BINARY is required}" "$@"
