#!/usr/bin/env bash
# The automatable half of apps/ugos/docs/upk-proof-procedure.md.
#
# Checks, from outside the UGOS desktop, the two acceptance criteria that don't need a
# signed-in browser session: that the app is registered as installed (indirectly, via the
# App Center's own on-disk records) and that its packaged backend answers /health/live for
# real, on the real NAS, over loopback.
#
# This is meant to fail loudly before the PoC exists (that's the RED half of this work
# package's TDD gate) and to pass once the app is actually installed and running.
set -euo pipefail

if [ $# -ne 3 ]; then
  echo "usage: $0 <ssh-target> <port> <app_id>" >&2
  echo "  e.g. $0 rom@192.168.0.10 29090 com.spdrman.upkproofb12" >&2
  exit 2
fi

target="$1"
port="$2"
app_id="$3"
fail_count=0

echo "==> looking for an installed-app record for ${app_id}"
if ssh "$target" "test -d /ugreen/@appstore/${app_id}" 2>/dev/null; then
  echo "PASS: /ugreen/@appstore/${app_id} exists"
else
  echo "FAIL: no /ugreen/@appstore/${app_id} directory (app not installed)"
  fail_count=$((fail_count + 1))
fi

echo "==> curling http://127.0.0.1:${port}/health/live on ${target}"
if body=$(ssh "$target" "curl -sS -m 5 http://127.0.0.1:${port}/health/live" 2>&1); then
  if printf '%s' "$body" | grep -q '"status"[[:space:]]*:[[:space:]]*"ok"'; then
    echo "PASS: /health/live returned: $body"
  else
    echo "FAIL: /health/live responded but not with the expected body: $body"
    fail_count=$((fail_count + 1))
  fi
else
  echo "FAIL: $body"
  fail_count=$((fail_count + 1))
fi

if [ "$fail_count" -eq 0 ]; then
  echo "GREEN: all checks passed."
  exit 0
else
  echo "RED: ${fail_count} check(s) failed."
  exit 1
fi
