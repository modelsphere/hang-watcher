# hang-watcher

A **hang-detection sidecar** for LLM inference engines (vLLM and sglang).

It runs in the same pod as the engine, scrapes the engine's **local** `/metrics`
to decide whether it is still making progress, and exposes `/healthz`
(`200` healthy, `503` hung) for the **engine container's livenessProbe**.

**Division of labour**: the sidecar only *reports*. Killing and restarting is
left to the kubelet — the engine container's liveness probe fails, so the
kubelet restarts it; with a LeaderWorkerSet configured for
`RecreateGroupOnPodRestart` that restart recreates the whole group. The sidecar
**never calls the Kubernetes API and needs no RBAC**.

## Why a sidecar

An engine that is wedged usually still accepts TCP connections and still answers
a shallow `/health`. A centralized watchdog polling from outside sees a healthy
HTTP server and does nothing, while every request piles up behind a scheduler
that has stopped turning. Sitting inside the pod, the sidecar can read the
engine's own progress counters and make a per-pod decision, and it can hand the
verdict to the kubelet through the mechanism Kubernetes already has for this.

## How liveness is decided

Every poll scrapes `/metrics` and evaluates, in priority order:

1. **`/metrics` reachability** — unreachable (the engine's HTTP layer is gone)
   for longer than `stall_sec` → hang.
2. **`token_progress` grew** — the sum of the engine's token counters is higher
   than last poll → it is working, healthy.
3. **Log fast path** — progress frozen for at least `log_stall_sec` *and* the
   engine log showed a hang signature within `log_window_sec` → hang, without
   waiting out `stall_sec`. Two independent signals are confidence enough.
4. **KV cache rising (vLLM)** — progress is flat but `vllm:kv_cache_usage_perc`
   is climbing → prefill is allocating blocks, so the engine is computing →
   healthy. See below.
5. **Active probe** — progress is flat: ask the engine directly. A 2xx means it
   is alive; repeated failures spanning `stall_sec` confirm the hang.
6. **Idle exemption** — stalled with no in-flight requests and no probe
   configured → idle, healthy. A quiet night must not look like a hang.
7. **Startup exemption** — before `/metrics` has ever been scraped
   successfully, always healthy. Model loading is the startupProbe's job.

### The two engine-specific blind spots

Both of these were found on real hardware, and both are the same shape: *a
counter that looks like a progress signal but only moves when a request
finishes*.

**sglang**: `generation_tokens_total` and `prompt_tokens_total` are incremented
only when a request completes. A single 200-second deep-reasoning request at low
concurrency therefore leaves every counter frozen, and a perfectly healthy
instance is declared hung. Fixed by also counting
`sglang:realtime_tokens_total`, which is incremented on **every forward pass**,
plus `sglang:cuda_graph_passes_total` as a redundant second leg.

**vLLM**: during **prefill**, *no* progress metric moves at all.
`generation_tokens_total` advances per decode step but not during prefill;
`prompt_tokens_total` books the whole prompt at once when prefill ends (verified
with a real 1M-token prompt: flat for ten seconds, then over a million tokens
inside a single two-second sample); and `iteration_tokens_total`, despite the
name, is accounted per request output. An exhaustive scan of every metric two
vLLM builds expose (88 and 108 series) found no iteration-level counter at all.

For vLLM the sidecar therefore tracks `vllm:kv_cache_usage_perc` as a **separate
leg that only counts increases**. Chunked prefill allocates KV blocks per chunk,
so the gauge climbs steadily while the token counters are flat. It is deliberately
*not* summed into `token_progress`: that sum is stateless and assumes monotonic
counters, whereas this is a gauge that *drops* when requests release their
blocks — mixed in, a wedged engine whose clients are timing out one by one would
reset the stall timer on every release and never be declared hung. A decrease
only re-baselines; it never counts as progress.

The two legs are complementary and neither works alone: during healthy decode
the KV gauge is flat for many seconds at a time, because a request only
allocates once every few hundred tokens.

### Why the probe fires on every stalled poll

An earlier version probed **once**, after progress had been frozen for the full
`stall_sec`, and a single timeout was enough to declare the hang. But once that
verdict lands, the kubelet needs only `failureThreshold × period` to start
killing, so the next poll's conclusion arrives at the same moment as the kill —
the chance to reverse the decision was illusory. Any unlucky timeout (a GC
pause, a blip, one slow response) could kill a healthy engine.

Probing every stalled poll gives consecutive-failure semantics for free, without
changing the kill condition: a successful probe really runs a forward pass, that
forward advances `token_progress`, and the stall timer resets. So the stall timer
can only reach `stall_sec` if **every** probe in that window failed — while the
verdict still lands at `stall_sec`, exactly as before.

A constraint worth respecting when tuning: keep
`active_probe_timeout_sec + poll_interval_sec` well under `stall_sec`.
Raising the probe timeout does not make false kills less likely — it makes them
*more* likely, by leaving room for fewer independent failures.

### Consequence of coupling engine liveness to the sidecar

The engine's liveness probe points at the sidecar, so if the sidecar dies, is
OOM-killed, or starts slowly, the probe cannot connect and a **healthy engine
gets restarted**. The binary is a static Go executable that restarts in under a
second and uses around 10Mi in practice, so the usual grace period is ample —
but do not set its memory limit too tight (64Mi is a reasonable floor).

## Configuration

Environment variables (fixed at startup):

| Variable | Default | Meaning |
|---|---|---|
| `ENGINE_URL` | `http://127.0.0.1:8050` | the local engine |
| `LISTEN` | `:9090` | where `/healthz` is served |
| `CONFIG_FILE` | `/etc/hang-watcher/config.json` | ConfigMap mount point |
| `LOG_FILE` | *(unset)* | engine log path or glob; unset disables the log fast path |
| `LOG_HANG_PATTERN` | *(built-in)* | single regex; replaces the default rather than adding to it |

ConfigMap keys — **all hot-reloaded**: edit, `kubectl apply`, and the sidecar
picks the change up on its next poll. No pod roll. Only a code or image change
needs one.

| Key | Default | Meaning |
|---|---|---|
| `poll_interval_sec` | 5 | `/metrics` scrape period |
| `stall_sec` | 30 | how long progress may stay frozen before a hang is declared |
| `metrics_timeout_sec` | 10 | `/metrics` scrape timeout |
| `active_probe_enabled` | `true` | ask the engine before declaring a hang |
| `active_probe_path` | `/health_generate` | probe path; for vLLM use `/v1/completions` |
| `active_probe_method` | `GET` | `GET` or `POST` |
| `active_probe_body` | `""` | request body for `POST` (e.g. a one-token completion) |
| `active_probe_timeout_sec` | 5 | probe timeout |
| `log_stall_sec` | 30 | short stall threshold, used only when the log agrees |
| `log_window_sec` | 120 | how recent a log signature must be to count |

`log_file` and `log_hang_pattern` can also be set in the ConfigMap, but unlike
everything else they are **not** hot-reloaded — the log tailer is constructed at
startup, so changing them needs a pod roll.

The defaults are not arbitrary. They come from tuning against a real hang
(SIGSTOP on the engine process) on a test cluster, which brought the time from
hang to the caller's connection being cut from over 420 seconds down to about
50: `stall 30 + poll ≤5 + probe 5 + kubelet liveness 2×5 + process exit`. The
built-in defaults, `deploy/configmap.yaml` and any chart that ships this sidecar
should agree, so that running the binary with no ConfigMap behaves like a full
deployment.

## Deployment

See `deploy/` — a ConfigMap plus two reference examples, one for a Deployment
and one for a LeaderWorkerSet. Adapt names, ports and image before applying.

Two things matter:

- The **livenessProbe goes on the engine container** (that is what gets
  restarted) but its `httpGet` points at the **sidecar's port 9090** — same pod,
  same network namespace.
- **Keep a startupProbe.** It gates model loading, which can take twenty
  minutes, and without it the engine is killed before it ever serves.

| | after liveness fails | extra configuration |
|---|---|---|
| **Deployment** (single pod) | the engine container restarts | none |
| **LeaderWorkerSet** | the leader restarts → whole group is recreated | `restartPolicy: RecreateGroupOnPodRestart` |

One sidecar and one probe cover both; LWS only adds the switch that turns a
single-pod restart into a group recreation.

## Build and test

```bash
go test ./...                        # verdict state machine and /metrics parsing
CGO_ENABLED=0 go build -o hang-watcher .
docker build -t hang-watcher:dev .
```

`smoke_test.sh` and `smoke_probe_test.sh` run the real binary against a fake
engine and assert end-to-end behaviour, including the case a unit test cannot
reach: that a *single* probe failure does not flip `/healthz` to 503.

## License

Apache-2.0. See [LICENSE](LICENSE).
