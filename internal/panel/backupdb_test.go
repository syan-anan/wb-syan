package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 整库备份 / 还原：包里必须带上凭证目录，还原时必须挡住越界路径与未知顶层名，
// 且覆盖前一定先留回滚点（先复制后修改是这行的硬规矩）。

type zipItem struct {
	name string
	body string
}

func makeZipTB(t *testing.T, items []zipItem) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, it := range items {
		fw, err := zw.Create(it.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(it.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newBackupPanel 造一个带 config/state/usage/auths 的临时工作区。
func newBackupPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(cfgPath, []byte(`{"api_key":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), []byte(`{"days":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	au := filepath.Join(dir, "auths", "u1")
	if err := os.MkdirAll(au, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(au, "token.json"), []byte(`{"refresh_token":"SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{Version: "test", APIKey: "k", ConfigPath: cfgPath, StateFile: statePath, AuthDir: filepath.Join(dir, "auths")})
	return p, dir
}

func grabBackup(t *testing.T, p *Panel) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq(httptest.NewRequest("GET", "/panel/api/maintenance/db_backup", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("备份应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("含凭证的响应必须 no-store，得到 %q", cc)
	}
	return rec.Body.Bytes()
}

func postBackup(t *testing.T, p *Panel, zipBody []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "backup.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBody); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := authReq(httptest.NewRequest("POST", "/panel/api/maintenance/db_restore", &buf))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func zipNames(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("备份不是有效 zip: %v", err)
	}
	out := map[string]string{}
	for _, fz := range zr.File {
		rc, err := fz.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[fz.Name] = string(body)
	}
	return out
}

func TestDBBackupMissingPathsReturns501(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq(httptest.NewRequest("GET", "/panel/api/maintenance/db_backup", nil)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未配置备份路径应 501，得到 %d", rec.Code)
	}
}

func TestDBBackupCarriesAuthsAndManifest(t *testing.T) {
	p, _ := newBackupPanel(t)
	got := zipNames(t, grabBackup(t, p))
	for _, want := range []string{"manifest.json", "config.json", "state.json", "usage.json", "auths/u1/token.json"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("备份缺少 %s，实得 %v", want, len(got))
		}
	}
	if got["auths/u1/token.json"] != `{"refresh_token":"SECRET"}` {
		t.Fatalf("凭证内容不对：%q", got["auths/u1/token.json"])
	}
	var mf dbBackupManifest
	if err := json.Unmarshal([]byte(got["manifest.json"]), &mf); err != nil {
		t.Fatalf("manifest 不可解析: %v", err)
	}
	if mf.Version != "test" {
		t.Fatalf("manifest 版本号应为 test，得到 %q", mf.Version)
	}
	// manifest 的文件列表必须真实列出内容文件，但不该把自身算进去。
	have := map[string]bool{}
	for _, f := range mf.Files {
		have[f] = true
	}
	if !have["config.json"] || !have["auths/u1/token.json"] || have["manifest.json"] {
		t.Fatalf("manifest.Files 不正确：%+v", mf.Files)
	}
}

func TestDBRestoreRoundTripKeepsRollback(t *testing.T) {
	p, dir := newBackupPanel(t)
	raw := grabBackup(t, p)

	// 换个目录当"新机"，只放一个不同的 config，验证整库覆盖。
	p2, dir2 := newBackupPanel(t)
	if err := os.WriteFile(filepath.Join(dir2, "config.json"), []byte(`{"api_key":"fresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := postBackup(t, p2, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("还原应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte(`"restored"`)) {
		t.Fatalf("响应缺少 restored 清单：%s", body)
	}
	got, err := os.ReadFile(filepath.Join(dir2, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"api_key":"old"}` {
		t.Fatalf("config 未被整库覆盖：%q", got)
	}
	// 覆盖前的内容必须进了 backups/restore-<stamp>/。
	matches, err := filepath.Glob(filepath.Join(dir2, "backups", "restore-*", "config.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("应生成一个还原前副本，得到 %v (%v)", matches, err)
	}
	old, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != `{"api_key":"fresh"}` {
		t.Fatalf("回滚点内容应是覆盖前的值，得到 %q", old)
	}
	_ = dir
}

func TestDBRestoreRejectsTraversal(t *testing.T) {
	p, dir := newBackupPanel(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	rec := postBackup(t, p, makeZipTB(t, []zipItem{
		{"../escaped.json", "PWNED"},
		{"config.json", `{"api_key":"x"}`},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界路径必须 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.json")); err == nil {
		t.Fatal("zip-slip：文件被写到了目录外")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if string(before) != string(after) {
		t.Fatalf("校验必须整体先于落盘，config 被改了：%q", after)
	}
}

func TestDBRestoreRejectsUnknownTopLevel(t *testing.T) {
	p, dir := newBackupPanel(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	rec := postBackup(t, p, makeZipTB(t, []zipItem{{"crontab.sh", "#!/bin/sh"}, {"config.json", `{"api_key":"x"}`}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知顶层名必须 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if string(before) != string(after) {
		t.Fatalf("未知文件混进来时不该动任何数据：%q", after)
	}
}

func TestDBRestoreEmptyPackageRejected(t *testing.T) {
	p, _ := newBackupPanel(t)
	rec := postBackup(t, p, makeZipTB(t, []zipItem{{"manifest.json", "{}"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("只有元数据的空包应 400，得到 %d", rec.Code)
	}
}

func TestMaintenanceCommandsShape(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq(httptest.NewRequest("GET", "/panel/api/maintenance/commands", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("命令页应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["deploy_dir"] != "/root/workbuddy2api" {
		t.Fatalf("默认部署目录不对：%v", out["deploy_dir"])
	}
	dk, ok := out["docker"].([]any)
	if !ok || len(dk) < 3 {
		t.Fatalf("docker 命令缺失：%v", out["docker"])
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("docker compose pull")) {
		t.Fatal("缺少 pull 命令")
	}
	// 红线原话必须在：别让面板把本地并发数当成上游配额。
	if !bytes.Contains(rec.Body.Bytes(), []byte("网关本地并发保护，不代表上游配额")) {
		t.Fatal("说明里丢了并发红线")
	}

	p2 := New(Config{Version: "test", APIKey: "k", DeployDir: "/srv/custom"})
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, authReq(httptest.NewRequest("GET", "/panel/api/maintenance/commands", nil)))
	if !bytes.Contains(rec2.Body.Bytes(), []byte("/srv/custom")) {
		t.Fatalf("DeployDir 未透进命令：%s", rec2.Body.String())
	}
}

// 命令页要能区分「已是最新 / 本地更新 / 有更新可升」，所以 current 与 ahead
// 必须由后端一起透出，前端不再自己比版本字符串。
func TestMaintenanceCommandsCarriesVersionDirection(t *testing.T) {
	srv, _ := updateStub(t, http.StatusOK, "v1.0.0", nil, http.StatusOK)
	p := New(Config{Version: "1.19.7-panel", APIKey: "k", UpdateCheck: true, UpdateRepo: "o/r"})
	p.upd.base = srv.URL
	p.upd.refresh()
	r := authReq(httptest.NewRequest("GET", "/panel/api/maintenance/commands", nil))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"ahead":true`) {
		t.Fatalf("缺少 ahead：%s", body)
	}
	if !strings.Contains(body, `"current":"1.19.7-panel"`) {
		t.Fatalf("缺少 current：%s", body)
	}
	if strings.Contains(body, `"has_update":true`) {
		t.Fatalf("线上更旧却报了有更新：%s", body)
	}
}
