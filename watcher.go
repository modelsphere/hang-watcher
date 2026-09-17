// hang-watcher:引擎(vllm/sglang)hang 检测 sidecar 的核心判活逻辑。
//
// 复刻 monitor lib/checks.py 的判活优先级(被动短路,不主动打流量):
//  1. /metrics 可达性  —— 拉不到(引擎 HTTP 层死)= 疑 hang
//  2. token_progress 涨 —— 本轮比上轮多出词/prefill → 在干活,健康
//     (sglang 靠 realtime_tokens_total 提供 iteration 级信号,
//     *_tokens_total 是请求结束才累加的,单独用会误杀长请求)
//  3. 停滞 + running>0   —— 计数器不涨、却有在途请求 → 真 hang
//     (开了主动探测时:停滞 → 探一次 → 失败后【再复核一次 progress】,仍无进度才判 hang;
//     探测耗时期间引擎可能已恢复,手上的快照是探测前拉的、已过期)
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
	// forward pass 计数器,与 realtime_tokens_total 同一处累加(metrics_reporter 每个 forward)。
	// 语义上比 token 数更直接:它数的就是「调度器循环转了几圈」,decode/prefill、走不走
	// cuda graph 都 +1(value 只决定 mode 标签,不决定加不加)。
	// 作为【冗余】信号加入:sglang v0.5.10.post1 上还没有这个指标(实测 grep -c = 0),
	// v0.5.19 才有,所以它替代不了 realtime_tokens_total,只是多一条腿 —— 万一将来
	// realtime 被改名/删掉,判活不至于直接退回 finish-only 口径。
	"sglang:cuda_graph_passes_total": true,
}

// kvGaugeMetrics:KV 水位类 gauge,【不能】并进 progressMetrics 求和 —— 那个和被当作
// 单调计数器用(变小 = 引擎重启过 → 重置基线),而 gauge 请求结束释放 block 时本来就会降,
// 混进去会被误判成计数器倒退。所以单独跟踪,且【只认上升】,见 step() 里的说明。
//
// 为什么 vLLM 需要这条腿(2026-09-17 在 the test cluster / Qwen3.8-27B-FP8 与生产 llm-prod 实测):
// vLLM 侧三个 progress counter 在【prefill 期间全部冻结】——
//
//	generation_tokens_total  decode 逐 step 涨,prefill 不动
//	prompt_tokens_total      prefill 结束时【一次性入账全长】(生产 1M prompt 实测:
//	                         连续 10s 不动,然后单个 2s 采样区间里 +1,013,999)
//	iteration_tokens_total   名字有 iteration,实际按请求产出记(5.6s prefill 里只 +1)
//
// 而 vLLM 没有 sglang:realtime_tokens_total / cuda_graph_passes_total 的对应物
// (实测两套 vLLM 的 88 / 108 个指标里,没有任何含 graph/cuda/forward/step/pass 的)。
// 于是一次长 prefill 期间 token_progress 是平的,低并发下会被判成 hang。
//
// kv_cache_usage_perc 恰好补上这一段:chunked prefill 每块都要分配 KV block,实测它
// 【逐块匀速爬升】(每 ~0.3s 一个台阶,步长固定 = 一个 chunk 的 block 数),
// 而同一时间 gen/prompt 纹丝不动。两者正好互补:decode 看 gen,prefill 看 kv。
var kvGaugeMetrics = map[string]bool{
	"vllm:kv_cache_usage_perc": true,
}

// metricsSnap:一轮 /metrics 解析结果。
type metricsSnap struct {
	tp      float64 // token_progress(progressMetrics 求和)
	haveTP  bool    // 是否解析到任一 progress counter
	running float64 // 在途请求数
	kv      float64 // KV 水位 gauge(kvGaugeMetrics 求和),prefill 期间的进度信号
	haveKV  bool    // 是否解析到任一 KV gauge(sglang 侧没有 → false,该腿自动不生效)
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
		if kvGaugeMetrics[name] {
			s.kv += val // 跨 DP 多 engine 求和,同 tp
			s.haveKV = true
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
	lastKV       float64   // 上一轮的 KV 水位(kvGaugeMetrics 求和),见 kvGaugeMetrics 的说明
	haveKVBase   bool      // 是否已建 KV 基线(引擎不暴露该 gauge 时恒 false,该腿不生效)
	lastGrow     time.Time // 上次 token_progress 真增长时刻
	started      bool      // 拿到过一次成功 /metrics(启动期豁免:未 started 一律健康)
	firstFail    time.Time // 引擎连续无响应起点(零值=当前可达)

	// 日志二次确认(可选,log_file 配了才启用)。见 logtail.go 的说明。
	logEnabled bool          // 配了日志通道
	logWindow  time.Duration // 特征行「算数」的时效窗
	logStall   time.Duration // 有日志佐证时的【短】停滞阈值(远小于 stall_sec)
	lastLogHit time.Time     // 最近一次匹配到特征行的时刻
	logBroken  bool          // 日志读不了(文件缺失/权限)→ 退回纯 progress 判定,不因此漏杀

	// 探测失败后的复核:重新取一次 token_progress。见 step() 里的调用点。
	recheck func() (float64, bool)

	// 连续探测失败次数。出词 / 探测成功 / 复核发现进度 都会清零。
	// 只用于日志与测试断言 —— 判死条件仍是 stalledFor >= stall,见 step() 里的说明:
	// 每轮停滞都探,成功的探测会推进 progress 把停滞计时按回零,所以「停滞累计到 stall」
	// 本身已经蕴含「这段时间里每次探测都失败」,不需要再拿这个计数器当门槛。
	probeFails int
}

// setRecheck:注册「再取一次 token_progress」的回调。主动探测失败后用它复核 ——
// 探测本身会耗掉 active_probe_timeout_sec,那段时间里引擎可能已经恢复出词。
// 不注册(nil)则退回原行为:探测失败即判 hang。
func (w *watcher) setRecheck(f func() (float64, bool)) {
	w.mu.Lock()
	w.recheck = f
	w.mu.Unlock()
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
		// 引擎失联期间探不了,计数清零 —— 否则恢复之后日志里的「连续失败 N 次」会把失联
		// 前后两段拼在一起,读起来像是一直在失败。
		w.probeFails = 0
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
		w.lastKV, w.haveKVBase = m.kv, m.haveKV
		w.set(false, "baseline", "建 token_progress 基线")
		return
	}
	if m.tp > w.lastTP {
		w.set(false, "growing", "出词 +"+ftoa(m.tp-w.lastTP))
		w.lastTP, w.lastGrow, w.probeFails = m.tp, now, 0
		w.lastKV, w.haveKVBase = m.kv, m.haveKV
		return
	}
	if m.tp < w.lastTP {
		w.set(false, "regress", "计数器倒退(引擎重启过),重置基线")
		w.lastTP, w.lastGrow, w.probeFails = m.tp, now, 0
		w.lastKV, w.haveKVBase = m.kv, m.haveKV
		return
	}

	// token_progress 没动,但 KV 水位【上升】了 → 引擎在 prefill(每个 chunk 都要分配
	// KV block),同样是在干活。这条腿专治 vLLM 的 prefill 盲区,见 kvGaugeMetrics 的说明。
	//
	// 为什么【只认上升】:
	//   上升 = 新分配 block = 有新算出来的 KV,必然对应真实计算;
	//   下降 = 请求结束/被 abort 释放 block,这恰恰可能发生在引擎已经卡死、客户端陆续
	//          超时断开的时候 —— 拿它当「在干活」会把 hang 掩盖成健康。
	// 所以下降只更新基线、不重置停滞计时,宁可少认一次也不漏判。
	//
	// 注意它【不能】替代 progress:实测健康 decode 期间 KV 也是平的
	// (block_size=784,一条请求每产 784 个 token 才分配一次,约 10 秒一动),
	// 所以两条腿是互补关系,谁都不能单独用。
	if w.haveKVBase && m.haveKV && m.kv > w.lastKV {
		w.set(false, "kv-growing", "token_progress 平,但 KV 水位 +"+gtoa(m.kv-w.lastKV)+
			"(prefill 在分配 block,视为在干活)")
		w.lastKV, w.lastGrow, w.probeFails = m.kv, now, 0
		return
	}
	if m.haveKV {
		// 只更新基线,不重置停滞计时(下降不算进度)
		w.lastKV, w.haveKVBase = m.kv, true
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
	stalled := dur(stalledFor)
	// 开了主动探测:【每一轮停滞都探】,不再等满 stall_sec 才探第一次。
	//
	// 旧行为下判死链路上只有【一次】探测:停滞满 stall_sec 才探,这次超时(默认 5s)+ 复核无进度
	// 就立刻 hung=true。而 hung 之后 kubelet 的 livenessProbe 只需 2×5s 就 Killing,下一轮
	// poll(5s)+ 下次探测(最多 5s)的结论恰好与 Killing 同时到达 —— 补救机会形同虚设。
	// 结果是任何一次偶发的 5s 超时(GC、瞬时抖动、一次偏慢的响应)都足以杀掉一台健康引擎。
	//
	// 每轮都探之后,判死条件不用动就自带了「连续失败」语义:探测走 /health_generate,成功时会真跑
	// 一次 forward,而 sglang:realtime_tokens_total 是 metrics_reporter 每个 forward 自增的
	// (与 health_generate 内部的 log_metrics=False 无关,那只影响 generation_tokens_total),
	// 所以成功的探测会推进 token_progress → 下一轮进 growing → lastGrow 归零。
	// 于是 stalledFor 能累加到 stall_sec,当且仅当这期间【每次探测都失败】。
	// 失败的探测会同步阻塞 active_probe_timeout_sec,而 poll 的 sleep 在干完活之后,所以真 hang 时
	// 一轮是 timeout+poll(默认 5+5=10s)—— 30s 里连败约 3 次才判死,判死时机也因此比 stall_sec
	// 晚不到一轮。单次抖动杀不死引擎,这是本次改动的全部目的。
	//
	// 开销:有流量时 progress 本来就在涨,走不到这里,零新增。空闲引擎从约每 stall_sec 一次变成约
	// 每 2 个 poll 一次,单次是 1 token 的生成,可忽略。
	if probe != nil {
		if probe() {
			w.probeFails = 0
			w.set(false, "stall-active-ok", "token 停滞 "+stalled+"(running="+ftoa(m.running)+"),主动探测存活 → 不判 hang")
			return
		}
		// 探测失败 ≠ 立刻判死。探测本身刚刚耗掉了 active_probe_timeout_sec(默认 5s,曾是 60s),
		// 这段时间里引擎完全可能已经恢复出词 —— 典型情形是它卡在一次超长 forward 里,而那次
		// forward 在探测等待期间结束了。此时手上的 m 是【探测之前】拉的快照,已经过期。
		// 所以再取一次 progress:涨了就说明引擎在干活,探测超时只是被那次 forward 挡住了。
		// 这把「探测超时」从判死的【充分证据】降级成【必要条件之一】。
		if w.recheck != nil {
			if tp, ok := w.recheck(); ok && tp > w.lastTP {
				w.probeFails = 0
				w.set(false, "probe-fail-but-growing", "token 停滞 "+stalled+"、主动探测失败,但复核发现 progress 已推进 +"+
					ftoa(tp-w.lastTP)+" → 不判 hang(探测期间引擎恢复了)")
				w.lastTP, w.lastGrow = tp, now
				return
			}
		}
		w.probeFails++
		fails := strconv.Itoa(w.probeFails)
		// 探测失败但停滞还没满 stall_sec:不判死。这正是本次改动的意义所在 ——
		// 让「探测失败」必须连续发生满 stall_sec 才算数,而不是一次就下手。
		if stalledFor < stall {
			w.set(false, "probe-fail-grace", "token 停滞 "+stalled+"、主动探测连续失败 "+fails+
				" 次(<stall "+dur(stall)+",继续观察)")
			return
		}
		w.set(true, "stall-hang", "token 停滞 "+stalled+"、主动探测连续失败 "+fails+
			" 次且复核仍无进度(running="+ftoa(m.running)+")→ hang")
		return
	}
	// 纯被动(未开主动探测):保持原语义 —— 先等满 stall_sec,再要求 running>0 才敢判 hang;
	// running==0 分不清 wedged/空闲 → 放过。
	if stalledFor < stall {
		w.set(false, "freeze-grace", "计数器冻结但 "+stalled+" 前出过词(<stall,视为在干活)")
		return
	}
	if m.running > 0 {
		w.set(true, "stall-hang", "token 停滞 "+stalled+" 且 running="+ftoa(m.running)+">0 → hang(被动)")
		return
	}
	w.set(false, "idle", "空闲(running=0,停滞不算 hang;wedged-idle 靠日志快判或 active_probe 覆盖)")
}

func dur(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) + "s" }
func ftoa(f float64) string      { return strconv.FormatFloat(f, 'f', 0, 64) }

// gtoa:给 0~1 的 gauge(KV 水位)用的格式化。不能复用 ftoa —— 那个是 0 位小数、
// 为 token 计数设计的,把 KV 的真实增量(实测一个 chunk 约 +0.000861)打成 "+0",
// 日志读起来成了「没变化却说在干活」,恰好毁掉这条日志的排查价值
// (2026-09-17 集成测试实拍:"KV 水位 +0")。
func gtoa(f float64) string { return strconv.FormatFloat(f, 'f', 6, 64) }
