// 整库备份 / 还原 / 升级命令（对齐上游 1.0.9 的 maintenance 模块）。
//
// 与 cmd/server 的「配置备份」（每次保存前复制 config.json）是两件事：
//   - 那个是单文件回滚点，自动发生、面向配置改坏；
//   - 这个是**整库导出**：config + state + usage + journal + 配额 + 模型缓存 + auths/，
//     面向换机、迁库、灾难恢复。用户点一次拿到一个 zip，还原时整包传回。
//
// 安全边界（必须保持）：
//  1. 下载响应 Cache-Control: no-store，临时文件写完即删；
//  2. zip 内含凭证，接口只允许面板凭据（withAuth 已挡子密钥）；
//  3. 还原前先把「将被覆盖的文件」复制到 backups/restore-<stamp>/（先复制后修改）；
//  4. 还原路径必须落在白名单内，拒绝绝对路径 / ".." / 未知顶层名（zip-slip）。
package panel

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dbBackupMaxUpload 还原包上限。整库（含 auths）通常 < 2MB，512MB 是防御性上限。
const dbBackupMaxUpload = 512 << 20

// dbBackupTop 允许出现在备份包顶层的名字（白名单，同时用于还原校验）。
var dbBackupTop = map[string]bool{
	"manifest.json":      true,
	"config.json":        true,
	"state.json":         true,
	"usage.json":         true,
	"journal.json":       true,
	"keys_quota.json":    true,
	"output_probes.json": true,
	"model.json":         true,
	"data":               true,
	"auths":              true,
}

type dbBackupManifest struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Files     []string  `json:"files"`
	Note      string    `json:"note"`
}

// dbStateSibling 与 cmd/server 的 stateSibling 同规则：state 同目录下的文件。
func dbStateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// deployDir 宿主机上的部署目录（docker compose 命令用）。
func (p *Panel) deployDir() string {
	if p.cfg.DeployDir != "" {
		return p.cfg.DeployDir
	}
	return "/root/workbuddy2api"
}

// dbBackup GET/POST /panel/api/maintenance/db_backup —— 整库 zip 下载。
func (p *Panel) dbBackup(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ConfigPath == "" || p.cfg.StateFile == "" {
		writeErr(w, http.StatusNotImplemented, "backup paths not configured")
		return
	}
	// 先落盘，保证导出的是「此刻」的流水与用量，而不是 30s 前的快照。
	if p.cfg.Journal != nil {
		p.cfg.Journal.Save()
	}
	if p.cfg.Usage != nil {
		p.cfg.Usage.Save()
	}
	tmp, err := os.CreateTemp("", "wb-syan-db-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建临时文件失败: "+err.Error())
		return
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name)
	}()
	zw := zip.NewWriter(tmp)
	manifest := dbBackupManifest{Version: p.cfg.Version, CreatedAt: time.Now()}
	add := func(arc, src string) error {
		raw, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		fw, err := zw.Create(arc)
		if err != nil {
			return err
		}
		if _, err := fw.Write(raw); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, arc)
		return nil
	}
	pairs := [][2]string{
		{"config.json", p.cfg.ConfigPath},
		{"state.json", p.cfg.StateFile},
		{"usage.json", dbStateSibling(p.cfg.StateFile, "usage.json")},
		{"journal.json", dbStateSibling(p.cfg.StateFile, "journal.json")},
		{"keys_quota.json", dbStateSibling(p.cfg.StateFile, "keys_quota.json")},
		{"output_probes.json", dbStateSibling(p.cfg.StateFile, "output_probes.json")},
		{"model.json", dbStateSibling(p.cfg.StateFile, "model.json")},
		{"data/qoder_checkin.json", filepath.Join("data", "qoder_checkin.json")},
	}
	for _, pr := range pairs {
		if err := add(pr[0], pr[1]); err != nil {
			writeErr(w, http.StatusInternalServerError, "打包 "+pr[0]+" 失败: "+err.Error())
			return
		}
	}
	// auths/：凭证目录，递归整包（还原后即可直接登录，无需重新扫码）。
	if p.cfg.AuthDir != "" {
		root := p.cfg.AuthDir
		_ = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
			if err != nil || fi == nil || fi.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			return add(filepath.ToSlash(filepath.Join("auths", rel)), path)
		})
	}
	manifest.Note = "整库备份：含凭证与余额状态，按机密文件保管"
	mf, err := json.MarshalIndent(manifest, "", "  ")
	if err == nil {
		if fw, cerr := zw.Create("manifest.json"); cerr == nil {
			_, _ = fw.Write(mf)
		}
	}
	if err := zw.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, "收尾 zip 失败: "+err.Error())
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeErr(w, http.StatusInternalServerError, "临时文件不可读: "+err.Error())
		return
	}
	fn := "wb-syan-backup-" + time.Now().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+fn+"\"")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, fn, time.Now(), tmp)
}

// dbRestore POST /panel/api/maintenance/db_restore —— 上传 zip 还原整库。
func (p *Panel) dbRestore(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ConfigPath == "" || p.cfg.StateFile == "" {
		writeErr(w, http.StatusNotImplemented, "backup paths not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, dbBackupMaxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "读取上传失败（上限 512MB）: "+err.Error())
		return
	}
	var src io.ReadCloser
	var hdrName string
	for _, field := range []string{"file", "backup", "zip"} {
		if f, h, err := r.FormFile(field); err == nil {
			src, hdrName = f, h.Filename
			break
		}
	}
	if src == nil {
		writeErr(w, http.StatusBadRequest, "缺少上传文件（字段名 file）")
		return
	}
	defer src.Close()
	tmp, err := os.CreateTemp("", "wb-syan-restore-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建临时文件失败: "+err.Error())
		return
	}
	tmpName := tmp.Name()
	defer func() { tmp.Close(); os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, src); err != nil {
		writeErr(w, http.StatusBadRequest, "写入临时文件失败: "+err.Error())
		return
	}
	zr, err := zip.OpenReader(tmpName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "不是有效的 zip: "+err.Error())
		return
	}
	defer zr.Close()

	// 先整体校验再落盘：坏包不动现有数据。
	type plan struct {
		z    *zip.File
		dest string
	}
	var plans []plan
	seenTop := map[string]bool{}
	cfgDir := filepath.Dir(p.cfg.ConfigPath)
	authRoot := p.cfg.AuthDir
	if authRoot == "" {
		authRoot = filepath.Join(cfgDir, "auths")
	}
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if strings.HasSuffix(name, "/") {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(name))
		if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || filepath.IsAbs(clean) {
			writeErr(w, http.StatusBadRequest, "非法路径: "+f.Name)
			return
		}
		top := strings.SplitN(clean, "/", 2)[0]
		if !dbBackupTop[top] {
			writeErr(w, http.StatusBadRequest, "备份包含未知文件: "+f.Name)
			return
		}
		seenTop[top] = true
		var dest string
		switch {
		case top == "auths":
			rel := strings.TrimPrefix(clean, "auths/")
			if rel == "" || rel == clean {
				continue
			}
			dest = filepath.Join(authRoot, filepath.FromSlash(rel))
		case top == "data":
			dest = filepath.Join(filepath.Dir(p.cfg.ConfigPath), filepath.FromSlash(clean))
		case clean == "config.json":
			dest = p.cfg.ConfigPath
		case clean == "state.json":
			dest = p.cfg.StateFile
		case clean == "usage.json", clean == "journal.json", clean == "keys_quota.json",
			clean == "output_probes.json", clean == "model.json":
			dest = dbStateSibling(p.cfg.StateFile, clean)
		default:
			continue // manifest.json 等只读元数据，不落盘
		}
		if !withinRoot(cfgDir, dest) && !withinRoot(authRoot, dest) && !withinRoot(filepath.Dir(p.cfg.StateFile), dest) {
			writeErr(w, http.StatusBadRequest, "目标路径越界: "+dest)
			return
		}
		plans = append(plans, plan{z: f, dest: dest})
	}
	if len(plans) == 0 {
		writeErr(w, http.StatusBadRequest, "备份中没有可还原的文件")
		return
	}
	// 先复制后修改：把将被覆盖的现有文件整体备份到 backups/restore-<stamp>/。
	stamp := time.Now().Format("20060102-150405")
	safeDir := filepath.Join(cfgDir, "backups", "restore-"+stamp)
	if err := os.MkdirAll(safeDir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建安全备份目录失败: "+err.Error())
		return
	}
	restored := make([]string, 0, len(plans))
	for _, pl := range plans {
		raw, err := os.ReadFile(pl.dest)
		if err != nil {
			continue
		}
		rel, rerr := filepath.Rel(cfgDir, pl.dest)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			rel = "auths/" + filepath.Base(pl.dest)
		}
		dst := filepath.Join(safeDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			continue
		}
		_ = os.WriteFile(dst, raw, 0o600)
	}
	for _, pl := range plans {
		if err := os.MkdirAll(filepath.Dir(pl.dest), 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, "创建目录失败: "+err.Error())
			return
		}
		rc, err := pl.z.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "读取 "+pl.z.Name+" 失败: "+err.Error())
			return
		}
		raw, err := io.ReadAll(io.LimitReader(rc, dbBackupMaxUpload))
		rc.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "解压 "+pl.z.Name+" 失败: "+err.Error())
			return
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(pl.dest, cfgDir+string(os.PathSeparator)+"data") {
			mode = 0o644
		}
		if err := os.WriteFile(pl.dest, raw, mode); err != nil {
			writeErr(w, http.StatusInternalServerError, "写入 "+pl.dest+" 失败: "+err.Error())
			return
		}
		restored = append(restored, pl.z.Name)
	}
	sort.Strings(restored)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"restored":     restored,
		"count":        len(restored),
		"backup_dir":   safeDir,
		"upload_name":  hdrName,
		"restart_hint": "还原已写入磁盘。请立即重启容器让新库生效（运行中的进程仍持有旧 state，重启前的余额刷新会覆盖 state.json）。",
		"top_seen":     len(seenTop),
	})
}

// maintenanceCommands GET /panel/api/maintenance/commands —— 升级 / 重启命令页。
func (p *Panel) maintenanceCommands(w http.ResponseWriter, r *http.Request) {
	info := updateInfo{}
	if p.upd != nil {
		info = p.upd.snapshot()
	}
	dir := p.deployDir()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     p.cfg.Version,
		"latest":      info.Latest,
		"has_update":  info.HasUpdate,
		"current":     info.Current,
		"ahead":       info.Ahead,
		"release_url": info.URL,
		"checked_at":  info.CheckedAt,
		"deploy_dir":  dir,
		"docker": []string{
			"cd " + dir + " && docker compose pull",
			"cd " + dir + " && docker compose up -d",
			"cd " + dir + " && docker compose restart",
			"cd " + dir + " && docker compose logs -f --tail 100",
		},
		"linux": []string{
			"cd " + dir + " && systemctl stop wb-syan",
			"cd " + dir + " && ./bin/wb-syan -config config.json",
			"cd " + dir + " && systemctl start wb-syan",
			"cd " + dir + " && journalctl -u wb-syan -f -n 100",
		},
		"notes": []string{
			"网关本地并发保护，不代表上游配额。",
			"所有命令都在宿主机执行；面板容器内的 /app 不等于宿主机路径。",
			"还原整库后立即重启，避免运行中的进程把旧 state 写回磁盘。",
		},
	})
}

// withinRoot 判断 path 是否落在 root 目录内（按路径段，不做字符串前缀匹配）。
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// humanSize 备份列表用的小工具。
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// panelStatus GET /panel/api/status —— 上游与路由快照（面板凭据）。
//
// 数据本身来自 server.Handler.StatusSummary，由 main 注入闭包递过来；
// 不让前端直接 fetch /status，是因为那需要网关密钥，而面板只有面板凭据。
func (p *Panel) panelStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.StatusSummary == nil {
		writeErr(w, http.StatusNotImplemented, "status summary not wired")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.StatusSummary())
}
