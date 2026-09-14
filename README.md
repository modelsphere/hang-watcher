# hang-watcher

引擎(vllm/sglang)**hang 检测 sidecar**。跟引擎同 pod,读【本地】引擎 `/metrics` 判活,暴露 `/healthz`(健康 `200` / hang `503`)给 engine 容器的 **livenessProbe** 探。

**分工**:sidecar 只【报告】—— 探到 hang 就 `/healthz` 返 503;真正 **kill + 重启由 kubelet 做**(engine 容器 liveness 失败 → 重启该容器;LWS 配 `RecreateGroupOnPodRestart` → 整组重建)。**不调 k8s API、不需要 RBAC。**

这是 monitor 中心化 auto-restart(`lib/restart.py`/`checks.py`)的 sidecar 化替代:判活逻辑一致,但「杀」交给 k8s、per-pod 判活更准。

## 判活逻辑(复刻 monitor 的被动短路,不主动打流量)
每轮拉引擎 `/metrics`,按优先级:
1. **/metrics 可达性** —— 拉不到(引擎 HTTP 层死)且持续超 `stall_sec` → hang。
2. **token_progress 涨** —— `sum(vllm|sglang:{generation,prompt}_tokens_total)` 比上轮多 → 在干活,健康。
3. **停滞 + running>0** —— 计数器不涨、却有在途请求(`num_requests_running`/`num_running_reqs`)→ 真 hang。
4. **空闲豁免** —— 停滞但 `running==0` → 空闲,健康(不误杀半夜无流量)。
5. **冻结宽限** —— 计数器冻结但 `stall_sec` 内出过词 → 容忍大 prefill 批。
6. **启动豁免** —— 还没成功拉到过 `/metrics` 前一律健康(交给 startupProbe 兜模型加载)。

### 可选:主动探测确认(`active_probe_enabled`,默认关)
纯被动的第 3/4 条(停滞 + running)有两个盲区:① running>0 停滞可能误杀(引擎慢但活着);② **scheduler 卡死时请求只堆在 tokenizer→scheduler 的 IPC 里,`num_running_reqs=0` 且 `num_queue_reqs=0`,从指标看跟真空闲一样 → 被判空闲、漏杀**(实测 SIGSTOP sglang scheduler 复现)。

开启主动探测后,**只要 token_progress 相比上一轮没涨就主动打一下引擎**(`POST /v1/completions`,body 由 chart 按 `model.name` 生成),**不再要求 running>0**:
- 探测 **2xx(通)** → 引擎仍能生成 → **不判 hang**(state=`stall-active-ok`),避免误杀;
- 探测 **失败/超时(`active_probe_timeout_sec`)** 且停滞未满 `stall_sec` → 不判 hang,累计连续失败次数(state=`probe-fail-grace`);
- 探测失败且停滞已满 `stall_sec` → 坐实 hang → 503。

这样既保留原「running>0 停滞」的确认,又补上 **wedged-idle(running=0)** 盲区,完全对齐 monitor(末环就是冻结超 grace → 主动 `/v1/completions` 确认)。默认关时行为与之前完全一致(纯被动:running==0 冻结仍判空闲)。

#### 为什么是「每轮探」而不是「满 stall_sec 探一次」(2026-09-14 改)
旧行为下判死链路上只有**一次**探测:停滞满 `stall_sec` 才探,这次超时(`active_probe_timeout_sec`,默认 5s)+ 复核无进度就立刻 `hung=true`。而 hung 之后 kubelet 的 livenessProbe 只需 `2×5s` 就 Killing,下一轮 poll(5s)+ 下次探测(最多 5s)的结论恰好与 Killing 同时到达 —— 补救机会形同虚设。**任何一次偶发的 5s 超时(GC、瞬时抖动、一次偏慢的响应)都足以杀掉一台健康引擎。**

每轮都探之后,判死条件不用动就自带了「连续失败」语义:探测走 `/health_generate`,成功时会真跑一次 forward,而 `sglang:realtime_tokens_total` 是 metrics_reporter **每个 forward** 自增的,所以**成功的探测会推进 token_progress** → 下一轮进 `growing` → 停滞计时归零。于是 `stalledFor` 能累加到 `stall_sec`,**当且仅当这期间每次探测都失败**。按默认 30s/5s 算连败 4 次以上才判死,而**判死时机(30s)一点没变**。

- **开销**:有流量时 progress 本来就在涨,走不到探测分支,零新增。空闲引擎从约每 `stall_sec` 一次变成约每 2 个 poll 一次,单次是 1 token 的生成;且 `/health_generate` 在引擎有活干时会直接返回(判据是 `last_receive_tstamp`,detokenizer 最近有**任何**响应就算存活),可忽略。
- **仍未覆盖**:超长 prefill 期间 scheduler 卡在 forward 里,探测**每次都会超时**,多探不能把它和真 wedged 区分开。实测 160k+ prompt 的 TTFT p90 达 13.56s、单条 129k token 的 prefill 实测 14.7s。要覆盖需调大 `active_probe_timeout_sec` 或引入 prefill 阶段的独立信号。

### 与 monitor 的行为差异
- **engine liveness 耦合 sidecar 可用性**:engine 的 livenessProbe 探本 sidecar `:9090`,故 **sidecar 崩溃/OOM/慢启 → 探针连不上 → 超 `failureThreshold×period` 会把健康的 engine 也重启**。Go 静态二进制重启 <1s、实际内存 ~10Mi(`/metrics` 有 8MB 读上限但真实响应通常 <100KB),45s 宽限远够;但**别把 sidecar 内存 limit 压太死**(建议 ≥64Mi)。

## 配置(env + ConfigMap 热加载)
| 来源 | 项 | 默认 | 说明 |
|---|---|---|---|
| env | `ENGINE_URL` | `http://127.0.0.1:8050` | 本地引擎 |
| env | `LISTEN` | `:9090` | /healthz 监听 |
| env | `CONFIG_FILE` | `/etc/hang-watcher/config.json` | ConfigMap 挂载点 |
| **ConfigMap(热更)** | `poll_interval_sec` | 15 | 拉 /metrics 周期 |
| **ConfigMap(热更)** | `stall_sec` | 180 | 停滞判 hang 阈值(对齐 monitor GRACE) |
| **ConfigMap(热更)** | `metrics_timeout_sec` | 10 | /metrics 超时 |
| **ConfigMap(热更)** | `active_probe_enabled` | `false` | 开启「判 hang 前主动探测确认」 |
| **ConfigMap(热更)** | `active_probe_path` | `/health_generate` | 探测路径(vllm 可用 `/v1/completions`) |
| **ConfigMap(热更)** | `active_probe_method` | `GET` | `GET` / `POST` |
| **ConfigMap(热更)** | `active_probe_body` | `""` | POST 请求体(如 completions JSON);GET 留空 |
| **ConfigMap(热更)** | `active_probe_timeout_sec` | 20 | 主动探测超时(生成可能慢于抓 metrics) |

**调参 = 改 ConfigMap `kubectl apply`,sidecar 下轮读到,不用滚 pod**(mtime 变才重读)。只有改 sidecar **代码/镜像**才滚 pod。

## 部署(见 `deploy/`,均为参考草案)
- `configmap.yaml` —— 判活参数(每 ns 一份)。
- `lws-patch.yaml` —— LWS leader 加 startupProbe + liveness(探 sidecar)+ sidecar。**llm-lws 现在 liveness/startup 全无,最需要补。**
- `deployment-patch.yaml` —— Deployment 把 liveness 从 `/health_generate` 换成探 sidecar + 加 sidecar。

**关键**:livenessProbe 挂在 **engine 容器**(失败重启的是它),但 `httpGet` 指向 **sidecar 的 9090**(同 pod 共享 netns 直达);**startupProbe 必留**(gate 住模型加载,启动期零误杀)。

## 构建 / 测试
```bash
go test ./...                        # 判活状态机 + /metrics 解析单测
CGO_ENABLED=0 go build -o hang-watcher .
```
镜像走 CI 打 tag(纯 Go 无依赖,`harbor.4pd.io/hardcore-tech/hang-watcher:<tag>`)。

## LWS vs Deployment
| | liveness 失败后 | 额外配置 |
|---|---|---|
| **Deployment**(单 pod TP4) | 重启 engine 容器 | 无 |
| **LWS**(leader+worker) | 重启 leader → `RecreateGroupOnPodRestart` → 整组重建 | `RecreateGroupOnPodRestart`(llm-lws 已有) |

一套 sidecar + probe 两种部署通用,LWS 只多一个「把单 pod 重启放大成整组重建」的开关。
