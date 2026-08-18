package main

import (
	"errors"
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
}

// 判活状态机:各分支
func TestWatcherVerdict(t *testing.T) {
	stall := 180 * time.Second
	t0 := time.Unix(1000, 0)
	snap := func(tp, running float64) metricsSnap { return metricsSnap{tp: tp, haveTP: true, running: running} }

	// ① 启动期:/metrics 不可达 → 不判 hang(交给 startupProbe)
	w := newWatcher()
	w.step(t0, metricsSnap{}, errors.New("conn refused"), stall)
	if h, _ := w.Hung(); h {
		t.Errorf("启动期不可达不应判 hang")
	}

	// ② 建基线 → 健康
	w.step(t0, snap(1000, 2), nil, stall)
	if h, _ := w.Hung(); h {
		t.Errorf("建基线应健康")
	}

	// ③ 出词增长 → 健康
	w.step(t0.Add(15*time.Second), snap(1500, 2), nil, stall)
	if h, _ := w.Hung(); h {
		t.Errorf("出词增长应健康")
	}

	// ④ 停滞但未超 stall → 健康(冻结宽限)
	w.step(t0.Add(120*time.Second), snap(1500, 2), nil, stall)
	if h, _ := w.Hung(); h {
		t.Errorf("停滞 105s<stall 应健康(宽限)")
	}

	// ⑤ 停滞超 stall 且 running>0 → HANG
	w.step(t0.Add(15*time.Second+stall+time.Second), snap(1500, 2), nil, stall)
	if h, r := w.Hung(); !h {
		t.Errorf("停滞超 stall + running>0 应判 hang,实际 healthy(%s)", r)
	}

	// ⑥ 空闲豁免:停滞超 stall 但 running==0 → 健康
	w2 := newWatcher()
	w2.step(t0, snap(1000, 0), nil, stall)                        // 基线
	w2.step(t0.Add(stall+time.Minute), snap(1000, 0), nil, stall) // 长期停滞 running=0
	if h, _ := w2.Hung(); h {
		t.Errorf("空闲(running=0)停滞不应判 hang")
	}

	// ⑦ 计数器倒退(引擎重启)→ 重置基线、健康
	w3 := newWatcher()
	w3.step(t0, snap(5000, 1), nil, stall)
	w3.step(t0.Add(15*time.Second), snap(10, 1), nil, stall) // 倒退
	if h, _ := w3.Hung(); h {
		t.Errorf("计数器倒退应重置基线、健康")
	}

	// ⑧ started 后持续不可达超 stall → HANG(引擎 HTTP 层死)
	w4 := newWatcher()
	w4.step(t0, snap(1000, 1), nil, stall)                                 // started
	w4.step(t0.Add(10*time.Second), metricsSnap{}, errors.New("x"), stall) // firstFail
	if h, _ := w4.Hung(); h {
		t.Errorf("刚不可达(未超 stall)不应立刻 hang")
	}
	w4.step(t0.Add(10*time.Second+stall+time.Second), metricsSnap{}, errors.New("x"), stall)
	if h, r := w4.Hung(); !h {
		t.Errorf("持续不可达超 stall 应判 hang,实际(%s)", r)
	}
}
