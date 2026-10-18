package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chatRow 拼一条 server/logging.go logChatRow 形态的对话表格行（分类主口径）。
func chatRow(seq, status, acct, model string) string {
	return "| #" + seq + " | 12:00:0" + seq[0:1] + " | " + model + " | stream | " + status +
		" | " + acct + " | TTFB=123ms | tok=254 | 548.6tok/s | total=5.2s | cr=0.0000 |"
}

func TestClassifyLogRowChatStatus(t *testing.T) {
	tests := []struct {
		name, status, want string
	}{
		{"2xx", "200", LogOK},
		{"429 记警告不记失败", "429", LogWarn},
		{"5xx", "503", LogErr},
	}
	for _, tc := range tests {
		e := LogEntry{Ch: ChChat, Text: chatRow("12", tc.status, "小明(a1b2c3d4)", "cn:deepseek-v4.1-flash")}
		info := classifyLogRow(e)
		if info.Res != tc.want {
			t.Errorf("%s: res=%s want %s", tc.name, info.Res, tc.want)
		}
	}
	e := LogEntry{Ch: ChChat, Text: chatRow("12", "200", "小明(a1b2c3d4)", "cn:deepseek-v4.1-flash")}
	info := classifyLogRow(e)
	if info.Account != "小明" || info.Model != "cn:deepseek-v4.1-flash" || info.Status != 200 {
		t.Errorf("字段提取错误: %+v", info)
	}
}

func TestClassifyLogRowLevelAndPhrase(t *testing.T) {
	tests := []struct {
		text, want string
	}{
		{"WARN: config backup failed: open x: denied", LogWarn}, // 级别前缀优先于「失败」短语
		{"ERROR: boom", LogErr},
		{"balance 成功 12 失败 0", LogInfo}, // 0 计数不算失败
		{"panel: 任务结果 成功 3 失败 2", LogErr},
		{"上游拒绝 · 账号 小明 · 上游返回 400", LogErr},
		{"模型被限流，进入冷却", LogWarn},
		{"checkin ok", LogInfo},
	}
	for _, tc := range tests {
		got := classifyLogRow(LogEntry{Ch: ChSys, Text: tc.text}).Res
		if got != tc.want {
			t.Errorf("%q: res=%s want %s", tc.text, got, tc.want)
		}
	}
}

func TestClassifyLogRowUpstreamDiagnose(t *testing.T) {
	bad := LogEntry{Ch: ChSys, Text: `[upstream] chat_stream acct=xiaoming(a1b2c3d4): upstream 400 client body={"code":11133,"msg":"bad"}`}
	info := classifyLogRow(bad)
	if info.Res != LogErr || info.Status != 400 || info.Account != "xiaoming" {
		t.Errorf("400 诊断行: %+v", info)
	}
	limit := LogEntry{Ch: ChSys, Text: `[upstream] chat_stream acct=x(a1): upstream 429 rate body={"code":1,"msg":"qpm limit"}`}
	if got := classifyLogRow(limit); got.Res != LogWarn {
		t.Errorf("429 诊断行应记警告: %+v", got)
	}
}

func TestBuildLogViewFilterAndLimit(t *testing.T) {
	entries := []LogEntry{
		{TS: time.Unix(1, 0), Ch: ChChat, Text: chatRow("1", "200", "A(aa)", "m1")},
		{TS: time.Unix(2, 0), Ch: ChChat, Text: chatRow("2", "429", "B(bb)", "m2")},
		{TS: time.Unix(3, 0), Ch: ChTask, Text: "checkin ok"},
		{TS: time.Unix(4, 0), Ch: ChSys, Text: "ERROR: 刷新失败"},
	}
	// 无筛选：全量、统计齐全
	rows, stats, trunc := buildLogView(entries, parseLogFilter(httptest.NewRequest("GET", "/panel/api/logs", nil)))
	if len(rows) != 4 || trunc {
		t.Fatalf("默认应返回全部 4 行: rows=%d trunc=%v", len(rows), trunc)
	}
	if stats.Total != 4 || stats.OK != 1 || stats.Warn != 1 || stats.Err != 1 || stats.Info != 1 {
		t.Errorf("统计错误: %+v", stats)
	}
	// 结果档位筛选
	f := parseLogFilter(httptest.NewRequest("GET", "/panel/api/logs?result=err", nil))
	if rows, _, _ := buildLogView(entries, f); len(rows) != 1 || rows[0].Ch != ChSys {
		t.Errorf("result=err 应只剩系统错误行: %+v", rows)
	}
	// 账号 + 频道组合
	f = parseLogFilter(httptest.NewRequest("GET", "/panel/api/logs?ch=chat&account=b", nil))
	if rows, _, _ := buildLogView(entries, f); len(rows) != 1 || rows[0].Account != "B" {
		t.Errorf("ch=chat&account=b: %+v", rows)
	}
	// limit 截尾保最新
	f = parseLogFilter(httptest.NewRequest("GET", "/panel/api/logs?limit=2", nil))
	rows, _, trunc = buildLogView(entries, f)
	if !trunc || len(rows) != 2 || rows[0].Text != entries[2].Text {
		t.Errorf("limit=2 应保留最后两行: rows=%d trunc=%v", len(rows), trunc)
	}
	// limit 非法值退回默认
	f = parseLogFilter(httptest.NewRequest("GET", "/panel/api/logs?limit=abc", nil))
	if f.Limit != defaultLogLimit {
		t.Errorf("非法 limit 未回默认: %d", f.Limit)
	}
}

func TestLogDetailChatAndDiagnose(t *testing.T) {
	e := LogEntry{Ch: ChChat, Text: chatRow("9", "200", "小明(a1b2c3d4)", "cn:glm-5.2")}
	info := classifyLogRow(e)
	d := logDetail(e, info)
	m := map[string]string{}
	for _, kv := range d {
		m[kv.K] = kv.V
	}
	for _, k := range []string{"频道", "结果", "序号", "模型", "方式", "状态", "账号", "首字", "输出", "吞吐", "总耗时", "扣分"} {
		if m[k] == "" {
			t.Errorf("对话行明细缺字段 %q: %+v", k, d)
		}
	}
	de := LogEntry{Ch: ChSys, Text: `[upstream] chat_stream acct=xiaoming(a1b2c3d4): upstream 400 client body={"code":11133,"msg":"bad"}`}
	dm := map[string]string{}
	for _, kv := range logDetail(de, classifyLogRow(de)) {
		dm[kv.K] = kv.V
	}
	if dm["本网关判断"] == "" || dm["上游原文"] == "" {
		t.Errorf("诊断行明细缺三层: %+v", dm)
	}
}

func TestLogSanitize(t *testing.T) {
	long := strings.Repeat("a1b2c3", 8) // 48 位十六进制
	lines := []string{
		"request Authorization: Bearer deadbeefcafe1234",
		"gateway key sk-abcdefghijklmnop used",
		"body {\"access_token\": \"abc.def.ghi\", \"model\": \"x\"}",
		"proxy http://user:sup3rsecret@host:8080/pool",
		"uid " + long + " saved",
	}
	wants := []string{"deadbeefcafe1234", "sk-abcdefghijklmnop", "abc.def.ghi", "sup3rsecret", long}
	for i, s := range lines {
		got := logSanitize(s)
		if strings.Contains(got, wants[i]) {
			t.Errorf("第 %d 行未脱敏: %s", i, got)
		}
		if !strings.Contains(got, "********") {
			t.Errorf("第 %d 行缺掩码: %s", i, got)
		}
	}
	// 键名/结构必须保留（排障要看是哪个字段）
	if got := logSanitize(lines[2]); !strings.Contains(got, "access_token") || !strings.Contains(got, "\"model\": \"x\"") {
		t.Errorf("JSON 键名结构丢失: %s", got)
	}
	// 不含敏感信息时原样返回
	if got := logSanitize("checkin ok 成功 3 失败 0"); got != "checkin ok 成功 3 失败 0" {
		t.Errorf("正常行被改动: %s", got)
	}
}
func TestBuildLogExport(t *testing.T) {
	entries := []LogEntry{
		{TS: time.Unix(100, 0), Ch: ChChat, Text: chatRow("7", "200", "小明(a1b2c3d4)", "cn:glm-5.2")},
		{TS: time.Unix(101, 0), Ch: ChSys, Text: "ERROR: 刷新失败 Bearer topsecretvalue123"},
	}
	f := logFilter{Limit: defaultLogLimit}
	rows, stats, trunc := buildLogView(entries, f)
	out := buildLogExport(rows, stats, f, trunc, "1.19.5-panel")
	for _, want := range []string{
		"# wb-syan 运行日志导出", "# 版本: 1.19.5-panel", "# 全环统计: 成功 1",
		"已脱敏", "# 筛选条件:", "[对话] [成功]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("导出缺 %q，全文:\n%s", want, out)
		}
	}
	if strings.Contains(out, "topsecretvalue123") {
		t.Errorf("导出未脱敏:\n%s", out)
	}
}

func TestLogsHandlerAndExportEndpoints(t *testing.T) {
	p := newTestPanel()
	p.Logs().Write([]byte(chatRow("3", "200", "A(aa)", "m1") + "\n" + "ERROR: 队列失败 sk-abcdefghijk0123\n"))
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer test-key")
		return req
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, auth(httptest.NewRequest("GET", "/panel/api/logs?result=err", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("logs 状态 %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Entries  []logViewRow `json:"entries"`
		Stats    logStats     `json:"stats"`
		Filtered int          `json:"filtered"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Filtered != 1 || len(got.Entries) != 1 || got.Entries[0].Res != LogErr {
		t.Errorf("筛选 result=err 应命中 1 行: %+v", got)
	}
	if got.Stats.Total != 2 || got.Stats.OK != 1 {
		t.Errorf("统计应走全环: %+v", got.Stats)
	}
	if len(got.Entries[0].Detail) == 0 {
		t.Errorf("行明细缺失")
	}
	if got.Entries[0].Text == "" || got.Entries[0].Ch == "" {
		t.Errorf("旧字段丢失: %+v", got.Entries[0])
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, auth(httptest.NewRequest("GET", "/panel/api/logs/export", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("export 状态 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("导出 Content-Type=%q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("导出 Content-Disposition=%q", cd)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-abcdefghijk0123") || !strings.Contains(body, "********") {
		t.Errorf("导出密钥未抹掉:\n%s", body)
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, auth(httptest.NewRequest("GET", "/panel/api/logs/export?format=json", nil)))
	if strings.Contains(rec.Body.String(), "sk-abcdefghijk0123") {
		t.Errorf("JSON 导出未脱敏")
	}
}
