# 部署与运维细则

面向自建服务器（Linux + Docker）。Windows / macOS 用 Docker Desktop 同样适用，命令不变。

## 1. 前置

| 项目 | 要求 |
|---|---|
| Docker | 20.10+（推荐 24+） |
| Docker Compose | v2（`docker compose`，非 `docker-compose`） |
| 架构 | linux/amd64 或 linux/arm64（镜像双架构） |
| 磁盘 | 预留 1 GB 足够 |
| 端口 | 宿主机默认 **18888**（容器内固定 7863），需放行 |

## 2. 从零部署

```bash
git clone https://github.com/syan-anan/wb-syan.git
cd <repo>

cp .env.example .env                 # 可选：填 GHCR_IMAGE / PANEL_PORT / PUID / PGID
cp config.example.json config.json   # 必做：挂载前文件必须存在
mkdir -p auths data

docker compose up -d --build         # 本地构建
# 或（.env 里填了 GHCR_IMAGE）
docker compose pull && docker compose up -d
```

健康检查：

```bash
curl -s http://127.0.0.1:18888/healthz
```

面板：`http://<主机IP>:18888/panel/`

## 3. 构建过程说明

`Dockerfile` 是两段式：

1. **build 阶段** `golang:1.23-alpine`：`go mod download` → 编译 4 个二进制
   （`wb-syan` / `signin_bin` / `login` / `credit`），全部 `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`。
2. **运行阶段** `alpine:3.20`：装 `wget ca-certificates tzdata python3 bash`，建 uid `10001` 的 `app` 用户，
   脚本做 CRLF→LF 并 `chmod 755`，`HEALTHCHECK` 打 `/healthz`。

国内网络下 `apk` / `go mod` 可能很慢，可给构建阶段配代理：

```bash
docker compose build --build-arg HTTP_PROXY=http://<proxy> --build-arg HTTPS_PROXY=http://<proxy>
```

镜像内**不含任何真实配置**，`config.json` 落的是 `config.example.json`，生产由挂载卷覆盖。

## 4. 资源占用

| 项目 | 大小 |
|---|---|
| 运行镜像 | 约 100–120 MB |
| 构建期临时占用 | 峰值额外 300–600 MB（golang 基础镜像 + 模块缓存），`docker builder prune -f` 可回收 |
| `auths/` | 约 3 KB / 账号 |
| `data/state.json` | 约 1 KB |
| `data/usage.json` | 约 0.3 KB / 请求（1 万请求/天 ≈ 3 MB/天，建议定期归档） |
| 容器日志 | 已按 10 MB × 3 轮转 |

## 5. 挂载目录属主

容器以 uid `10001` 运行。宿主机目录属主不同会报 `auths/*.json.tmp: permission denied`：

```bash
sudo chown -R 10001:10001 ./auths ./data ./config.json
# 或在 .env 里 PUID=$(id -u) PGID=$(id -g)
```

`config.json` **不能挂 `:ro`** —— 面板「配置」页要写回它（保存后热生效）。

## 6. 备份与回滚

```bash
# 备份（数据全在挂载目录里）
tar czf wb-syan-backup-$(date +%F).tar.gz auths data config.json

# 回滚
docker compose down
# 还原 auths/ data/ config.json，或 checkout 旧 commit 后
docker compose up -d --build
```

## 7. 反向代理（可选）

面板与 API 都是普通 HTTP。挂在 Nginx / Caddy 后面时注意：

- SSE 流式响应必须关缓冲：`proxy_buffering off;` / `proxy_read_timeout 300s;`
- 转发 `Authorization` 头，不要改写。
- 想要 HTTPS 就用 Caddy 自动证书，或 Nginx + certbot。

## 8. 升级

```bash
git pull
docker compose pull && docker compose up -d     # 用 GHCR 镜像
docker compose up -d --build                    # 或本地重建
```

## 9. 本地开发构建（可选）

需要 Go 1.22+：

```bash
go build ./...
go test ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb-syan ./cmd/server
```