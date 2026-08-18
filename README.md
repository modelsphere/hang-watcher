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

## 配置(env + ConfigMap 热加载)
| 来源 | 项 | 默认 | 说明 |
|---|---|---|---|
| env | `ENGINE_URL` | `http://127.0.0.1:8050` | 本地引擎 |
| env | `LISTEN` | `:9090` | /healthz 监听 |
| env | `CONFIG_FILE` | `/etc/hang-watcher/config.json` | ConfigMap 挂载点 |
| **ConfigMap(热更)** | `poll_interval_sec` | 15 | 拉 /metrics 周期 |
| **ConfigMap(热更)** | `stall_sec` | 180 | 停滞判 hang 阈值(对齐 monitor GRACE) |
| **ConfigMap(热更)** | `metrics_timeout_sec` | 10 | /metrics 超时 |

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
