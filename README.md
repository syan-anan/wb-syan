# WorkBuddy2API-Panel

把 WorkBuddy / CodeBuddy 的账号池聚合成一个 **OpenAI 兼容** 的本地 API 网关，自带 Web 面板：
多账号轮询、额度与到期感知调度、会话粘性、签到 / 领奖 / 活动自动任务、用量统计。

> 自建自用项目。账号由你自己在面板里添加，凭据只留在你自己的机器上。

---

## 一、一键跑起来

把下面命令里的 `syan-anan/workbuddy2api-panel` 换成这个仓库的实际地址（`ghcr.io/syan-anan/workbuddy2api-panel:latest`）。

### 方式 A · 预构建镜像 + compose（推荐）

```bash
mkdir -p wb2api && cd wb2api

curl -fsSLO https://raw.githubusercontent.com/syan-anan/workbuddy2api-panel/main/docker-compose.yml
curl -fsSL  https://raw.githubusercontent.com/syan-anan/workbuddy2api-panel/main/config.example.json -o config.json
curl -fsSL  https://raw.githubusercontent.com/syan-anan/workbuddy2api-panel/main/.env.example -o .env

mkdir -p auths data
sed -i 's|^GHCR_IMAGE=.*|GHCR_IMAGE=ghcr.io/syan-anan/workbuddy2api-panel:latest|' .env

docker compose pull
docker compose up -d
```

### 方式 B · `docker run` 一行

```bash
mkdir -p auths data
curl -fsSL https://raw.githubusercontent.com/syan-anan/workbuddy2api-panel/main/config.example.json -o config.json

docker run -d --name workbuddy2api --restart unless-stopped \
  -p 7863:7863 \
  -e TZ=Asia/Shanghai \
  -v "$PWD/auths:/app/auths" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/config.json:/app/config.json" \
  ghcr.io/syan-anan/workbuddy2api-panel:latest
```

> `config.json` 必须在启动前存在。缺了它 Docker 会把挂载点创建成**目录**，容器会启动失败。

### 方式 C · 从源码构建（不需要预构建镜像）

```bash
git clone https://github.com/syan-anan/workbuddy2api-panel.git
cd <repo>
cp config.example.json config.json
mkdir -p auths data
docker compose up -d --build
```

启动后打开面板：**`http://<主机IP>:7863/panel/`**

---

## 二、首次配置

1. **设置 API Key**：编辑 `config.json`，把 `api_key` 改成自己的强随机串。
   留空的话程序首次启动会自动生成，并打印在日志里：

   ```bash
   docker compose logs wb2api | grep -i api_key
   ```

2. **登录面板**：浏览器打开 `http://<主机IP>:7863/panel/`，用上一步的 `api_key` 登录。

3. **加账号**：面板「账号」页添加 WorkBuddy / CodeBuddy 账号（手机号 + 验证码，或导入已有的 auth json）。

4. **接客户端**：把客户端的 base_url 指向 `http://<主机IP>:7863/v1`，key 用 `api_key`（或在面板里签发子密钥）。

### 客户端配置

| 项 | 值 |
|---|---|
| Base URL | `http://<主机IP>:7863/v1` |
| API Key | `config.json` 里的 `api_key`，或面板签发的子密钥 |
| 可用模型 | `GET /v1/models`，或面板「模型」页 |

---

## 三、运维速查

```bash
docker compose logs -f --tail=200 wb2api     # 看日志
curl -s http://127.0.0.1:7863/healthz        # 健康检查（无可用账号时 healthy=0 属正常）
docker compose pull && docker compose up -d  # 升级到新镜像
docker compose down                          # 停止（数据在 ./auths ./data ./config.json，不会丢）
tar czf wb2api-backup-$(date +%F).tar.gz auths data config.json   # 备份
```

### 改端口

`.env` 里改 `PANEL_PORT`，然后 `docker compose up -d`。记得放行防火墙：

```bash
sudo ufw allow 7863/tcp
```

### 挂载目录属主（最常见的坑）

容器以 uid `10001` 运行。若日志出现 `auths/*.json.tmp: permission denied`：

```bash
# 二选一
sudo chown -R 10001:10001 ./auths ./data ./config.json
# 或在 .env 里把 PUID/PGID 改成 `id -u` / `id -g` 的结果
```

### 数据与存储

| 路径 | 内容 | 量级 |
|---|---|---|
| `auths/*.json` | 每个账号一份凭据 | 约 3 KB / 账号 |
| `data/state.json` | 池状态（额度、冷却、到期时间等） | 约 1 KB |
| `data/usage.json` | 逐请求用量记录 | 约 0.3 KB / 请求，建议定期归档 |
| 容器日志 | json-file，已按 10 MB × 3 轮转 | ≤ 30 MB |

**预留 1 GB 足够。**

---

## 四、目录结构

```
.
├── cmd/                    # 入口：server / login / signin / credit / trial
├── internal/               # 网关主体
│   ├── pool/               # 账号池：选号、冷却、降级、额度感知
│   ├── server/             # OpenAI 兼容 HTTP 层
│   ├── session/            # 会话粘性
│   ├── upstream/           # 上游协议、SSE、工具调用配对
│   ├── panel/              # Web 面板（内嵌前端）
│   ├── scheduler/          # 签到 / 活动 / 任务调度
│   └── usage/              # 用量统计
├── scripts/                # 辅助脚本
├── Dockerfile              # 多阶段构建（golang -> alpine）
├── docker-compose.yml
├── config.example.json
├── .env.example
├── README.full.md          # 详细功能手册（上游文档）
├── DEPLOY.md               # 部署与运维细则
└── LICENSE
```

---

## 五、常见问题

**Q：`docker compose up -d` 报 `config.json` 是目录？**
挂载前文件不存在。`rm -rf config.json && cp config.example.json config.json` 后重来。

**Q：面板打不开 / 一直转圈？**
先在本机 `curl -s http://127.0.0.1:7863/healthz` 确认容器活着；再查防火墙 / 安全组是否放行端口。

**Q：`docker compose pull` 报 `denied` / `unauthorized`？**
GHCR 的包默认是 **private**。去 GitHub → 你的头像 → **Packages** → 这个包 → **Package settings** → Change visibility → **Public**。

**Q：新加的账号一直不被调用？**
会话粘性（`session_sticky`）会把同一个对话固定到某个账号上。新号只会被"新会话"选中；
面板里把新号 `禁用 → 等 1~2 分钟 → 复活`，再对一个老号做同样操作，即可把它重新变成空闲号被优先选中。
把 `config.json` 里 `session_sticky.enabled` 改成 `false` 可以关闭该行为（需重启容器）。

**Q：想换端口 / 反代到 443？**
面板是普通 HTTP 服务，直接挂在 Nginx / Caddy 后面即可，注意 SSE 流式响应要关掉缓冲（`proxy_buffering off`）。

更多细节见 [`DEPLOY.md`](DEPLOY.md) 和 [`README.full.md`](README.full.md)。

---

## 六、许可与声明

MIT License，见 [`LICENSE`](LICENSE)。本项目为上游项目的 fork，版权与来源在 LICENSE 中保留。

本项目仅供自建自用与技术研究；请遵守上游服务的使用条款，账号与数据由使用者自行负责。