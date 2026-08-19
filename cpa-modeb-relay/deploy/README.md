# 部署指南

本目录提供这个 fork 的 Linux 部署脚本。相比上游，这里的重点是 **Cursor 凭据（`crsr_...` User API Key）** 的接入方式，其余配置与上游 `config.example.yaml` 一致。

## 一键部署

```bash
git clone <本仓库地址> && cd cpa-modeb-relay
sudo ./deploy/deploy.sh install
```

脚本会依次完成：

1. 检查 Go 工具链。`go.mod` 要求 Go 1.26，只要本机有 Go ≥ 1.21，`GOTOOLCHAIN=auto` 就会自动拉取 1.26；没有 Go 或版本低于 1.21 时，脚本会下载一份到 `~/.cache/cliproxyapi/`，不动系统里已有的 Go。
2. 用 `CGO_ENABLED=1` 编译 `cmd/server`，并注入版本号、commit 和构建时间。
3. 安装二进制到 `/opt/cliproxyapi/bin/CLIProxyAPI`。
4. 如果 `/etc/cliproxyapi/config.yaml` 不存在，生成一份最小配置，并**打印随机生成的客户端 API Key 和管理密钥**（管理密钥首次启动后会被哈希写回，务必当场保存）。
5. 创建 `cliproxy` 系统用户，注册并启动 systemd 服务 `cliproxyapi`，最后探测 `/healthz`。

非 root 执行时，脚本会退到 `~/.local/share/cliproxyapi` 并注册 systemd user 服务，适合没有 root 的机器。容器里通常有 `systemctl` 但连不上 bus，脚本会检测到这一点，只装二进制和配置，然后打印手动启动命令。

## 常用命令

```bash
sudo ./deploy/deploy.sh status      # 服务状态 + /healthz 探活
sudo ./deploy/deploy.sh logs        # journalctl -f
sudo ./deploy/deploy.sh update      # 拉取代码后重新编译并重启
sudo ./deploy/deploy.sh restart
sudo ./deploy/deploy.sh uninstall   # 删除服务和二进制，保留配置与凭据
```

可用环境变量覆盖默认路径和端口：`PREFIX`、`CONFIG_DIR`、`DATA_DIR`、`HOST`、`PORT`、`SERVICE_NAME`、`SERVICE_USER`，以及 `NO_SYSTEMD=1`（只装二进制和配置，不注册服务）。例如换端口部署第二个实例：

```bash
sudo PORT=8318 SERVICE_NAME=cliproxyapi-2 CONFIG_DIR=/etc/cliproxyapi-2 \
     DATA_DIR=/var/lib/cliproxyapi-2 ./deploy/deploy.sh install
```

## 配置 Cursor 凭据

拿到 Cursor 控制台签发的 `crsr_...` User API Key 后，三种方式任选其一，效果相同（都会写进 `config.yaml` 的 `cursor-api-key` 块，服务监听到文件变化后热重载，不需要重启）：

**命令行**

```bash
sudo -u cliproxy /opt/cliproxyapi/bin/CLIProxyAPI \
  --config /etc/cliproxyapi/config.yaml \
  --cursor-api-key "crsr_..."
```

**管理 API**

```bash
curl -X PUT http://127.0.0.1:8317/v0/management/cursor-api-key \
  -H "Authorization: Bearer <管理密钥>" \
  -H "Content-Type: application/json" \
  -d '[{"api-key":"crsr_...","prefix":"cursor"}]'
```

同一路由还支持 `GET` 查看、`PATCH` 增量更新、`DELETE` 删除。

**TUI**

```bash
/opt/cliproxyapi/bin/CLIProxyAPI --config /etc/cliproxyapi/config.yaml --tui
```

进入 API Keys 页，按 `s` 切到 Cursor API Keys，按 `a` 新增。

如果更想用 OAuth 而不是 API Key，改用 `--cursor-login`，凭据会以 JSON 落在 auth 目录里。

写入成功后可以验证模型已经挂上：

```bash
curl -s http://127.0.0.1:8317/v1/models -H "Authorization: Bearer <客户端 API Key>"
```

带 `prefix: cursor` 的凭据会同时暴露裸名和 `cursor/` 前缀两种模型 ID，例如 `claude-4.6-sonnet` 和 `cursor/claude-4.6-sonnet`。前缀的用途是在配置了多个上游时把请求钉到指定凭据上。

## 客户端接入

服务同时提供 OpenAI、Claude、Gemini 三套兼容接口，用配置里的客户端 API Key 作为 bearer token：

| 客户端 | 地址 |
| --- | --- |
| OpenAI 兼容 | `http://<host>:8317/v1` |
| Claude 兼容 | `http://<host>:8317` （`ANTHROPIC_BASE_URL`） |
| Gemini 兼容 | `http://<host>:8317` |

默认配置里 `host: ""` 监听所有网卡，`remote-management.allow-remote: false` 只允许本机访问管理接口。如果要把服务暴露到公网，请在前面套一层 TLS 反向代理，不要直接开放 8317 和管理接口。

## Docker 部署

仓库根目录已有 `Dockerfile` 和 `docker-compose.yml`，需要 Docker 环境时用它们更简单：

```bash
cp config.example.yaml config.yaml   # 按需修改
./docker-build.sh                    # 选 2 从源码构建
docker compose logs -f
```

compose 会把 `./config.yaml`、`./auths`、`./logs` 挂进容器，路径可以用 `CLI_PROXY_CONFIG_PATH`、`CLI_PROXY_AUTH_PATH`、`CLI_PROXY_LOG_PATH` 覆盖。

## 排查

- **`bind: address already in use`**：8317 被占用，换 `PORT` 或停掉占用进程。
- **`/v1/models` 返回空列表**：没有任何可用凭据，检查 `config.yaml` 里的 `cursor-api-key` 是否写入成功。
- **日志出现 `cursor: api key exchange failed ... 401 Invalid User API Key`**：key 无效或已撤销，此时会退回内置模型列表，实际请求仍会失败。
- **改了配置没生效**：确认服务对 `config.yaml` 有写权限（systemd unit 里已经把 `CONFIG_DIR` 加进 `ReadWritePaths`），并在日志里找 `config file changed, reloading`。
