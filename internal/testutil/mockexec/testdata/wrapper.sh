#!/bin/sh
# Shared entry point for mock executables written by mockexec.Write. Each mock
# is a link to this file, so macOS assesses one stable executable instead of
# every script a test writes. The link's sibling payload holds the script.
payload="$0.payload"
if [ ! -f "$payload" ]; then
  echo "mockexec wrapper: missing payload: $payload" >&2
  exit 127
fi
IFS= read -r first < "$payload" || first=
case "$first" in
  '#!'*) exec ${first#??} "$payload" "$@" ;;
  *) exec /bin/sh "$payload" "$@" ;;
esac
