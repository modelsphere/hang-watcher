package main

import (
	"errors"
	"fmt"
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
