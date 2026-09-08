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

# ⚠️ 显式关主动探测:它现在默认【开】,而下面的假引擎对任何 GET 都答 200 ->
#    探测恒成功 -> 判定停在 stall-active-ok,永远等不到 503。本段验的是被动路径。
echo '{"poll_interval_sec":1,"stall_sec":2,"metrics_timeout_sec":3,"active_probe_enabled":false}' > "$WORK/config.json"
ENGINE_URL="http://127.0.0.1:$EPORT" LISTEN="127.0.0.1:$HPORT" CONFIG_FILE="$WORK/config.json" \
  "$WORK/hang-watcher" > "$WORK/sc.log" 2>&1 &
SC=$!
for i in $(seq 1 20); do curl -s -o /dev/null "http://127.0.0.1:$HPORT/healthz" && break; sleep 0.3; done

code(){ curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$HPORT/healthz"; }
fail=0
sleep 1; C1=$(code); echo "t=1s  /healthz=$C1 (建基线,应 200)"; [ "$C1" = 200 ] || { echo "  ✗"; fail=1; }
sleep 4; C2=$(code); B2=$(curl -s "http://127.0.0.1:$HPORT/healthz"); echo "t=5s  /healthz=$C2  ($B2)"
[ "$C2" = 503 ] || { echo "  ✗ 停滞+running>0 超 stall 应翻 503"; fail=1; }

# ---- 第二阶段:日志快判(停滞 log_stall 短阈值 + 日志特征 → 提前判 hang)----
# 用 stall_sec=60(远够不着)+ log_stall=3s,验证「没日志时按 60s 等、有日志时 3s 就杀」
kill $SC 2>/dev/null; wait $SC 2>/dev/null; SC=
LOGDIR="$WORK/pods/ns_pod_uid/sglang"; mkdir -p "$LOGDIR"
echo "2026-09-08T04:00:00.000000000Z stdout F [启动] INFO 正常行" > "$LOGDIR/0.log"

echo '{"poll_interval_sec":1,"stall_sec":60,"metrics_timeout_sec":3,"active_probe_enabled":false}' > "$WORK/config2.json"
ENGINE_URL="http://127.0.0.1:$EPORT" LISTEN="127.0.0.1:$HPORT" CONFIG_FILE="$WORK/config2.json" \
  LOG_FILE="$WORK/pods/ns_pod_*/sglang/*.log" LOG_WINDOW_SEC=30 LOG_STALL_SEC=3 \
  "$WORK/hang-watcher" > "$WORK/sc2.log" 2>&1 &
SC=$!
for i in $(seq 1 20); do curl -s -o /dev/null "http://127.0.0.1:$HPORT/healthz" && break; sleep 0.3; done

sleep 8; C3=$(code); B3=$(curl -s "http://127.0.0.1:$HPORT/healthz")
echo "t=8s  /healthz=$C3  ($B3)"
[ "$C3" = 200 ] || { echo "  ✗ 停滞 8s 已过 log_stall 但日志没喊 → 应继续等 stall_sec=60s"; fail=1; }

# 追加一条真实格式的 detokenizer 超时行(k8s CRI 日志前缀 + sglang 原文)
echo "2026-09-08T04:05:00.000000000Z stderr F [2026-09-08 04:05:00] ERROR: Health check failed. Server couldn't get a response from detokenizer for last 20 seconds. tic start time: 04:04:40. last_heartbeat time: 04:00:01" >> "$LOGDIR/0.log"
sleep 3; C4=$(code); B4=$(curl -s "http://127.0.0.1:$HPORT/healthz")
echo "t=11s /healthz=$C4  ($B4)"
[ "$C4" = 503 ] || { echo "  ✗ 停滞>log_stall + 日志特征 → 应快判 hang(远早于 stall_sec=60s)"; fail=1; }

echo "== 结果 =="; [ "$fail" = 0 ] && echo "✅ 通过:hang 检测 + 日志快判 + /healthz 全链路 OK" || { echo "❌ 失败"; echo "--- sidecar log 1 ---"; cat "$WORK/sc.log"; echo "--- sidecar log 2 ---"; cat "$WORK/sc2.log" 2>/dev/null; }
exit $fail
