# 发布到 GitHub · 操作清单

本目录已经是**可以直接推上去的仓库根目录**（本地 git 仓库已初始化并完成首次提交）。
按顺序做完下面 6 步即可。

---

## 0. 推送前自检（已完成，可复查）

```bash
git log --oneline          # 应有 1 条初始提交
git status --short         # 应为空
```

敏感文件已排除，确认一下这三个不存在于版本库里：

```bash
git ls-files | grep -Ei 'config\.json$|auths/|data/|\.env$' || echo "OK: 无凭据文件"
```

---

## 1. 把占位符换成你自己的地址

把 `syan-anan/workbuddy2api-panel` 全部替换成你的 GitHub 用户名 / 仓库名（`README.md`、`.env.example` 里有）：

```bash
# 在仓库根目录执行，OWNER/REPO 换成你的
grep -rl 'syan-anan/workbuddy2api-panel' . --exclude-dir=.git | xargs sed -i 's|syan-anan/workbuddy2api-panel|OWNER/REPO|g'
git add -A && git commit -m "chore: 填入仓库地址"
```

---

## 2. 在 GitHub 上建一个空仓库

- 打开 <https://github.com/new>
- Repository name 填 `workbuddy2api-panel`（随便，但要和上一步一致）
- **不要**勾选 Add a README / .gitignore / license（本地已经有了）
- 可见性：`Private` 或 `Public` 都行 —— 镜像能不能被别人拉取决于 **GHCR 包**的可见性，不是仓库

---

## 3. 推送

```bash
git remote add origin https://github.com/syan-anan/workbuddy2api-panel.git
git branch -M main
git push -u origin main
```

> 首次推送会要求登录。用 Personal Access Token（Settings → Developer settings →
> Personal access tokens → Fine-grained，勾 `Contents: Read and write`）当密码，
> 或者先装 GitHub CLI 再 `gh auth login`。

---

## 4. 等镜像自动构建

推上去后 `.github/workflows/docker-ghcr.yml` 会自动跑：

- 仓库页 → **Actions** → `docker-ghcr` → 看进度（首次约 5–15 分钟，arm64 走 QEMU 会慢）
- 构建完成后镜像地址：`ghcr.io/syan-anan/workbuddy2api-panel:latest`

也可以手动触发：Actions → docker-ghcr → **Run workflow**。

---

## 5. 【关键】把 GHCR 包设为公开

**默认是 private，别人拉不到。** 这一步不做，"一键拉取"就是空话。

GitHub → 右上角头像 → **Your packages** → 找到 `workbuddy2api-panel` →
右侧 **Package settings** → 拉到底 **Danger Zone** → **Change visibility** → **Public** → 输入包名确认。

验证（换台机器 / 退出登录后）：

```bash
docker pull ghcr.io/syan-anan/workbuddy2api-panel:latest
```

能拉下来就成功了。

---

## 6. 把地址告诉别人

别人只需要：

```bash
docker run -d --name wb2api --restart unless-stopped -p 7863:7863 \
  -e TZ=Asia/Shanghai \
  -v "$PWD/auths:/app/auths" -v "$PWD/data:/app/data" -v "$PWD/config.json:/app/config.json" \
  ghcr.io/syan-anan/workbuddy2api-panel:latest
```

（前置：先 `cp config.example.json config.json`、`mkdir -p auths data`）

或者直接用 compose：

```bash
curl -fsSLO https://raw.githubusercontent.com/syan-anan/workbuddy2api-panel/main/docker-compose.yml
curl -fsSL https://raw.githubusercontent.com/syan-anan/workbuddy2api-panel/main/config.example.json -o config.json
curl -fsSL https://raw.githubusercontent.com/syan-anan/workbuddy2api-panel/main/.env.example -o .env
mkdir -p auths data && docker compose up -d
```

---

## 发布前检查表

- [ ] `git ls-files` 里没有 `config.json` / `auths/` / `data/` / `.env`
- [ ] 仓库里没有真实 `api_key`、面板密码、服务器 IP、SSH 密码
- [ ] `LICENSE` 里保留了上游版权声明
- [ ] Actions 跑绿，`ghcr.io/syan-anan/workbuddy2api-panel:latest` 能 `docker pull`
- [ ] GHCR 包可见性已改成 **Public**
- [ ] 换一台机器实测 `docker run` 能起来、面板能打开

---

## 后续更新流程

```bash
# 改代码
git add -A && git commit -m "feat: xxx"
git push
# Actions 自动重建 latest；要发版就打 tag：
git tag v1.11.10 && git push origin v1.11.10
```

打 `v*` tag 会同时触发 `docker-ghcr.yml`（推 `1.11.10` / `1.11` / `1` / `latest` 多标签）
和 `go-binaries.yml`（五平台二进制 + Release + checksums）。

---

## 合规提醒

这是对第三方商业服务的账号级反向代理。公开仓库有可能被上游以违反 ToS 为由投诉下架，
GHCR 包也可能被删。建议：

- 仓库描述低调，不要写营销话术、不要投放到任何推广渠道；
- 如果你只是自己用，仓库设 `Private` 也不影响你本机 `docker compose up -d --build`；
- 真要给别人用，至少保留 LICENSE 归属声明。