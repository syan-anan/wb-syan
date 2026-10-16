// Package clientprofile 客户端参数档案：UA / 版本号 / 风控头 / 桌面指纹的
// 「兜底 -> 保存 -> 环境变量」三层合成，供所有出站请求取用。
//
// 为什么要有这一层：这些值原本散落在配置字段与硬编码里（client_version、cli_version、
// client_name、user_agent…），一改就要动多处、还容易漏。更要紧的是**版本号与 UA 的
// 一致性**：UA、X-IDE-Version、指纹里的 ideVersion 必须报同一个版本 —— 官方客户端
// 三处同源，报错位就是一个可识别的差异。
//
// 三条硬规则（都是踩坑换来的，改之前先读）：
//
//  1. **UA 不落盘、始终现拼**。它 100% 由 desktop_version + cli_version + 产品名决定，
//     存下来必然出现「同步过一次 UA 之后，版本号再变而 UA 不变」的陈旧陷阱。
//     要整条覆盖 UA 只有环境变量一个入口。
//
//  2. **headers 逐层合并，不是整体替换**。后台只改 Accept-Language 时，其余默认风控头
//     必须还在；整层替换会让它们凭空消失。
//
//  3. **X-Product 是部署形态（SaaS），不是产品名**。回落到产品名正是老 bug —— 官方
//     客户端从不发那个值（官方是 X-Product 取 product.json 的 deploymentType）。
//
// 本包只依赖标准库：它是纯数据变换，引第三方只会增加供应链面。
package clientprofile

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// SourceAuto / SourceSaved 两个模式：决定「现场探测」与「后台保存」谁优先。
//
//   - auto ：探测优先（跟着客户端版本自动走）
//   - saved：保存优先（锁死在后台配置的那一份）
//
// 网关通常跑在服务器上、没有客户端可探测，两个模式结果相同；但语义要留对，
// 因为运维在装了客户端的机器上跑网关时 auto 才有意义。
const (
	SourceAuto  = "auto"
	SourceSaved = "saved"
)

// maxStr 字符串字段长度上限（防把整个文件塞进配置）；maxHeader 头名/值的上限。
const (
	maxStr    = 512
	maxHeader = 256
)

// strKeys / intKeys 键的分类（决定校验方式）。user_agent 刻意不在其中：见文件头规则 1。
var (
	strKeys = []string{
		"desktop_version", "cli_version", "turing_product_name",
		"ide_name", "ide_type", "product", "application_name", "application_name_global",
		"fp_product", "ext_name", "os", "arch", "os_version", "timezone", "commit",
	}
	intKeys = []string{
		"cpu_cores", "memory_size", "report_delay", "turing_channel_id", "release_date_ms",
	}
)

// Defaults 描述「一台 Windows 上的官方 WorkBuddy 桌面端」——这是**伪装目标**，
// 不是本机真实环境。网关跑在 Linux 上也照样报 win32，因为要看起来像官方 Windows 客户端。
func Defaults() map[string]any {
	return map[string]any{
		// 版本号默认值 = 本项目线上现行值（对齐官方 WorkBuddy Desktop 分发包 5.5.4）。
		// 参考实现的默认是 5.5.6；这里不跟 —— 版本号进 UA 与 X-IDE-Version，
		// 改默认等于在生产上做一次未经验证的指纹变更。要用更新版本：
		// 面板「客户端参数」改 desktop_version，或设 WB2A_DESKTOP_VERSION。
		"desktop_version": "5.5.4",
		"cli_version":     "2.137.1",

		"turing_product_name": "WorkBuddy",

		"ide_name": "WorkBuddy",
		"ide_type": "WorkBuddy",
		// X-Product。
		//
		// 参考实现实测：官方是 `X-Product ||= deploymentType ?? "SaaS"`，桌面端
		// product.json 的 deploymentType 就是 "SaaS"，所以**应该**填 SaaS，填产品名
		// 是官方从不发的值。
		//
		// 本项目默认仍填 "WorkBuddy"：这是现行线上值（实测可用），换值属于指纹变更，
		// 不在没有实测的前提下动它。要按参考实现的取法，把这里改成 "SaaS" 即可
		// （面板「客户端参数」页可直接改）。
		"product": "WorkBuddy",
		// UA 里的产品名（官方 product.json 的 applicationName）。
		"application_name": "WorkBuddy",
		// 国际版 UA 的产品名：官方 intl 包 productName 为 WorkBuddy AI。
		// 单独一个键而不是复用它 —— 送错平台段会触发上游 403 code 11140
		// request illegal，这条差异实测过，不能合并成同一个值。
		"application_name_global": "WorkBuddy AI",
		// 桌面事件指纹里的 product（遥测用，取 deploymentType）——
		// 与请求头的 X-Product 同源但不同用。
		"fp_product": "SaaS",

		// 风控/审计头（官方客户端所有 API 请求必带）。
		"headers": map[string]any{
			"X-CodeBuddy-Request": "1",
			"X-Requested-With":    "XMLHttpRequest",
			"Accept-Language":     "zh-CN",
			// 数据用途声明：官方每次模型请求都带。语义是「这份数据是否属于不可用于
			// 模型优化的私有数据」——优化开启（默认）则报 false。缺它就是一个可识别的
			// 差异；若部署方要求数据不参与优化，把它改成 true 即可。
			"X-Private-Data": "false",
		},

		// 桌面事件指纹。
		"ext_name":     "workbuddy-desktop",
		"os":           "win32",
		"arch":         "x64",
		"os_version":   "10.0.26220",
		"cpu_cores":    20,
		"memory_size":  24,
		"timezone":     "Asia/Shanghai",
		"report_delay": 2000,
	}
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneHeaders 深拷贝嵌套的 headers 层（它是唯一嵌套结构；浅拷贝会让调用方改脏缓存）。
func cloneHeaders(in map[string]any) map[string]any {
	out := make(map[string]any)
	if h, ok := in["headers"].(map[string]any); ok {
		for k, v := range h {
			out[k] = v
		}
	}
	return out
}

func intOrNone(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, t >= 0
	case int64:
		return int(t), t >= 0
	case float64:
		n := int(t)
		return n, t >= 0 && float64(n) == t
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, n >= 0
	}
	return 0, false
}

func toStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// Normalize 校验并归一化一份档案（后台保存 / 探测结果进内存前都过这里）。
//
// 只接受已知键，未知键被忽略并记一条说明 —— 前端多传字段不会写脏配置，
// 同时用户能看清哪个字段没被接受。返回的 errs 非空不代表失败：全部被拒才算失败。
func Normalize(raw map[string]any) (map[string]any, []string) {
	if raw == nil {
		return map[string]any{}, []string{"档案必须是 JSON 对象"}
	}
	var errs []string
	clean := map[string]any{}
	for _, k := range strKeys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		s := strings.TrimSpace(toStr(v))
		if s == "" {
			errs = append(errs, k+" 不能为空（不需要请删除该字段）")
			continue
		}
		if len(s) > maxStr {
			errs = append(errs, k+" 过长（超过 "+strconv.Itoa(maxStr)+" 字符）")
			continue
		}
		clean[k] = s
	}
	for _, k := range intKeys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		n, okInt := intOrNone(v)
		if !okInt {
			errs = append(errs, k+" 必须是非负整数")
			continue
		}
		clean[k] = n
	}
	if hv, ok := raw["headers"]; ok {
		m, isMap := hv.(map[string]any)
		if !isMap {
			errs = append(errs, "headers 必须是对象（header 名 -> 值）")
		} else {
			hh := map[string]any{}
			for hk, hval := range m {
				name := strings.TrimSpace(toStr(hk))
				val := strings.TrimSpace(toStr(hval))
				if name == "" || len(name) > maxHeader || len(val) > maxHeader {
					errs = append(errs, "header 名或值非法/过长（已忽略）")
					continue
				}
				hh[name] = val
			}
			clean["headers"] = hh
		}
	}
	if ua := strings.TrimSpace(toStr(raw["user_agent"])); ua != "" {
		// 存下来必然陈旧（见文件头规则 1），明确拒绝并给正确做法，
		// 比「静静存下但永不生效」好 —— 后者会让人以为改了却看不到效果。
		errs = append(errs, "user_agent 不可保存（由 desktop_version + cli_version + 产品名"+
			"自动派生，保存会变成陈旧值）。要整条覆盖请设环境变量 WB2A_USER_AGENT")
	}
	return clean, errs
}

// EnvFor 键 -> 环境变量名。WB2A_ 前缀是本项目的约定，同时兼容参考实现的
// WORKBUDDY_ 前缀（从参考项目迁过来时环境变量不用改）。
var EnvFor = map[string][]string{
	"desktop_version":     {"WB2A_DESKTOP_VERSION", "WORKBUDDY_DESKTOP_VERSION"},
	"cli_version":         {"WB2A_CLI_VERSION", "WORKBUDDY_CLI_VERSION"},
	"turing_channel_id":   {"WB2A_TURING_CHANNEL_ID", "WORKBUDDY_TURING_CHANNEL_ID"},
	"turing_product_name": {"WB2A_TURING_PRODUCT_NAME", "WORKBUDDY_TURING_PRODUCT_NAME"},
	"commit":              {"WB2A_COMMIT", "WORKBUDDY_COMMIT"},
	"release_date_ms":     {"WB2A_RELEASE_DATE_MS", "WORKBUDDY_RELEASE_DATE_MS"},
	"os":                  {"WB2A_OS", "WORKBUDDY_OS"},
	"arch":                {"WB2A_ARCH", "WORKBUDDY_ARCH"},
	"os_version":          {"WB2A_OS_VERSION", "WORKBUDDY_OS_VERSION"},
	"cpu_cores":           {"WB2A_CPU_CORES", "WORKBUDDY_CPU_CORES"},
	"memory_size":         {"WB2A_MEMORY_SIZE", "WORKBUDDY_MEMORY_SIZE"},
	"ide_name":            {"WB2A_IDE_NAME", "WORKBUDDY_IDE_NAME"},
	"ext_name":            {"WB2A_EXT_NAME", "WORKBUDDY_EXT_NAME"},
	"user_agent":          {"WB2A_USER_AGENT", "WORKBUDDY_USER_AGENT"},
}

// applyEnv 用环境变量覆盖档案对应键（只覆盖显式设了非空值的）。
//
// WB2A_USER_AGENT 是唯一的「整条 UA 覆盖」入口；其余情况 UA 都由版本号现拼，
// 避免出现「UA 报旧版本、X-IDE-Version 报新版本」的错位。
func applyEnv(p map[string]any, getenv func(string) string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for key, names := range EnvFor {
		raw := ""
		for _, n := range names {
			if v := strings.TrimSpace(getenv(n)); v != "" {
				raw = v
				break
			}
		}
		if raw == "" {
			continue
		}
		if isIntKey(key) {
			if n, ok := intOrNone(raw); ok {
				p[key] = n
			}
			continue
		}
		p[key] = raw
	}
}

func isIntKey(k string) bool {
	for _, ik := range intKeys {
		if ik == k {
			return true
		}
	}
	return false
}

// Compose 把三层（兜底 / 保存或探测 / 环境变量）合成最终生效档案。
//
// **层的顺序是这段代码里最容易写反的地方**：下面逐层覆盖，越靠后优先级越高，
// 所以优先级最高的那一层必须放在列表末尾。
//
//   - saved 模式：已保存 > 现场探测，列表写 [探测, 保存]
//   - auto  模式：现场探测 > 已保存，列表写 [保存, 探测]
//
// 写反的后果是「auto 模式下版本升级后自动跟上」这条承诺根本不成立 —— 只要保存过
// 一次，真实探测值就永远被那份可能已过期的档案压住。
func Compose(saved map[string]any, source string, detected map[string]any) map[string]any {
	out := cloneMap(Defaults())
	out["headers"] = cloneHeaders(Defaults())
	base := out["headers"].(map[string]any)

	layers := []map[string]any{saved, detected}
	if source == SourceSaved {
		layers = []map[string]any{detected, saved}
	}
	for _, layer := range layers {
		for k, v := range layer {
			if k == "headers" {
				if hm, ok := v.(map[string]any); ok {
					// 逐层合并：整层替换会让没被显式覆盖的默认风控头消失。
					for hk, hv := range hm {
						base[hk] = hv
					}
				}
				continue
			}
			if k == "user_agent" || v == nil {
				continue
			}
			// turing_channel_id / release_date_ms 的 0 视为「没探测到」。
			if (k == "turing_channel_id" || k == "release_date_ms") && toStr(v) == "0" {
				continue
			}
			out[k] = v
		}
	}
	applyEnv(out, nil)

	// 派生 UA：**始终**按版本号 + 产品名现拼（见文件头规则 1）。
	// 这里存的是 CN 形态；国际版的平台段在 UAByRealm 里按域切换。
	if strings.TrimSpace(toStr(out["user_agent"])) == "" {
		app := toStr(out["application_name"])
		out["user_agent"] = BuildUARealm(toStr(out["desktop_version"]), toStr(out["cli_version"]), app, app)
	}
	return out
}

// BuildUARealm 按官方三段式拼 UA：`<appName>/<v> <platform>/<v> CLI/<cli>`。
//
// 第一段固定是产品名（applicationName），第二段是**平台段**——CN 与产品名同值，
// 国际版是 `WorkBuddy AI`（官方 intl 包 productName）。第三段是内嵌 CLI 版本。
// 平台段送错会触发上游 403 code 11140 request illegal，所以两段分开传，
// 不合并成一个 brand 参数。
func BuildUARealm(desktop, cli, appName, platform string) string {
	v := strings.TrimSpace(desktop)
	if v == "" {
		v = toStr(Defaults()["desktop_version"])
	}
	c := strings.TrimSpace(cli)
	if c == "" {
		c = toStr(Defaults()["cli_version"])
	}
	a := strings.TrimSpace(appName)
	if a == "" {
		a = toStr(Defaults()["application_name"])
	}
	p := strings.TrimSpace(platform)
	if p == "" {
		p = a
	}
	return a + "/" + v + " " + p + "/" + v + " CLI/" + c
}

// BuildUA 两段同品牌的形态（参考实现的三段式）。
func BuildUA(desktop, cli, brand string) string {
	return BuildUARealm(desktop, cli, brand, brand)
}

// Effective 合成后档案的类型化只读视图（调用方唯一该用的入口）。
type Effective struct {
	DesktopVersion        string
	CLIVersion            string
	IDEName               string
	IDEType               string
	Product               string
	ApplicationName       string
	ApplicationNameGlobal string
	FPProduct             string
	TuringProductName     string
	TuringChannelID       int
	ExtName               string
	OS                    string
	Arch                  string
	OSVersion             string
	CPUCores              int
	MemorySize            int
	ReportDelay           int
	Timezone              string
	Commit                string
	ReleaseDateMS         int
	UserAgent             string
	// UserAgentOverride 环境变量整条指定了 UA（此时域无关，直接用它）。
	UserAgentOverride bool
	ExtraHeaders      map[string]string
	Raw               map[string]any
}

// Parse 把合成后的 map 转成类型化视图（缺项一律回落到内置默认，绝不返回空串）。
func Parse(m map[string]any) Effective {
	d := Defaults()
	pick := func(k string) string {
		if s := strings.TrimSpace(toStr(m[k])); s != "" {
			return s
		}
		return toStr(d[k])
	}
	pickInt := func(k string) int {
		if n, ok := intOrNone(m[k]); ok {
			return n
		}
		n, _ := intOrNone(d[k])
		return n
	}
	e := Effective{
		DesktopVersion:        pick("desktop_version"),
		CLIVersion:            pick("cli_version"),
		IDEName:               pick("ide_name"),
		IDEType:               pick("ide_type"),
		Product:               pick("product"),
		ApplicationName:       pick("application_name"),
		ApplicationNameGlobal: pick("application_name_global"),
		FPProduct:             pick("fp_product"),
		TuringProductName:     pick("turing_product_name"),
		TuringChannelID:       pickInt("turing_channel_id"),
		ExtName:               pick("ext_name"),
		OS:                    pick("os"),
		Arch:                  pick("arch"),
		OSVersion:             pick("os_version"),
		CPUCores:              pickInt("cpu_cores"),
		MemorySize:            pickInt("memory_size"),
		ReportDelay:           pickInt("report_delay"),
		Timezone:              pick("timezone"),
		Commit:                pick("commit"),
		ReleaseDateMS:         pickInt("release_date_ms"),
		UserAgent:             strings.TrimSpace(toStr(m["user_agent"])),
		ExtraHeaders:          map[string]string{},
		Raw:                   m,
	}
	if e.TuringChannelID <= 0 {
		e.TuringChannelID = 109144 // 官方 CN 渠道号
	}
	if h, ok := m["headers"].(map[string]any); ok {
		for k, v := range h {
			e.ExtraHeaders[k] = toStr(v)
		}
	}
	if e.UserAgent == "" {
		e.UserAgent = BuildUARealm(e.DesktopVersion, e.CLIVersion, e.ApplicationName, e.ApplicationName)
	} else if e.UserAgent != BuildUARealm(e.DesktopVersion, e.CLIVersion, e.ApplicationName, e.ApplicationName) {
		// 与派生值不同 = 来自环境变量的整条覆盖（保存路径已被 Normalize 拒掉）。
		e.UserAgentOverride = true
	}
	return e
}

// UAByRealm 按账号域返回出站 UA。
//
// 平台段（第二段）按 realm 切换：CN 用 application_name，国际版用 application_name_global。
// 送错平台段会触发上游 403 code 11140 request illegal —— 这条差异实测过，
// 所以两个键分开、默认值分别是 WorkBuddy 与 WorkBuddy AI。
func (e Effective) UAByRealm(global bool) string {
	if e.UserAgentOverride && e.UserAgent != "" {
		// 环境变量整条覆盖：最高优先级，与域无关。
		return e.UserAgent
	}
	platform := e.ApplicationName
	if global {
		platform = e.ApplicationNameGlobal
	}
	return BuildUARealm(e.DesktopVersion, e.CLIVersion, e.ApplicationName, platform)
}

// RiskHeaders 官方客户端的风控/审计头（不含 IP、不含账号级与会话级头）。
//
// 四个归属头先定，再从额外头里合并（setdefault 语义）：后加入的不能覆盖它们，
// 否则一个手滑的配置就能把 X-IDE-Name 改成风控头的值。
func (e Effective) RiskHeaders() map[string]string {
	name := e.IDEName
	if name == "" {
		name = toStr(Defaults()["ide_name"])
	}
	h := map[string]string{
		"X-IDE-Name":    name,
		"X-IDE-Type":    e.IDEType,
		"X-Product":     e.Product, // 部署形态，不能回落到 ide_name（那是产品名）
		"X-IDE-Version": e.DesktopVersion,
	}
	for k, v := range e.ExtraHeaders {
		if _, taken := h[k]; !taken {
			h[k] = v
		}
	}
	return h
}

// Fingerprint 桌面事件指纹（每个桌面事件都要带）。
// machineID / sessionID 由调用方按 uid 稳定派生后传入。
func (e Effective) Fingerprint(machineID, sessionID string) map[string]any {
	out := map[string]any{
		"timezone":    e.Timezone,
		"reportDelay": e.ReportDelay,
		"product":     e.FPProduct,
		"ideName":     e.IDEName,
		"ideType":     e.IDEType,
		"ideVersion":  e.DesktopVersion,
		"machineId":   machineID,
		"sessionId":   sessionID,
		"extName":     e.ExtName,
		"extVersion":  e.DesktopVersion,
		"os":          e.OS,
		"arch":        e.Arch,
		"osVersion":   e.OSVersion,
		"cpuCores":    e.CPUCores,
		"memorySize":  e.MemorySize,
	}
	if e.ReleaseDateMS > 0 {
		out["releaseDate"] = e.ReleaseDateMS
	}
	if e.Commit != "" {
		out["commit"] = e.Commit
	}
	return out
}

// Sources 每个键当前取自哪一层（排障用：面板上直接显示「这个值为什么是这个值」）。
func Sources(saved map[string]any, source string, detected map[string]any, getenv func(string) string) map[string]string {
	if getenv == nil {
		getenv = os.Getenv
	}
	out := map[string]string{}
	seen := map[string]bool{}
	mark := func(k string) {
		if seen[k] {
			return
		}
		seen[k] = true
		for _, env := range EnvFor[k] {
			if strings.TrimSpace(getenv(env)) != "" {
				out[k] = "环境变量 " + env
				return
			}
		}
		_, inSaved := saved[k]
		_, inDetected := detected[k]
		if source == SourceSaved {
			switch {
			case inSaved:
				out[k] = "已保存"
			case inDetected:
				out[k] = "现场探测"
			default:
				out[k] = "内置兜底"
			}
			return
		}
		switch {
		case inDetected:
			out[k] = "现场探测"
		case inSaved:
			out[k] = "已保存"
		default:
			out[k] = "内置兜底"
		}
	}
	for k := range Defaults() {
		mark(k)
	}
	for k := range saved {
		mark(k)
	}
	for k := range detected {
		mark(k)
	}
	out["source_mode"] = source
	return out
}

// Detect 现场探测本机客户端。
//
// 网关跑在服务器/容器里，**没有客户端安装目录可读**，所以恒返回空档案 —— 这是
// 正确结果而不是缺陷：参考实现的同一逻辑在服务器上同样为空，靠同步过来的档案工作。
// 空值时 auto 模式的探测层不起作用，生效值就是「保存 + 环境变量」，与服务器实际需求
// 一致。保留这个函数是为了两种模式的语义完整，以及将来在同一台机器上装了客户端时
// 能真的探测到东西。
func Detect() map[string]any { return map[string]any{} }

// ---------------------------------------------------------------------------
// 进程内共享持有者
// ---------------------------------------------------------------------------

// Holder 档案持有者：出站请求走它会拿到**已合成**的生效值，避免每个请求重算三层
// （探测 + 环境变量扫描 + 合成）。
//
// 一致性模型：写入方（面板保存 / 启动装载）持写锁整体替换并用 Compose 重算缓存；
// 读方（每个出站请求）持读锁取一份类型化快照。Effective 里的 Raw map 是共享引用，
// 调用方**不得修改**（只读视图的约定与 map 本身的可变性在这里只能靠纪律，
// 所以额外提供 Snapshot() 给需要改的场景）。
type Holder struct {
	mu       sync.RWMutex
	saved    map[string]any
	source   string
	detected map[string]any
	computed Effective
}

// NewHolder 构造一个空档案持有者（source=auto，无保存值 → 全内置兜底）。
func NewHolder() *Holder {
	h := &Holder{}
	h.Set(nil, SourceAuto)
	return h
}

// Set 整体替换保存值与模式，并重算生效档案（面板保存 / 启动装载的唯一入口）。
func (h *Holder) Set(saved map[string]any, source string) {
	if h == nil {
		return
	}
	if source != SourceSaved {
		source = SourceAuto
	}
	clean, _ := Normalize(saved)
	detected := Detect()

	h.mu.Lock()
	h.saved = clean
	h.source = source
	h.detected = detected
	h.computed = Parse(Compose(clean, source, detected))
	h.mu.Unlock()
}

// Saved 已保存层（副本；调用方改动不影响持有者）。
func (h *Holder) Saved() map[string]any {
	if h == nil {
		return map[string]any{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return cloneMap(h.saved)
}

// Source 当前模式。
func (h *Holder) Source() string {
	if h == nil {
		return SourceAuto
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.source
}

// Detected 现场探测层（服务器上恒为空）。
func (h *Holder) Detected() map[string]any {
	if h == nil {
		return map[string]any{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return cloneMap(h.detected)
}

// Effective 生效档案的类型化视图（每个出站请求都调它，所以只做值复制）。
func (h *Holder) Effective() Effective {
	if h == nil {
		return Parse(Compose(nil, SourceAuto, nil))
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.computed
}

// Snapshot 生效档案的 map 视图（副本；面板展示 / 排障用）。
func (h *Holder) Snapshot() map[string]any {
	if h == nil {
		return Compose(nil, SourceAuto, nil)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := cloneMap(h.computed.Raw)
	out["headers"] = cloneHeaders(h.computed.Raw)
	return out
}

// SourcesView 每个键取自哪一层（面板排障视图）。
func (h *Holder) SourcesView() map[string]string {
	if h == nil {
		return Sources(nil, SourceAuto, nil, nil)
	}
	h.mu.RLock()
	saved, source, detected := cloneMap(h.saved), h.source, cloneMap(h.detected)
	h.mu.RUnlock()
	return Sources(saved, source, detected, nil)
}
