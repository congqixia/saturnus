# Saturnus Linux systemd 部署指南

本文将 Saturnus 部署为 Linux 本地服务，覆盖编译、安装、SQLite、Lark bot 配置、Lark bridge、systemd unit、启动验证、升级和故障排查。

假设：

- Linux 使用 systemd
- server 监听 127.0.0.1:8787
- SQLite 位于 /var/lib/saturnus/saturnus.db
- bridge 使用 bot 身份消费 im.message.receive_v1
- server 启动时由 systemd 自动拉起 bridge

## 1. 运行结构

~~~text
saturnus-server.service
  ├── HTTP API / Web UI
  ├── SQLite
  └── Wants → saturnus-lark-bridge.service
                  └── lark-cli event consume → POST /lark/events
~~~

saturnus-agent codex-hook 是 Codex 按需启动的一次性 hook 进程，不作为常驻服务。

## 2. 安装目录

~~~text
/usr/local/bin/saturnus-server
/usr/local/bin/saturnus-agent
/usr/local/bin/lark-cli
/usr/local/libexec/saturnus-codex-hook

/usr/share/saturnus/web/
/var/lib/saturnus/saturnus.db
/etc/saturnus/server.env
/etc/saturnus/bridge.env
/etc/systemd/system/saturnus-server.service
/etc/systemd/system/saturnus-lark-bridge.service
~~~

不要直接从 Git 工作区运行生产服务，避免相对路径和运行版本不可控。

## 3. 编译依赖

Debian/Ubuntu：

~~~sh
sudo apt-get update
sudo apt-get install -y build-essential pkg-config ca-certificates curl git nodejs npm
~~~

检查：

~~~sh
go version
node --version
npm --version
gcc --version
~~~

项目使用 go-sqlite3，依赖 CGO；应在目标 Linux 或 ABI 兼容环境编译。

## 4. 编译

在仓库根目录执行：

~~~sh
git status --short

cd web
npm ci
npm run build
cd ..

CGO_ENABLED=1 make build
make test
~~~

产物：

~~~text
bin/saturnus-server
bin/saturnus-agent
web/dist/
~~~

## 5. 创建运行用户和目录

~~~sh
sudo useradd \
  --system \
  --home /var/lib/saturnus \
  --create-home \
  --shell /usr/sbin/nologin \
  saturnus
~~~

用户已存在时跳过 useradd。

~~~sh
sudo install -d -o saturnus -g saturnus -m 0750 /var/lib/saturnus
sudo install -d -o root -g saturnus -m 0750 /etc/saturnus
sudo install -d -o root -g root -m 0755 /usr/share/saturnus/web
sudo install -d -o root -g root -m 0755 /usr/local/libexec

sudo install -m 0755 bin/saturnus-server /usr/local/bin/saturnus-server
sudo install -m 0755 bin/saturnus-agent /usr/local/bin/saturnus-agent
sudo cp -a web/dist/. /usr/share/saturnus/web/
~~~

## 6. 安装 lark-cli

systemd 不会加载交互式 shell 的 PATH、.bashrc、.zshrc 或 nvm 环境。`saturnus` 运行用户不需要安装 npm 或 nvm；bridge 只需要一个位于系统目录中的 lark-cli 原生二进制。

不要直接创建下面的软链接：

~~~sh
sudo ln -s \
  /home/silverxia/.nvm/versions/node/v20.15.1/bin/lark-cli \
  /usr/local/bin/lark-cli
~~~

这个 nvm 路径中的 `lark-cli` 本身只是指向 `scripts/run.js` 的链接，启动脚本使用 `#!/usr/bin/env node`：

- systemd 默认 PATH 找不到 nvm 中的 node
- bridge unit 的 `ProtectHome=true` 会阻止服务读取 `/home/silverxia`
- silverxia 更新或删除 nvm 版本会破坏服务
- 系统服务不应执行另一个普通用户可修改的程序

当前 npm 包已经下载了静态链接的 Linux 原生二进制。可以将它复制为 root 管理的系统命令；这里复制的是 `lib/node_modules/.../bin/lark-cli`，不是 nvm 的 Node 启动脚本：

~~~sh
file \
  /home/silverxia/.nvm/versions/node/v20.15.1/lib/node_modules/@larksuite/cli/bin/lark-cli

sudo install -o root -g root -m 0755 \
  /home/silverxia/.nvm/versions/node/v20.15.1/lib/node_modules/@larksuite/cli/bin/lark-cli \
  /usr/local/bin/lark-cli
~~~

该文件应显示为 Linux ELF executable；当前本机版本是 1.0.86。复制后运行时不再依赖 Node、npm、nvm 或 `/home/silverxia`。

验证：

~~~sh
file /usr/local/bin/lark-cli
/usr/local/bin/lark-cli --version

sudo -u saturnus -H \
  env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli --version
~~~

在全新机器上，可以先通过官方 npm 安装器下载对应平台的原生二进制，再把包内的 `bin/lark-cli` 安装到 `/usr/local/bin`。npm 只在安装阶段使用，不要让 systemd unit 依赖 npm 的 launcher。升级 lark-cli 时重复安装和验证步骤，然后重启 bridge：

~~~sh
sudo systemctl restart saturnus-lark-bridge.service
~~~

如果只是临时验证，也可以让 unit 把 nvm 的 bin 目录加入 PATH，并将 `ProtectHome` 调整为 `read-only`；这种方式会让生产服务依赖 silverxia 的 HOME，不建议作为正式部署方案。

## 7. 配置 Lark bot

server 环境变量：

~~~text
LARK_APP_ID
LARK_APP_SECRET
LARK_NOTIFY_CHAT_ID
~~~

创建 /etc/saturnus/server.env：

~~~ini
LARK_APP_ID=cli_xxx
LARK_APP_SECRET=xxx
LARK_NOTIFY_CHAT_ID=oc_xxx
~~~

不要写 export，不要提交到 Git。

~~~sh
sudo chown root:saturnus /etc/saturnus/server.env
sudo chmod 0640 /etc/saturnus/server.env
~~~

### 7.1 将已有 App 凭证写入 lark-cli

`lark-cli config init --new` 是浏览器引导流程，用于创建/配置应用，不是读取环境变量的验证命令。对于已经在 `/etc/saturnus/server.env` 中准备好的 App ID/Secret，应使用 `config init --app-id --app-secret-stdin` 将凭证一次性写入 `saturnus` 用户自己的配置目录。

注意：lark-cli 的标准本地配置流程不会自动读取 Saturnus 的 `LARK_APP_ID` / `LARK_APP_SECRET`。不要把 Secret 放进命令行参数或进程列表；通过 stdin 传入。

在 root shell 中加载受保护的环境文件，再将 Secret 通过管道交给 `saturnus` 用户：

~~~sh
. /etc/saturnus/server.env

printf '%s\n' "$LARK_APP_SECRET" | \
sudo -u saturnus -H \
  env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli config init \
    --app-id "$LARK_APP_ID" \
    --app-secret-stdin \
    --brand feishu

unset LARK_APP_ID LARK_APP_SECRET LARK_NOTIFY_CHAT_ID
~~~

这一步不是 `config init --new`：`--new` 只在需要通过浏览器创建新应用时使用。已有飞书应用应使用上面的非交互参数。

成功后，配置位于 `/var/lib/saturnus/.lark-cli/config.json`，Secret 由 lark-cli 加密保存；`config show` 只显示掩码值：

~~~sh
sudo -u saturnus -H \
  env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli config show
~~~

### 7.2 分层验证

第一层：只验证本地配置，不访问网络：

~~~sh
sudo -u saturnus -H \
  env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli doctor --offline
~~~

应看到 `"ok": true`，并且 `config_file`、`app_resolved`、`bot_identity` 均为 `pass`。

第二层：验证真实 App ID/Secret 能获取 bot tenant token：

~~~sh
sudo -u saturnus -H \
  env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli auth status --json --verify
~~~

成功时应有 `identity: "bot"`、`identities.bot.verified: true`。如果出现 `invalid_client`，检查 App ID/Secret；如果出现 `missing_scope`，将错误中的 `console_url` 原样打开，并在飞书开发者后台为 bot 添加权限。bot 不需要执行 `lark-cli auth login`。

第三层：验证事件订阅的前置条件：

~~~sh
sudo -u saturnus -H \
  env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli event consume \
  im.message.receive_v1 --as bot --dry-run
~~~

检查返回 JSON 中 `ok` 为 `true`，且 `data.decision.status` 不是 `blocked`；同时确认 `data.decision.preconditions` 的 `credentials_available`、`console_event_published`、`scopes_granted` 均不为 `blocked`。注意：某些版本即使前置条件被阻断，dry-run 顶层仍可能返回 `ok: true`，因此不能只检查顶层 `ok`。`dry-run` 不会启动常驻消费；实际消费由 bridge unit 执行。

## 8. Bridge 环境变量

创建 /etc/saturnus/bridge.env：

~~~ini
HOME=/var/lib/saturnus
SATURNUS_SERVER=http://127.0.0.1:8787
SATURNUS_LARK_CLI=/usr/local/bin/lark-cli
SATURNUS_LARK_EVENT_KEY=im.message.receive_v1
SATURNUS_LARK_AS=bot
SATURNUS_LARK_READY_TIMEOUT=30s
~~~

常驻 bridge 不要在 bridge.env 中添加以下变量，否则 consumer 会按事件数或时间自动退出：

~~~text
SATURNUS_LARK_MAX_EVENTS
SATURNUS_LARK_CONSUME_TIMEOUT
~~~

~~~sh
sudo chown root:saturnus /etc/saturnus/bridge.env
sudo chmod 0640 /etc/saturnus/bridge.env
~~~

## 9. Server unit

创建 /etc/systemd/system/saturnus-server.service：

~~~ini
[Unit]
Description=Saturnus AI CLI Control Plane
Wants=network-online.target saturnus-lark-bridge.service
After=network-online.target

[Service]
Type=simple
User=saturnus
Group=saturnus
WorkingDirectory=/var/lib/saturnus
EnvironmentFile=-/etc/saturnus/server.env

ExecStart=/usr/local/bin/saturnus-server \
    -addr 127.0.0.1:8787 \
    -data /var/lib/saturnus/saturnus.db \
    -static /usr/share/saturnus/web

Restart=on-failure
RestartSec=3
TimeoutStopSec=20
KillSignal=SIGTERM
StandardOutput=journal
StandardError=journal

NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ReadWritePaths=/var/lib/saturnus

[Install]
WantedBy=multi-user.target
~~~

server 当前支持 SIGINT/SIGTERM、15 秒 graceful shutdown、超时强制关闭和 SQLite 关闭，因此 TimeoutStopSec 应大于 15 秒。

## 10. Bridge unit

创建 /etc/systemd/system/saturnus-lark-bridge.service：

~~~ini
[Unit]
Description=Saturnus Lark Event Bridge
BindsTo=saturnus-server.service
After=saturnus-server.service network-online.target

[Service]
Type=simple
User=saturnus
Group=saturnus
WorkingDirectory=/var/lib/saturnus
EnvironmentFile=/etc/saturnus/bridge.env

ExecStartPre=/usr/bin/test -x /usr/local/bin/lark-cli
ExecStartPre=/usr/bin/test -x /usr/local/bin/saturnus-agent
ExecStart=/usr/local/bin/saturnus-agent lark-bridge

Restart=on-failure
RestartSec=5
TimeoutStopSec=30
KillSignal=SIGTERM
KillMode=control-group
StandardOutput=journal
StandardError=journal

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ReadWritePaths=/var/lib/saturnus
~~~

bridge 不需要 Install 段，也不需要单独 enable；它由 server 的 Wants 自动拉起。

lark-cli event consume 无限模式不能接受 stdin EOF，否则会退出。当前 bridge 使用 io.Pipe 保持 stdin 存活。停止时应使用 SIGTERM，不要 kill -9。

## 11. 启用和启动

~~~sh
sudo systemctl daemon-reload
sudo systemctl enable saturnus-server.service
sudo systemctl start saturnus-server.service
~~~

检查：

~~~sh
sudo systemctl status saturnus-server.service
sudo systemctl status saturnus-lark-bridge.service
sudo systemctl list-dependencies saturnus-server.service
~~~

## 12. 启动验证

server：

~~~sh
curl -fsS http://127.0.0.1:8787/api/health
~~~

预期：

~~~json
{"ok":true,"time":"..."}
~~~

bridge：

~~~sh
sudo journalctl -u saturnus-lark-bridge.service -n 100 --no-pager
~~~

应看到：

~~~text
[event] ready event_key=im.message.receive_v1
~~~

Lark event bus：

~~~sh
sudo -u saturnus -H \
  env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli event status --json --fail-on-orphan
~~~

Web UI：

~~~text
http://127.0.0.1:8787
~~~

## 13. 启停行为

~~~sh
sudo systemctl start saturnus-server
sudo systemctl restart saturnus-server
sudo systemctl stop saturnus-server
sudo systemctl restart saturnus-lark-bridge
~~~

- 启动 server 会自动启动 bridge
- 重启或停止 server 时 bridge 会随 BindsTo 停止
- server 重启完成后，Wants 会再次拉起 bridge
- 单独重启 bridge 不会停止 server

日志：

~~~sh
sudo journalctl -u saturnus-server.service -f
sudo journalctl -u saturnus-lark-bridge.service -f
~~~

## 14. Codex hook 稳定路径

Codex hook 不需要常驻 agent。安装 wrapper：

~~~sh
sudo tee /usr/local/libexec/saturnus-codex-hook >/dev/null <<'EOF'
#!/bin/sh
set -eu

export SATURNUS_SERVER="${SATURNUS_SERVER:-http://127.0.0.1:8787}"
export SATURNUS_CODEX_APPROVAL_WAIT="${SATURNUS_CODEX_APPROVAL_WAIT:-9m}"

exec /usr/local/bin/saturnus-agent codex-hook
EOF

sudo chmod 0755 /usr/local/libexec/saturnus-codex-hook
~~~

将 Codex hook command 改为：

~~~json
{
  "type": "command",
  "command": "/usr/local/libexec/saturnus-codex-hook",
  "timeout": 600,
  "statusMessage": "Waiting for Saturnus approval"
}
~~~

修改 hook 后，在 Codex 的 /hooks 中重新 review/trust。

## 15. 升级

备份数据库：

~~~sh
sudo cp -a \
  /var/lib/saturnus/saturnus.db \
  /var/lib/saturnus/saturnus.db.backup.$(date +%Y%m%d%H%M%S)
~~~

重新构建：

~~~sh
cd web
npm ci
npm run build
cd ..
CGO_ENABLED=1 make build
~~~

停止、替换、启动：

~~~sh
sudo systemctl stop saturnus-server.service

sudo install -m 0755 bin/saturnus-server /usr/local/bin/saturnus-server
sudo install -m 0755 bin/saturnus-agent /usr/local/bin/saturnus-agent

sudo rm -rf /usr/share/saturnus/web/*
sudo cp -a web/dist/. /usr/share/saturnus/web/

sudo systemctl start saturnus-server.service
~~~

确认：

~~~sh
sudo systemctl is-active saturnus-server.service
sudo systemctl is-active saturnus-lark-bridge.service
~~~

不要随意删除或回滚 SQLite；先确认旧版本 schema 兼容性。

## 16. 常见故障

### server 启动失败

~~~sh
sudo systemctl status saturnus-server.service
sudo journalctl -u saturnus-server.service -n 100 --no-pager
sudo -u saturnus test -w /var/lib/saturnus
test -f /usr/share/saturnus/web/index.html
ss -ltnp | grep 8787
~~~

常见原因：8787 被占用、数据库目录不可写、静态目录不存在、CGO/SQLite 构建失败。

### bridge 启动失败

~~~sh
sudo journalctl -u saturnus-lark-bridge.service -n 100 --no-pager
sudo -u saturnus -H /usr/local/bin/lark-cli --version
sudo -u saturnus -H env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli event consume \
  im.message.receive_v1 --as bot --dry-run
~~~

常见原因：lark-cli 路径错误、HOME 错误、bot 配置未初始化、应用缺少权限、同一 EventKey 已有其他 consumer。

### bridge 不断重启

确认 bridge.env 没有设置：

~~~ini
SATURNUS_LARK_MAX_EVENTS
SATURNUS_LARK_CONSUME_TIMEOUT
~~~

查看：

~~~sh
sudo systemctl show saturnus-lark-bridge.service --property=Environment
~~~

## 17. 安全注意事项

当前部署适合本机使用：

- server 监听 127.0.0.1
- 不要直接暴露 8787 到公网
- API 当前没有完整认证
- CORS 配置较宽松
- 公网 Lark webhook 需要 HTTPS 和验签

远程访问可使用 SSH tunnel：

~~~sh
ssh -N -L 8787:127.0.0.1:8787 user@server
~~~

环境文件权限：

~~~text
/etc/saturnus/server.env 0640 root:saturnus
/etc/saturnus/bridge.env 0640 root:saturnus
~~~

不要将 app secret、token 或 saturnus.db 提交到 Git。

## 18. 最小上线检查清单

~~~sh
test -x /usr/local/bin/saturnus-server
test -x /usr/local/bin/saturnus-agent
test -f /usr/share/saturnus/web/index.html

sudo -u saturnus -H /usr/local/bin/lark-cli --version
sudo -u saturnus -H env HOME=/var/lib/saturnus \
  /usr/local/bin/lark-cli event consume \
  im.message.receive_v1 --as bot --dry-run

sudo systemctl daemon-reload
sudo systemctl enable saturnus-server.service
sudo systemctl start saturnus-server.service

curl -fsS http://127.0.0.1:8787/api/health
sudo systemctl is-active saturnus-server.service
sudo systemctl is-active saturnus-lark-bridge.service
sudo journalctl -u saturnus-server.service -n 50 --no-pager
sudo journalctl -u saturnus-lark-bridge.service -n 50 --no-pager
~~~
完成后，机器重启只需要 server unit；bridge 会由 server 自动拉起。
