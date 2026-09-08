package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 钉住实测调优出来的那一组默认值。改动它们是有意为之才对 —— 这个测试的作用是让「无意的漂移」
// 变成一次红色,而不是等到某天生产上重启时间莫名其妙变长(或者 stall 被调小导致误杀)。
// 三处必须一致:这里、deploy/configmap.yaml、sglang chart 的 hangWatcher.config。
func TestDefaults(t *testing.T) {
	d := defaultHot()
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"PollIntervalSec", d.PollIntervalSec, 5},
		{"StallSec", d.StallSec, 30},
		{"MetricsTimeoutSec", d.MetricsTimeoutSec, 10},
		{"ActiveProbeTimeoutSec", d.ActiveProbeTimeoutSec, 5},
		{"LogStallSec", d.LogStallSec, 30},
		{"LogWindowSec", d.LogWindowSec, 120},
	} {
		if c.got != c.want {
			t.Errorf("默认 %s = %d, 想要 %d(改默认值请同步 deploy/configmap.yaml 与 sglang chart)", c.name, c.got, c.want)
		}
	}
	if !d.ActiveProbeEnabled {
		t.Errorf("主动探测应默认开:纯被动分不清「真空闲」和「调度器卡死」(running 都是 0)")
	}
	if d.ActiveProbePath != "/health_generate" || d.ActiveProbeMethod != "GET" {
		t.Errorf("探测端点应是 GET /health_generate(有界;/v1/completions 无上界),实际 %s %s",
			d.ActiveProbeMethod, d.ActiveProbePath)
	}
	if d.ActiveProbeTimeoutSec >= d.StallSec {
		t.Errorf("探测超时(%ds)不该大于等于 stall(%ds),否则它会主导检出时间", d.ActiveProbeTimeoutSec, d.StallSec)
	}
	if d.LogFile != "" {
		t.Errorf("日志快判应默认关(LogFile 为空),实际 %q", d.LogFile)
	}
}

// loadHot 的健壮性。ConfigMap 是模板渲染出来的,不能假设它总是完整、总是合法。
func TestLoadHot(t *testing.T) {
	base := defaultHot()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// ① 文件不存在 → 默认值(首启就是这条路)
	if got := loadHot(filepath.Join(dir, "nope.json"), base); got != base {
		t.Errorf("文件不存在应返回默认值")
	}

	// ② JSON 坏了 → 默认值,不 panic 不退出
	if got := loadHot(write("bad.json", `{"stall_sec": }`), base); got != base {
		t.Errorf("坏 JSON 应回落默认值")
	}

	// ③ 只写了一部分键 → 其余保持默认(helm --reuse-values 就会渲染出这种残缺 ConfigMap)
	got := loadHot(write("partial.json", `{"stall_sec": 300}`), base)
	if got.StallSec != 300 {
		t.Errorf("显式写的 stall_sec 应生效,得 %d", got.StallSec)
	}
	if got.PollIntervalSec != base.PollIntervalSec || got.ActiveProbePath != base.ActiveProbePath ||
		got.ActiveProbeTimeoutSec != base.ActiveProbeTimeoutSec {
		t.Errorf("没写的键应保持默认,得 poll=%d path=%q timeout=%d",
			got.PollIntervalSec, got.ActiveProbePath, got.ActiveProbeTimeoutSec)
	}

	// ④ 显式的 0 / 负数 → 回落默认。这不是吹毛求疵:stall_sec=0 会让「任何冻结」立刻判 hang,
	//    log_stall_sec=0 会让「任何一条特征日志」立刻判 hang —— 模板缺键渲染成 0 是真实场景。
	z := loadHot(write("zero.json", `{"poll_interval_sec":0,"stall_sec":0,"metrics_timeout_sec":-1,
		"active_probe_timeout_sec":0,"log_stall_sec":0,"log_window_sec":0,"active_probe_path":"","active_probe_method":""}`), base)
	if z.StallSec != base.StallSec || z.PollIntervalSec != base.PollIntervalSec ||
		z.MetricsTimeoutSec != base.MetricsTimeoutSec || z.ActiveProbeTimeoutSec != base.ActiveProbeTimeoutSec ||
		z.LogStallSec != base.LogStallSec || z.LogWindowSec != base.LogWindowSec ||
		z.ActiveProbePath != base.ActiveProbePath || z.ActiveProbeMethod != base.ActiveProbeMethod {
		t.Errorf("0/负数/空串应回落默认,得 %+v", z)
	}

	// ⑤ 布尔 false 是合法取值,不能被当成「没写」硬掰回默认
	f := loadHot(write("off.json", `{"active_probe_enabled": false}`), base)
	if f.ActiveProbeEnabled {
		t.Errorf("显式 active_probe_enabled:false 必须生效(smoke_test 和被动验证都靠它)")
	}
}

// 日志快判排在主动探测【之前】:两个独立信号都指向 hang 时,不必再花一次探测的时间去确认。
func TestLogFastPathShortCircuitsProbe(t *testing.T) {
	stall := 60 * time.Second
	t0 := time.Unix(1000, 0)
	snap := func(tp, running float64) metricsSnap { return metricsSnap{tp: tp, haveTP: true, running: running} }

	probed := false
	probe := func() bool { probed = true; return true } // 探测说「活着」,若被调用就会盖掉裁决

	w := newWatcher()
	w.enableLogConfirm(30*time.Second, 120*time.Second)
	w.step(t0, snap(1000, 1), nil, stall, probe) // 基线
	at := t0.Add(40 * time.Second)               // 过了 log_stall(30)但没到 stall(60)
	w.noteLogHits(at.Add(-5*time.Second), 1, nil)
	w.step(at, snap(1000, 1), nil, stall, probe)

	if h, st, r := w.Hung(); !h || st != "stall-hang-log" {
		t.Errorf("停滞 40s + 日志特征应走快判,实际 hung=%v state=%s(%s)", h, st, r)
	}
	if probed {
		t.Errorf("走了日志快判就不该再调主动探测(白花一次探测时间,而且探测成功会盖掉正确裁决)")
	}
}
