#!/usr/bin/env bash
# check_selftest.sh —— 部署后自检（只读探测，不写任何状态）
#
# 用法：
#   WBSY_BASE=http://127.0.0.1:18888 WBSY_TOKEN=<面板密码或密钥> ./scripts/check_selftest.sh
#   ./scripts/check_selftest.sh --live     # 额外真跑三协议各一次（会消耗少量积分）
#
# 凭据只从环境变量读取：脚本绝不读磁盘上的 config.json / state.json / auths/，
# 也不打印 token。这样这份脚本可以随手丢进 CI 或拷给别人，不会带走任何密钥。
#
# 退出码：0 = 全部通过；非 0 = 失败项数（上限 255）。
set -u

BASE="${WBSY_BASE:-http://127.0.0.1:18888}"
TOKEN="${WBSY_TOKEN:-}"
# 数据面（/v1/models、/v1/chat/completions 等）认网关 api_key 或子密钥；
# 面板认 panel.password。两者配成不同值时是两把钥匙，不能混用。
GKEY="${WBSY_API_KEY:-}"
LIVE=0
[ "${1:-}" = "--live" ] && LIVE=1

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }
skip() { printf 'SKIP  %s\n' "$1"; }

have() { command -v curl >/dev/null 2>&1; }
if ! have; then
  echo "FAIL  需要 curl（apt/apk install curl）"
  exit 1
fi

# code <method> <path> [body] -> 打印 HTTP 状态码
code() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-s -o /dev/null -w '%{http_code}' -m 20 -X "$method" "$BASE$path")
  local tok="${AUTH_TOK:-$TOKEN}"
  [ -n "$tok" ] && args+=(-H "Authorization: Bearer $tok")
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi
  curl "${args[@]}" 2>/dev/null || echo "000"
}

# body <method> <path> [body] -> 打印响应体
body() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-s -m 25 -X "$method" "$BASE$path")
  local tok="${AUTH_TOK:-$TOKEN}"
  [ -n "$tok" ] && args+=(-H "Authorization: Bearer $tok")
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi
  curl "${args[@]}" 2>/dev/null
}

check() {
  local label="$1" path="$2" want="$3"
  local got
  got="$(code GET "$path")"
  if [ "$got" = "$want" ]; then
    pass "$label  ($path -> $got)"
  else
    fail "$label  ($path -> $got，期望 $want)"
  fi
}

printf '== 部署后自检 ==\nbase=%s  token=%s  live=%s\n\n' "$BASE" "$([ -n "$TOKEN" ] && echo given || echo none)" "$LIVE"

# 1) 存活
check "健康检查" "/healthz" "200"

# 2) 鉴权：没有 token 时面板接口必须 401（用临时 curl 不带 Authorization）
if command -v curl >/dev/null 2>&1; then
  noauth="$(curl -s -o /dev/null -w '%{http_code}' -m 15 "$BASE/panel/api/overview" 2>/dev/null || echo 000)"
  case "$noauth" in
    401|403) pass "未授权访问被拒（$noauth）" ;;
    200) fail "未授权却能读面板接口（200）——检查 panel.password / api_key" ;;
    *) skip "未授权探测返回 $noauth（服务未起或路径变化）" ;;
  esac
fi

# 3) 版本
ver="$(body GET /panel/api/overview | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -1)"
if [ -n "$ver" ]; then pass "版本可读：v$ver"; else fail "读不到 /panel/api/overview 的 version 字段"; fi

# 4) 数据面端点（只读，不消耗积分）。Anthropic / Responses 协议复用同一个 /v1/models，
#    真正区分的是 POST 路径，见 --live。
if [ -z "$GKEY" ]; then
  skip "数据面探测（需要 WBSY_API_KEY=网关 api_key 或子密钥；面板密码不通用）"
else
  AUTH_TOK="$GKEY"
  for ep in /v1/models /status; do
    c="$(code GET "$ep")"
    case "$c" in
      200) pass "数据面 $ep -> 200" ;;
      401|403) fail "数据面 $ep 鉴权失败（$c）——检查 WBSY_API_KEY" ;;
      *) fail "数据面 $ep -> $c" ;;
    esac
  done
  n="$(body GET /v1/models | grep -o '"id":"' | wc -l | tr -d ' ')"
  if [ "${n:-0}" != "0" ]; then pass "模型列表有 $n 个模型"; else fail "/v1/models 返回 200 但模型数为 0（池里没有可用账号？）"; fi
  AUTH_TOK=""
fi

# 5) 本轮新增的面板接口必须存在（404 = 路由没注册 = 二进制不是这一版）
for ep in /panel/api/journal /panel/api/status /panel/api/maintenance/commands; do
  c="$(code GET "$ep")"
  case "$c" in
    200|400) pass "接口存在 $ep -> $c" ;;
    401|403) skip "$ep 需要鉴权（$c），带上 WBSY_TOKEN 再跑一次" ;;
    404) fail "$ep -> 404：路由未注册，二进制版本不对" ;;
    *) fail "$ep -> $c" ;;
  esac
done

# 5b) 整库备份接口（只探存在性，不下载：下载会带走凭证）
bc="$(code POST /panel/api/maintenance/db_backup "{}")"
case "$bc" in
  200) pass "整库备份可出包（POST db_backup -> 200）" ;;
  401|403) skip "整库备份需鉴权（$bc）" ;;
  501) fail "整库备份返回 501：备份路径未装配（检查 main 是否注入 config/state）" ;;
  *) fail "整库备份 -> $bc" ;;
esac

# 6) 流水接口真取一次数据（只读）
js="$(body GET "/panel/api/journal?page=1&size=5")"
case "$js" in
  *'"total"'*) pass "积分流水可读（total 字段在）" ;;
  *) fail "积分流水响应异常：$(printf '%s' "$js" | head -c 160)" ;;
esac
case "$js" in
  *'confirmed_total'*) pass "流水两套账小计在（confirmed_total / observed_total）" ;;
  *) fail "流水缺小计字段：前端两张卡会空" ;;
esac

# 6b) 上游与路由快照：必须拿面板凭据就能读到。
#     数据面 /status 认的是网关 api_key，面板凭据与主密钥配成不同值是常态 ——
#     这里 501 或缺字段就说明装配断了，面板的「上游与路由」卡会变空白。
ss="$(body GET /panel/api/status)"
case "$ss" in
  *'"upstreams"'*) pass "路由快照可读（面板凭据即可，upstreams 字段在）" ;;
  *'"status summary not wired"'*) fail "路由快照未装配：main 没把 handler.StatusSummary 接进面板" ;;
  *) fail "路由快照响应异常：$(printf '%s' "$ss" | head -c 160)" ;;
esac

# 7) 可选：三协议各一发（消耗积分，默认不跑）
if [ "$LIVE" = "1" ]; then
  AUTH_TOK="$GKEY"
  if [ -z "$GKEY" ]; then
    skip "--live 需要 WBSY_API_KEY（数据面凭据）"
  else
    m="$(body GET /v1/models | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)"
    [ -n "$m" ] || m="deepseek-v4.1-flash"
    oc="$(code POST /v1/chat/completions "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"say ok\"}],\"max_tokens\":8}")"
    [ "$oc" = "200" ] && pass "OpenAI 协议 chat 通（$m）" || fail "OpenAI 协议 chat -> $oc"
    an="$(code POST /v1/messages "{\"model\":\"$m\",\"messages\":[{\"role\":\"user\",\"content\":\"say ok\"}],\"max_tokens\":8}")"
    [ "$an" = "200" ] && pass "Anthropic 协议 messages 通" || fail "Anthropic 协议 -> $an"
    rs="$(code POST /v1/responses "{\"model\":\"$m\",\"input\":\"say ok\",\"max_output_tokens\":8}")"
    case "$rs" in
      200) pass "Responses 协议通" ;;
      404) skip "Responses 协议 -> 404（未开启）" ;;
      *) fail "Responses 协议 -> $rs" ;;
    esac
  fi
else
  skip "三协议实际调用（加 --live 打开，会消耗少量积分）"
fi

printf '\n== 结果：%s ==\n' "$([ "$fails" -eq 0 ] && echo '全部通过' || echo "$fails 项失败")"
[ "$fails" -gt 255 ] && fails=255
exit "$fails"