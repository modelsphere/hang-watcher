package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseMetrics(t *testing.T) {
	// sglang:token_progress = generation+prompt(跨系列求和);running = num_running_reqs
	sglang := `# HELP
sglang:generation_tokens_total 1000
sglang:prompt_tokens_total 5000
sglang:num_running_reqs 3
sglang:num_queue_reqs 1
`
	s := parseMetrics(sglang)
	if !s.haveTP || s.tp != 6000 {
		t.Errorf("sglang tp = %v(have=%v), 想要 6000", s.tp, s.haveTP)
	}
	if s.running != 3 {
		t.Errorf("sglang running = %v, 想要 3", s.running)
	}

	// sglang:多系列(DP)running 求和(而非只取最后一条)
	sglangDP := `sglang:generation_tokens_total{dp="0"} 10
sglang:num_running_reqs{dp="0"} 2
sglang:num_running_reqs{dp="1"} 5
`
	sd := parseMetrics(sglangDP)
	if sd.running != 7 {
		t.Errorf("sglang DP running = %v, 想要 7(2+5,求和)", sd.running)
	}

	// vllm:多 engine(DP)running 求和 + progress 求和
	vllm := `vllm:generation_tokens_total{engine="0"} 100
vllm:generation_tokens_total{engine="1"} 200
vllm:prompt_tokens_total{engine="0"} 300
vllm:num_requests_running{engine="0"} 2
vllm:num_requests_running{engine="1"} 4
`
	v := parseMetrics(vllm)
	if v.tp != 600 {
		t.Errorf("vllm tp = %v, 想要 600(100+200+300)", v.tp)
	}
	if v.running != 6 {
		t.Errorf("vllm running = %v, 想要 6(2+4)", v.running)
	}

	// 无 progress 计数
	none := parseMetrics("process_start_time_seconds 123\n")
	if none.haveTP {
		t.Errorf("无 progress 计数时 haveTP 应 false")
	}

	// sglang:realtime_tokens_total 必须计入 token_progress(带 mode label 的多系列求和)。
	// 它是 iteration 级的,*_tokens_total 是请求结束才累加 —— 少了它长请求会被误判 hang。
	rt := `sglang:generation_tokens_total 1000
sglang:prompt_tokens_total 5000
sglang:realtime_tokens_total{mode="decode"} 700
sglang:realtime_tokens_total{mode="prefill_compute"} 200
sglang:realtime_tokens_total{mode="prefill_cache"} 100
sglang:num_running_reqs 1
`
	r := parseMetrics(rt)
	if r.tp != 7000 {
		t.Errorf("含 realtime 的 tp = %v, 想要 7000(1000+5000+700+200+100)", r.tp)
	}

	// cuda_graph_passes_total 也计入:它是 forward pass 计数器(decode/prefill 各带 mode 标签),
	// 与 realtime 同一处累加。作为冗余信号 —— 老版本(v0.5.10.post1)没有它,新版本才有。
	cg := `sglang:generation_tokens_total 100
sglang:realtime_tokens_total{mode="decode"} 200
sglang:cuda_graph_passes_total{mode="decode_cuda_graph"} 30
sglang:cuda_graph_passes_total{mode="prefill_none"} 7
sglang:num_running_reqs 1
`
	if got := parseMetrics(cg); got.tp != 337 {
		t.Errorf("含 cuda_graph_passes 的 tp = %v, 想要 337(100+200+30+7)", got.tp)
	}

	// 旧版 sglang 无 realtime_tokens_total → 退化回原四项,不 panic 不丢 haveTP
	old := parseMetrics("sglang:generation_tokens_total 42\n")
	if !old.haveTP || old.tp != 42 {
		t.Errorf("旧引擎(无 realtime)应退化为 tp=42,实际 %v(have=%v)", old.tp, old.haveTP)
	}
}

// 判活状态机:各分支
func TestWatcherVerdict(t *testing.T) {
	stall := 180 * time.Second
	t0 := time.Unix(1000, 0)
	snap := func(tp, running float64) metricsSnap { return metricsSnap{tp: tp, haveTP: true, running: running} }

	// ① 启动期:/metrics 不可达 → 不判 hang(交给 startupProbe)
	w := newWatcher()
	w.step(t0, metricsSnap{}, errors.New("conn refused"), stall, nil)
	if h, _, _ := w.Hung(); h {
		t.Errorf("启动期不可达不应判 hang")
	}

	// ② 建基线 → 健康
	w.step(t0, snap(1000, 2), nil, stall, nil)
	if h, _, _ := w.Hung(); h {
		t.Errorf("建基线应健康")
	}

	// ③ 出词增长 → 健康
	w.step(t0.Add(15*time.Second), snap(1500, 2), nil, stall, nil)
	if h, _, _ := w.Hung(); h {
		t.Errorf("出词增长应健康")
	}

	// ④ 停滞但未超 stall → 健康(冻结宽限)
	w.step(t0.Add(120*time.Second), snap(1500, 2), nil, stall, nil)
	if h, _, _ := w.Hung(); h {
		t.Errorf("停滞 105s<stall 应健康(宽限)")
	}

	// ⑤ 停滞超 stall 且 running>0 → HANG
	w.step(t0.Add(15*time.Second+stall+time.Second), snap(1500, 2), nil, stall, nil)
	if h, _, r := w.Hung(); !h {
		t.Errorf("停滞超 stall + running>0 应判 hang,实际 healthy(%s)", r)
	}

	// ⑥ 空闲豁免:停滞超 stall 但 running==0 → 健康
	w2 := newWatcher()
	w2.step(t0, snap(1000, 0), nil, stall, nil)                        // 基线
	w2.step(t0.Add(stall+time.Minute), snap(1000, 0), nil, stall, nil) // 长期停滞 running=0
	if h, _, _ := w2.Hung(); h {
		t.Errorf("空闲(running=0)停滞不应判 hang")
	}

	// ⑦ 计数器倒退(引擎重启)→ 重置基线、健康
	w3 := newWatcher()
	w3.step(t0, snap(5000, 1), nil, stall, nil)
	w3.step(t0.Add(15*time.Second), snap(10, 1), nil, stall, nil) // 倒退
	if h, _, _ := w3.Hung(); h {
		t.Errorf("计数器倒退应重置基线、健康")
	}

	// ⑦b 回归:长请求场景 —— *_tokens_total 全程冻结(请求未结束不累加),
	//      仅 realtime_tokens_total 在涨。tp 是求和,必须因此判「在干活」而非 hang。
	//      少了 realtime 项时,这里会在 stall 后误杀一台正常实例。
	//      走真实 /metrics 文本(经 parseMetrics),这样删掉 progressMetrics 里的 realtime 项时这里会红。
	w3b := newWatcher()
	realtime := 500
	text := func(rt int) string { // gen/prompt 恒定,只有 realtime 在推进
		return fmt.Sprintf("sglang:generation_tokens_total 800\n"+
			"sglang:prompt_tokens_total 200\n"+
			"sglang:realtime_tokens_total{mode=\"decode\"} %d\n"+
			"sglang:num_running_reqs 1\n", rt)
	}
	w3b.step(t0, parseMetrics(text(realtime)), nil, stall, nil) // 基线
	for i := 1; i <= 12; i++ {                                  // 12×30s = 360s > stall
		realtime += 40 // 每轮 decode iteration 推进
		w3b.step(t0.Add(time.Duration(i)*30*time.Second), parseMetrics(text(realtime)), nil, stall, nil)
	}
	if h, _, r := w3b.Hung(); h {
		t.Errorf("长请求(仅 realtime 在涨)不应判 hang,实际 hang(%s)", r)
	}

	// ⑧ started 后持续不可达超 stall → HANG(引擎 HTTP 层死)
	w4 := newWatcher()
	w4.step(t0, snap(1000, 1), nil, stall, nil)                                 // started
	w4.step(t0.Add(10*time.Second), metricsSnap{}, errors.New("x"), stall, nil) // firstFail
	if h, _, _ := w4.Hung(); h {
		t.Errorf("刚不可达(未超 stall)不应立刻 hang")
	}
	w4.step(t0.Add(10*time.Second+stall+time.Second), metricsSnap{}, errors.New("x"), stall, nil)
	if h, _, r := w4.Hung(); !h {
		t.Errorf("持续不可达超 stall 应判 hang,实际(%s)", r)
	}
}

// 可选主动探测:疑似 stall-hang 时探测通过→不判 hang;失败→坐实 hang;健康分支不触发。
func TestActiveProbe(t *testing.T) {
	stall := 180 * time.Second
	t0 := time.Unix(1000, 0)
	snap := func(tp, running float64) metricsSnap { return metricsSnap{tp: tp, haveTP: true, running: running} }

	// 探测返回 true(引擎仍存活)→ 停滞超 stall + running>0 也不判 hang
	wOK := newWatcher()
	wOK.step(t0, snap(1000, 2), nil, stall, nil) // 基线
	wOK.step(t0.Add(stall+time.Second), snap(1000, 2), nil, stall, func() bool { return true })
	if h, s, _ := wOK.Hung(); h || s != "stall-active-ok" {
		t.Errorf("主动探测通过应不判 hang,得 hung=%v state=%s", h, s)
	}

	// 探测返回 false(引擎打不通)→ 坐实 hang
	wBad := newWatcher()
	wBad.step(t0, snap(1000, 2), nil, stall, nil)
	wBad.step(t0.Add(stall+time.Second), snap(1000, 2), nil, stall, func() bool { return false })
	if h, _, _ := wBad.Hung(); !h {
		t.Errorf("主动探测失败应判 hang")
	}

	// 探测只在冻结超 grace 时触发:出词增长(健康)时不应调用
	called := false
	wGrow := newWatcher()
	wGrow.step(t0, snap(1000, 2), nil, stall, nil)
	wGrow.step(t0.Add(15*time.Second), snap(2000, 2), nil, stall, func() bool { called = true; return false })
	if called {
		t.Errorf("出词增长(健康)不应触发主动探测")
	}

	// 缺口修复:探测开 + running==0 + 冻结超 stall + 探测失败 → 判 hang(wedged-idle,scheduler 卡死)
	wIdleBad := newWatcher()
	wIdleBad.step(t0, snap(1000, 0), nil, stall, nil)
	wIdleBad.step(t0.Add(stall+time.Second), snap(1000, 0), nil, stall, func() bool { return false })
	if h, _, _ := wIdleBad.Hung(); !h {
		t.Errorf("running==0 但冻结超 stall + 探测失败(wedged-idle)应判 hang")
	}

	// 探测开 + running==0 + 探测成功 → 健康(真空闲,不误杀)
	wIdleOK := newWatcher()
	wIdleOK.step(t0, snap(1000, 0), nil, stall, nil)
	wIdleOK.step(t0.Add(stall+time.Second), snap(1000, 0), nil, stall, func() bool { return true })
	if h, _, _ := wIdleOK.Hung(); h {
		t.Errorf("running==0 且探测成功(真空闲)不应判 hang")
	}

	// 被动(probe=nil)+ running==0 + 冻结超 stall → 仍判空闲(不改被动行为)
	wPassive := newWatcher()
	wPassive.step(t0, snap(1000, 0), nil, stall, nil)
	wPassive.step(t0.Add(stall+time.Second), snap(1000, 0), nil, stall, nil)
	if h, _, _ := wPassive.Hung(); h {
		t.Errorf("被动模式 running==0 冻结应仍判空闲(分不清 wedged/空闲)")
	}
}

// 主动探测失败后的复核。探测本身要耗 active_probe_timeout_sec,那段时间里引擎可能已经恢复
// 出词(典型:卡在一次超长 forward 里,forward 在探测等待期间结束了),而 step() 手上的快照
// 是探测【之前】拉的、已经过期。不复核就会把这种情况判成 hang。
func TestProbeFailRecheck(t *testing.T) {
	stall := 30 * time.Second
	t0 := time.Unix(1000, 0)
	snap := func(tp, running float64) metricsSnap { return metricsSnap{tp: tp, haveTP: true, running: running} }
	failProbe := func() bool { return false }

	// ① 探测失败,但复核发现 progress 已经涨了 → 不判 hang,并把停滞计时重置
	w := newWatcher()
	w.setRecheck(func() (float64, bool) { return 1500, true }) // 探测期间涨到 1500
	w.step(t0, snap(1000, 1), nil, stall, failProbe)
	w.step(t0.Add(stall+time.Second), snap(1000, 1), nil, stall, failProbe)
	if h, st, r := w.Hung(); h || st != "probe-fail-but-growing" {
		t.Errorf("探测失败但复核有进度,不应判 hang;实际 hung=%v state=%s(%s)", h, st, r)
	}
	// 复核成功后停滞计时应归零:紧接着再来一轮(仍是旧 tp)不该立刻判 hang
	w.step(t0.Add(stall+2*time.Second), snap(1500, 1), nil, stall, failProbe)
	if h, _, _ := w.Hung(); h {
		t.Errorf("复核后 lastGrow 应已刷新,下一轮不该马上判 hang")
	}

	// ② 探测失败且复核仍无进度 → 判 hang(原行为)
	w2 := newWatcher()
	w2.setRecheck(func() (float64, bool) { return 1000, true }) // 没动
	w2.step(t0, snap(1000, 1), nil, stall, failProbe)
	w2.step(t0.Add(stall+time.Second), snap(1000, 1), nil, stall, failProbe)
	if h, _, r := w2.Hung(); !h {
		t.Errorf("探测失败且复核无进度应判 hang,实际健康(%s)", r)
	}

	// ③ 复核本身拿不到数(/metrics 也挂了)→ 不能因此放过,仍判 hang
	w3 := newWatcher()
	w3.setRecheck(func() (float64, bool) { return 0, false })
	w3.step(t0, snap(1000, 1), nil, stall, failProbe)
	w3.step(t0.Add(stall+time.Second), snap(1000, 1), nil, stall, failProbe)
	if h, _, r := w3.Hung(); !h {
		t.Errorf("复核取不到数时不该放过(引擎连 /metrics 都答不了更像真死),实际健康(%s)", r)
	}

	// ④ 没注册复核(nil)→ 完全走原行为
	w4 := newWatcher()
	w4.step(t0, snap(1000, 1), nil, stall, failProbe)
	w4.step(t0.Add(stall+time.Second), snap(1000, 1), nil, stall, failProbe)
	if h, _, _ := w4.Hung(); !h {
		t.Errorf("未注册复核时应保持原行为(探测失败即判 hang)")
	}
}

// 每轮停滞都主动探测(2026-09-14 改动)。
//
// 旧行为:停滞满 stall_sec 才探【第一次】,这一次探测超时 + 复核无进度就立刻判 hang。
// 而 hung 之后 kubelet livenessProbe 只需 2×5s 就 Killing,下一轮 poll 的补救结论恰好与
// Killing 同时到达 —— 等于任何一次偶发的探测超时都能杀掉一台健康引擎。
//
// 新行为:每轮停滞都探。成功的探测会真跑一次 forward、推进 token_progress → 下轮 growing →
// 停滞计时归零。于是「停滞累计到 stall_sec」当且仅当这期间每次探测都失败,判死时机不变,
// 却自带了连败语义。
func TestProbeEveryStalledPoll(t *testing.T) {
	stall := 30 * time.Second
	t0 := time.Unix(1000, 0)
	snap := func(tp, running float64) metricsSnap { return metricsSnap{tp: tp, haveTP: true, running: running} }

	// ① 停滞但未满 stall 时就应该已经探过了(旧行为此时根本不调 probe)
	calls := 0
	okProbe := func() bool { calls++; return true }
	w := newWatcher()
	w.step(t0, snap(1000, 0), nil, stall, okProbe)                    // 建基线
	w.step(t0.Add(5*time.Second), snap(1000, 0), nil, stall, okProbe) // 停滞 5s,远小于 stall
	if calls != 1 {
		t.Fatalf("停滞 5s(<stall)就应主动探测一次,实际调用 %d 次", calls)
	}
	if h, st, r := w.Hung(); h || st != "stall-active-ok" {
		t.Errorf("探测存活不应判 hang;实际 hung=%v state=%s(%s)", h, st, r)
	}

	// ② 探测失败但停滞未满 stall → 不判 hang,只累计失败次数
	fails := 0
	failProbe := func() bool { fails++; return false }
	w2 := newWatcher()
	w2.setRecheck(func() (float64, bool) { return 1000, true }) // 复核也没进度
	w2.step(t0, snap(1000, 1), nil, stall, failProbe)
	for i := 1; i <= 5; i++ { // 5s、10s ... 25s,都 < stall
		w2.step(t0.Add(time.Duration(i*5)*time.Second), snap(1000, 1), nil, stall, failProbe)
		if h, st, r := w2.Hung(); h {
			t.Fatalf("停滞 %ds(<stall)探测失败不应判 hang;实际 state=%s(%s)", i*5, st, r)
		}
	}
	if fails != 5 {
		t.Errorf("应每轮都探,5 轮期望 5 次探测,实际 %d 次", fails)
	}
	if w2.probeFails != 5 {
		t.Errorf("连续失败计数应为 5,实际 %d", w2.probeFails)
	}
	// 满 stall 之后才判 hang,且此时已连败多次
	w2.step(t0.Add(stall+time.Second), snap(1000, 1), nil, stall, failProbe)
	if h, st, _ := w2.Hung(); !h || st != "stall-hang" {
		t.Errorf("停滞满 stall 且连续探测失败应判 hang;实际 hung=%v state=%s", h, st)
	}

	// ③ 中途探测成功一次 → 失败计数清零(单次抖动不累积)
	w3 := newWatcher()
	w3.setRecheck(func() (float64, bool) { return 1000, true })
	flaky := true
	probe := func() bool { flaky = !flaky; return flaky } // 交替 成功/失败
	w3.step(t0, snap(1000, 1), nil, stall, probe)
	for i := 1; i <= 10; i++ {
		w3.step(t0.Add(time.Duration(i*5)*time.Second), snap(1000, 1), nil, stall, probe)
	}
	if w3.probeFails > 1 {
		t.Errorf("探测成功应把连续失败计数清零,实际 %d", w3.probeFails)
	}
	// 即使停滞已远超 stall,只要还能交替探通就不该判 hang
	w3.step(t0.Add(stall+time.Minute), snap(1000, 1), nil, stall, func() bool { return true })
	if h, _, _ := w3.Hung(); h {
		t.Errorf("探测存活时不应判 hang(哪怕停滞已超 stall)")
	}

	// ④ 被动模式(probe=nil)行为不变:未满 stall 走 freeze-grace
	w4 := newWatcher()
	w4.step(t0, snap(1000, 1), nil, stall, nil)
	w4.step(t0.Add(5*time.Second), snap(1000, 1), nil, stall, nil)
	if h, st, r := w4.Hung(); h || st != "freeze-grace" {
		t.Errorf("被动模式未满 stall 应为 freeze-grace;实际 hung=%v state=%s(%s)", h, st, r)
	}
}

// TestKVGaugeCoversPrefill:vLLM 的 prefill 盲区靠 kv_cache_usage_perc 这条腿补上。
//
// 背景(2026-09-17 实测):vLLM 侧三个 progress counter 在 prefill 期间全部冻结,
// 而它又没有 sglang:realtime_tokens_total 的对应物,于是一次长 prefill 会被判成 hang。
// kv_cache_usage_perc 在 chunked prefill 里逐块爬升,正好覆盖这一段。
func TestKVGaugeCoversPrefill(t *testing.T) {
	stall := 30 * time.Second
	t0 := time.Unix(2000, 0)
	// tp 恒定 = prefill 期间的真实形态;kv 逐块爬升
	snapKV := func(tp, kv, running float64) metricsSnap {
		return metricsSnap{tp: tp, haveTP: true, kv: kv, haveKV: true, running: running}
	}

	// ① tp 不动但 KV 在涨 -> 视为在干活,停滞计时被重置,久了也不判 hang
	w := newWatcher()
	w.step(t0, snapKV(1000, 0.10, 1), nil, stall, nil) // 基线
	for i := 1; i <= 20; i++ {                         // 100 秒,远超 stall=30s
		at := t0.Add(time.Duration(i*5) * time.Second)
		w.step(at, snapKV(1000, 0.10+float64(i)*0.001, 1), nil, stall, nil)
	}
	if hung, _, reason := w.Hung(); hung {
		t.Fatalf("tp 平但 KV 持续上升(prefill 在分配 block)不该判 hang,实际:%s", reason)
	}

	// ② tp 和 KV 【同时】冻结 -> 真 hang,仍要判出来(这条腿不能把 hang 掩盖掉)
	w2 := newWatcher()
	w2.step(t0, snapKV(1000, 0.10, 1), nil, stall, nil)
	w2.step(t0.Add(stall+time.Second), snapKV(1000, 0.10, 1), nil, stall, nil)
	if hung, _, _ := w2.Hung(); !hung {
		t.Fatal("tp 与 KV 同时冻结且 running>0,应判 hang")
	}

	// ③ KV 【下降】不算进度 —— 引擎卡死后客户端陆续超时断开也会释放 block,
	//    拿下降当"在干活"会把 hang 掩盖成健康。
	w3 := newWatcher()
	w3.step(t0, snapKV(1000, 0.90, 1), nil, stall, nil)
	for i := 1; i <= 10; i++ { // 50 秒,KV 一路下降
		at := t0.Add(time.Duration(i*5) * time.Second)
		w3.step(at, snapKV(1000, 0.90-float64(i)*0.01, 1), nil, stall, nil)
	}
	if hung, _, _ := w3.Hung(); !hung {
		t.Fatal("tp 平 + KV 只降不升,应判 hang(下降不算进度)")
	}

	// ④ 引擎不暴露该 gauge(sglang)-> 这条腿自动不生效,行为与改动前一致
	w4 := newWatcher()
	noKV := func(tp, running float64) metricsSnap {
		return metricsSnap{tp: tp, haveTP: true, running: running}
	}
	w4.step(t0, noKV(1000, 1), nil, stall, nil)
	w4.step(t0.Add(stall+time.Second), noKV(1000, 1), nil, stall, nil)
	if hung, _, _ := w4.Hung(); !hung {
		t.Fatal("没有 KV gauge 时应退回原逻辑:tp 冻结 + running>0 -> hang")
	}
}

// TestParseKVGauge:确认 kv_cache_usage_perc 被解析,且跨 DP 多 engine 求和。
func TestParseKVGauge(t *testing.T) {
	txt := `vllm:generation_tokens_total{engine="0"} 100
vllm:kv_cache_usage_perc{engine="0",model_name="m"} 0.25
vllm:kv_cache_usage_perc{engine="1",model_name="m"} 0.35
vllm:num_requests_running{engine="0"} 2`
	s := parseMetrics(txt)
	if !s.haveKV {
		t.Fatal("应当解析到 kv_cache_usage_perc")
	}
	if s.kv < 0.5999 || s.kv > 0.6001 {
		t.Errorf("kv 跨 engine 求和应为 0.6,实际 %v", s.kv)
	}
	// sglang 没有这个 gauge
	s2 := parseMetrics(`sglang:generation_tokens_total 5
sglang:num_running_reqs 1`)
	if s2.haveKV {
		t.Error("sglang 文本里不该出现 KV gauge")
	}
}

// TestKVDeltaFormatting:KV 增量必须打得出来。
// 集成测试实拍过 "KV 水位 +0" —— 复用了给 token 计数设计的 0 位小数格式化,
// 把一个 chunk 的真实增量(约 +0.000861)显示成 0,日志读起来像"没变化却说在干活"。
func TestKVDeltaFormatting(t *testing.T) {
	stall := 30 * time.Second
	t0 := time.Unix(3000, 0)
	snapKV := func(tp, kv float64) metricsSnap {
		return metricsSnap{tp: tp, haveTP: true, kv: kv, haveKV: true, running: 1}
	}
	w := newWatcher()
	w.step(t0, snapKV(1000, 0.010000), nil, stall, nil)
	w.step(t0.Add(5*time.Second), snapKV(1000, 0.010861), nil, stall, nil) // 一个 chunk 的量级
	_, state, reason := w.Hung()
	if state != "kv-growing" {
		t.Fatalf("应走 kv-growing 分支,实际 state=%s reason=%s", state, reason)
	}
	if strings.Contains(reason, "+0(") || strings.Contains(reason, "+0 ") {
		t.Errorf("KV 增量被格式化成了 0,日志无法排查:%s", reason)
	}
	// 不断言具体字符串 —— gtoa 用 'g' 自适应,不同量级的输出形态不同。
	// 要保证的是「能看出是个非零的小数」,而不是某个固定写法。
	if !strings.Contains(reason, "0.000861") {
		t.Errorf("应显示真实增量 0.000861,实际:%s", reason)
	}
}

// TestLogHangBeatsKVGrowing:日志快判必须排在 kv-growing【前面】。
//
// 第一版把 kv 分支插在快判之前,于是「prefill 期间 KV 在涨,同时引擎日志已经喊了
// fatal error」这种双信号组合会被 kv 这一条腿单方面否决掉。快判存在的意义就是
// 两个独立证据同时成立时提前判,不该被它挡住。code review 时改的,这条测试锁住顺序。
func TestLogHangBeatsKVGrowing(t *testing.T) {
	stall := 60 * time.Second
	t0 := time.Unix(4000, 0)
	snap := func(tp, kv float64) metricsSnap {
		return metricsSnap{tp: tp, haveTP: true, kv: kv, haveKV: true, running: 1}
	}
	w := newWatcher()
	w.logEnabled, w.logStall, w.logWindow = true, 15*time.Second, 120*time.Second
	w.step(t0, snap(1000, 0.10), nil, stall, nil) // 基线

	// 引擎刚喊过特征行,且停滞已超 logStall;同时 KV 还在涨(prefill 在分配 block)
	w.lastLogHit = t0.Add(18 * time.Second)
	w.step(t0.Add(20*time.Second), snap(1000, 0.20), nil, stall, nil)

	hung, state, reason := w.Hung()
	if !hung || state != "stall-hang-log" {
		t.Fatalf("日志快判应当先于 kv-growing 生效,实际 hung=%v state=%s reason=%s", hung, state, reason)
	}
}

// TestKVDeltaAcrossScales:不同部署的 KV 增量量级差几个数量级(= 1/总block数),
// 格式化必须都能读出来,不能有一个规模打成 0。
func TestKVDeltaAcrossScales(t *testing.T) {
	for _, d := range []float64{0.000861, 0.00097, 1.2e-05, 5e-07} {
		got := gtoa(d)
		if got == "0" || strings.HasPrefix(got, "0.00000") && !strings.ContainsAny(got, "e123456789") {
			t.Errorf("增量 %v 被格式化成 %q,日志无法排查", d, got)
		}
	}
}
