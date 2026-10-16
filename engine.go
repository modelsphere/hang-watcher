// Engine instance identity: which engine process is the sidecar looking at,
// and when did it start. Served on GET /engine so that something sharing state
// with the engine -- a prefill/decode router, for one -- can tell that the
// engine behind it was replaced and act on it.
//
// Why the sidecar and not the engine: the kubelet restarts the engine
// container in place, keeping the pod and its IP, so a client that only
// watches pod events or endpoints sees nothing. And the engines do not agree
// on how to say it: vLLM exposes process_start_time_seconds, sglang does not.
// One endpoint with one meaning saves every consumer from learning both.
//
// Why /metrics and not the process itself: containers in a pod have separate
// pid namespaces, so the sidecar cannot see the engine's processes (its /proc
// holds only its own). shareProcessNamespace would change that, but it makes
// the pause container pid 1, and engine charts commonly rely on the engine
// being pid 1 of its container (a preStop that signals pid 1 and then kills
// every other process it can see would take down the whole pod).
//
// The identity comes from the engine's own /metrics, strongest first:
//
//  1. process_start_time_seconds (vLLM, and anything using the default
//     Prometheus process collector) -- an exact start time, so the id is
//     derived from it and started_at is exact.
//  2. sglang's startup_* gauges (startup_time_seconds,
//     startup_cuda_graph_time_seconds, sglang >= 0.5.19). They are durations,
//     not timestamps, but they are set once per process and measured to the
//     nanosecond, so two processes practically never report the same set.
//     That makes them a fingerprint: the id is stable across sidecar restarts,
//     but started_at is only the time this sidecar first saw the instance.
//  3. Neither present: the id is minted by the sidecar on first sight and
//     re-minted when the token counters go backwards. This one does NOT
//     survive a sidecar restart -- a restarted sidecar reports a new id for
//     the same engine -- so source tells consumers how far to trust it.
//
// Consumers should compare engine_id, not started_at: for sources 2 and 3
// started_at is an observation time and moves when the sidecar restarts.
package main

import (
	"encoding/json"
	"hash/fnv"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

const processStartMetric = "process_start_time_seconds"

// startupMetrics: sglang gauges that are written once, at startup.
var startupMetrics = map[string]bool{
	"sglang:startup_time_seconds":            true,
	"sglang:startup_cuda_graph_time_seconds": true,
}

const (
	idProcessStart = "process_start_time"
	idStartup      = "startup_fingerprint"
	idObserved     = "observed"
)

type engineState struct {
	id        string
	idSource  string
	startedAt time.Time
	restarts  int // restarts seen by this sidecar; for its log only

	// The startup fingerprint is kept as series -> value rather than as a hash,
	// because the series do not all appear at once: a scrape during startup can
	// see some phases and miss others. New series joining is the same process;
	// only a series changing value, or one disappearing, is a new process.
	startup map[string]float64
}

// noteEngine folds one successful scrape into the identity and reports whether
// the engine was replaced since the last one. The first identity ever seen is
// not a restart, and neither is moving to a stronger source (an sglang engine
// whose startup gauges only show up after its first scrape).

func (w *watcher) noteEngine(now time.Time, m metricsSnap, metricsOK bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := &w.eng
	if !metricsOK {
		return false
	}
	switch {
	case m.startTime > 0:
		id := "pst-" + strconv.FormatFloat(m.startTime, 'f', -1, 64)
		if e.idSource == idProcessStart && e.id == id {
			return false
		}
		restarted := e.idSource == idProcessStart
		e.id, e.idSource, e.startup = id, idProcessStart, nil
		// to the millisecond: the float carries no more, and finer digits are noise
		e.startedAt = time.UnixMilli(int64(math.Round(m.startTime * 1000))).UTC()
		if restarted {
			e.restarts++
		}
		return restarted
	case len(m.startup) > 0:
		if e.idSource == idStartup && sameProcess(e.startup, m.startup) {
			if len(m.startup) > len(e.startup) {
				e.startup = m.startup // more phases reported; the id stays put
			}
			return false
		}
		restarted := e.idSource == idStartup
		e.id, e.idSource, e.startup = "sfp-"+fingerprint(m.startup), idStartup, m.startup
		if restarted || e.startedAt.IsZero() {
			e.startedAt = now
		}
		if restarted {
			e.restarts++
		}
		return restarted
	default:
		if e.id == "" {
			e.id, e.idSource, e.startedAt = observedID(now, 0), idObserved, now
		}
		return false
	}
}

// noteRegress handles token counters going backwards. That is only the
// identity signal when the engine offers nothing better; with a real identity
// noteEngine has already decided, and a regression without an identity change
// is not a second restart.
func (w *watcher) noteRegress(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := &w.eng
	if e.idSource != idObserved {
		return
	}
	e.restarts++
	e.id, e.startedAt = observedID(now, e.restarts), now
}

// sameProcess: every series seen before still has the same value.
func sameProcess(old, cur map[string]float64) bool {
	for k, v := range old {
		if nv, ok := cur[k]; !ok || nv != v {
			return false
		}
	}
	return true
}

// fingerprint hashes the startup series. Computed when the identity is first
// taken, so a series joining later does not change the id.
func fingerprint(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		_, _ = h.Write([]byte(k + "=" + strconv.FormatFloat(m[k], 'g', -1, 64) + "\n"))
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

func observedID(now time.Time, n int) string {
	return "obs-" + strconv.FormatInt(now.UnixNano(), 36) + "-" + strconv.Itoa(n)
}

// engineInfo is the /engine response body. source says how far started_at can
// be trusted: process_start_time is the engine's real start;
// startup_fingerprint and observed are fallbacks, where started_at is when this
// sidecar first saw the instance.
type engineInfo struct {
	EngineID  string `json:"engine_id"`
	StartedAt string `json:"started_at"`
	Source    string `json:"source"`
}

func (w *watcher) Engine() (engineInfo, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	e := w.eng
	return engineInfo{EngineID: e.id, StartedAt: rfc(e.startedAt), Source: e.idSource}, e.id != ""
}

// engineRestarts: restarts seen by this sidecar, for its own log.
func (w *watcher) engineRestarts() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.eng.restarts
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// engineHandler serves GET /engine: 200 with the identity once one is known,
// 503 before the first sighting, so "not known yet" is never read as an id.
func engineHandler(w *watcher) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			rw.Header().Set("Allow", "GET, HEAD")
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		info, known := w.Engine()
		rw.Header().Set("Cache-Control", "no-store")
		if !known {
			http.Error(rw, "engine not identified yet", http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(info)
	}
}
