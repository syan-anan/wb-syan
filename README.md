# wb-syan — WorkBuddy / CodeBuddy 账号池网关

**当前版本：v1.13.0-panel** ｜ OpenAI 兼容 API ｜ 多账号池 + 自动任务 ｜ Docker 一键部署

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

## 🔄 升级与更新提示

### 面板里的「有新版本」提示

侧栏底部（版本号下面）出现黄色小胶囊 = 有新版本。点开能看到当前版本、最新版本、
一键复制的升级命令，以及发布说明链接。

- 检查频率：服务端启动 20 秒后查一次，之后每 **6 小时**一次；只读 GitHub 的公开接口
- 结果缓存在服务端内存里，面板只读缓存 —— **外网慢不会拖慢面板**
- 断网 / 内网 / GitHub 不通：什么都不显示，静默失败，不刷日志
- 想彻底关掉（离线、内网部署）：`config.json` 里 `"panel": { "update_check": false }`

### 手动升级（推荐）

```bash
docker compose pull && docker compose up -d
```

数据都在 `auths/`、`data/`、`config.json` 三个挂载点里，升级不会碰它们。
升级前想留后路先备份：

```bash
tar czf wb-syan-backup-$(date +%F).tar.gz auths data config.json
```

### 整库备份 / 还原（面板里点一下）

侧栏「更新与备份」页可以直接下载一个 zip，里面是**跑起来的全部状态**：

- `config.json`、账号池 `state.json`、用量 `usage.json`、积分流水 `journal.json`
- 子密钥配额、模型上限探测缓存、Qoder 签到记录
- `auths/` 全部凭证 —— 还原后即可直接登录，**不用重新扫码**

包里含凭证，按机密文件保管：下载响应带 `Cache-Control: no-store`，服务端临时文件写完即删。
还原走同一个页面：选中 zip 上传即可，还原前会先把**将被覆盖的文件**复制到
`backups/restore-<时间戳>/`，出问题原路退回。包内路径只接受白名单顶层名，
带 `../` 或未知文件一律拒绝（防 zip-slip）。

> 还原写完磁盘就生效了吗？**没有。** 运行中的进程仍持有旧 state，重启前的余额刷新会把它写回去。
> 还原后立刻重启容器：`docker compose restart`。页面上那条提示就是为这个留的。

### 自动升级（可选，watchtower）

不想手动升，让 watchtower 每小时帮你检查并自动拉新镜像：

```bash
docker compose -f docker-compose.yml -f docker-compose.watchtower.yml up -d
```

它只动带 `com.centurylinklabs.watchtower.enable` 标签的容器（也就是 wb-syan 自己），
不会碰你机器上其它服务。**三个前提先读清楚**：

1. watchtower 要挂 `/var/run/docker.sock` —— 等于把宿主机 Docker 的控制权交给它，只在自己的机器上开
2. 自动升级会在你不看的时候重启容器（想只在凌晨升，见 `docker-compose.watchtower.yml` 里的 `--schedule` 注释）
3. 升级失败不会自动回滚；要稳就用面板提示手动升

### 部署后自检

仓库自带一个不依赖 Go / Python 的自检脚本，只用 `curl`，适合刚部署完确认「接口都活着」：

```bash
WBSY_BASE=http://127.0.0.1:18888 WBSY_TOKEN=<你的面板凭据> bash scripts/check_selftest.sh
# 想连对话一起验（每个协议真发一次请求，会消耗少量积分）：
WBSY_BASE=http://127.0.0.1:18888 WBSY_TOKEN=<你的面板凭据> bash scripts/check_selftest.sh --live
```

它检查健康端点、鉴权是否生效、版本号、三协议的模型列表、新接口是否存在，以及整库备份、积分流水
与上游路由快照能不能正常返回。这里刻意只带面板凭据也能跑完：数据面（`/v1/models`、`/status`）认的是
网关 api_key，面板凭据与它是两把钥匙，脚本按各自的边界分别探测。**凭据只从环境变量读**，脚本不会去
磁盘上翻任何配置文件。退出码就是失败项数，可以直接接进 CI 或 cron。

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
| **账号** | 账号池总览（默认**按添加顺序**排列，可一键切「国内 / 国际」分组）、单账号详情（积分 / 到期时间 / 冷却 / 在途）、启用禁用、手动签到、刷新余额、账号对比、按批次到期日聚合、积分到期分布；每行「并发」按钮给单个号设**在途请求上限**（跟随全局 / 不限 / 固定值三态） |
| **任务中心** | 成长任务扫描 / 接受 / 领取 / 一键自动执行；签到、旅行巡检、保活、活跃上报；批量「全部签到 / 全部保活 / 全部领奖」 |
| **模型** | 模型能力清单、支持的思考档位、上下文长度、最大输出；实时探测上游 |
| **套餐** | 各账号可用套餐详情 |
| **用量** | 用量总览（请求 / token / 延迟 / 失败 / **窗口内积分已用**）；按账号 / 按模型 / 按域 / 按客户端四张分表，**维度多选**（想看哪张勾哪张，全勾=全部）；窗口一键切近 24 小时 / 3 天 / 7 天 / 30 天 / 全部历史，**「积分已用」与四张分表全部随窗口口径变化**（逐请求实测，精确到个位、不做 k/m 缩写）；还能填**起止日期**查任意区间（含结束当天） |
| **积分构成** | 每个账号的积分到期分布（按剩余天数分桶）、账号对比、逐包明细；顶部**按「域 / 账号」多选筛选**（留空=全部），三块内容共用同一份筛选结果 |
| **密钥** | 新建子密钥（分流）：可绑定**独占账号** / 账号白名单 / 模型白名单 / 域 / 每日生效时段 / 有效期 / 每时·每日请求配额；卡片式列表一眼看清每条 key 的全部限制与配额用量，可随时停用、重置计数、查看/删除；子密钥进不了面板 |
| **积分流水** | 网关自己的记账本，两套账分开列：**确认事件**（任务奖励 / 每日权益，金额来自上游领取响应）与**余额观察**（刷新余额时观测到的净变化，含扣减与来源不明）。可按类型 / 账号 / 来源 / 日期区间筛选，顶部小计随筛选重算，正负分色。两套账有重叠，故意不给合计数 |
| **更新与备份** | 版本状态（已是最新 / 本地版本更新 / 有更新可升）+ 一键复制的升级命令（Docker 与 systemd 两组，路径取你配置的部署目录）+ **整库备份 / 还原**：一个 zip 打包 config、账号池状态、用量、积分流水、配额、模型缓存与 `auths/` 凭证 |
| **配置** | 账号池与流量治理、上游与高级、定时任务时点、系统提示词模式（保存后热生效） |
| **日志** | 最近 500 行运行日志：频道（任务/对话/系统）× 结果档位（成功/警告/失败/其它）双排筛选，账号 / 模型 / 关键词检索；点行展开明细（对话行=字段拆解，错误行=三层诊断）；「导出」按当前筛选拿脱敏纯文本（Bearer / sk- / token / 密码已抹掉） |

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

响应里的 `waf_protection` 是出口保护状态（上游 WAF 判定网关出口 IP 被拦时置位，`active` / `remaining_sec`），置位期间后台维护任务整轮顺延，不发上游调用。

### 子密钥分流

面板「**密钥**」页新建子密钥，一条 key 可以同时叠加下面**全部**限制维度（不填 = 该维度不限）：

| 维度 | 说明 |
|------|------|
| **账号** | 勾选账号后，用这条 key 发起的请求**只会落在勾选的账号上**，不会走其它账号；不勾选 = 走全部账号 |
| **独占账号** | 勾选后，所选账号**只对这条 key 开放**：主密钥与其他子密钥（含自动换号、测速、全冷却兜底）都会跳过它。需要且只能选一个账号，同一账号不能被两条 key 同时独占（停用其中一条即可转移） |
| **模型** | 只允许调用列出的模型，逗号分隔；支持结尾 `*` 前缀通配（`glm-*` 匹配所有 glm 开头的模型），不区分大小写。**同时 `GET /v1/models` 也只返回白名单内的模型**（客户端模型下拉里看不到其它模型，不是「列全部、只放开一条通道」） |
| **域** | 只允许 `cn`（国内）或 `global`（国际）域模型；两个都勾 = 不限 |
| **每日生效时段** | 例如 `09:00` → `18:00`；只填一个 = 从该点起 / 到该点止；跨零点（`22:00` → `06:00`）自动按跨天窗口判定 |
| **有效期** | 填一个到期时间，到点自动失效（列表里会显示「已过期」） |
| **请求配额** | 每天 / 每小时最多多少次请求，0 = 不限；用量实时显示在密钥卡片上，撞限额后返回 402（`code=key_quota_exceeded`）并说明原因，可点「重置计数」人工放行 |

其它能力：

- **随时停用 / 恢复**：停用后立即失效，配置保留，想开随时开
- **独立分发**：把 key 发给别人用，base_url 不变，你随时能在面板改限制、改备注或删掉
- **安全边界**：子密钥**进不了管理面板**（面板鉴权只认主 key / 面板密码），所以一个受限 key 无法自我提权

操作细节：

- 名称必填；密钥留空则由服务端随机生成 `sk-…`，**明文只在创建时返回一次**，之后列表里是掩码，需要时点「复制」重新取
- 自定义密钥至少 8 个字符，且不能与已有密钥重复
- 绑定账号多选支持**搜索 + 按域筛选 + 全选/清空（当前筛选）**
- 在面板里改完**下一个请求即生效**，不需要重启容器；配额计数独立落盘（`data/keys_quota.json`），重启不丢

被限制拦下时，网关返回标准 OpenAI 格式错误：结构性约束（模型白名单 / 域 / 时段 / 有效期 / 独占）是 HTTP 403 `code=key_restricted`，请求配额类（日/时/单模型限额）是 HTTP 402 `code=key_quota_exceeded`（4xx 语义分流，客户端按状态码重试逻辑更好写），消息里都写清是哪一条限制不通过（例如「该密钥不允许调用模型 gpt-4o」「已超出每日配额（上限 100 次，明日 00:00 重置）」）。

用子密钥时把客户端 API Key 换成子密钥即可，base_url 不变。

### Anthropic 兼容端点（Claude Code / Anthropic SDK）

网关同时暴露 `POST /v1/messages`，把 Anthropic Messages 请求翻成内部 Chat 形态再走同一条转发链路
（选号 / 粘性 / 轮转 / 降级 / 用量记账与 OpenAI 端点完全一致），响应再翻回 Anthropic 形态：

```bash
curl -X POST http://127.0.0.1:18888/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: 你的APIKEY" \
  -d '{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

- 鉴权同时接受 `x-api-key`（Anthropic SDK / Claude Code 的默认头）与 `Authorization: Bearer`
- `stream: true` 返回标准 Anthropic SSE 事件序列（`message_start` → `content_block_*` → `message_delta` → `message_stop`）
- 出错时返回 Anthropic 形状的错误体（`{"type":"error","error":{"type":...,"message":...}}`），上游原文保留在 `message` 里

### 加密凭证（.info）导入 / 导出

新版客户端导出的 `.info` 里，`accessToken` / `refreshToken` / 手机号 / 昵称等字段是
`$wbEncrypted` 信封（AES-256-GCM）。网关要收这种文件，得先有解密密钥——服务端没有
客户端安装目录，取不到客户端自带的 at-rest secret，因此由运维提供一份 key 文件：

```bash
# credential-key.json（与服务端同机的私有文件，建议 chmod 600）
# {"keyId": "<sha256(key) 前 16 位十六进制>", "key": "<32 字节 key 的 base64>"}
export WB2A_CREDENTIAL_KEY_FILE=/root/workbuddy2api/credential-key.json
# 也兼容参考实现的 WORKBUDDY_CREDENTIAL_KEY_FILE
```

配好之后（或在 `config.json` 里写 `"credential_key_file": "..."`）：

```bash
# 导入：.info 单文件，或 .jsonl 每行一个凭证
curl -X POST http://127.0.0.1:18888/panel/api/import/info \
  -H "Authorization: Bearer 面板密码" -F "file=@workbuddy-xxx.info"
# 也可以直接把内容放进 body
curl -X POST http://127.0.0.1:18888/panel/api/import/info \
  -H "Authorization: Bearer 面板密码" --data-binary @workbuddy-xxx.info

# 导出：把当前生效的凭证封成 .info（每个字段独立 nonce，每次导出密文都不同）
curl -X POST http://127.0.0.1:18888/panel/api/accounts/<uid>/export \
  -H "Authorization: Bearer 面板密码" -o workbuddy-<uid>.info
```

两点口径：

- **没配密钥也能用**：明文 `.info` 照常导入；遇到信封时会明确告诉你「未配置凭证解密密钥」
  并指出该配哪个字段，而不是含糊地报「格式错误」
- **导出必须加密**：没配密钥时导出直接拒绝——把明文 token 换成一个更像"文件"的形式交出去，
  比不导出更危险
### Responses 兼容端点（Codex CLI 等）

网关同时暴露 `POST /v1/responses`（另有同入口别名 `POST /responses`）。请求会先**投影**成 Chat 形态：
`instructions` → 置顶 system 消息；`input` 数组按项落位（`message` / `function_call` 合并进同一条
assistant 消息 / `function_call_output` → `role:tool`）；`tools` 从 Responses 的扁平行改成 Chat 嵌套行；
`reasoning:{effort}` → 扁平 `reasoning_effort`；`max_output_tokens` → `max_tokens`。之后走的就是同一条
转发链路（选号 / 粘性 / 轮转 / 降级 / 用量记账与另外两个端点完全一致）。

```bash
curl -X POST http://127.0.0.1:18888/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer 你的APIKEY" \
  -d '{"model":"glm-5.2","input":"hi","max_output_tokens":64}'
```

事件契约（改之前先读一遍）：本层只发 Responses 规范的**必需事件子集** ——
`response.created` / `response.output_item.added|done` / `response.output_text.delta` /
`response.reasoning_summary_part.added|done` / `response.reasoning_summary_text.delta` /
`response.function_call_arguments.delta` / `response.completed|incomplete|failed`。
`response.in_progress`、`response.content_part.*`、`*.done` 属于可选扩展，并非所有客户端都实现，
发了会被严格校验的客户端判非法 —— 而校验不通过时客户端是**逐条丢弃**该事件、不报错，
表现为"思考完就断、没有正文"。同理 `response.output_text.delta` 的 `item_id`、
`response.completed.response.usage` 都是必填，本层保证字段齐全（上游不吐 usage 时补零）。
### 客户端参数档案（UA / 版本号 / 风控头 / 指纹）

出站请求的「像不像官方客户端」由一份档案统一决定，三层合成、优先级从低到高：

**内置兜底 → 现场探测或后台保存 → 环境变量**。`client_profile_source` 决定中间那层的取法：

`auto`（探测优先，跟着客户端版本自动走）/ `saved`（保存优先，锁死在后台配置的那一份）。

网关通常跑在服务器/容器里、没有客户端安装目录可读，**现场探测恒为空是预期结果**，

实际生效值来自「保存 + 环境变量」。

```bash
# 看当前生效档案 + 每个键取自哪一层（排障时最有用的一屏）
curl http://127.0.0.1:18888/panel/api/client_profile -H "Authorization: Bearer 面板密码"

# 保存（默认与已有值合并；只改一个字段不用重发整份）
curl -X POST http://127.0.0.1:18888/panel/api/client_profile \
  -H "Authorization: Bearer 面板密码" -H "Content-Type: application/json" \
  -d '{"profile":{"desktop_version":"5.5.6"},"source":"saved"}'

# 重置回「纯兜底 + 环境变量」
curl -X POST http://127.0.0.1:18888/panel/api/client_profile/reset -H "Authorization: Bearer 面板密码"
```

三条硬规则（都是踩坑换来的）：

- **UA 不落盘、始终现拼**：它由 `desktop_version` + `cli_version` + 产品名决定，存下来必然出现
  「同步过一次 UA 之后，版本号再变而 UA 不变」的陈旧陷阱。要整条覆盖 UA 只有 `WB2A_USER_AGENT`
  一个入口（保存 `user_agent` 会被明确拒绝并说明原因）
- **headers 逐层合并**：后台只改 `Accept-Language` 时，其余默认风控头（`X-CodeBuddy-Request`、
  `X-Requested-With`、`X-Private-Data`）必须还在；四个归属头（`X-IDE-Name/Type/Version`、`X-Product`）
  优先级高于额外头，不能被它们覆盖
- **版本号三处同源**：UA、`X-IDE-Version`、桌面指纹里的 `ideVersion`/`extVersion` 一律取档案里的
  `desktop_version`，不会出现「UA 报一个版本、归因头报另一个」

单项也能用环境变量覆盖（`WB2A_DESKTOP_VERSION`、`WB2A_CLI_VERSION`、`WB2A_CPU_CORES` …，

同时兼容参考实现的 `WORKBUDDY_` 前缀）。环境变量永远最高优先级，是运维的逃生门。

两点值得说明的取值：

- `desktop_version` 默认 **5.5.4**（本项目线上现行值）。参考实现默认 5.5.6 —— 没跟，因为改版本号
  等于做一次未经验证的指纹变更；要跟就在面板/环境变量里改
- `X-Product` 默认 **WorkBuddy**。参考实现实测官方取的是部署形态 `SaaS`（`deploymentType`），
  填产品名是官方从不发的值；同样因为「换值属于指纹变更」而保持现行值。要按参考实现取法，
  把档案里的 `product` 改成 `SaaS` 即可
### 手机号登录（+86 / +852 / +853）

除了浏览器授权登录，还可以直接用手机号 + 短信验证码把账号加进池子（面板「添加账号」里选手机号登录，

或直接打接口）：

```bash
# 1) 建会话（会真的打上游 5 个请求，但不发短信，对用户是"瞬时"的）
curl -X POST http://127.0.0.1:18888/panel/api/login/phone/start -H "Authorization: Bearer 面板密码"
#    -> {"ok":true,"sid":"...","countries":[{"code":"86",...},{"code":"852",...},{"code":"853",...}]}

# 2) 发验证码
curl -X POST http://127.0.0.1:18888/panel/api/login/phone/send \
  -H "Authorization: Bearer 面板密码" -H "Content-Type: application/json" \
  -d '{"sid":"<上一步的 sid>","mobile":"13800000000","country_code":"86"}'

# 3) 校验验证码 -> 自动换凭证 -> 落盘 + 热加载入池
curl -X POST http://127.0.0.1:18888/panel/api/login/phone/verify \
  -H "Authorization: Bearer 面板密码" -H "Content-Type: application/json" \
  -d '{"sid":"<sid>","code":"123456"}'

# 会话状态（前端轮询 / 排障）
curl "http://127.0.0.1:18888/panel/api/login/phone/status?sid=<sid>" -H "Authorization: Bearer 面板密码"
```

链路是 11 步（设备授权 state → Keycloak → OneID → 短信 → broker 回调 → 换凭证），

每一步的实现都在 `internal/phonelogin` 的文件头里标了"为什么"。三个必须照做的点：

- **每个会话有自己的 cookie jar**：OneID 登录态横跨 `workbuddy.cn` 与 `oauth2.account.tencent.com`，

  共用会把不同用户串在一起（并发登录表现为"串号"）

- **重定向只在本域白名单内跳**（workbuddy.cn / codebuddy.cn / workbuddy.ai / tencent.com），

  跳去站外就停 —— 防被引走

- **校验对同一会话幂等**：`state_token` 是一次性的，并发/重复校验会变成"成功一次 + 报一次无效 token"，

  用户看到的是莫名其妙的失败

关于图形验证码：上游对**单个手机号**做频率风控 —— 同一号码短时间内请求发码次数过多会要求图形验证码

（实测是按号不是按 IP）。这一步纯协议过不去，接口如实返回 `step=sms_send` / `reason=need_captcha`

并提示"换号或稍后再试"，不会把失败伪装成网络错误。换号或等一会儿即可恢复。

手机号登录只有 CN（短信体系仅国内有）；国际版账号请走浏览器授权登录。
### 成长中心（只读快照）

面板「成长中心」一屏看清一个账号的能量 / 全年活跃热力图 / 连登与兑换 / 盲盒额度 / 当前宠物 /

宠物图鉴 / 旅行礼物 / 抽奖次数 / 成长等级。

```bash
# 单个账号的快照（带 10 分钟缓存；refresh=1 强制重拉）
curl "http://127.0.0.1:18888/panel/api/growth?uid=<uid>" -H "Authorization: Bearer 面板密码"
curl "http://127.0.0.1:18888/panel/api/growth?uid=<uid>&refresh=1" -H "Authorization: Bearer 面板密码"

# 已缓存的所有账号（不触发上游）
curl http://127.0.0.1:18888/panel/api/growth/overview -H "Authorization: Bearer 面板密码"
```

三条边界：

- **只读**：这一层只发 GET，不触发对话、不领奖、不兑换。打开页面不该改变账号状态

- **缓存**：一期快照 = 10 个上游请求，所以有 10 分钟 TTL；「刷新」按钮才强制重拉。

  概览接口只回缓存，绝不在打开页面时对 9 个账号打 90 个请求（那必然触发风控）

- **单段失败不影响整体**：上游某个活动下线/接口抖动都很常见，某段报错就只把这段的说明显示出来，

  其余段照常展示（快照的价值是"一眼看清"，不是"全绿"）

返回的每一段都经过**字段白名单投影**：上游响应里可能夹带邮箱、物流地址、内部 id 之类的字段，

只透出展示需要的那几个（`internal/upstream/growth_snapshot.go` 的 `ProjectGrowthSection`）。
### 接到常见客户端

| 客户端 | 填法 |
|--------|------|
| Cherry Studio / NextChat / LobeChat | 新建 OpenAI 兼容供应商 → Base URL 填 `http://<IP>:18888/v1`，Key 填 `api_key` |
| OpenAI SDK (Python) | `OpenAI(base_url="http://<IP>:18888/v1", api_key="...")` |
| Cline / Roo Code | Provider 选 OpenAI Compatible，Base URL 同上 |
| Claude Code 类工具 | `ANTHROPIC_BASE_URL=http://<IP>:18888`（不用带 `/v1`），key 走 `ANTHROPIC_API_KEY`，模型填 `GET /v1/models` 里的名字 |
| Codex CLI / Responses 兼容客户端 | `base_url` 指向 `http://<IP>:18888/v1`（网关也接受根路径下的 `/responses`），key 走常规 Bearer，模型同样取 `GET /v1/models` |

---

## ⚙️ 配置说明（config.json）

改完保存到 `config.json` 后：**面板「配置」页里改 = 热生效**；直接改文件 = 需要 `docker compose restart`。

| 字段 | 默认 | 说明 |
|------|------|------|
| `listen` | `:7863` | 容器内监听地址，一般不用改 |
| `api_key` | 空 | 网关主密钥，同时是面板登录密码 |
| `api_keys` | `[]` | 子密钥（分流）列表，由面板「密钥」页维护，一般不用手改（每项可含 `accounts` / `models` / `realms` / `time_start` / `time_end` / `expires_at` / `daily_limit` / `hourly_limit`） |
| `auth_dir` | `./auths` | 账号凭据目录 |
| `state_file` | `./data/state.json` | 池状态持久化文件 |
| `panel.password` | 空 | 面板独立密码（留空则用 `api_key`） |
| `schedule.checkin_hours` | `[9,21]` | 自动签到时点 |
| `schedule.activity_hours` | `[10]` | 活动领奖时点 |
| `schedule.keepalive_hours` | `[22]` | 保活时点 |
| `schedule.balance_refresh_minutes` | `5` | 余额后台刷新间隔 |
| `pool.max_in_flight` | `3` | 单账号最大在途请求 |
| `pool.max_in_flight_global` | `2` | 国际版账号在途上限 |
| 单账号在途上限 | 跟随全局 | 不在 config 里：面板「账号」页每行「并发」按钮逐号覆盖，三态（跟随全局 / 不限 / 固定值），随 state 持久化，重启不丢 |
| `pool.prefer_expiring` | `true` | 快过期积分优先消耗 |
| `pool.expiring_soon` | `168h` | 「快过期」窗口 |
| `session_sticky.enabled` | `true` | 同一会话固定走同一账号（改这项**需重启**） |
| `session_sticky.ttl` | `30m` | 粘性会话有效期 |
| `upstream.timeout_seconds` | `120` | 上游请求超时 |
| `prompt.mode` | `passthrough` | 系统提示词模式，`passthrough` 原样透传 |
| `upstash.url` / `token` | 空 | 可选的 Upstash Redis，用于跨实例共享状态 |

完整字段见 [`config.example.json`](config.example.json)，逐项解释见 [`README.full.md`](README.full.md)。

服务端自己写的数据文件（都在 `state_file` 同目录，升级/换机时一起搬走，或用面板「更新与备份」整库打包）：

| 文件 | 内容 |
|------|------|
| `state.json` | 账号池状态（余额、冷却、连败、单账号并发上限覆盖） |
| `usage.json` | 逐请求用量，按小时与天分桶 |
| `journal.json` | 积分流水（确认事件 + 余额观察），上限 2 万条，超出裁最旧的 |
| `keys_quota.json` | 子密钥的每时 / 每日请求计数 |
| `output_probes.json` | 模型最大输出上限的探测结果缓存 |
| `model.json` | 模型能力清单缓存 |

**这是网关本地的并发保护，不代表上游配额。** 上游不会因为你在面板里把某个号设成 8 就给你 8 路；
设大了只会让更多请求撞到上游的 429 上。默认值（全局 3 / 国际版 2）是照着上游实际限流调出来的，
没有明确证据不要抬。

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