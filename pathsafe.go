package main

// 路径安全（从 clawhubadapter/pathsafe.go 移植，2026-11-04）。
//
// ## 为什么需要这个文件
//
// 适配器有多条入口把**外部字符串**拼进文件系统路径：
//
//	① 包发现：node_modules 下的目录名（来自包作者，不是用户）
//	② 将来的 plugin_install / plugin_uninstall：名字来自**模型**
//	   （它是工具参数），不是人
//
// 名字一旦是 `..`，`filepath.Join(skillsDir, "..")` 就等于整个数据目录
// （memory / documents / media / config.db 都在那里），一次
// os.RemoveAll 就能把数据目录清空。
//
// ## 两处实测事故（clawhubadapter 探针复现，均已修）
//
// ① 路径穿越：name=".." ⇒ Join(skillsDir, "..") == data_dir
//    ⇒ os.RemoveAll(data_dir) 把整个数据目录清空。
//
// ② ZIP Slip：下载的包若含条目 "../../x"，解压时
//    filepath.Join(dest, f.Name) 会写到 dest 之外 —— 任意路径写文件。
//    而下载**没有**任何校验和/签名验证，等于远程内容直通磁盘。
//
// 两个修法都是"先校验、后落地"，并且都留了根内前缀的双保险：
// 名字校验能挡住 ".."，前缀校验能挡住名字校验没想到的新形态。

import (
	"fmt"
	"path/filepath"
	"strings"
)

// checkSafeName 拒绝会让路径逃出插件根的条目名。
//
// 规则：
//   - 空串、"."、".." 一律拒绝
//   - 以 "." 开头的段拒绝（目录扫描会跳过隐藏目录 ⇒
//     写进去了却扫不回来，变成"内存有、盘上根外"的幽灵条目）
//   - 含路径分隔符的段拒绝（name 是**单层目录名**，不是相对路径）
//
// 为什么连 "/" 也拒绝：install 的 name 会同时用作包名与本地目录名。
// 允许 "a/b" 会让"包名"与"目录名"不再是同一个东西，前缀校验之外的语义也会漂移。
func checkSafeName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("插件名为空")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("插件名含非法路径段 %q: %q", seg, name)
		}
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("插件名不能以 . 开头（会被目录扫描跳过）: %q", name)
		}
	}
	// 单层目录名：分隔符（含 Windows 的反斜杠）一律拒绝。
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("插件名不能含路径分隔符: %q", name)
	}
	return nil
}

// resolveUnderRoot 把单层名字解析到 root 之下，并确认解析结果仍在 root 内。
//
// 这是"双保险"的第二道：即使 checkSafeName 因将来的改动漏掉某种形态，
// 前缀校验仍会拒绝写到根外。
func resolveUnderRoot(root, name string) (string, error) {
	if err := checkSafeName(name); err != nil {
		return "", err
	}
	rootClean := filepath.Clean(root)
	dir := filepath.Join(rootClean, name)
	// dir 必须严格位于 root 之下（dir == root 也拒绝：那等于对根动手）。
	if !strings.HasPrefix(filepath.Clean(dir), rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("拒绝越出插件根（%s）的路径: %q", rootClean, name)
	}
	return dir, nil
}

// ensureWithin 校验一个**已经拼好**的绝对路径仍位于 root 之下。
//
// 用于解压：条目名来自归档，不是单层名字，不能用 checkSafeName。
// 这里只能做前缀判断，所以额外要求条目名不得是绝对路径、
// 也不得含 ".." 段 —— 前缀判断对 "dest/../../x" 这类形式依赖
// filepath.Clean 的归一化，显式拒绝更不容易被将来的改动绕过。
func ensureWithin(root, target string) error {
	rootClean := filepath.Clean(root)
	targetClean := filepath.Clean(target)
	if targetClean == rootClean {
		return nil // 根目录自身（归档里的 "./" 之类）无害
	}
	if !strings.HasPrefix(targetClean, rootClean+string(filepath.Separator)) {
		return fmt.Errorf("归档条目越出目标目录（%s）: %q", rootClean, targetClean)
	}
	return nil
}

// safeArchiveTarget 把归档条目名解析成 root 之下的绝对路径。
//
// 归档条目名的两种危险形态都要挡：
//   - 绝对路径（"/etc/cron.d/x"、Windows 盘符）
//   - 含 ".." 的相对路径（"../../x"）
//
// 注意 filepath.Join 会**静默**把 "/etc/x" 变成 "root/etc/x"，
// 这看似安全却掩盖了恶意意图；显式拒绝能让日志说明真实原因。
func safeArchiveTarget(root, entryName string) (string, error) {
	if entryName == "" {
		return "", fmt.Errorf("归档条目名为空")
	}
	if filepath.IsAbs(entryName) || strings.HasPrefix(entryName, `\`) {
		return "", fmt.Errorf("归档条目是绝对路径: %q", entryName)
	}
	for _, seg := range strings.Split(strings.ReplaceAll(entryName, `\`, "/"), "/") {
		if seg == ".." {
			return "", fmt.Errorf("归档条目含 .. 段: %q", entryName)
		}
	}
	target := filepath.Join(root, filepath.FromSlash(entryName))
	if err := ensureWithin(root, target); err != nil {
		return "", err
	}
	return target, nil
}
