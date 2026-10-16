// credential.go 面板「加密凭证（.info）」的导入 / 导出接口。
//
//	POST /panel/api/import/info            → 上传 .info / .jsonl，解密后入池
//	POST /panel/api/accounts/{uid}/export  → 导出该账号的加密 .info（文本）
//
// 解密能力来自运维配置的 key 文件（config.credential_key_file），与参考实现的
// WORKBUDDY_CREDENTIAL_KEY_FILE 同格式 —— 服务端没有客户端安装目录，取不到
// 客户端自带的 at-rest secret，只能由运维提供。没配 key 时明文 .info 照常能导入，
// 加密的会得到一条明确的可执行提示（而不是"格式错误"这种让人摸不着头脑的话）。
package panel

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/credential"
)

// loadCredentialKey 按配置路径加载密钥；未配置返回 (nil, nil) —— 明文导入仍可用。
// 每次调用都读一遍文件：key 文件只有百来字节，省掉缓存失效这一整类坑
// （运维轮换密钥后无需重启，也不会出现"缓存里还是旧 key"的幽灵故障）。
func (p *Panel) loadCredentialKey() (*credential.Key, error) {
	path := strings.TrimSpace(p.cfg.CredentialKeyFile)
	if path == "" {
		return nil, nil
	}
	k, err := credential.LoadKeyFile(path)
	if errors.Is(err, credential.ErrKeyMissing) {
		return nil, nil
	}
	return k, err
}

// importInfo 导入客户端 `.info`（单对象）或 `.jsonl`（每行一个对象）。
//
// 支持两种投递方式：multipart 的 file 字段（面板上传），或把内容直接放在请求体里
// （curl 粘贴）。逐条独立处理，单条失败不影响其它条目（批量导入最常见的就是
// "有几条是坏的"，整批回滚反而要把好的重新筛一遍）。
func (p *Panel) importInfo(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "import not available")
		return
	}
	raw, err := readImportBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := p.loadCredentialKey()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	items, err := splitCredentialItems(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, "没有解析到任何凭证条目")
		return
	}

	var imported, skipped int
	var errs []string
	for i, item := range items {
		plain, err := credential.Normalize(item, key)
		if err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("#%d: %v", i+1, err))
			continue
		}
		a, err := auth.Parse(plain)
		if err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("#%d: 凭证格式无效: %v", i+1, err))
			continue
		}
		if !validUID(a.UID) {
			skipped++
			errs = append(errs, fmt.Sprintf("#%d: uid 非法或缺失", i+1))
			continue
		}
		if _, err := os.Stat(p.cfg.AuthDir); err != nil {
			if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
				writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
				return
			}
		}
		a.FilePath = filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", a.UID))
		// realm 显式落盘（幂等）：与面板登录/ cockpit 导入同口径，auth 文件形态统一。
		if a.IsGlobal() {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("#%d: 设置 realm 失败: %v", i+1, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}
		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("#%d: 落盘失败: %v", i+1, err))
			continue
		}
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(a.UID)
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(a.UID, rm, tt)
		}
		log.Printf("panel: .info 导入成功 uid=%s nickname=%q realm=%s", a.UID, a.Nickname, a.Realm())
		imported++
	}
	log.Printf("panel: .info 导入完成 total=%d imported=%d skipped=%d", len(items), imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(items), "imported": imported, "skipped": skipped, "errors": errs,
	})
}

// readImportBody 取导入内容：优先 multipart 的 file 字段，其次原始请求体。
func readImportBody(r *http.Request) ([]byte, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, errors.New("parse form: " + err.Error())
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			return nil, errors.New("missing file field: " + err.Error())
		}
		defer file.Close()
		return io.ReadAll(file)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return nil, errors.New("read body: " + err.Error())
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, errors.New("请求体为空（上传 file 字段或直接放凭证内容）")
	}
	return b, nil
}

// splitCredentialItems 拆分导入内容：整体是一个 JSON 对象 → 单条；否则按行拆（.jsonl）。
// 空行与 `#` 注释行跳过（运维手写的清单里常见）。
func splitCredentialItems(raw []byte) ([][]byte, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
	if strings.HasPrefix(trimmed, "{") {
		// 多行但整体是单个对象（pretty-print 的 .info）——按完整 JSON 校验一次。
		var probe map[string]any
		if json.Unmarshal([]byte(trimmed), &probe) == nil && probe["auth"] != nil {
			return [][]byte{[]byte(trimmed)}, nil
		}
	}
	var out [][]byte
	sc := bufio.NewScanner(strings.NewReader(trimmed))
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, []byte(line))
	}
	if err := sc.Err(); err != nil {
		return nil, errors.New("读取内容失败: " + err.Error())
	}
	return out, nil
}

// exportCredential 导出单个账号的加密 `.info` 文本。
//
// 未配置密钥时直接拒绝：导出的意义就是把敏感字段封起来，退回明文等于把 token
// 以更"像文件"的形式交出去，比不导出更危险。
func (p *Panel) exportCredential(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "export not available")
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	key, err := p.loadCredentialKey()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if key == nil {
		writeErr(w, http.StatusBadRequest, "未配置凭证加密密钥（credential_key_file），无法导出加密 .info")
		return
	}

	// 直接从内存凭据构造，不读磁盘：磁盘文件可能是别的工具写的旧形态，
	// 而这里的语义是"把当前生效的凭据导出"。
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessTokenValue(),
			"refreshToken": a.RefreshTokenValue(),
			"expiresAt":    time.Now().Unix(),
			"domain":       a.DomainValue(),
			"realm":        a.Realm(),
		},
		"account": map[string]any{
			"uid":      a.UID,
			"nickname": a.Nickname,
		},
	}
	plain, err := json.Marshal(doc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	enc, err := credential.Encrypt(plain, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := fmt.Sprintf("workbuddy-%s.info", a.UID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(enc)
}
