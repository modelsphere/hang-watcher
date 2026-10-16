package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Metric lines as a real vLLM (v0.29 nightly) and sglang (0.5.19) print them.
const (
	vllmText = `# HELP process_start_time_seconds Start time of the process since unix epoch in seconds.
process_start_time_seconds 1.79194364129e+09
vllm:generation_tokens_total{model_name="m"} 100
vllm:num_requests_running{model_name="m"} 0
`
	vllmRestartedText = `process_start_time_seconds 1.79195001337e+09
vllm:generation_tokens_total{model_name="m"} 5000
vllm:num_requests_running{model_name="m"} 1
`
	sglangPartial = `sglang:startup_time_seconds{engine_type="unified",model_name="qwen",phase="load_weight"} 6.029548802413046
sglang:realtime_tokens_total{mode="decode"} 0
sglang:num_running_reqs 0
`
	sglangFull = `sglang:startup_time_seconds{engine_type="unified",model_name="qwen",phase="load_weight"} 6.029548802413046
sglang:startup_time_seconds{engine_type="unified",model_name="qwen",phase="scheduler_e2e"} 109.03901622537524
sglang:startup_cuda_graph_time_seconds{engine_type="unified",model_name="qwen",phase="decode"} 17.055109737440944
sglang:realtime_tokens_total{mode="decode"} 0
sglang:num_running_reqs 0
`
	sglangRestarted = `sglang:startup_time_seconds{engine_type="unified",model_name="qwen",phase="load_weight"} 5.98812301
sglang:startup_time_seconds{engine_type="unified",model_name="qwen",phase="scheduler_e2e"} 108.71100912
sglang:startup_cuda_graph_time_seconds{engine_type="unified",model_name="qwen",phase="decode"} 17.30122841
sglang:realtime_tokens_total{mode="decode"} 0
sglang:num_running_reqs 0
`
	bareText = "sglang:generation_tokens_total 1000\nsglang:num_running_reqs 0\n"
)

// view: the /engine body plus what the tests also assert on -- whether an id is
// known, and the restarts this sidecar counted.
type engView struct {
	EngineID, StartedAt, IDSource string
	Known, Exact                  bool
	RestartsObserved              int
}

func view(w *watcher) engView {
	info, known := w.Engine()
	return engView{info.EngineID, info.StartedAt, info.Source, known,
		info.Source == idProcessStart, w.engineRestarts()}
}

func scrape(w *watcher, at time.Time, text string) {
	w.stepNoWarmup(at, parseMetrics(text), nil, 30*time.Second, func() bool { return true })
}

func TestEngineVLLMStartTimeIsExact(t *testing.T) {
	w := newWatcher()
	t0 := time.Now()
	scrape(w, t0, vllmText)
	e := view(w)
	if !e.Known || e.IDSource != idProcessStart || !e.Exact {
		t.Fatalf("want an exact process_start_time identity, got %+v", e)
	}
	if e.StartedAt != "2026-10-14T02:07:21.29Z" {
		t.Errorf("started_at = %s, want the engine's own start time 2026-10-14T02:07:21.29Z", e.StartedAt)
	}
	id := e.EngineID
	scrape(w, t0.Add(5*time.Second), vllmText)
	if e := view(w); e.EngineID != id || e.RestartsObserved != 0 {
		t.Fatalf("same process must keep its id: %+v", e)
	}
}

// The restart that counter regression cannot see: the new process has already
// produced more tokens than the old one had in total by the next poll. The
// control shows that without a start time this goes unnoticed, so the test
// would fail if the restart were detected by the counters instead.
func TestEngineRestartWithoutCounterRegression(t *testing.T) {
	t0 := time.Now()

	ctrl := newWatcher()
	scrape(ctrl, t0, "vllm:generation_tokens_total 100\n")
	scrape(ctrl, t0.Add(5*time.Second), "vllm:generation_tokens_total 5000\n")
	if e := view(ctrl); e.RestartsObserved != 0 {
		t.Fatalf("control: without an identity, counters alone cannot see this restart; got %+v", e)
	}

	w := newWatcher()
	scrape(w, t0, vllmText)
	before := view(w).EngineID
	scrape(w, t0.Add(5*time.Second), vllmRestartedText)
	e := view(w)
	if e.RestartsObserved != 1 || e.EngineID == before {
		t.Fatalf("a new process_start_time is a restart: %+v", e)
	}
	if info, _ := w.Engine(); info.Restarts != 1 {
		t.Errorf("restarts on /engine = %d, want 1", info.Restarts)
	}
	if e.StartedAt != "2026-10-14T03:53:33.37Z" {
		t.Errorf("started_at should follow the new process, got %s", e.StartedAt)
	}
	if _, state, _ := w.Hung(); state != "baseline" {
		t.Errorf("state = %q, want baseline (a restart starts over from a fresh baseline)", state)
	}
	if !w.readyAt.Equal(t0.Add(5 * time.Second)) {
		t.Errorf("warm-up must re-arm at the restart, readyAt=%v", w.readyAt)
	}
}

// An idle engine never moves its counters, so a restart leaves them at 0 and
// only the identity notices.
func TestEngineIdleRestart(t *testing.T) {
	w := newWatcher()
	t0 := time.Now()
	scrape(w, t0, sglangFull)
	scrape(w, t0.Add(5*time.Second), sglangRestarted)
	if e := view(w); e.RestartsObserved != 1 {
		t.Fatalf("idle sglang restart must be detected from the startup gauges: %+v", e)
	}
}

// Startup gauges arriving over several scrapes are the same process; a value
// changing is a new one.
func TestEngineSglangFingerprintGrowsWithoutRestart(t *testing.T) {
	w := newWatcher()
	t0 := time.Now()
	scrape(w, t0, sglangPartial)
	e1 := view(w)
	if e1.IDSource != idStartup || e1.Exact {
		t.Fatalf("want a startup fingerprint with an observed start time: %+v", e1)
	}
	scrape(w, t0.Add(5*time.Second), sglangFull)
	if e := view(w); e.EngineID != e1.EngineID || e.RestartsObserved != 0 {
		t.Fatalf("more phases appearing is not a restart: before %+v after %+v", e1, e)
	}
	scrape(w, t0.Add(10*time.Second), sglangRestarted)
	if e := view(w); e.EngineID == e1.EngineID || e.RestartsObserved != 1 {
		t.Fatalf("a changed startup value is a restart: %+v", e)
	}
}

// What makes the fingerprint worth having over an observed id: a sidecar that
// restarts and sees the same engine reports the same id.
func TestEngineIDSurvivesSidecarRestart(t *testing.T) {
	t0 := time.Now()
	for _, text := range []string{vllmText, sglangFull} {
		a, b := newWatcher(), newWatcher()
		scrape(a, t0, text)
		scrape(b, t0.Add(time.Hour), text)
		if view(a).EngineID != view(b).EngineID {
			t.Errorf("same engine, two sidecars, different ids: %s vs %s", view(a).EngineID, view(b).EngineID)
		}
	}
	// ...and an observed id does not, which is why id_source is reported.
	a, b := newWatcher(), newWatcher()
	scrape(a, t0, bareText)
	scrape(b, t0.Add(time.Hour), bareText)
	if view(a).EngineID == view(b).EngineID {
		t.Errorf("observed ids are per sidecar; equal ids would mean the test is not exercising that path")
	}
}

func TestEngineObservedFallsBackToRegression(t *testing.T) {
	w := newWatcher()
	t0 := time.Now()
	scrape(w, t0, bareText)
	e1 := view(w)
	if e1.IDSource != idObserved {
		t.Fatalf("no identity metrics: want observed, got %+v", e1)
	}
	scrape(w, t0.Add(5*time.Second), "sglang:generation_tokens_total 3\nsglang:num_running_reqs 0\n")
	if e := view(w); e.RestartsObserved != 1 || e.EngineID == e1.EngineID {
		t.Fatalf("counters going backwards must mint a new observed id: %+v", e)
	}
}

// With a real identity a regression is not counted a second time.
func TestEngineRegressionNotDoubleCounted(t *testing.T) {
	w := newWatcher()
	t0 := time.Now()
	scrape(w, t0, vllmRestartedText) // tp 5000
	scrape(w, t0.Add(5*time.Second), vllmText)
	if e := view(w); e.RestartsObserved != 1 {
		t.Fatalf("one restart, got %d", e.RestartsObserved)
	}
}

func TestEngineUnreachableKeepsID(t *testing.T) {
	w := newWatcher()
	t0 := time.Now()
	scrape(w, t0, vllmText)
	id := view(w).EngineID
	w.stepNoWarmup(t0.Add(5*time.Second), metricsSnap{}, errTest, 30*time.Second, nil)
	if e := view(w); e.EngineID != id || e.RestartsObserved != 0 {
		t.Fatalf("an outage is not a new id: %+v", e)
	}
}

var errTest = &httpErr{500}

func TestEngineHandler(t *testing.T) {
	w := newWatcher()
	h := engineHandler(w)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/engine", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before the first scrape: want 503, got %d %s", rec.Code, rec.Body.String())
	}

	scrape(w, time.Now(), vllmText)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/engine", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
	}
	want := map[string]any{"engine_id": "pst-1791943641.29", "started_at": "2026-10-14T02:07:21.29Z", "source": "process_start_time",
		"restarts": float64(0), "watcher_started_at": rfc(w.bornAt)}
	if rec.Code != http.StatusOK || len(got) != len(want) {
		t.Fatalf("want 200 with exactly %v, got %d %v", want, rec.Code, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/engine", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: want 405, got %d", rec.Code)
	}
}
