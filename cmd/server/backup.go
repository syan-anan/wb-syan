package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 配置备份策略：每次面板保存配置前，把"保存前的那一份"复制到 backups/ 下。
// 保留最近 backupKeep 份，更早的自动清理。
//
// 为什么备份的是"保存前"而不是"保存后"：用户改坏了配置时，需要回滚到的正是
// 改坏之前那一份。备份保存后的版本等于备份了错误。
const (
	backupDirName = "backups"
	backupKeep    = 20
)

// backupConfigFile 备份一份配置文件，返回备份路径。源文件不存在时视为无需备份。
func backupConfigFile(path string) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dir := filepath.Join(filepath.Dir(path), backupDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 文件名带纳秒，避免同一秒内连续保存互相覆盖。
	name := fmt.Sprintf("%s-%s.json",
		filepath.Base(path), time.Now().Format("20060102-150405.000000000"))
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return err
	}
	return pruneBackups(dir, backupKeep)
}

// pruneBackups 只保留最近 keep 份（按文件名排序，时间戳前缀天然可排序）。
func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= keep {
		return nil
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, n))
	}
	return nil
}

// listBackups 返回备份文件名（面板展示用，新的在前）。
func listBackups(configPath string) []string {
	dir := filepath.Join(filepath.Dir(configPath), backupDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// restoreBackup 用指定备份覆盖当前配置。
//
// 安全约束：name 只允许是 backups 目录下的纯文件名——带路径分隔符或 .. 的
// 一律拒绝。面板是管理面，但管理面也不该成为任意文件写入的原语。
func restoreBackup(configPath, name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("非法的备份名: %q", name)
	}
	dir := filepath.Join(filepath.Dir(configPath), backupDirName)
	src := filepath.Join(dir, name)
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取备份失败: %w", err)
	}
	// 恢复前先把"当前这一份"也备份掉：恢复点错了还能再回来。
	if berr := backupConfigFile(configPath); berr != nil {
		return fmt.Errorf("恢复前备份失败: %w", berr)
	}
	tmp := configPath + ".restore.tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath)
}
