package main

import (
	"errors"
	"testing"
	"time"
)

// stepNoWarmup is the old signature, with warmup disabled. The other tests
// exercise the verdict logic itself and should not be perturbed by the grace
// period, so they all go through here; the grace period is covered below.
func (w *watcher) stepNoWarmup(now time.Time, m metricsSnap, fetchErr error, stall time.Duration, probe func() bool) {
	w.step(now, m, fetchErr, stall, 0, probe)
}

const warmup = 10 * time.Minute

// A stall with a failing probe: declared a hang without the grace period, not
// declared within it. The control matters -- a test that only asserted "no hang
// during warm-up" would also pass against an implementation that never declares
// a hang at all.
func TestWarmupSuppressesStallHang(t *testing.T) {
	dead := func() bool { return false }
	t0 := time.Now()

	// Control: warmup=0, so a stall past stall_sec is a hang.
	ctrl := newWatcher()
	ctrl.stepNoWarmup(t0, metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, dead)
	ctrl.stepNoWarmup(t0.Add(5*time.Second), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, dead)
	ctrl.stepNoWarmup(t0.Add(40*time.Second), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, dead)
	if hung, _, reason := ctrl.Hung(); !hung {
		t.Fatalf("control (warmup=0) should declare a hang, got healthy: %s", reason)
	}

	// Same input 45s after readiness, with a 10min grace period: no hang.
	w := newWatcher()
	w.step(t0, metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	w.step(t0.Add(5*time.Second), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	w.step(t0.Add(40*time.Second), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	hung, state, reason := w.Hung()
	if hung {
		t.Fatalf("no hang should be declared during warm-up: %s", reason)
	}
	if state != "warmup-stall-hang" {
		t.Errorf("state = %q, want warmup-stall-hang (a suppressed verdict must be distinguishable from a healthy engine)", state)
	}
	if !contains(reason, "warm-up grace") {
		t.Errorf("the reason must say it was suppressed, or a real fault goes unnoticed: %s", reason)
	}
}

// Once the grace period is over the same stall is a hang, so the grace period
// defers a verdict rather than waiving it permanently.
func TestWarmupExpires(t *testing.T) {
	dead := func() bool { return false }
	t0 := time.Now()
	w := newWatcher()
	w.step(t0, metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)

	w.step(t0.Add(9*time.Minute), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	if hung, _, _ := w.Hung(); hung {
		t.Fatalf("9min after readiness is still inside the grace period")
	}
	w.step(t0.Add(11*time.Minute), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	hung, state, _ := w.Hung()
	if !hung {
		t.Fatalf("11min after readiness the grace period is over; expected a hang")
	}
	if state != "stall-hang" {
		t.Errorf("state = %q, want stall-hang", state)
	}
}

// The log fast path -- the one that fired in the kimi-k3 incident -- is covered
// by the grace period too.
func TestWarmupSuppressesLogFastPath(t *testing.T) {
	t0 := time.Now()
	w := newWatcher()
	w.enableLogConfirm(15*time.Second, 120*time.Second)
	w.step(t0, metricsSnap{tp: 100, haveTP: true, running: 0}, nil, 30*time.Second, warmup, nil)
	w.noteLogHits(t0.Add(20*time.Second), 1, nil)
	// 25s stalled with a signature line in the window: a hang without warm-up.
	w.step(t0.Add(25*time.Second), metricsSnap{tp: 100, haveTP: true, running: 0}, nil, 30*time.Second, warmup, nil)
	hung, state, reason := w.Hung()
	if hung {
		t.Fatalf("the log fast path must also be held during warm-up: %s", reason)
	}
	if state != "warmup-stall-hang-log" {
		t.Errorf("state = %q, want warmup-stall-hang-log", state)
	}
}

// An unreachable /metrics is covered as well: all four verdict paths must go
// through setHang, or the grace period would only be half-applied.
func TestWarmupSuppressesUnreach(t *testing.T) {
	t0 := time.Now()
	w := newWatcher()
	w.step(t0, metricsSnap{tp: 100, haveTP: true}, nil, 30*time.Second, warmup, nil) // ready
	w.step(t0.Add(5*time.Second), metricsSnap{}, errors.New("conn refused"), 30*time.Second, warmup, nil)
	w.step(t0.Add(60*time.Second), metricsSnap{}, errors.New("conn refused"), 30*time.Second, warmup, nil)
	if hung, state, reason := w.Hung(); hung {
		t.Fatalf("unreachable must also be held during warm-up (state=%s): %s", state, reason)
	}
	w.step(t0.Add(11*time.Minute), metricsSnap{}, errors.New("conn refused"), 30*time.Second, warmup, nil)
	if hung, _, _ := w.Hung(); !hung {
		t.Fatalf("after the grace period, unreachable is a hang")
	}
}

// A restarted engine (token counters go backwards) re-arms the grace period:
// the new process reloads the model and captures cuda graphs, exactly as a
// first start does.
func TestWarmupRestartsOnEngineRestart(t *testing.T) {
	dead := func() bool { return false }
	t0 := time.Now()
	w := newWatcher()
	w.step(t0, metricsSnap{tp: 10000, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)

	// 20 minutes in, the grace period is long gone, so a stall is a hang.
	w.step(t0.Add(20*time.Minute), metricsSnap{tp: 10000, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	if hung, _, _ := w.Hung(); !hung {
		t.Fatalf("at 20min the grace period is over; expected a hang")
	}

	// Engine restart: counters reset, so the grace period starts again.
	w.step(t0.Add(21*time.Minute), metricsSnap{tp: 5, haveTP: true, running: 0}, nil, 30*time.Second, warmup, dead)
	if hung, state, _ := w.Hung(); hung || state != "regress" {
		t.Fatalf("a counter reset should take the regress path and not hang, got hung=%v state=%s", hung, state)
	}
	w.step(t0.Add(22*time.Minute), metricsSnap{tp: 5, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	if hung, _, reason := w.Hung(); hung {
		t.Fatalf("the grace period must be re-armed after a restart: %s", reason)
	}
}

// A flap must not renew the grace period: firstFail is set by a single failed
// scrape, so treating a recovery as a restart would let an engine that flaps
// every nine minutes never be declared hung -- the protection would become a
// hole.
func TestWarmupNotExtendedByFlap(t *testing.T) {
	dead := func() bool { return false }
	t0 := time.Now()
	w := newWatcher()
	w.step(t0, metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)

	// One failed scrape, then recovery, with no counter reset.
	w.step(t0.Add(5*time.Minute), metricsSnap{}, errors.New("timeout"), 30*time.Second, warmup, dead)
	w.step(t0.Add(5*time.Minute+5*time.Second), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)

	// 11 minutes after the original readiness a stall must still be a hang.
	w.step(t0.Add(11*time.Minute), metricsSnap{tp: 100, haveTP: true, running: 1}, nil, 30*time.Second, warmup, dead)
	if hung, state, reason := w.Hung(); !hung {
		t.Fatalf("a flap must not renew the grace period; expected a hang at 11min (state=%s): %s", state, reason)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
