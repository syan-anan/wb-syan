# wb-syan — WorkBuddy / CodeBuddy 账号池网关

**当前版本：v1.11.9-panel** ｜ OpenAI 兼容 API ｜ 多账号池 + 自动任务 ｜ Docker 一键部署

把多个 WorkBuddy / CodeBuddy 账号聚合成**一个 OpenAI 兼容的本地 API 网关**，自带 Web 面板：
多账号轮询、额度与到期感知调度、会话粘性、签到 / 领奖 / 任务自动执行、子密钥分流、用量统计。

> 自建自用项目。账号由你自己在面板里添加，凭据只留在你自己的机器上。

---

## ⚡ 一键部署（推荐）

只需一条命令，自动完成克隆、准备配置、拉镜像、启动、健康检查：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/syan-anan/wb-syan/main/deploy.sh)
```

> 国内网络不通时，用加速地址下载脚本：
>
> ```bash
> bash <(curl -fsSL https://ghproxy.net/https://raw.githubusercontent.com/syan-anan/wb-syan/main/deploy.sh)
> ```

脚本内部已经做了容错，不需要你手动处理：

- **代码克隆**：直连 `github.com` 失败 → 自动回退 `ghproxy.net` → 再回退 `gh-proxy.com`
- **镜像拉取**：`ghcr.io` 拉不动 → 自动改为本地构建（需要能访问 Docker Hub 与 Alpine 源）
- **端口/目录**：`PORT=18888 APP_DIR=/opt/wb-syan` 可覆盖；`REPO_URL=...` 可强制指定克隆源

可选环境变量：

```bash
PORT=18888 APP_DIR=/opt/wb-syan bash <(curl -fsSL https://raw.githubusercontent.com/syan-anan/wb-syan/main/deploy.sh)
```

部署完成后验证：

```bash
curl http://127.0.0.1:18888/healthz
# {"healthy":0,"total":0,"service":"wb-syan"}   ← healthy 为 0 表示还没加账号，正常
```

---

## 🐳 手动 Docker 部署

### 方式一：docker run（用预构建镜像，最快）

```bash
mkdir -p wb-syan && cd wb-syan
curl -fsSL https://raw.githubusercontent.com/syan-anan/wb-syan/main/config.example.json -o config.json
mkdir -p auths data

docker run -d \
  --name wb-syan \
  --restart unless-stopped \
  -p 18888:7863 \
  -e TZ=Asia/Shanghai \
  -v "$PWD/auths:/app/auths" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/config.json:/app/config.json" \
  ghcr.io/syan-anan/wb-syan:latest
```

> `config.json` 必须在启动前存在。缺了它 Docker 会把挂载点创建成**目录**，容器会启动失败。

### 方式二：docker compose（推荐长期使用）

```bash
git clone https://github.com/syan-anan/wb-syan.git
cd wb-syan
cp .env.example .env        # 可选：改端口 / PUID / PGID
cp config.example.json config.json
mkdir -p auths data

docker compose pull && docker compose up -d     # 用预构建镜像
# 或不用镜像、本地构建：
docker compose up -d --build
```

### 方式三：完全从源码构建

```bash
git clone https://github.com/syan-anan/wb-syan.git
cd wb-syan
cp config.example.json config.json
mkdir -p auths data
docker compose up -d --build
```

### 常用管理命令

| 操作 | 命令 |
|------|------|
| 查看日志 | `docker compose logs -f --tail=200 wb-syan` |
| 健康检查 | `curl http://127.0.0.1:18888/healthz` |
| 重启 | `docker compose restart` |
| 停止并删除容器 | `docker compose down` |
| 升级到新镜像 | `docker compose pull && docker compose up -d` |
| 更新代码后重建 | `git pull && docker compose up -d --build` |
| 备份数据 | `tar czf wb-syan-backup-$(date +%F).tar.gz auths data config.json` |

---

## 🚀 首次使用（3 步）

### 1. 设置 API Key

编辑 `config.json`，把 `api_key` 改成自己的强随机串：

```json
{ "api_key": "换成你自己的随机串" }
```

留空（或仍是 `test_key`）时，程序首次启动会**自动生成**一个并打印在日志里：

```bash
docker compose logs wb-syan | grep -i api_key
```

改完重启生效：`docker compose restart`

### 2. 打开面板登录

浏览器访问 **`http://<服务器IP>:18888/panel/`**，用上一步的 `api_key` 当密码登录。

### 3. 添加账号

面板「**账号**」页 → 右上角「**添加账号**」，两种方式：

| 方式 | 说明 |
|------|------|
| **浏览器登录** | 点「获取授权链接」→ 在浏览器打开并登录 → 回到面板等待自动检测完成 |
| **导入 JSON** | 已有 auth json（含 cockpit 格式）直接粘贴导入 |

加完账号后 `healthz` 的 `healthy` 就会变成可用账号数。

### 4. 接客户端

把客户端的 base_url 指向网关即可：

| 项 | 值 |
|---|---|
| **Base URL** | `http://<服务器IP>:18888/v1` |
| **API Key** | `config.json` 里的 `api_key`，或面板签发的子密钥 |
| **可用模型** | `GET /v1/models`，或面板「模型」页 |

---

## 🖥️ 面板功能导览

| 视图 | 能干什么 |
|------|----------|
| **账号** | 账号池总览、单账号详情（积分 / 到期时间 / 冷却 / 在途）、启用禁用、手动签到、刷新余额、账号对比、按批次到期日聚合、积分到期分布 |
| **任务中心** | 成长任务扫描 / 接受 / 领取 / 一键自动执行；签到、旅行巡检、保活、活跃上报；批量「全部签到 / 全部保活 / 全部领奖」 |
| **模型** | 模型能力清单、支持的思考档位、上下文长度、最大输出；实时探测上游 |
| **套餐** | 各账号可用套餐详情 |
| **用量** | 用量总览，按模型 / 按账号 / 按域统计，近 24 小时 / 7 天 / 30 天 |
| **密钥** | 新建子密钥（分流）：可绑定指定账号白名单、可随时停用、可查看/删除；子密钥进不了面板 |
| **配置** | 账号池与流量治理、上游与高级、定时任务时点、系统提示词模式（保存后热生效） |
| **日志** | 最近 500 行运行日志，自动滚动 |

---

## 🔌 API 使用教程

服务端口 `18888`（容器内 7863），鉴权统一用 `Authorization: Bearer <api_key>`。

### 健康检查（无鉴权）

```bash
curl http://127.0.0.1:18888/healthz
```

```json
{ "healthy": 5, "total": 5, "service": "wb-syan" }
```

> `healthy=0` 时返回 **503** —— 这是探活语义（负载均衡 / 编排据此摘除节点），不是故障。

### 模型列表

```bash
curl http://127.0.0.1:18888/v1/models \
  -H "Authorization: Bearer 你的APIKEY"
```

### 流式聊天

```bash
curl -N http://127.0.0.1:18888/v1/chat/completions \
  -H "Authorization: Bearer 你的APIKEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "模型名",
    "stream": true,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### 非流式聊天

```bash
curl http://127.0.0.1:18888/v1/chat/completions \
  -H "Authorization: Bearer 你的APIKEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "模型名",
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### 运行状态

```bash
curl http://127.0.0.1:18888/status \
  -H "Authorization: Bearer 你的APIKEY"
```

### 子密钥分流

面板「**密钥**」页新建子密钥，可以做到：

- **绑定账号白名单**：勾选账号后，用这条 key 发起的请求**只会落在勾选的账号上**，不会走其它账号；不勾选 = 走全部账号
- **随时停用 / 恢复**：停用后立即失效，配置保留，想开随时开
- **独立分发**：把 key 发给别人用，base_url 不变，你随时能在面板改绑定、改备注或删掉
- **安全边界**：子密钥**进不了管理面板**（面板鉴权只认主 key / 面板密码），所以一个受限 key 无法自我提权

操作细节：

- 名称必填；密钥留空则由服务端随机生成 `sk-…`，**明文只在创建时返回一次**，之后列表里是掩码，需要时点「显示」重新查看
- 自定义密钥至少 8 个字符，且不能与已有密钥重复
- 在面板里改完**下一个请求即生效**，不需要重启容器

> 注意：当前版本子密钥**只支持「按账号白名单」分流**，没有按模型 / 按域的限制。

用子密钥时把客户端 API Key 换成子密钥即可，base_url 不变。

### 接到常见客户端

| 客户端 | 填法 |
|--------|------|
| Cherry Studio / NextChat / LobeChat | 新建 OpenAI 兼容供应商 → Base URL 填 `http://<IP>:18888/v1`，Key 填 `api_key` |
| OpenAI SDK (Python) | `OpenAI(base_url="http://<IP>:18888/v1", api_key="...")` |
| Cline / Roo Code | Provider 选 OpenAI Compatible，Base URL 同上 |
| Claude Code 类工具 | 指向 `/v1` 的 Anthropic 兼容端点（面板「模型」页确认可用模型） |

---

## ⚙️ 配置说明（config.json）

改完保存到 `config.json` 后：**面板「配置」页里改 = 热生效**；直接改文件 = 需要 `docker compose restart`。

| 字段 | 默认 | 说明 |
|------|------|------|
| `listen` | `:7863` | 容器内监听地址，一般不用改 |
| `api_key` | 空 | 网关主密钥，同时是面板登录密码 |
| `api_keys` | `[]` | 子密钥（分流）列表，由面板「密钥」页维护，一般不用手改 |
| `auth_dir` | `./auths` | 账号凭据目录 |
| `state_file` | `./data/state.json` | 池状态持久化文件 |
| `panel.password` | 空 | 面板独立密码（留空则用 `api_key`） |
| `schedule.checkin_hours` | `[9,21]` | 自动签到时点 |
| `schedule.activity_hours` | `[10]` | 活动领奖时点 |
| `schedule.keepalive_hours` | `[22]` | 保活时点 |
| `schedule.balance_refresh_minutes` | `5` | 余额后台刷新间隔 |
| `pool.max_in_flight` | `3` | 单账号最大在途请求 |
| `pool.max_in_flight_global` | `2` | 国际版账号在途上限 |
| `pool.prefer_expiring` | `true` | 快过期积分优先消耗 |
| `pool.expiring_soon` | `168h` | 「快过期」窗口 |
| `session_sticky.enabled` | `true` | 同一会话固定走同一账号（改这项**需重启**） |
| `session_sticky.ttl` | `30m` | 粘性会话有效期 |
| `upstream.timeout_seconds` | `120` | 上游请求超时 |
| `prompt.mode` | `passthrough` | 系统提示词模式，`passthrough` 原样透传 |
| `upstash.url` / `token` | 空 | 可选的 Upstash Redis，用于跨实例共享状态 |

完整字段见 [`config.example.json`](config.example.json)，逐项解释见 [`README.full.md`](README.full.md)。

---

## ❓ 常见问题

**Q：`docker compose up -d` 报 `config.json` 是目录 / 容器起不来？**

挂载前文件不存在。`rm -rf config.json && cp config.example.json config.json` 后重来。

**Q：日志里 `auths/*.json.tmp: permission denied`？**

容器以 uid `10001` 运行，挂载目录属主不同。两种解法：

```bash
sudo chown -R 10001:10001 ./auths ./data ./config.json
# 或在 .env 里把 PUID/PGID 改成 `id -u` / `id -g` 的结果
```

**Q：面板打不开 / 一直转圈？**

先在本机 `curl -s http://127.0.0.1:18888/healthz` 确认容器活着；再查防火墙 / 云安全组是否放行端口（`sudo ufw allow 18888/tcp`）。

**Q：`docker compose pull` 报 `denied` / `unauthorized`？**

镜像包默认可能是 private。GitHub → 头像 → **Your packages** → `wb-syan` → **Package settings** → Change visibility → **Public**。

**Q：新加的账号一直不被调用？**

这是**会话粘性**：同一个对话会被固定到某个账号上，新号只会被「新会话」选中。两种办法：

1. 面板里把新号 `禁用 → 等 1~2 分钟 → 复活`，再对一个老号做同样操作，它就会重新变成空闲号被优先选中；
2. 把 `config.json` 里 `session_sticky.enabled` 改成 `false`（**需重启容器**）。

**Q：`healthz` 返回 503？**

说明当前没有可用账号（或全部在途占满）。去面板「账号」页确认账号状态。

**Q：想换端口 / 挂到域名后面？**

`.env` 里改 `PANEL_PORT` 后 `docker compose up -d`。挂在 Nginx / Caddy 后面时注意 **SSE 流式响应要关缓冲**：`proxy_buffering off;`、`proxy_read_timeout 300s;`。

**Q：数据存哪、要多大空间？**

| 路径 | 内容 | 量级 |
|------|------|------|
| `auths/*.json` | 每账号一份凭据 | 约 3 KB / 账号 |
| `data/state.json` | 池状态 | 约 1 KB |
| `data/usage.json` | 逐请求用量 | 约 0.3 KB / 请求，建议定期归档 |
| 容器日志 | json-file，已按 10 MB × 3 轮转 | ≤ 30 MB |

**预留 1 GB 足够。**

---

## 📜 版本历史

- **v1.11.9-panel（当前）**：账号池 + 任务中心 + 子密钥分流 + 会话粘性 + 到期感知调度；Docker 一键部署
- 更早版本：见 [Releases](https://github.com/syan-anan/wb-syan/releases)

---

## ⚠️ 免责声明与许可协议

**最后更新：2026-10-02 · 请在使用前仔细阅读，下载、克隆或运行本项目的任何部分即视为已阅读并同意本协议全部内容**

### 一、项目性质

本项目（以下简称"本软件"）是一个自建的 API 网关与账号管理工具，仅供**个人学习、技术研究与自有账号的自动化管理**使用。本软件不预置任何针对特定网站或商业系统的攻击能力，不上传、不外传任何账号凭据与用户数据——所有凭据仅保存在使用者自己的机器上。

### 二、使用目的

1. 本软件仅可用于：合法的技术研究、个人学习、**自有账号**（本人拥有所有权或已获书面授权的账号）的自动化管理与测试验收。
2. **严禁用于任何商业用途**，包括但不限于：出售、租赁、转授权、集成到商业产品中、以任何形式收取费用或牟利。
3. **严禁用于以下行为：**
   - 使用他人账号，或未经授权批量获取、注册、租借第三方服务账号；
   - 绕过任何未获得明确书面授权的第三方服务的安全机制、风控策略或使用限制；
   - 干扰、破坏第三方服务的正常运营秩序；
   - 任何违反所在国家/地区法律法规的活动，包括但不限于《中华人民共和国网络安全法》《数据安全法》《个人信息保护法》《计算机信息网络国际联网安全保护管理办法》及其他司法管辖区的相关法律。

### 三、责任归属

1. 使用者使用本软件进行的一切操作及其产生的后果，均属**使用者个人行为**，与本软件的开发者、贡献者、维护者及其关联方无关。
2. 使用者应自行承担因使用本软件而产生的全部法律责任和风险，包括但不限于由此引发的：法律诉讼、行政处罚、刑事追诉、**账号封禁**、服务终止、经济损失、数据丢失、设备损坏等。
3. 若因使用者不当使用导致第三方权益受损，由使用者自行承担全部赔偿责任，开发者不承担任何连带责任。
4. 使用者应自行确认其使用行为是否符合第三方服务的使用条款；因违反第三方服务条款而产生的后果由使用者自行承担。

### 四、无担保与责任限制

1. 本软件按 **"现状"（AS IS）** 提供，不附带任何明示或默示的担保，包括但不限于对适销性、特定用途适用性、准确性、完整性、可用性的保证。
2. 开发者不保证服务的连续性、稳定性、无误性，不对因网络中断、上游协议变更、第三方平台升级或账号异常导致的请求失败承担任何责任。
3. 在适用法律允许的最大范围内，开发者对任何直接的、间接的、附带的、特殊的或后果性的损害概不负责——即使已被告知发生此类损害的可能性。

### 五、知识产权

1. 本项目基于上游开源项目二次开发，原始版权与来源声明完整保留于 [`LICENSE`](LICENSE) 文件中，遵循 MIT 协议。
2. 本项目所引用的开源组件（如 Go 标准库、go-redis 等）遵循其各自的原始开源协议。
3. 本项目与所对接的第三方服务不存在任何隶属、合作或授权关系。

### 六、协议终止

若您不同意上述任何条款，请立即停止使用并删除本软件的所有副本。一旦发现使用者违反本协议，开发者有权随时终止对该用户提供的一切支持与服务，并保留追究法律责任的权利。

### 七、其他

1. 本协议未尽事宜，参照国家相关法律法规及行业惯例执行。
2. 本协议最终解释权归软件开发者所有。

---

**继续使用本软件，即表示您已充分阅读、理解并同意本免责声明的全部内容，并自愿承担因使用本软件而产生的一切责任与风险。**