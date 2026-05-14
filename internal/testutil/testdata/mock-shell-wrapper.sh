#!/bin/bash
payload="${0}.payload.sh"
if [ ! -f "$payload" ]; then
  echo "mock-shell-wrapper: missing payload: $payload" >&2
  exit 127
fi
exec /bin/bash "$payload" "$@"
