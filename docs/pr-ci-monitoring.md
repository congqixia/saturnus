# PR / CI 监控与 check bypass

PR 模块独立保存 GitHub PR 和 CI 检查快照，不要求先创建 task 或运行 AI review。
`task_prs` 和 `review_requests` 通过 `repo + PR number` 与该模型关联；详情包含对应的
review ID 和 task IDs。CI 状态与 Saturnus review 的执行状态、LGTM/NIT/RC 结论分别保存。

## 启用和扫描范围

server 配置：

```ini
SATURNUS_PRS_ENABLED=1
SATURNUS_PRS_REFRESH_INTERVAL=5m
SATURNUS_PRS_REFRESH_TIMEOUT=2m
SATURNUS_PRS_AUTHOR=@me
SATURNUS_PRS_GH_CLI=gh
SATURNUS_PRS_BYPASS_FILE=/etc/saturnus/pr-check-bypass.json
```

| 配置 | 默认值 | 含义 |
| --- | --- | --- |
| `SATURNUS_PRS_ENABLED` | `1` | `0` 关闭 PR 监控模块；原 `/sat prs` 实时列表仍可使用 |
| `SATURNUS_PRS_REFRESH_INTERVAL` | `5m` | 后台扫描间隔；`0` 仅关闭后台扫描，保留手动刷新和查询 |
| `SATURNUS_PRS_REFRESH_TIMEOUT` | `2m` | 一轮发现/刷新总预算；单个 PR 最多 30 秒，最多并行刷新 3 个 |
| `SATURNUS_PRS_AUTHOR` | `@me` | 自动发现该 GitHub 用户的 open PR；默认使用 server 上 gh 登录的账号 |
| `SATURNUS_PRS_GH_CLI` | `gh` | server 上的 GitHub CLI 可执行文件 |
| `SATURNUS_PRS_BYPASS_FILE` | 空 | 可选 bypass JSON 文件；未配置表示没有 bypass |

仓库白名单复用 `SATURNUS_REVIEW_REPOS_FILE` / `SATURNUS_REVIEW_REPOS`。
CI 查询只需要 server 的 gh 和网络权限，不会操作 checkout；worker 上的 gh 登录信息不参与查询。
gh 需要支持 `api --paginate --slurp`。私有仓库需要读取 PR、Checks、Commit statuses、Actions 的权限。

启动后立即扫描，随后按间隔刷新。扫描对象包含：

- 白名单仓库中指定作者的所有 open PR，分页获取；不是界面显示的前 10 条。
- 已有关联 task 或 Saturnus review 的 PR。
- 通过 `/sat pr` 或 API 显式跟踪的 PR，可以来自其他作者。

每个 `repo + number` 只生成一个 `pull_requests` 记录。MERGED/CLOSED PR 停止定时刷新；
自动发现重新打开的 PR 后会恢复。暂停的 PR 不会因下一次发现而自动恢复。
查询失败不会把 PR 当成已关闭，也不会删除旧记录。

## 命令

```text
/sat prs
/sat prs refresh
/sat prs all
/sat pr https://github.com/milvus-io/milvus/pull/54733
/sat pr milvus-io/milvus#54733
/sat describe pr milvus-io/milvus#54733
/sat pr pause milvus-io/milvus#54733
/sat pr resume milvus-io/milvus#54733
```

- `/sat prs` 读取缓存，显示 CI、bypass 数量和最近完整检查时间。缓存为空时尝试发现和刷新。
- `/sat prs refresh` 显式触发完整扫描，显示部分失败信息；其他成功结果仍然保存。
- `/sat prs all` 包含已关闭的已跟踪 PR。聊天列表最多显示 10 条；监控和 API 不受此展示限制。
- `/sat pr`、`/sat describe pr` 显式跟踪/刷新一个 PR，并展示每项检查的真实状态、bypass 规则、
  实际被测 SHA、详情链接。也可使用列表中的 `pr_...` ID。
- `pause/resume` 只修改 Saturnus 的后台轮询开关。暂停时仍可手动刷新；resume 后由下一次扫描刷新，
  也可立即执行 `/sat pr <ref>`。

## 配置 bypass

文件示例见仓库根目录 [pr-check-bypass.example.json](../pr-check-bypass.example.json)。
以下示例演示全局规则和单仓库规则：

```json
{
  "global": [
    "informational-report"
  ],
  "repos": {
    "milvus-io/milvus": [
      "devops-v2-label-manager",
      "optional-coverage-report"
    ],
    "milvus-io/birdwatcher": [
      "optional-coverage-report"
    ]
  }
}
```

使用步骤：

1. 执行 `/sat pr <PR URL>`，复制详情表 **Check** 列中的完整名称。
2. 仅对一个仓库生效时，加入 `repos["owner/repo"]`；所有仓库都适用时才加入 `global`。
3. 在 server 的环境文件中设置 `SATURNUS_PRS_BYPASS_FILE`，重启 server。
4. 再次查询该 PR：对应项应显示 `YES (repo:...)` 或 `YES (global)`，列表的 Bypass 计数增加。

匹配规则：

- 名称**区分大小写、完整精确匹配**；支持名称中包含空格，不使用 CSV 拆分。
- 不支持 glob、正则或子串匹配。`build*` 只会匹配字面名称 `build*`。
- 普通检查匹配 Check Run 的 `name` 或 Commit Status 的 `context`。
  若要 bypass 某个 workflow 中的多个 job，需要分别列出它们的 check 名称。
  未生成 job 或有未被 job 表达的状态时，会出现 `workflow_run` 行，此类行使用 workflow 名称匹配。
  以详情的 **Source** 和 **Check** 列为准；同名检查会同时命中，不能按 workflow 单独限定规则。
- 仓库名不区分大小写；单仓库规则与 global 取并集，命中单仓库规则时优先显示它的来源。
- 文件不存在、不是 JSON 对象、有未知字段、名称为空/带首尾空格、同一列表重复名称，都会阻止 server 启动。
- 文件只在启动时加载。改名或删除规则后重启生效；读取缓存时会按新规则重新计算，通常不必等待下一次 GitHub 请求。

Bypass **只影响 Saturnus 的 CI 聚合**，不修改 GitHub 检查状态、branch protection 或合并权限。
被 bypass 的 FAIL/PENDING/CANCEL 仍会在详情中展示真实状态和链接，但不阻塞有效 CI 结果。

例如：

| Check | GitHub 真实结果 | Bypass | 对聚合的影响 |
| --- | --- | --- | --- |
| `build` | success | no | 计入 PASS |
| `optional-coverage-report` | failure | YES | 不计入失败；详情仍显示 failure |

该例整体为 `PASS`；如果 `build` 也失败，整体仍为 `FAIL`。全部检查均被 bypass 时显示
`BYPASS`，不会伪装为 PASS。没有检查时显示 `NO_CHECKS`。

## 采集和一致性

采集通过 server 上的 `gh api --method GET` 读取 REST API：PR 元数据、分页的 Check Runs、
Commit Statuses 和 Actions Workflow Runs。GitHub Actions 的 job 检查和 Jenkins 等向 GitHub 上报的 pipeline 都可覆盖。
没有向 GitHub 上报结果的外部 pipeline 不会出现在此列表中；外部系统的详细日志仍需访问详情链接。

Check Run 按 app 和检查名去重；Actions job 额外区分 workflow ID 和触发事件，
Commit Status 按 context 去重。采集全部 check runs 后在本地选择最新结果；
Actions 使用同一 SHA、workflow 和事件的最新 run，包含已从新 run 删除的旧 job 也会被替换。
active test-merge 提交存在时也会采集其检查；同名检查优先采用 test-merge 结果，
head 上独有的检查保留。每项都保存实际被测 SHA。

Actions 检查同时显示所属 workflow。没有生成 job 的 workflow 启动失败仍会以 `workflow_run` 行显示。
workflow 的状态已由 job 表达时不重复计入父级结果，避免 optional job 被 bypass 后，其失败又通过父级阻塞聚合。

刷新结束前再次读取 PR 元数据；head、test-merge SHA 或 PR 生命周期在采集中变化时，不发布该次 CI 结果。
任一来源报错、超时或返回不完整数据时，整体为 `UNKNOWN`，并保留上次完整检查列表及其 SHA、时间。
不会将缺少数据或 API 错误当作 PASS。两类记录在同一 SQLite 事务中保存；同一 PR 的刷新和暂停操作串行化。

有效检查的聚合顺序：`FAIL → UNKNOWN → PENDING → CANCEL → PASS → SKIP`。
`skipped/neutral` 属于 SKIP；cancelled 独立显示为 CANCEL；全部 bypass 则为 BYPASS。
UI/API 同时显示同步错误与最后完整同步时间，以区分缓存、未完成采集和新的 CI 结论。

本阶段提供状态采集、持久化、查询和定时刷新，不新增飞书失败消息推送。

## HTTP API

```text
GET   /api/prs                 # 所有已跟踪的 OPEN/UNKNOWN PR
GET   /api/prs?all=1           # 包含 CLOSED/MERGED
GET   /api/prs/{pr_id}         # pr、checks、checks_stale、review_id、task_ids
POST  /api/prs                 # {"url":"owner/repo#123"}，跟踪并刷新
POST  /api/prs/{pr_id}/refresh
PATCH /api/prs/{pr_id}         # {"monitoring":false} 或 true
```

列表每项包含 `ci_state`、`counts`、`head_sha`、`checked_head_sha`、`monitoring`、同步时间和错误。
详情每项 check 包含 `source/name/external_id/head_sha/status/conclusion/bucket/url`，以及
`bypassed/bypass_rule`；Actions 检查还包含 `workflow/workflow_id/workflow_run_id/run_attempt/event`。
原始 `bucket` 不因 bypass 改写。

刷新请求在 GitHub 采集失败但能保存诊断快照时仍返回该快照，HTTP 200；调用方必须检查
`pr.sync_error`、`pr.ci_state` 和 `checks_stale`，不能只根据 HTTP 状态认定 CI 通过。

GitHub API 参考：[Check Runs](https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference)、
[Workflow Runs](https://docs.github.com/en/rest/actions/workflow-runs#list-workflow-runs-for-a-repository)。
