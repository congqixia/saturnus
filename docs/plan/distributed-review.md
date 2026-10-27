# 分布式 Review Worker 设计方案（WebSocket 控制通道）

## 1. 现状梳理

当前架构是**单体执行**：

- `cmd/saturnus-server/main.go` 是唯一大脑，HTTP API 与 Review 执行器同进程。
- `SATURNUS_REVIEW_REPOS='owner/repo=/local/path'` 把 repo 映射到 **server 本机**的 checkout。
- `internal/review/service.go:1284 process()` 在 worker 池里：`gh` 解析 PR → `RenderCommand` → `review.Run()`（`executor.go:145` 起子进程，cwd=本机 checkout）→ 存结果 → 通知 reviewer。
- Tool 通过 `ResultAPIURL`（含 `{review_id}` 占位）直接 POST 结构化结果到 `POST /api/reviews/{id}/result`（`main.go:362`，已有 Bearer token 鉴权）。
- 已存在的 `/api/agents/register` + `/heartbeat`（`store.go:683/768`）目前只服务 Codex 会话/审批，**不承担 review 执行**。

关键约束：`session_id` 是**主机本地**的（`captureSessionID` 在 checkout 里跑 `opencode session list`），重试依赖同一主机上的会话文件。

## 2. 目标

- 多个宿主机各自持有 repo checkout 和 review 工具，作为 **review worker** 提供执行能力。
- worker 在中心 saturnus（控制面）上注册，server 保持调度、状态、通知、评论发布等职责不变。
- 本地模式完全保留（server 自己注册为 local worker）。
- 控制通道采用**单条出站 WebSocket 长连接**，替代 pull 轮询，实现准实时派发与双向控制。

## 3. 总体架构：Master-Worker + WebSocket 控制通道

```
 ┌──────────────────────── saturnus server (控制面) ────────────────────────┐
 │ HTTP API / lark / 存储 / 调度器 / 通知 / gh 评论发布 / Feishu task 同步  │
 │ workers 注册表(repo→worker) │ 调度(原子状态迁移) │ 对账/孤儿回收 │ 鉴权  │
 │                                   ▲                                     │
 │                           WS 连接管理器(连接表/广播/取消)                 │
 └───────────────────────────────────┼─────────────────────────────────────┘
                                      │ 单条出站 WS (wss)
   ┌──────────────────┐   ┌───────────┼────────────┐   ┌──────────────────┐
   │ host A worker    │   │ host B worker            │   │ host C worker   │
   │ repo1 checkout   │   │ repo2 checkout           │   │ repo1 checkout  │
   │ opencode/gh      │   │ opencode/gh              │   │ opencode/gh     │
   └──────────────────┘   └──────────────────────────┘   └──────────────────┘
```

**选型理由**：

- **长连接选 WebSocket**（`coder/websocket`，server 与 worker 同用）：真双向——既要 server 推 `claim`/`cancel`，又要 worker 回传 heartbeat/progress/complete；一条出站长连接即满足；作为 `net/http` 的 upgrade 路由可无缝复用鉴权/CORS 中间件；NAT 友好（worker 只出站）。
- **不选 gRPC**：protobuf 契约 + keepalive/流控确实更严谨，但本项目零 codegen 工具链，控制面消息又是小 JSON，引入重型协议属过度设计。若未来要标准化契约、worker 规模大，再迁移。
- **不选 MQ/NATS/Redis**：为一个小控制面引入外部中间件增加运维负担。

## 4. 核心设计

### 4.1 Worker 注册

新二进制 `cmd/saturnus-worker`（守护进程），启动后：

- `POST /api/workers/register`，携带：
  - `worker_id`（默认 `hostname` 派生，如 `wrk_...`）
  - `host`、`version`、`tool`（如 `opencode`）、`tool_version`
  - **capabilities**：`repos`（`owner/repo→/local/path`，即 worker 本机的 `SATURNUS_REVIEW_REPOS`）、`gh` 是否可用、`max_concurrent`
  - 鉴权：携带注册密钥 `SATURNUS_WORKER_REGISTER_KEY`，server 校验后为该 worker 下发/确认 `worker_token`。
- 注册成功后建立 WS 连接（`wss://`）。

server 维护 `repo → [在线 worker]` 聚合索引，由注册事件增量维护（内存 + 落库）。

### 4.2 调度与亲和

- `submit()` 里的白名单校验（`service.go:431 repoDirFor`）改为查**聚合注册表**：至少一台在线 worker 服务该 repo 才接受；否则拒绝（"no worker currently serves repo X"）。server 本机不再需要 checkout。
- 调度器（server 内新增 scheduler，取代本地 worker 池）选择 worker：
  1. **repo 亲和**：只选 capabilities 覆盖该 repo 的在线 worker；
  2. **session 亲和**（见 4.4）：retry/reaction 必须优先回到 `review_runs.session_id` 对应的原 worker；
  3. **负载均衡**：选 in-flight 少于 `max_concurrent` 的 worker。

### 4.3 WebSocket 控制协议

```
worker (每台一个守护进程)
  └── 单条出站 WS 连接 → saturnus server (wss)
        ├─ 派发: server→worker  claim / cancel / ping
        └─ 上报: worker→server  hello / heartbeat / progress / complete / cancel_ack / error
REST 保留: 注册握手发 token / 外部 tool 的结果 POST / WS 被阻断时的 fallback
```

#### Server → Worker（Command）

| type | 方向语义 | 载荷 |
|---|---|---|
| `claim` | 派发一个 job | 完整 `JobPayload` |
| `cancel` | 取消正在跑的 job | `{review_id, reason}`（超时/归档/人工重派） |
| `ping` | 探活（配合 WS 层 ping 帧） | 空 |

#### Worker → Server（Event）

| type | 载荷 |
|---|---|
| `hello` | `{worker_id, token, in_flight:[review_id...]}` — 连接建立时对账 |
| `heartbeat` | `{load, in_flight_count, repos_ok}` — 或仅用 WS ping 帧 |
| `progress` | `{review_id, text}` — 每 ~5s，落 `result_text` |
| `complete` | `{review_id, ok, exit_code, session_id, error, verdict?, summary?}` |
| `cancel_ack` | `{review_id}` — 已终止本地子进程（复用 `setupProcessGroup` 杀整树） |
| `error` | `{message}` — 协议/本地环境错误 |

#### JobPayload

```
review_id, repo, pr_number, pr_url, title, base_branch, head_commit,
retry_reason, session_id(若有), prior_findings,
command_template(server 下发的模板，单一真源),
tool_args(Worktree 除外，由 worker 填本机 checkout 路径),
guide_content(server 解析显式 SATURNUS_REVIEW_GUIDES 后下发；in-repo 的由 worker 本地发现),
result_api_url(server 内网可达地址，含 {review_id}), result_token,
timeout, log_upload_url
```

#### 派发流程（push，server 单点决策）

1. server 提交/重试时：调度器选 worker（repo 亲和 → session 亲和 → 负载），**原子执行 `pending→reviewing + worker_id`**（没有多 worker 竞争，状态迁移是 server 自己完成）。
2. server 向目标 worker 发 `claim` 帧。worker 收到后：校验本机 checkout → 渲染命令（复用 `RenderCommand`，`Worktree` 用自己的 checkout）→ `review.Run()` 执行 → 捕获 `session_id`。
3. worker 若已达 `max_concurrent`，`claim` 帧返回 `error{code:"busy"}`，server 将该 job 重新入队给其它 worker（原子回滚 `reviewing→pending`）。

#### 连接生命周期与对账

- **建连**：worker `POST /api/workers/register` 握手拿 `worker_token`（首次），然后以 token 建立 WS；连接建立即发 `hello`。
- **对账（重连）**：worker 重连时 `hello.in_flight` 上报本地还在跑的 job；server 与 `review_runs.worker_id + status=reviewing` 交叉核对——一致的保留，不一致（server 侧已被孤儿回收）的 worker 收到 `cancel`。
- **租约**：`progress` 即续租；server 按"worker 离线 / 超过 timeout 无 progress"判定孤儿 → 原子 `reviewing→pending` 重排。
- **双向保活**：WS 应用层 `ping` + worker 侧自动重连（指数退避），重连后走 hello 对账。

### 4.4 会话亲和性（关键点）

`opencode` 会话文件存在 worker 本机。因此：

- **同一次 review 的所有 run（含 retry）必须落在同一 worker**，否则无法 `--session` 续聊。
- `review_runs.worker_id` 作为亲和依据；调度器把 retry/reaction 强制路由回原 worker。
- 若原 worker 永久离线，降级为**全新 run**：清空 `session_id`、保留 `prior_findings`（复用现有 SaveFindings upsert 语义），并向 reviewer 说明续聊不可用。

### 4.5 进度与结果回传

- **进度**：worker 每 ~5s 通过 WS `progress` 帧上报，server 落 `result_text`（复用 `UpdateReviewProgress` 语义）。
- **结果**：优先保持现有机制——tool 直接 POST 到 `result_api_url`（worker 主机可达 server 即可），复用 `submitReviewResult` 的校验/落库/去重逻辑，**零改动**。若 worker 内网无法直达 server，fallback：worker 解析 tool 输出后统一 `POST /api/reviews/{id}/result`。
- **完成**：worker 发 WS `complete` 帧。server 只在此之后做 `finish` 收尾：写 `review_runs` 终态、发布 inline comments（用 **server 本机 gh 凭据**，与现状一致）、通知 reviewer、Feishu task 同步。

> 评论发布/任务同步留在 server 侧，因为 gh 凭据与 Lark 集成都集中在这。

### 4.6 失败恢复（孤儿回收）

现有 `FailInterruptedReviews`（`store.go:1537`）在 server 重启时把 `reviewing` 全标 failed。分布式下要**区分**：

- server 重启：查 `review_requests.worker_id`，把对应 worker 仍在线 的 run 保留为 `reviewing`（worker 会通过 WS 续报进度/完成）；真正失联的才回收。
- worker 心跳超时（离线/崩溃）：该 worker 的 `reviewing` job 回收为 `pending` 重新入队（session 亲和失效则按 4.4 降级）。用 **claim 租约 + progress 续租** 实现，避免双执行（worker 收到 claim 但 server 已超时回收 → worker 完成时 server 以 409 幂等拒绝，worker 丢弃结果即可）。

### 4.7 鉴权与安全

- 注册：`SATURNUS_WORKER_REGISTER_KEY`（共享密钥）→ server 返回/确认每 worker `worker_token`。
- WS upgrade 握手校验 `worker_token`（`Sec-WebSocket-Protocol` 或 query 携带），与 REST 共享 token 校验逻辑。
- `submitReviewResult` 沿用现有 `SATURNUS_REVIEW_RESULT_TOKEN`。
- 生产建议 worker↔server 走 `wss://`（反代/VPN 终结 TLS）；worker 不持有任何 Lark/GitHub 全局凭据，只需本机 `gh`/tool。

## 5. 数据模型变更

复用并扩展现有表，最小化迁移：

- **agents**（`store.go:20`）加列：`role TEXT DEFAULT ''`（`review-worker`/`codex`）、`capabilities TEXT`（JSON：repos/tool/max_concurrent/gh）、`worker_token TEXT`、`last_seen` 已有 → 在线判定。
- **review_requests** 加列：`worker_id TEXT DEFAULT ''`（当前分配的 worker）。
- **review_runs**（`store.go:100`）加列：`worker_id TEXT DEFAULT ''`（该 run 的执行者，traceability + 亲和依据）。`session_id` 已在。
- 新表可选：`worker_events`（上下线审计）或复用 `audit_logs`。
- `in_flight` 不需要落库：连接内存态 + `review_runs.status=reviewing` 即对账依据。

## 6. 关键代码改动

| 位置 | 改动 |
|---|---|
| `internal/review/executor.go` | 抽出可复用的 `RunJob(ctx, JobPayload, localCheckout, progressSink)`，`RenderCommand` 保留 |
| 新 `internal/rpc/workerws.go` | WS 帧编解码 + 连接管理器（连接表、自动重连、对账、broadcast/cancel） |
| 新 `internal/reviewworker/` | worker 守护循环：register→WS 建连→hello→执行→progress/complete；供 `cmd/saturnus-worker` 与 server 本地模式共用 |
| `internal/review/service.go` `process()` | 拆出「纯执行」与「状态/通知」；执行部分委托给 worker/本地 worker |
| `internal/review/service.go` `submit/retry` | 白名单校验改查 worker 注册表；入队改调度器 |
| `cmd/saturnus-server/main.go` | 新增 `/api/workers/register` 与 `GET /api/workers/ws` upgrade 路由；scheduler 取代本地 worker 池 |
| `internal/store/store.go` | worker 表列、`ClaimReview` 原子状态迁移、worker 感知的 `FailInterruptedReviews` |
| 新 `cmd/saturnus-worker/main.go` | 守护进程入口 |

## 7. 兼容与迁移

- `SATURNUS_REVIEW_ENABLED=1` 且**未配置任何远程 worker** 时，server 注册自身为 local worker（repos=本机 `SATURNUS_REVIEW_REPOS`），行为与今天**完全一致**。
- 逐步迁移：先让新主机作为额外 worker 上线（repo 不在 server 本机），server 本机 worker 可逐步下线。
- WS 连接不稳定时，worker 可降级回 REST long-poll（fallback 复用同一套调度器与 store 状态机）。

## 8. 可选演进（本期可不做）

- worker 能力 tag/调度策略（如按 repo 分区、专机跑重 repo）。
- gRPC bidi stream 迁移：若未来要严格契约/流控/大规模，`workerws.go` 的帧协议可映射为 protobuf，REST 与 WS 入口已隔离。
- 任务看板：web UI `/sat workers` 展示各主机在线状态、负载、服务 repo、执行历史。
