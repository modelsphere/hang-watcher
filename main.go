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
	StallSec          int `json:"stall_sec"`           // token 停滞多久(且 running>0)判 hang;对齐 monitor GRACE=180
	MetricsTimeoutSec int `json:"metrics_timeout_sec"` // 拉 /metrics 超时
}

func defaultHot() hotConfig {
	return hotConfig{
		PollIntervalSec:   envInt("POLL_INTERVAL_SEC", 15),
		StallSec:          envInt("STALL_SEC", 180),
		MetricsTimeoutSec: envInt("METRICS_TIMEOUT_SEC", 10),
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
	return cur
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
		hung, reason := w.Hung()
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
	var lastReason string
	var lastMtime time.Time
	hot := loadHot(configFile, defaults)

	for {
		// 热加载配置(仅 mtime 变时重读 + 打日志)
		if fi, err := os.Stat(configFile); err == nil && fi.ModTime() != lastMtime {
			lastMtime = fi.ModTime()
			hot = loadHot(configFile, defaults)
			log.Printf("配置热更:poll=%ds stall=%ds timeout=%ds", hot.PollIntervalSec, hot.StallSec, hot.MetricsTimeoutSec)
		}

		text, err := fetchMetrics(client, engineURL, time.Duration(hot.MetricsTimeoutSec)*time.Second)
		var snap metricsSnap
		if err == nil {
			snap = parseMetrics(text)
		}
		w.step(time.Now(), snap, err, time.Duration(hot.StallSec)*time.Second)

		if hung, reason := w.Hung(); reason != lastReason { // 裁决变化才打日志(避免刷屏)
			lastReason = reason
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
