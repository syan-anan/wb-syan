// qoder_accounts.go —— Qoder 账号的落盘与热加载。
//
// 磁盘真相只有一处：data/qoder_accounts.json。config.json 只在文件不存在时
// 作为初始来源。任何写操作都走「读文件 → 改 → 原子写 → Replace 内存池」，
// 不重启，也不会让两处状态分叉。
package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/syan-anan/wb-syan/internal/upstream"
)

const qoderAccountsFile = "data/qoder_accounts.json"

// seedQoderAccounts 在文件还没生成时，把 config.json 里配的账号搬过来，
// 避免第一次从面板加号时把启动配置里的号弄丢。
func seedQoderAccounts(cfg *Config) []upstream.QoderAccount {
	var out []upstream.QoderAccount
	for _, a := range cfg.Qoder.Accounts {
		out = append(out, upstream.QoderAccount{
			Name:      a.Name,
			PAT:       a.PersonalAccessToken,
			TokenFile: a.TokenFile,
			Realm:     a.Realm,
			UID:       a.UID,
			Nickname:  a.Nickname,
			UserType:  a.UserType,
			OrgID:     a.OrgID,
			OrgName:   a.OrgName,
		})
	}
	if len(out) == 0 {
		if pat := strings.TrimSpace(cfg.Qoder.PersonalAccessToken); pat != "" {
			out = append(out, upstream.QoderAccount{PAT: pat, Realm: "intl"})
		}
	}
	return out
}

func addQoderAccount(a upstream.QoderAccount, cfg *Config, pool *upstream.QoderPool) error {
	cur, err := upstream.LoadQoderAccounts(qoderAccountsFile)
	if err != nil {
		return err
	}
	if len(cur) == 0 {
		cur = seedQoderAccounts(cfg)
	}
	for _, existing := range cur {
		if existing.Name == a.Name {
			return fmt.Errorf("账号名 %q 已存在，先删掉再重建", a.Name)
		}
		if existing.PAT == a.PAT {
			return fmt.Errorf("这个 PAT 已经在池里了")
		}
	}
	cur = append(cur, a)
	if err := upstream.SaveQoderAccounts(qoderAccountsFile, cur); err != nil {
		return err
	}
	if pool != nil {
		pool.Replace(cur, filepath.Join("data"))
		log.Printf("qoder: 已加入账号 %q（realm=%s），当前 %d 个", a.Name, a.Realm, len(cur))
	}
	return nil
}

func deleteQoderAccount(name string, pool *upstream.QoderPool) error {
	cur, err := upstream.LoadQoderAccounts(qoderAccountsFile)
	if err != nil {
		return err
	}
	out := make([]upstream.QoderAccount, 0, len(cur))
	found := false
	for _, a := range cur {
		if a.Name == name {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		return fmt.Errorf("没有叫 %q 的账号", name)
	}
	if err := upstream.SaveQoderAccounts(qoderAccountsFile, out); err != nil {
		return err
	}
	if pool != nil {
		pool.Replace(out, filepath.Join("data"))
		log.Printf("qoder: 已移除账号 %q，当前 %d 个", name, len(out))
	}
	return nil
}

// qoderMachineID 读取（或首次生成）本地设备标识。
// 同一个值贯穿所有账号的授权，换值等于换设备，会触发上游风控。
func qoderMachineID() string {
	const path = "data/qoder_machine_id"
	if raw, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(raw)); s != "" {
			return s
		}
	}
	b := make([]byte, 16)
	_, _ = randRead(b)
	_ = os.MkdirAll("data", 0o755)
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
	return id
}

func randRead(b []byte) (int, error) { return rand.Read(b) }

// setQoderDisabled 停用/启用账号：改文件 + 原地替换内存池。
func setQoderDisabled(name string, disabled bool, pool *upstream.QoderPool) error {
	cur, err := upstream.LoadQoderAccounts(qoderAccountsFile)
	if err != nil {
		return err
	}
	found := false
	for i := range cur {
		if cur[i].Name == name {
			cur[i].Disabled = disabled
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("没有叫 %q 的账号", name)
	}
	if err := upstream.SaveQoderAccounts(qoderAccountsFile, cur); err != nil {
		return err
	}
	if pool != nil {
		pool.Replace(cur, filepath.Join("data"))
	}
	return nil
}

// exportQoderAccounts 导出完整账号文件（含凭据），供备份/迁移。
func exportQoderAccounts() ([]byte, error) {
	return os.ReadFile(qoderAccountsFile)
}
