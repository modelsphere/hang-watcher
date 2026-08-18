#!/usr/bin/env bash
# 本地端到端冒烟(不连 k8s):fake 引擎 /metrics(停滞 + running>0)→ 起 hang-watcher →
# 验 /healthz 从 ok 翻成 503(hang 判定 + HTTP 全链路)。
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; WORK="$(mktemp -d)"
EPORT=18055; HPORT=19090
trap 'kill ${FAKE:-0} ${SC:-0} 2>/dev/null; rm -rf "$WORK"' EXIT

echo "== build =="; ( cd "$HERE" && CGO_ENABLED=0 go build -o "$WORK/hang-watcher" . ) || { echo BUILD_FAIL; exit 1; }

# fake 引擎 /metrics:固定值(token_progress 不涨)+ running=2 → 停滞判 hang 的场景
python3 - "$EPORT" > "$WORK/fake.log" 2>&1 <<'PY' &
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
BODY=b"sglang:generation_tokens_total 1000\nsglang:prompt_tokens_total 5000\nsglang:num_running_reqs 2\n"
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(BODY)
    def log_message(self,*a): pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
FAKE=$!
sleep 1

echo '{"poll_interval_sec":1,"stall_sec":2,"metrics_timeout_sec":3}' > "$WORK/config.json"
ENGINE_URL="http://127.0.0.1:$EPORT" LISTEN="127.0.0.1:$HPORT" CONFIG_FILE="$WORK/config.json" \
  "$WORK/hang-watcher" > "$WORK/sc.log" 2>&1 &
SC=$!
for i in $(seq 1 20); do curl -s -o /dev/null "http://127.0.0.1:$HPORT/healthz" && break; sleep 0.3; done

code(){ curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$HPORT/healthz"; }
fail=0
sleep 1; C1=$(code); echo "t=1s  /healthz=$C1 (建基线,应 200)"; [ "$C1" = 200 ] || { echo "  ✗"; fail=1; }
sleep 4; C2=$(code); B2=$(curl -s "http://127.0.0.1:$HPORT/healthz"); echo "t=5s  /healthz=$C2  ($B2)"
[ "$C2" = 503 ] || { echo "  ✗ 停滞+running>0 超 stall 应翻 503"; fail=1; }

echo "== 结果 =="; [ "$fail" = 0 ] && echo "✅ 通过:hang 检测 + /healthz 503 全链路 OK" || { echo "❌ 失败"; echo "--- sidecar log ---"; cat "$WORK/sc.log"; }
exit $fail
