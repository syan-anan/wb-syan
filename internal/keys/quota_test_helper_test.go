package keys

import "os"

// writeFile 测试辅助：写原始字节（配额文件损坏场景）。
func writeFile(path string, raw []byte) error { return os.WriteFile(path, raw, 0o600) }
