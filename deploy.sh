#!/usr/bin/env bash
# wb-syan 一键部署脚本
#   bash <(curl -fsSL https://raw.githubusercontent.com/syan-anan/wb-syan/main/deploy.sh)
#   # 国内网络不通时走加速：
#   bash <(curl -fsSL https://ghproxy.net/https://raw.githubusercontent.com/syan-anan/wb-syan/main/deploy.sh)
#
# 可选环境变量：
#   PORT=18888 APP_DIR=/opt/wb-syan bash <(curl -fsSL ...)
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/syan-anan/wb-syan.git}"
APP_DIR="${APP_DIR:-/opt/wb-syan}"
PORT="${PORT:-18888}"
IMAGE="${IMAGE:-ghcr.io/syan-anan/wb-syan:latest}"

say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 1. 依赖检查 ----------
command -v docker >/dev/null 2>&1 || die "未检测到 docker，请先安装 Docker：https://docs.docker.com/engine/install/"
docker info >/dev/null 2>&1 || die "docker 守护进程不可用（当前用户可能没有权限，试试 sudo，或把用户加入 docker 组）"
docker compose version >/dev/null 2>&1 || die "未检测到 docker compose v2（注意是 'docker compose'，不是 'docker-compose'）"

# ---------- 2. 拉取 / 更新代码 ----------
if [ -d "$APP_DIR/.git" ]; then
  say "更新已有代码：$APP_DIR"
  git -C "$APP_DIR" pull --ff-only || say "git pull 失败，继续用当前代码"
else
  say "克隆代码到 $APP_DIR"
  command -v git >/dev/null 2>&1 || die "未检测到 git"
  mkdir -p "$(dirname "$APP_DIR")"
  git clone --depth 1 "$REPO_URL" "$APP_DIR"
fi
cd "$APP_DIR"

# ---------- 3. 准备配置与数据目录 ----------
[ -f config.json ] || { cp config.example.json config.json; say "已生成 config.json（请稍后修改 api_key）"; }
mkdir -p auths data

# ---------- 4. 生成 .env ----------
if [ ! -f .env ]; then
  cat > .env <<EOF
GHCR_IMAGE=$IMAGE
PANEL_PORT=$PORT
PUID=$(id -u)
PGID=$(id -g)
TZ=Asia/Shanghai
EOF
  say "已生成 .env（端口 $PORT，容器以 uid $(id -u) 运行）"
else
  sed -i "s|^PANEL_PORT=.*|PANEL_PORT=$PORT|" .env 2>/dev/null || true
  say "复用已有 .env"
fi

# ---------- 5. 启动（优先用预构建镜像，失败则本地构建）----------
if docker compose pull --quiet 2>/dev/null; then
  say "已拉取预构建镜像 $IMAGE"
  docker compose up -d
else
  say "预构建镜像不可用，改为本地构建（首次约 2-5 分钟）…"
  docker compose up -d --build
fi

# ---------- 6. 等待健康检查 ----------
say "等待服务就绪…"
ok=0
for _ in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] || say "健康检查未通过，请查看日志：docker compose logs -f --tail=100"

# ---------- 7. 输出接入信息 ----------
KEY="$(sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' config.json | head -n1)"
if [ -z "$KEY" ] || [ "$KEY" = "test_key" ]; then
  KEY="$(docker compose logs wb-syan 2>/dev/null | sed -n 's/.*api_key[^A-Za-z0-9]*\([A-Za-z0-9_-]\{12,\}\).*/\1/p' | tail -n1 || true)"
  [ -n "$KEY" ] && say "程序自动生成的 api_key 已从日志提取"
fi

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
echo "==================== 部署完成 ===================="
echo "  面板地址 : http://${IP:-<服务器IP>}:$PORT/panel/"
echo "  API 地址 : http://${IP:-<服务器IP>}:$PORT/v1"
echo "  健康检查 : curl http://127.0.0.1:$PORT/healthz"
echo "  配置目录 : $APP_DIR"
echo
if [ -n "${KEY:-}" ]; then
  echo "  API Key（同时也是面板登录密码）：$KEY"
else
  echo "  API Key：请编辑 $APP_DIR/config.json 的 api_key 字段，"
  echo "           或 docker compose logs wb-syan | grep -i api_key 查看自动生成的密钥"
fi
echo "=================================================="
echo
echo "别忘了放行防火墙：sudo ufw allow $PORT/tcp"
echo "下一步：打开面板 → 添加账号 → 把客户端 base_url 指向上面的 API 地址"