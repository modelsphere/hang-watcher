# Changelog

Notable changes, newest first. Versions up to and including 0.1.22 were released
from the repository this project was split out of; the commit history for them
was carried over, but the release tags were not.

## Unreleased

- Split into a standalone repository. The image name and the configuration are
  unchanged.
- Base images in the `Dockerfile` are now public (`docker.io/library/...`) and
  overridable via `GO_IMAGE` / `RUNTIME_IMAGE` build args, so the build works
  outside the network it was written in.
- `deploy/` examples rewritten to be deployment-agnostic.
- README rewritten; its configuration table had drifted and still documented the
  pre-0.1.18 defaults (`stall_sec` 180, probing off).

## 0.1.22

- **vLLM: cover the prefill blind spot.** No vLLM progress metric moves during
  prefill — `generation_tokens_total` is decode-only, `prompt_tokens_total`
  books the whole prompt when prefill ends, and `iteration_tokens_total` is
  per request output. A long prefill therefore looked like a hang. Added
  `vllm:kv_cache_usage_perc` as a separate leg that only counts increases;
  a decrease re-baselines without resetting the stall timer.
- **Probe on every stalled poll.** Previously a single probe timeout could kill
  a healthy engine, because the kill verdict and the kubelet's restart arrived
  together and left no room to reverse it. The kill condition is unchanged, but
  successful probes now advance progress and reset the stall timer, so reaching
  `stall_sec` requires consecutive failures.
- The log fast path now takes precedence over the KV leg: when the engine has
  already logged a fatal error, that evidence outranks "still allocating
  blocks".
- KV deltas are printed with adaptive precision, so a small delta no longer
  prints as `+0` and contradicts the message it appears in.
- New `smoke_probe_test.sh`: runs the real binary against a fake engine and
  asserts that a single probe failure does not flip `/healthz` to 503.

## 0.1.18

- **Fix false hangs on long sglang requests.** `generation_tokens_total` and
  `prompt_tokens_total` only move when a request finishes, so one long request
  at low concurrency froze every counter and got a healthy engine killed. Now
  also counts the per-forward `sglang:realtime_tokens_total`, with
  `sglang:cuda_graph_passes_total` as a redundant second leg.
- **New log fast path** (optional): progress frozen for `log_stall_sec` *and* a
  hang signature in the engine log within `log_window_sec` declares the hang
  without waiting out `stall_sec`. ORs with the existing path, so detection only
  improves.
- **Re-check progress after a failed probe.** The probe itself takes seconds,
  during which the engine may have recovered; a failed probe is no longer
  sufficient evidence on its own.
- **Empirically tuned defaults**: `poll_interval_sec` 15→5, `stall_sec` 180→30,
  `active_probe_timeout_sec` 20→5, active probing on by default, probe endpoint
  `/health_generate`. Time from hang to the caller's connection being cut went
  from over 420s to about 50s.
- `log_stall_sec` and `log_window_sec` are now genuinely hot-reloaded; before,
  the reload was logged but had no effect.

## 0.1.10

- The active probe now also covers wedged-idle engines. With the scheduler
  stopped, requests pile up in the tokenizer-to-scheduler IPC and the metrics
  are indistinguishable from a genuinely idle engine, so the hang was missed.

## 0.1.9

- Optional active probe: ask the engine directly before declaring a stall a
  hang.

## 0.1.8

- Sum sglang `num_running_reqs` across series, so data-parallel deployments do
  not under-count in-flight requests.
- Deduplicate log lines by coarse state instead of by reason string.

## 0.1.7

- First release: the sidecar, `/healthz`, metrics-based liveness, hot-reloaded
  configuration, and deployment examples.
