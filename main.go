// hang-watcher:同 pod sidecar,读【本地引擎】(vllm/sglang)的 /metrics 判 hang,
// 暴露 /healthz(健康 200 / hang 503)供 engine 容器的 livenessProbe 探。
//
// 分工:本 sidecar 只【报告】—— 探到 hang 就 /healthz 返 503;真正 kill+重启由 kubelet 做
// (engine 容器 liveness 失败 → kubelet 重启该容器;LWS 配 RecreateGroupOnPodRestart → 整组重建)。
// 不调用任何 k8s API、不需要 RBAC。
//
// 判活阈值(poll 间隔 / 停滞秒数 / 超时)从 ConfigMap 挂的 JSON 文件热加载 → 调参不用滚 pod。
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// hotConfig:可热加载的判活参数(ConfigMap 改了不用滚 pod)。
type hotConfig struct {
	PollIntervalSec   int `json:"poll_interval_sec"`
	StallSec          int `json:"stall_sec"`           // token 停滞多久(且 running>0)判 hang
	MetricsTimeoutSec int `json:"metrics_timeout_sec"` // 拉 /metrics 超时

	// 日志快判(可选,默认关):配了 log_file 才启用。progress 停滞 >= log_stall_sec(默认 30s)
	// 【且】引擎日志最近 log_window_sec 内出现过 log_hang_pattern → 直接判 hang,不必等满
	// stall_sec(60s)。两个独立信号叠加,置信度够,判定更快。
	// 与 stall_sec 路径是【或】关系:日志没喊仍按 stall_sec 判,检测能力只增不减。
	// sglang 场景无需额外流量:openresty 每秒打的 /health 就是真探活(见 logtail.go 头部说明)。
	LogFile        string `json:"log_file"`         // 日志路径,支持 glob(如 /var/log/pods/<ns>_<pod>_*/sglang/*.log)
	LogHangPattern string `json:"log_hang_pattern"` // 特征行正则;留空用 detokenizer 超时默认值
	LogWindowSec   int    `json:"log_window_sec"`   // 特征行时效窗
	LogStallSec    int    `json:"log_stall_sec"`    // 有日志佐证时的【短】停滞阈值(默认 30,远小于 stall_sec)

	// 主动探测(可选,默认关):疑似 stall-hang 时主动打一下引擎确认,通了不判 hang(减少误杀)。
	ActiveProbeEnabled    bool   `json:"active_probe_enabled"`
	ActiveProbePath       string `json:"active_probe_path"`        // 如 /health_generate(sglang);vllm 可用 POST /v1/completions
	ActiveProbeMethod     string `json:"active_probe_method"`      // GET / POST
	ActiveProbeBody       string `json:"active_probe_body"`        // POST 时的请求体(如 completions JSON);GET 留空
	ActiveProbeTimeoutSec int    `json:"active_probe_timeout_sec"` // 主动探测超时(生成可能比抓 metrics 慢)
}

func defaultHot() hotConfig {
	return hotConfig{
		// 这一组默认值 = 2026-09-08 在测试集群 the test cluster 上用 SIGSTOP 造真 hang、逐项调参
		// 实测出来的:从 hang 发生到调用方连接被切断 420s+ -> 50s。构成大致是
		//   stall 30 + poll <=5 + 主动探测 5 + kubelet liveness 2x5 + preStop/退出。
		// 与 sglang chart 的 hangWatcher.config 默认值保持一致 —— 不挂 ConfigMap 直接跑这个
		// 二进制,行为应当和 chart 部署出来的一样。
		PollIntervalSec:   envInt("POLL_INTERVAL_SEC", 5),
		StallSec:          envInt("STALL_SEC", 30),
		MetricsTimeoutSec: envInt("METRICS_TIMEOUT_SEC", 10),
		LogFile:           envOr("LOG_FILE", ""),
		LogHangPattern:    envOr("LOG_HANG_PATTERN", ""),
		LogWindowSec:      envInt("LOG_WINDOW_SEC", 120),
		LogStallSec:       envInt("LOG_STALL_SEC", 30),
		// 主动探测默认【开】(此前默认关)。纯被动路径分不清「真空闲」和「调度器卡死导致
		// 没有在途请求」—— 两种情况 running 都是 0,wedged-idle 只能靠它兜。
		// 端点用 /health_generate 而非 /v1/completions:两者都走到 scheduler,但前者有界 ——
		// 收到 scheduler 任何回应就立刻返回,并在引擎自己的 SGLANG_HEALTH_CHECK_TIMEOUT(20s)
		// 处答 503;后者要真跑完一次生成,耗时受排队影响、没有上界(所以它的超时被迫给到 60s)。
		// 超时 5s:探测只在进度已冻结 stall_sec(30s)之后才发起,正常出词的引擎根本走不到;
		// 空闲引擎的 1-token 请求会被立刻调度。实测 5s 足够,且它曾是 stall 之后最大的一块开销。
		ActiveProbeEnabled:    true,
		ActiveProbePath:       "/health_generate",
		ActiveProbeMethod:     "GET",
		ActiveProbeTimeoutSec: 5,
	}
}

// loadHot:从 JSON 文件读覆盖(缺字段用默认)。文件不存在/坏 → 返回默认(不报错,首启即用默认)。
func loadHot(path string, base hotConfig) hotConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		return base
	}
	cur := base
	if err := json.Unmarshal(b, &cur); err != nil {
		log.Printf("配置文件 %s 解析失败(%v),用上次/默认值", path, err)
		return base
	}
	if cur.PollIntervalSec <= 0 {
		cur.PollIntervalSec = base.PollIntervalSec
	}
	if cur.StallSec <= 0 {
		cur.StallSec = base.StallSec
	}
	if cur.MetricsTimeoutSec <= 0 {
		cur.MetricsTimeoutSec = base.MetricsTimeoutSec
	}
	if cur.ActiveProbePath == "" {
		cur.ActiveProbePath = base.ActiveProbePath
	}
	if cur.ActiveProbeMethod == "" {
		cur.ActiveProbeMethod = base.ActiveProbeMethod
	}
	if cur.ActiveProbeTimeoutSec <= 0 {
		cur.ActiveProbeTimeoutSec = base.ActiveProbeTimeoutSec
	}
	// 这两个此前漏了兜底。log_stall_sec 若是 0,「停滞 >= 0s 且窗口内有日志」就恒成立 ——
	// 任何一条特征行都会立刻判 hang。ConfigMap 是模板渲染出来的,缺键渲染成 0 完全可能
	// (helm --reuse-values 就会让新增键取不到默认值),所以别指望上游一定给对。
	if cur.LogStallSec <= 0 {
		cur.LogStallSec = base.LogStallSec
	}
	if cur.LogWindowSec <= 0 {
		cur.LogWindowSec = base.LogWindowSec
	}
	return cur
}

// activeProbe:按 hot 配置构造主动探测闭包(未开启返回 nil,step 里即不探)。
// 打一个 HTTP 请求(GET/POST path,带 body),2xx 视为引擎存活(true)。
func makeActiveProbe(client *http.Client, base string, hot hotConfig) func() bool {
	if !hot.ActiveProbeEnabled {
		return nil
	}
	return func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(hot.ActiveProbeTimeoutSec)*time.Second)
		defer cancel()
		var body io.Reader
		if hot.ActiveProbeBody != "" {
			body = strings.NewReader(hot.ActiveProbeBody)
		}
		req, err := http.NewRequestWithContext(ctx, hot.ActiveProbeMethod, base+hot.ActiveProbePath, body)
		if err != nil {
			return false
		}
		if hot.ActiveProbeBody != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return false // 超时/连不上 = 探测失败 → 坐实 hang
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
}

// probeDesc:主动探测配置的一行日志描述。
func probeDesc(h hotConfig) string {
	if !h.ActiveProbeEnabled {
		return "off"
	}
	return h.ActiveProbeMethod + " " + h.ActiveProbePath + "(超时" + strconv.Itoa(h.ActiveProbeTimeoutSec) + "s)"
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[hang-watcher] ")

	engineURL := envOr("ENGINE_URL", "http://127.0.0.1:8050")
	listen := envOr("LISTEN", ":9090")
	configFile := envOr("CONFIG_FILE", "/etc/hang-watcher/config.json") // ConfigMap 挂载点
	defaults := defaultHot()

	w := newWatcher()

	// /healthz:engine 的 livenessProbe 探这里。健康 200 / hang 503。
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		hung, _, reason := w.Hung()
		if hung {
			rw.WriteHeader(http.StatusServiceUnavailable) // 503 → engine liveness 失败 → kubelet 重启
			_, _ = io.WriteString(rw, "hung: "+reason)
			return
		}
		_, _ = io.WriteString(rw, "ok: "+reason)
	})
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("HTTP on %s (/healthz);engine=%s;config=%s", listen, engineURL, configFile)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := &http.Client{}
	var lastState string
	hot := loadHot(configFile, defaults)
	var lastMtime time.Time
	if fi, err := os.Stat(configFile); err == nil {
		lastMtime = fi.ModTime() // 预置:首轮不再冗余重读,「热更」日志只在真改动时出
	}
	log.Printf("初始配置:poll=%ds stall=%ds timeout=%ds 主动探测=%s 日志确认=%s", hot.PollIntervalSec, hot.StallSec, hot.MetricsTimeoutSec, probeDesc(hot), logDesc(hot))

	// 日志二次确认(log_file 配了才启)。pattern/路径是启动期固定的,不参与热更 ——
	// 换路径要滚 pod;窗口 log_window_sec 也在启动时定,想热调再说(改一行即可)。
	var tailer *logTailer
	if hot.LogFile != "" {
		t, terr := newLogTailer(hot.LogFile, hot.LogHangPattern)
		if terr != nil {
			log.Printf("日志确认启用失败(pattern 编译错:%v),退回纯 progress 判定", terr)
		} else {
			tailer = t
			defer tailer.close()
			w.enableLogConfirm(time.Duration(hot.LogStallSec)*time.Second, time.Duration(hot.LogWindowSec)*time.Second)
			log.Printf("日志快判已启用:glob=%s log_stall=%ds window=%ds pattern=%s", hot.LogFile, hot.LogStallSec, hot.LogWindowSec, tailer.pattern.String())
		}
	}
	var lastLogErr string

	// 主动探测失败后的复核回调:重新拉一次 /metrics 取 token_progress。
	// 探测会耗掉 active_probe_timeout_sec,期间引擎可能已恢复出词,而 step() 手上的快照
	// 是探测【之前】拉的、已经过期 —— 不复核就会把「探测被一次超长 forward 挡住」当成 hang。
	w.setRecheck(func() (float64, bool) {
		txt, err := fetchMetrics(client, engineURL, time.Duration(hot.MetricsTimeoutSec)*time.Second)
		if err != nil {
			return 0, false
		}
		s := parseMetrics(txt)
		return s.tp, s.haveTP
	})

	for {
		// 热加载配置(仅 mtime 变时重读 + 打日志)
		if fi, err := os.Stat(configFile); err == nil && fi.ModTime() != lastMtime {
			lastMtime = fi.ModTime()
			hot = loadHot(configFile, defaults)
			log.Printf("配置热更:poll=%ds stall=%ds timeout=%ds 主动探测=%s 日志确认=%s", hot.PollIntervalSec, hot.StallSec, hot.MetricsTimeoutSec, probeDesc(hot), logDesc(hot))
		}

		if tailer != nil {
			hits, lerr := tailer.poll()
			w.noteLogHits(time.Now(), hits, lerr)
			if hits > 0 {
				log.Printf("引擎日志命中 hang 特征 %d 行(仅在 progress 也停滞时才判 hang)", hits)
			}
			cur := ""
			if lerr != nil {
				cur = lerr.Error()
			}
			if cur != lastLogErr { // 通道状态变化才打,免刷屏
				lastLogErr = cur
				if cur != "" {
					log.Printf("日志通道不可用(%s)→ 本轮起退回纯 progress 判定", cur)
				} else {
					log.Printf("日志通道恢复可读")
				}
			}
		}

		text, err := fetchMetrics(client, engineURL, time.Duration(hot.MetricsTimeoutSec)*time.Second)
		var snap metricsSnap
		if err == nil {
			snap = parseMetrics(text)
		}
		w.step(time.Now(), snap, err, time.Duration(hot.StallSec)*time.Second, makeActiveProbe(client, engineURL, hot))

		if hung, state, reason := w.Hung(); state != lastState { // 粗状态变化才打日志(reason 内数字每轮变,按 state 去重免刷屏)
			lastState = state
			if hung {
				log.Printf("HANG 判定 → /healthz 返 503:%s", reason)
			} else {
				log.Printf("健康:%s", reason)
			}
		}

		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = srv.Shutdown(shutCtx)
			cancel()
			log.Printf("stopped")
			return
		case <-time.After(time.Duration(hot.PollIntervalSec) * time.Second):
		}
	}
}

// fetchMetrics:GET engine/metrics,返回文本。非 200 或网络错都算「不可达」(err!=nil)。
func fetchMetrics(client *http.Client, base string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &httpErr{resp.StatusCode}
	}
	return string(body), nil
}

type httpErr struct{ code int }

func (e *httpErr) Error() string { return "HTTP " + strconv.Itoa(e.code) }

// logDesc:日志快判配置的一行摘要(供启动/热更日志)。
func logDesc(h hotConfig) string {
	if h.LogFile == "" {
		return "off"
	}
	return "on(" + h.LogFile + ",停滞" + strconv.Itoa(h.LogStallSec) + "s+日志,窗口" + strconv.Itoa(h.LogWindowSec) + "s)"
}
