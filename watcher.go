// hang-watcher:引擎(vllm/sglang)hang 检测 sidecar 的核心判活逻辑。
//
// 复刻 monitor lib/checks.py 的判活优先级(被动短路,不主动打流量):
//  1. /metrics 可达性  —— 拉不到(引擎 HTTP 层死)= 疑 hang
//  2. token_progress 涨 —— 本轮比上轮多出词/prefill → 在干活,健康
//  3. 停滞 + running>0   —— 计数器不涨、却有在途请求 → 真 hang
//  4. 空闲豁免           —— 停滞但 running==0 → 空闲,健康(不误杀半夜无流量)
//  5. 冻结宽限           —— 计数器冻结但 GRACE 秒内出过词 → 容忍大 prefill 批
//
// 本文件只做「一次 poll 结果 → 更新裁决」的纯状态机 + /metrics 文本解析,便于单测。
// HTTP 服务 + poll 循环 + 配置热更在 main.go。
package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// token_progress = 这几个 counter 求和(跨 DP 多系列)。与 monitor _PROGRESS_METRICS 一致。
var progressMetrics = map[string]bool{
	"vllm:generation_tokens_total":   true,
	"vllm:prompt_tokens_total":       true,
	"sglang:generation_tokens_total": true,
	"sglang:prompt_tokens_total":     true,
}

// metricsSnap:一轮 /metrics 解析结果。
type metricsSnap struct {
	tp      float64 // token_progress(progressMetrics 求和)
	haveTP  bool    // 是否解析到任一 progress counter
	running float64 // 在途请求数
}

// parseMetrics:解析 Prometheus 文本 → token_progress + running。
// running:vllm 用 num_requests_running(跨 engine 求和)、sglang 用 num_running_reqs。
func parseMetrics(text string) metricsSnap {
	var s metricsSnap
	var vllmRunning, sglangRunning float64
	var haveVllmRunning, haveSglangRunning bool
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		fullKey := fields[0]
		name := fullKey
		if i := strings.IndexByte(fullKey, '{'); i >= 0 {
			name = fullKey[:i]
		}
		val, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		if progressMetrics[name] {
			s.tp += val
			s.haveTP = true
		}
		switch name {
		case "vllm:num_requests_running":
			vllmRunning += val // DP 多 engine 求和
			haveVllmRunning = true
		case "sglang:num_running_reqs":
			sglangRunning += val // 同 vllm:跨 DP 多系列求和(单系列结果不变)
			haveSglangRunning = true
		}
	}
	if haveVllmRunning {
		s.running = vllmRunning
	} else if haveSglangRunning {
		s.running = sglangRunning
	}
	return s
}

// watcher:hang 判活状态机。step() 每轮更新裁决,Hung() 供 /healthz 读。
type watcher struct {
	mu     sync.RWMutex
	hung   bool
	state  string // 粗状态(startup/growing/stall-hang/... )供日志去重:状态不变不重复打印
	reason string // 详细原因(含变化的数字),供 /healthz body

	// 进度跟踪
	haveBaseline bool
	lastTP       float64
	lastGrow     time.Time // 上次 token_progress 真增长时刻
	started      bool      // 拿到过一次成功 /metrics(启动期豁免:未 started 一律健康)
	firstFail    time.Time // 引擎连续无响应起点(零值=当前可达)
}

func newWatcher() *watcher {
	return &watcher{state: "startup", reason: "启动中(未取到首个 metrics)"}
}

func (w *watcher) set(hung bool, state, reason string) {
	w.mu.Lock()
	w.hung, w.state, w.reason = hung, state, reason
	w.mu.Unlock()
}

// Hung:当前裁决(供 /healthz)。返回 是否 hang / 粗状态(去重用)/ 详细原因。
func (w *watcher) Hung() (bool, string, string) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.hung, w.state, w.reason
}

// step:处理一轮 poll 结果。fetchErr!=nil 表示 /metrics 拉不到。stall 是停滞判 hang 的阈值。
// 未 started(启动期,还没成功拉到过 metrics)→ 一律健康,交给 startupProbe 兜启动。
//
// probe 是【可选】的主动探测(nil=关):仅在被动逻辑将判 stall-hang 时调用 —— 主动打一下引擎
// (如 GET /health_generate),通了就认为引擎仍存活、不判 hang(对齐 monitor 的『杀前确认』,减少
// running>0 停滞的误杀);打不通才坐实 hang。其它分支(空闲/不可达/出词)不触发,不额外加载。
func (w *watcher) step(now time.Time, m metricsSnap, fetchErr error, stall time.Duration, probe func() bool) {
	if fetchErr != nil {
		if !w.started {
			w.set(false, "startup", "启动中:/metrics 暂不可达(startupProbe 兜)")
			return
		}
		if w.firstFail.IsZero() {
			w.firstFail = now
		}
		if now.Sub(w.firstFail) >= stall {
			w.set(true, "unreach-hang", "引擎 /metrics 持续无响应 "+dur(now.Sub(w.firstFail)))
		} // 未超 stall:保持上次裁决(短暂抖动不误杀)
		return
	}
	w.firstFail = time.Time{}
	w.started = true

	if !m.haveTP {
		// 无 progress 计数(旧引擎/未开 metrics):无法按进度判活 → 健康,靠 readiness/health 兜
		w.set(false, "no-tp", "无 token_progress 计数(无法按进度判 hang)")
		return
	}
	if !w.haveBaseline {
		w.haveBaseline, w.lastTP, w.lastGrow = true, m.tp, now
		w.set(false, "baseline", "建 token_progress 基线")
		return
	}
	if m.tp > w.lastTP {
		w.set(false, "growing", "出词 +"+ftoa(m.tp-w.lastTP))
		w.lastTP, w.lastGrow = m.tp, now
		return
	}
	if m.tp < w.lastTP {
		w.set(false, "regress", "计数器倒退(引擎重启过),重置基线")
		w.lastTP, w.lastGrow = m.tp, now
		return
	}
	// m.tp == lastTP:停滞
	if now.Sub(w.lastGrow) < stall {
		w.set(false, "freeze-grace", "计数器冻结但 "+dur(now.Sub(w.lastGrow))+" 前出过词(<stall,视为在干活)")
		return
	}
	if m.running > 0 {
		stalled := dur(now.Sub(w.lastGrow))
		if probe != nil {
			if probe() {
				w.set(false, "stall-active-ok", "token 停滞 "+stalled+" 且 running="+ftoa(m.running)+",但主动探测存活 → 不判 hang")
				return
			}
			w.set(true, "stall-hang", "token 停滞 "+stalled+" 且 running="+ftoa(m.running)+">0,主动探测失败 → hang")
			return
		}
		w.set(true, "stall-hang", "token 停滞 "+stalled+" 且 running="+ftoa(m.running)+">0 → hang")
		return
	}
	w.set(false, "idle", "空闲(running=0,停滞不算 hang)")
}

func dur(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) + "s" }
func ftoa(f float64) string      { return strconv.FormatFloat(f, 'f', 0, 64) }
