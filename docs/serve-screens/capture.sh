#!/usr/bin/env bash
# Screenshots every page of `slai serve --demo` (a fake, seeded project) with headless Chrome.
#   docs/serve-screens/capture.sh            # writes the PNGs next to this script
#   OUT=/tmp/shots docs/serve-screens/capture.sh
# Needs: go, Node 22 (global WebSocket), google-chrome. Deterministic: the demo has a fixed
# clock and fixed ids, TZ is pinned, so a re-run produces the same pages.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="${OUT:-$here}"
work="$(mktemp -d)"
pids=()
cleanup() { for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$work"; }
trap cleanup EXIT

export TZ=UTC LANG=C.UTF-8
(cd "$root" && go build -o "$work/slai" .)

# start <name> <extra serve args...>: prints the URL it listens on
start() {
  local name="$1"; shift
  "$work/slai" serve --demo --demo-dir "$work/$name" "$@" >"$work/$name.out" 2>"$work/$name.err" &
  pids+=("$!")
  for _ in $(seq 1 100); do
    url="$(grep -o 'http://[^ ]*' "$work/$name.out" 2>/dev/null | head -1 || true)"
    [ -n "$url" ] && { echo "${url%/}"; return; }
    sleep 0.1
  done
  echo "serve --demo ($name) did not start:" >&2; cat "$work/$name.err" >&2; exit 1
}

main="$(start main --listen 127.0.0.1:0)"
first="$(start first --listen 127.0.0.1:0 --demo-first-run)"
ro="$(start ro --listen 127.0.0.1:0 --read-only)"
signin=""
lan="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
if [ -n "$lan" ]; then
  port=$((20000 + RANDOM % 20000))
  "$work/slai" serve --demo --demo-dir "$work/lan" --listen "$lan:$port" --insecure-http >"$work/lan.out" 2>"$work/lan.err" &
  pids+=("$!")
  for _ in $(seq 1 100); do grep -q 'open http' "$work/lan.out" 2>/dev/null && break; sleep 0.1; done
  signin="http://$lan:$port"
else
  echo "no non-loopback address: skipping the sign-in screenshots" >&2
fi

args=("$out" "$here/pages.txt" "$main" --readonly "$ro" --firstrun "$first")
[ -n "$signin" ] && args+=(--signin "$signin")
node "$here/capture.mjs" "${args[@]}"
