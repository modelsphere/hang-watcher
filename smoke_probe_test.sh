#!/usr/bin/env bash
# 本地端到端(不连 k8s):专测「每轮停滞都探测」这条路径 —— smoke_test.sh 显式关了主动探测,
# 覆盖不到这里。
#
# 假引擎(必须多线程:单线程时 /health_generate 睡死会把 /metrics 一起堵掉,
# 测出来的 503 就成了 unreach-hang 而不是探测失败路径 —— 第一版就踩了这个坑):
#   /metrics         realtime_tokens_total = 已成功服务的探测次数,模拟真实引擎上
#                    「探测跑一次 forward → 计数器自增」这个本次改动依赖的关键性质
#   /health_generate 正常立刻 200;FAIL_ONCE 存在则只让下一次超时(用后即删),
#                    FAIL_ALL 存在则一直超时
#
# 场景 A(本次改动的核心):探测只失败一轮 → /healthz 必须保持 200。旧行为下这一次
#                        失败就会 hung=true,正是误杀来源。
# 场景 B(不能漏杀):探测持续失败 → 必须在 stall_sec 后翻 503,且原因须是探测连败,
#                  连败次数 > 1。
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; WORK="$(mktemp -d)"
EPORT=18056; HPORT=19091
STALL=10; POLL=2; PTIMEOUT=2
trap 'kill ${FAKE:-0} ${SC:-0} 2>/dev/null; rm -rf "$WORK"' EXIT

echo "== build =="
( cd "$HERE" && CGO_ENABLED=0 go build -o "$WORK/hang-watcher" . ) || { echo BUILD_FAIL; exit 1; }

cat > "$WORK/fake.py" <<'PY'
import sys, os, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port, work = int(sys.argv[1]), sys.argv[2]
FAIL_ONCE = os.path.join(work, "FAIL_ONCE")
FAIL_ALL  = os.path.join(work, "FAIL_ALL")
state = {"probes": 0}
class H(BaseHTTPRequestHandler):
    def _send(self, body):
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        if self.path.startswith("/health_generate"):
            if os.path.exists(FAIL_ALL):
                time.sleep(60); return
            if os.path.exists(FAIL_ONCE):
                os.remove(FAIL_ONCE)
                time.sleep(60); return
            state["probes"] += 1
            self._send(b"ok"); return
        self._send(("sglang:realtime_tokens_total %d\nsglang:num_running_reqs 0\n"
                    % state["probes"]).encode())
    def log_message(self, *a): pass
ThreadingHTTPServer.daemon_threads = True
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
PY
python3 "$WORK/fake.py" "$EPORT" "$WORK" > "$WORK/fake.log" 2>&1 &
FAKE=$!
sleep 1

cat > "$WORK/config.json" <<JSON
{
  "poll_interval_sec": $POLL,
  "stall_sec": $STALL,
  "metrics_timeout_sec": 3,
  "active_probe_enabled": true,
  "active_probe_path": "/health_generate",
  "active_probe_method": "GET",
  "active_probe_body": "",
  "active_probe_timeout_sec": $PTIMEOUT
}
JSON

ENGINE_URL="http://127.0.0.1:$EPORT" LISTEN=":$HPORT" CONFIG_FILE="$WORK/config.json" \
  "$WORK/hang-watcher" > "$WORK/hw.log" 2>&1 &
SC=$!
sleep 3

code() { curl -s -o "$WORK/body" -w '%{http_code}' -m 3 "http://127.0.0.1:$HPORT/healthz"; }
body() { cut -c1-120 < "$WORK/body"; }
FAILED=0

echo "== 场景 A:探测只失败一轮,随后自动恢复 =="
# 定点采样会错过:探测成功会推进 progress,下一轮走 growing 不探,所以探测是隔轮发生的,
# 失败状态只存在 2~4s。改成连续采样,记录窗口内出现过的【所有】状态。
touch "$WORK/FAIL_ONCE"
SAW_FAIL=0; SAW_503=0
for i in $(seq 1 30); do          # 15s,覆盖 失败探测 -> 恢复 全过程
  c=$(code)
  b=$(body)
  case "$b" in *"连续失败"*) SAW_FAIL=1;; esac
  [ "$c" = 503 ] && SAW_503=1
  sleep 0.5
done
echo "  窗口内是否观察到探测失败分支: $([ $SAW_FAIL = 1 ] && echo 是 || echo 否)"
echo "  窗口内是否出现过 503:         $([ $SAW_503 = 1 ] && echo 是 || echo 否)"
if [ $SAW_FAIL != 1 ]; then
  echo "  ❌ 没造出探测失败,本场景无效(不是通过,是没测到)"; FAILED=1
elif [ $SAW_503 = 1 ]; then
  echo "  ❌ 单次探测失败就判 hang —— 正是本次要修的问题"; FAILED=1
else
  echo "  ✅ 单次探测超时全程未翻 503,且自动恢复"
fi
echo "  该阶段 watcher 日志:"
grep -aE "probe-fail-grace|stall-active-ok|growing|连续失败" "$WORK/hw.log" | tail -5 | sed 's/^/    /'

echo "== 场景 B:探测持续失败 =="
touch "$WORK/FAIL_ALL"
sleep $((STALL + PTIMEOUT + POLL + 3))
c=$(code); echo "  持续失败 >stall 后: /healthz=$c  ($(body))"
if [ "$c" != 503 ]; then echo "  ❌ 持续探测失败应判 hang(漏杀)"; FAILED=1; fi
if grep -q "主动探测连续失败" "$WORK/body"; then
  n=$(sed -n 's/.*主动探测连续失败 \([0-9]*\) 次.*/\1/p' "$WORK/body")
  echo "  判死时连续失败次数 = ${n:-?}"
  if [ "${n:-0}" -gt 1 ]; then
    echo "  ✅ 真 hang 仍能检出,且是连败 ${n} 次后才判(非单次)"
  else
    echo "  ❌ 应连败多次才判死,实际 ${n:-0} 次"; FAILED=1
  fi
else
  echo "  ❌ 503 不是来自探测失败路径,而是:$(body)"; FAILED=1
fi

echo "== 结果 =="
if [ $FAILED = 0 ]; then echo "✅ 通过"; else echo "❌ 失败"; tail -25 "$WORK/hw.log"; exit 1; fi
