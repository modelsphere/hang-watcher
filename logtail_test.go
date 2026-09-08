package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sampleHang = `[2026-09-08 04:35:55] ERROR: Health check failed. Server couldn't get a response from detokenizer for last 20 seconds. tic start time: 04:35:35. last_heartbeat time: 04:30:01`

func writeLines(t *testing.T, p string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogTailer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "0.log")
	writeLines(t, p, "启动前的旧告警(不该被算进来):"+sampleHang)

	tl, err := newLogTailer(filepath.Join(dir, "*.log"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.close()

	// ① 首次 poll 从文件【末尾】起,历史行不算(否则 sidecar 重启会把陈年告警当刚发生)
	if h, err := tl.poll(); err != nil || h != 0 {
		t.Fatalf("首轮应从末尾起、hits=0,实际 hits=%d err=%v", h, err)
	}

	// ② 新增匹配行 → 命中
	writeLines(t, p, "INFO 正常行", sampleHang, "INFO 又一行")
	if h, err := tl.poll(); err != nil || h != 1 {
		t.Fatalf("应命中 1 行,实际 %d err=%v", h, err)
	}

	// ③ 没有新行 → 0,且不重复统计上一轮的
	if h, err := tl.poll(); err != nil || h != 0 {
		t.Fatalf("无新行应 0(不重复计),实际 %d err=%v", h, err)
	}

	// ④ 秒数不同(SGLANG_HEALTH_CHECK_TIMEOUT 可调)仍要匹配 —— pattern 不含秒数
	writeLines(t, p, "[x] ERROR: Health check failed. Server couldn't get a response from detokenizer for last 45 seconds. tic start time: 1")
	if h, _ := tl.poll(); h != 1 {
		t.Fatalf("秒数变了也应匹配,实际 %d", h)
	}

	// ⑤ 半行(写了一半没换行)不误判,补齐后才计
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	f.WriteString("[y] ERROR: Health check failed. Server couldn't ")
	f.Close()
	if h, _ := tl.poll(); h != 0 {
		t.Fatalf("半行不应计入,实际 %d", h)
	}
	writeLines(t, p, "get a response from detokenizer for last 20 seconds.")
	if h, _ := tl.poll(); h != 1 {
		t.Fatalf("半行补齐后应计 1,实际 %d", h)
	}

	// ⑥ 截断(copytruncate)→ 从头读,不因 off > size 卡死
	os.Truncate(p, 0)
	writeLines(t, p, sampleHang)
	if h, err := tl.poll(); err != nil || h != 1 {
		t.Fatalf("截断后应从头读到 1,实际 %d err=%v", h, err)
	}

	// ⑦ 轮转到新文件(glob 取 mtime 最新)→ 新文件从头读
	p2 := filepath.Join(dir, "1.log")
	writeLines(t, p2, "INFO 新文件第一行", sampleHang)
	os.Chtimes(p2, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if h, err := tl.poll(); err != nil || h != 1 {
		t.Fatalf("轮转到新文件应从头读到 1,实际 %d err=%v", h, err)
	}
}

// pattern 的三条路径:空(用内置默认)/ 自定义 / 非法正则。
// chart 里 logHang.pattern 留空时模板不下发 LOG_HANG_PATTERN,走的就是第一条。
func TestLogTailerPattern(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	writeLines(t, p, "seed")

	// ① 空 pattern -> 内置 detokenizer 正则(chart 默认 logHang.pattern: "")
	def, err := newLogTailer(p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer def.close()
	if def.pattern.String() != defaultLogHangPattern {
		t.Errorf("空 pattern 应回落到内置默认,实际 %q", def.pattern.String())
	}
	def.poll() // 建立偏移
	writeLines(t, p, sampleHang, "EngineCore encountered a fatal error")
	if h, _ := def.poll(); h != 1 {
		t.Errorf("内置默认只应命中 detokenizer 那行(1),实际 %d", h)
	}

	// ② 自定义 pattern(vllm 场景:换成 EngineCore 的特征行)
	p2 := filepath.Join(dir, "b.log")
	writeLines(t, p2, "seed")
	cus, err := newLogTailer(p2, `EngineCore encountered a fatal error`)
	if err != nil {
		t.Fatal(err)
	}
	defer cus.close()
	cus.poll()
	writeLines(t, p2, sampleHang, "ERROR EngineCore encountered a fatal error")
	if h, _ := cus.poll(); h != 1 {
		t.Errorf("自定义 pattern 应只命中 EngineCore 那行(1),实际 %d", h)
	}

	// ③ 多个特征行:pattern 是【一条正则】,用 | 交替即可,不是列表也不与默认叠加。
	//    配了 pattern 就【完全取代】内置默认;想同时保留默认,把它写进交替分支里。
	p3 := filepath.Join(dir, "c.log")
	writeLines(t, p3, "seed")
	multi, err := newLogTailer(p3, defaultLogHangPattern+`|EngineCore encountered a fatal error|Watchdog timeout`)
	if err != nil {
		t.Fatal(err)
	}
	defer multi.close()
	multi.poll()
	writeLines(t, p3,
		sampleHang,
		"ERROR EngineCore encountered a fatal error",
		"ERROR Watchdog timeout (self.watchdog_last_forward_ct=...)",
		"INFO 无关行")
	if h, _ := multi.poll(); h != 3 {
		t.Errorf("交替 pattern 应命中 3 行,实际 %d", h)
	}

	// ④ 非法正则 -> 报错(main.go 据此打日志并退回纯 progress 判定,不 crash)
	if _, err := newLogTailer(p, "("); err == nil {
		t.Errorf("非法正则应返回 err")
	}
}

func TestLogTailerMissing(t *testing.T) {
	tl, _ := newLogTailer(filepath.Join(t.TempDir(), "nope-*.log"), "")
	defer tl.close()
	if _, err := tl.poll(); err == nil {
		t.Fatalf("文件不存在应返回 err(调用方据此退回纯 progress 判定)")
	}
}

// 日志快判:停滞 log_stall(30s)+ 日志特征 → 提前判 hang;与 stall_sec(180s)是【或】关系。
func TestLogConfirm(t *testing.T) {
	stall := 180 * time.Second   // 纯 progress 路径(老逻辑)
	logStall := 30 * time.Second // 有日志佐证时的短阈值
	window := 120 * time.Second
	t0 := time.Unix(1000, 0)
	snap := func(tp, running float64) metricsSnap { return metricsSnap{tp: tp, haveTP: true, running: running} }
	newW := func() *watcher {
		w := newWatcher()
		w.enableLogConfirm(logStall, window)
		w.step(t0, snap(1000, 1), nil, stall, nil) // 基线
		return w
	}
	// 关键时刻:停滞 40s —— 已过 log_stall(30s),但远没到 stall_sec(180s)
	fast := t0.Add(40 * time.Second)
	slow := t0.Add(stall + time.Minute) // 连 stall_sec 都过了

	// ① 核心诉求:停滞 40s + 日志喊了 → 立刻判 hang(不等 180s)
	w := newW()
	w.noteLogHits(fast.Add(-10*time.Second), 1, nil)
	w.step(fast, snap(1000, 1), nil, stall, nil)
	if h, st, r := w.Hung(); !h || st != "stall-hang-log" {
		t.Errorf("停滞 40s + 日志特征应快判 hang,实际 hung=%v state=%s(%s)", h, st, r)
	}

	// ② 停滞没到 log_stall(才 20s)→ 即使日志喊了也不判(避免抖一下就杀)
	early := t0.Add(20 * time.Second)
	w2 := newW()
	w2.noteLogHits(early, 1, nil)
	w2.step(early, snap(1000, 1), nil, stall, nil)
	if h, st, _ := w2.Hung(); h || st != "freeze-grace" {
		t.Errorf("停滞 20s < log_stall 30s 不应判 hang,实际 hung=%v state=%s", h, st)
	}

	// ③ 停滞 40s 但日志没喊 → 走老路径,还没到 180s → 健康(不比现状差,也不更激进)
	w3 := newW()
	w3.noteLogHits(fast, 0, nil)
	w3.step(fast, snap(1000, 1), nil, stall, nil)
	if h, st, _ := w3.Hung(); h || st != "freeze-grace" {
		t.Errorf("无日志佐证时 40s 不应判 hang(等 stall_sec),实际 hung=%v state=%s", h, st)
	}

	// ④ 没有日志佐证但停滞满 180s → 老路径照常判 hang(能力只增不减)
	w4 := newW()
	w4.noteLogHits(slow, 0, nil)
	w4.step(slow, snap(1000, 1), nil, stall, nil)
	if h, st, r := w4.Hung(); !h || st != "stall-hang" {
		t.Errorf("停滞满 stall_sec 应按老路径判 hang,实际 hung=%v state=%s(%s)", h, st, r)
	}

	// ⑤ 日志特征太老(超出 window)→ 不给快判资格,退回老路径
	w5 := newW()
	w5.noteLogHits(fast.Add(-(window + time.Minute)), 1, nil)
	w5.step(fast, snap(1000, 1), nil, stall, nil)
	if h, _, _ := w5.Hung(); h {
		t.Errorf("过期的日志特征不应触发快判")
	}

	// ⑥ 日志喊了但 progress 还在涨 → 健康(日志不是充分条件,单条 health 超时不杀)
	w6 := newW()
	w6.noteLogHits(t0.Add(10*time.Second), 3, nil)
	w6.step(t0.Add(15*time.Second), snap(2000, 1), nil, stall, nil)
	if h, st, _ := w6.Hung(); h || st != "growing" {
		t.Errorf("progress 在涨时日志不应触发 hang,实际 hung=%v state=%s", h, st)
	}

	// ⑦ 日志通道坏了 → 不快判,但老路径照常(不漏杀)
	w7 := newW()
	w7.noteLogHits(fast, 1, errors.New("no such file"))
	w7.step(fast, snap(1000, 1), nil, stall, nil)
	if h, _, _ := w7.Hung(); h {
		t.Errorf("日志通道不可用时不应快判(证据不可信)")
	}
	w7.noteLogHits(slow, 1, errors.New("no such file"))
	w7.step(slow, snap(1000, 1), nil, stall, nil)
	if h, _, r := w7.Hung(); !h {
		t.Errorf("日志通道不可用但停滞满 stall_sec,应按老路径判 hang,实际健康(%s)", r)
	}

	// ⑧ wedged-idle:running==0 但日志在喊 → 快判 hang。
	//    纯被动时 running=0 只能放过(分不清「真空闲」和「调度器卡死导致没有在途请求」),
	//    日志正好补上这个盲区:真空闲的引擎 /health 是通的、不会打这行。
	w8 := newW()
	w8.noteLogHits(fast, 5, nil)
	w8.step(fast, snap(1000, 0), nil, stall, nil)
	if h, _, r := w8.Hung(); !h {
		t.Errorf("wedged-idle(running=0 但日志报 detokenizer 超时)应判 hang,实际健康(%s)", r)
	}

	// ⑨ 真空闲:running==0 且日志没喊 → 放过(不误杀半夜无流量)
	w9 := newW()
	w9.noteLogHits(slow, 0, nil)
	w9.step(slow, snap(1000, 0), nil, stall, nil)
	if h, _, _ := w9.Hung(); h {
		t.Errorf("真空闲(running=0 且无日志特征)不应判 hang")
	}
}
