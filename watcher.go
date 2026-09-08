// hang-watcher:引擎(vllm/sglang)hang 检测 sidecar 的核心判活逻辑。
//
// 复刻 monitor lib/checks.py 的判活优先级(被动短路,不主动打流量):
//  1. /metrics 可达性  —— 拉不到(引擎 HTTP 层死)= 疑 hang
//  2. token_progress 涨 —— 本轮比上轮多出词/prefill → 在干活,健康
//     (sglang 靠 realtime_tokens_total 提供 iteration 级信号,
//     *_tokens_total 是请求结束才累加的,单独用会误杀长请求)
//  3. 停滞 + running>0   —— 计数器不涨、却有在途请求 → 真 hang
//  4. 空闲豁免           —— 停滞但 running==0 → 空闲,健康(不误杀半夜无流量)
//  5. 冻结宽限           —— 计数器冻结但 GRACE 秒内出过词 → 容忍大 prefill 批
//  6. 日志快判(可选)   —— 停滞 log_stall_sec(默认 30,短于 stall_sec)+ 引擎日志最近喊过
//     hang 特征行 → 两个独立信号叠加,提前判 hang、缩短重启时间
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

// token_progress = 这几个 counter 求和(跨 DP 多系列)。
//
// ⚠️ 2026-09-08:必须包含 sglang:realtime_tokens_total,否则长请求会被误判 hang。
// sglang 的 generation_tokens_total / prompt_tokens_total 只在【请求结束】时一次性累加
// (tokenizer_manager.py → observe_one_finished_request → generation_tokens_total.inc()),
// 一条请求从 prefill、decode 到尾部 tool-call parser 的【整个生命周期内它们都不动】。
// 低并发场景(半夜 / 单实例)跑一条 200s+ 的深推理请求时,没有别的请求完成把计数器顶上去 →
// token_progress 冻结超过 stall_sec 且 running>0 → 命中「停滞+running>0=真 hang」
// → 把一台正在正常干活的实例杀掉。生产至今没爆只是因为并发高、总有请求在完成。
//
// realtime_tokens_total 是 iteration 级的:metrics_reporter.py 每个 forward 都 inc
// (decode 加 batch_size、prefill 加 log_input_tokens/log_hit_tokens,带 mode label 三分),
// 注释原文 "Every-iteration work: realtime token counting" —— 这才是真正的实时进度信号。
// 空闲时没有 forward、不会自增,所以不会把 idle 误判成「在干活」。
// vllm 侧的 generation_tokens_total 本身就是按 step 累加的,不受此问题影响,无需对应项。
// 旧版 sglang 没有这个 counter,自动退化回原来的四项。
var progressMetrics = map[string]bool{
	"vllm:generation_tokens_total":   true,
	"vllm:prompt_tokens_total":       true,
	"sglang:generation_tokens_total": true,
	"sglang:prompt_tokens_total":     true,
	"sglang:realtime_tokens_total":   true,
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

	// 日志二次确认(可选,log_file 配了才启用)。见 logtail.go 的说明。
	logEnabled bool          // 配了日志通道
	logWindow  time.Duration // 特征行「算数」的时效窗
	logStall   time.Duration // 有日志佐证时的【短】停滞阈值(远小于 stall_sec)
	lastLogHit time.Time     // 最近一次匹配到特征行的时刻
	logBroken  bool          // 日志读不了(文件缺失/权限)→ 退回纯 progress 判定,不因此漏杀
}

// enableLogConfirm:开启日志快判。停滞 >= logStall 且 window 内出现过特征行 → 直接判 hang。
// 与原 stall_sec 路径是【或】关系:日志没喊仍按 stall_sec 判,检测能力只增不减。
func (w *watcher) enableLogConfirm(logStall, window time.Duration) {
	w.mu.Lock()
	w.logEnabled, w.logStall, w.logWindow = true, logStall, window
	w.mu.Unlock()
}

// noteLogHits:每轮 poll 后把 tail 结果喂进来。hits>0 刷新「最近命中时刻」;
// err!=nil 表示日志通道不可用 —— 标记 broken,判定退回纯 progress(宁可误杀也不漏杀)。
func (w *watcher) noteLogHits(now time.Time, hits int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.logBroken = err != nil
	if hits > 0 {
		w.lastLogHit = now
	}
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
	//
	// 【快路径】日志二次确认:两个独立信号同时成立时,不必等满 stall_sec。
	// 纯 progress 判定要等满 stall_sec,是因为单一信号不敢下手(大 prefill 批、低并发长请求都会
	// 让计数器冻结)。而引擎自己喊 detokenizer 超时是另一个独立证据 —— 两者叠加时置信度足够,
	// 用短得多的 log_stall_sec(默认 30s)就能判,进一步缩短重启时间。
	// 不影响原路径:日志没喊 / 通道坏了,仍按 stall_sec 走下面的老逻辑,只增不减。
	stalledFor := now.Sub(w.lastGrow)
	if w.logEnabled && !w.logBroken && stalledFor >= w.logStall &&
		!w.lastLogHit.IsZero() && now.Sub(w.lastLogHit) <= w.logWindow {
		w.set(true, "stall-hang-log", "token 停滞 "+dur(stalledFor)+"(>=log_stall "+dur(w.logStall)+
			",running="+ftoa(m.running)+")且 "+dur(now.Sub(w.lastLogHit))+
			" 前引擎日志报 hang 特征 → hang(双信号快判)")
		return
	}
	if stalledFor < stall {
		w.set(false, "freeze-grace", "计数器冻结但 "+dur(stalledFor)+" 前出过词(<stall,视为在干活)")
		return
	}
	stalled := dur(stalledFor)
	// 冻结超 grace。开了主动探测:无论 running 与否都主动打一下确认 —— 这能抓到 scheduler
	// 卡死这类「看着空闲(running=0/queue=0)」的 hang(纯被动从指标分不清 wedged 与真空闲)。
	// 探测本身生成 1 个 token → 健康引擎下轮即 growing、停滞计时自动重置,故对真空闲引擎约每
	// stall_sec 才探一次,不会每轮骚扰。
	if probe != nil {
		if probe() {
			w.set(false, "stall-active-ok", "token 停滞 "+stalled+"(running="+ftoa(m.running)+"),主动探测存活 → 不判 hang")
			return
		}
		w.set(true, "stall-hang", "token 停滞 "+stalled+"、主动探测失败(running="+ftoa(m.running)+")→ hang")
		return
	}
	// 纯被动(未开主动探测):只有在途请求还卡着才敢判 hang;running==0 分不清 wedged/空闲 → 放过。
	if m.running > 0 {
		w.set(true, "stall-hang", "token 停滞 "+stalled+" 且 running="+ftoa(m.running)+">0 → hang(被动)")
		return
	}
	w.set(false, "idle", "空闲(running=0,停滞不算 hang;开 active_probe 可覆盖 wedged-idle)")
}

func dur(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) + "s" }
func ftoa(f float64) string      { return strconv.FormatFloat(f, 'f', 0, 64) }
