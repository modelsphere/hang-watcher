#!/usr/bin/env bash
# End-to-end check of GET /engine against a fake engine (no Kubernetes):
#   1. before the first scrape     -> 503
#   2. engine up                   -> 200, {engine_id, started_at, source}
#   3. engine restarted            -> new id
#   4. sidecar restarted, same engine -> same id (what consumers rely on)
# Run for both identity sources: vLLM process_start_time and sglang startup gauges.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; WORK="$(mktemp -d)"
EPORT=${EPORT:-18056}; HPORT=${HPORT:-19091}
# Only non-empty pids: the helpers below clear them, and `kill 0` would signal
# this script's own process group.
trap 'for p in ${FAKE:-} ${SC:-}; do kill "$p" 2>/dev/null; done; rm -rf "$WORK"' EXIT

echo "== build =="; ( cd "$HERE" && CGO_ENABLED=0 go build -o "$WORK/hang-watcher" . ) || { echo BUILD_FAIL; exit 1; }

# The fake engine serves $WORK/metrics.txt, re-read on every request, so the
# test "restarts" it by rewriting the file.
cat > "$WORK/fake.py" <<'PY'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = open(sys.argv[2], "rb").read()
        self.send_response(200); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
echo '{"poll_interval_sec":1,"stall_sec":30,"metrics_timeout_sec":3,"active_probe_enabled":false}' > "$WORK/config.json"

start_sidecar() {
  WARMUP_SEC=0 ENGINE_URL="http://127.0.0.1:$EPORT" LISTEN="127.0.0.1:$HPORT" CONFIG_FILE="$WORK/config.json" \
    "$WORK/hang-watcher" >> "$WORK/sc.log" 2>&1 &
  SC=$!
  for _ in $(seq 1 30); do curl -s -o /dev/null "http://127.0.0.1:$HPORT/engine" && return; sleep 0.2; done
}
stop_sidecar() { kill $SC 2>/dev/null; wait $SC 2>/dev/null; SC=; }
field() { python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get(sys.argv[2]))' "$1" "$2"; }

fail=0
check() { # $1 description, $2 actual, $3 expected
  if [ "$2" = "$3" ]; then echo "  ok   $1 = $2"; else echo "  FAIL $1 = $2, want $3"; fail=1; fi
}

run_case() { # $1 name, $2 first metrics, $3 restarted metrics, $4 expected source
  echo "== $1 =="
  rm -f "$WORK/metrics.txt"
  # 1. sidecar up, engine down
  start_sidecar
  code=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$HPORT/engine")
  check "before first scrape: status" "$code" 503
  # 2. engine up
  printf '%b' "$2" > "$WORK/metrics.txt"
  python3 "$WORK/fake.py" "$EPORT" "$WORK/metrics.txt" > /dev/null 2>&1 &
  FAKE=$!
  sleep 2.5
  b=$(curl -s "http://127.0.0.1:$HPORT/engine"); id1=$(field "$b" engine_id)
  check "engine up: source" "$(field "$b" source)" "$4"
  check "engine up: fields" "$(python3 -c 'import json,sys; print(",".join(sorted(json.loads(sys.argv[1]))))' "$b")" "engine_id,restarts,source,started_at,watcher_started_at"
  # 3. engine restarted
  printf '%b' "$3" > "$WORK/metrics.txt"
  sleep 2.5
  b=$(curl -s "http://127.0.0.1:$HPORT/engine"); id2=$(field "$b" engine_id)
  check "engine restarted: id changed" "$([ "$id1" != "$id2" ] && echo yes || echo no)" yes
  check "engine restarted: restarts" "$(field "$b" restarts)" 1
  # 4. sidecar restarted, engine untouched
  stop_sidecar; start_sidecar; sleep 2.5
  b=$(curl -s "http://127.0.0.1:$HPORT/engine")
  check "sidecar restarted: same id" "$(field "$b" engine_id)" "$id2"
  check "sidecar restarted: restarts counts from 0 again" "$(field "$b" restarts)" 0
  stop_sidecar; kill $FAKE 2>/dev/null; wait $FAKE 2>/dev/null; FAKE=
}

run_case vllm \
  'process_start_time_seconds 1.79194364129e+09\nvllm:generation_tokens_total 100\nvllm:num_requests_running 0\n' \
  'process_start_time_seconds 1.79195001337e+09\nvllm:generation_tokens_total 5000\nvllm:num_requests_running 0\n' \
  process_start_time
run_case sglang \
  'sglang:startup_time_seconds{phase="scheduler_e2e"} 109.03901622537524\nsglang:realtime_tokens_total 0\nsglang:num_running_reqs 0\n' \
  'sglang:startup_time_seconds{phase="scheduler_e2e"} 108.71100912\nsglang:realtime_tokens_total 0\nsglang:num_running_reqs 0\n' \
  startup_fingerprint

if [ $fail = 0 ]; then echo "✅ 通过"; else echo "❌ 失败"; tail -20 "$WORK/sc.log"; fi
exit $fail
