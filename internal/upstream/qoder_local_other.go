//go:build !windows

package upstream

import "fmt"

// ImportLocalQoder 只在 Windows 上可用：桌面客户端的凭证由 DPAPI 保护。
func ImportLocalQoder() ([]QoderAccount, error) {
	return nil, fmt.Errorf("qoder: 本机凭证导入仅支持 Windows")
}
