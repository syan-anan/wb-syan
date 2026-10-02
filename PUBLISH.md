# 维护与发版手册

仓库：<https://github.com/syan-anan/wb-syan> ｜ 镜像：`ghcr.io/syan-anan/wb-syan`

---

## 一、日常更新流程

```bash
# 改代码 / 改文档
git add -A
git commit -m "feat: xxx"
git push
```

push 到 `main` 后 GitHub Actions 自动跑两条流水线：

| 工作流 | 干什么 |
|---|---|
| `docker-ghcr` | 多架构（amd64 + arm64）构建镜像并推送 `ghcr.io/syan-anan/wb-syan:latest` |
| `go-binaries` | 跑 `go test ./...` + 五平台编译验证（产物只存 artifact，不发 Release） |

镜像构建完约 5 分钟，之后 `docker compose pull && docker compose up -d` 即可升级。

## 二、发版本

```bash
git tag v1.11.10
git push origin v1.11.10
```

打 `v*` tag 会额外触发：

- `docker-ghcr` 推多标签镜像：`1.11.10` / `1.11` / `1` / `latest`
- `go-binaries` 出五平台二进制 + `checksums.txt` + 建 Release

> ⚠️ tag 版本号必须与 `cmd/server/main.go` 里的 `appVersion` 一致（工作流会断言，`-ci` 后缀的演练 tag 豁免）。
> `appVersion` 带 `-panel` 后缀，比对前会被剥掉。

## 三、镜像可见性

GHCR 包**继承仓库可见性**：仓库 public → 包自动 public，别人无需登录即可 `docker pull`。
如果哪天发现拉不动了，去 GitHub → 头像 → **Your packages** → `wb-syan` → **Package settings** → Change visibility → **Public**。

匿名验证（不需要 Docker）：

```bash
TOKEN=$(curl -s "https://ghcr.io/token?service=ghcr.io&scope=repository:syan-anan/wb-syan:pull" | sed 's/.*"token":"\([^"]*\)".*/\1/')
curl -s -H "Authorization: Bearer $TOKEN" \
     -H "Accept: application/vnd.oci.image.index.v1+json" \
     "https://ghcr.io/v2/syan-anan/wb-syan/manifests/latest" | head -c 300
```

## 四、本机网络注意

本机 `github.com:443`（git push / release 下载）**不通**，但 `api.github.com`、`ssh.github.com:443`、`github.com:22` 通。
所以：

- `git push` 走 **SSH**（remote 已配成 `git@github.com:syan-anan/wb-syan.git`），并且仓库里写死了
  `core.sshCommand = C:/WINDOWS/System32/OpenSSH/ssh.exe -o StrictHostKeyChecking=accept-new`
  （Codex 自带的那份 git 的 bundled ssh 读不到 known_hosts）。
- 下载 GitHub Release 资产用加速镜像：`https://gh-proxy.com/https://github.com/...`
- `gh` CLI 已装在 `%LOCALAPPDATA%\Programs\gh\bin`（winget 直连下载会失败，是手动解压装的），已 `gh auth login` 过。

## 五、发布前检查表

- [ ] `git ls-files` 里没有 `config.json` / `auths/` / `data/` / `.env`
- [ ] 仓库里没有真实 `api_key`、面板密码、服务器 IP、SSH 密码
- [ ] `LICENSE` 里保留了上游版权声明
- [ ] `go build ./...` 与 `go test ./...` 通过
- [ ] Actions 跑绿，`ghcr.io/syan-anan/wb-syan:latest` 能匿名 `docker pull`
- [ ] 换一台机器实测 `docker run` 能起来、面板能打开

## 六、合规提醒

这是对第三方商业服务的账号级网关。公开仓库有可能被上游以违反 ToS 为由投诉下架，镜像也可能被删。
建议保持低调、不要投放推广；仅自用的话把仓库设为 Private 也不影响本机 `docker compose up -d --build`。