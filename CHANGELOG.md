# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Versions up to and
including 0.1.22 were released from the repository this project was split out
of; the commit history for them was carried over, but the release tags were not.

## [Unreleased]

### Added
- CI on every pull request: `gofmt`, `go vet`, `go test`, both smoke tests and
  a docker build, plus a gate for the license text and committed credentials.
- Dependabot for the GitHub Actions, which are pinned to commit SHAs.
- `NOTICE`.

### Removed
- The internal GitLab pipeline (`.gitlab-ci.yml`).

### Fixed
- The smoke tests waited for a 503 that the warm-up grace period suppresses;
  they now run with `WARMUP_SEC=0`.

## [0.1.26] - 2026-09-30

### Added
- **Warm-up grace period after readiness (`warmup_sec`, default 600).** For ten
  minutes after the engine first serves `/metrics`, hang verdicts are computed
  and logged but not acted on. The first minutes are when a healthy engine is
  least likely to look healthy — cuda graph capture, the first long prefills and
  the KV pool filling out can each freeze `token_progress` past `stall_sec` —
  while a restart costs a full model load. Measured on kimi-k3 (2026-09-30): the
  engine was killed 3m40s after becoming ready, for a 27-minute reload.
  Suppressed verdicts are logged with a `warmup-` state prefix so the grace
  period cannot hide a real fault. The clock re-arms when the engine restarts
  (token counters go backwards), but deliberately **not** on recovery from a
  brief scrape failure, which would let a flapping engine renew it forever.
  All four verdict paths now route through one function, so the grace period
  cannot be half-applied.

### Fixed
- **The example ConfigMap's settings never applied.** Its explanatory
  comments were indented into the `config.json: |` block scalar, making them part
  of the value; Go rejects anything after a top-level JSON value, so the sidecar
  logged a parse failure and fell back to its built-in defaults with every key
  ignored. This went unnoticed because those defaults match what the file says.
  The comments are now YAML comments outside the block.

## [0.1.25] - 2026-09-22

### Added
- Release workflow: a version tag builds the image for linux/amd64 and
  linux/arm64 and pushes it to Docker Hub.

### Changed
- Go module path follows the GitHub organization rename, to
  `github.com/modelsphere/hang-watcher`.

## [0.1.24] - 2026-09-18

### Changed
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

[Unreleased]: https://github.com/modelsphere/hang-watcher/compare/0.1.26...HEAD
[0.1.26]: https://github.com/modelsphere/hang-watcher/compare/0.1.25...0.1.26
[0.1.25]: https://github.com/modelsphere/hang-watcher/compare/0.1.24...0.1.25
[0.1.24]: https://github.com/modelsphere/hang-watcher/releases/tag/0.1.24
