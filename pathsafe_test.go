package main

import (
	"strings"
	"testing"
)

// 判据：包名不能逃出插件根（名字来自外部——包作者或模型，不是用户）。
// ".." 正好是整个数据目录，一次 RemoveAll 就能清空它。
func TestPackageNameCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveUnderRoot(root, ".."); err == nil {
		t.Error("resolveUnderRoot(..) 应当拒绝")
	}
	if _, err := resolveUnderRoot(root, "a/b"); err == nil {
		t.Error("resolveUnderRoot(a/b) 应当拒绝（单层目录名）")
	}
	if _, err := resolveUnderRoot(root, ".hidden"); err == nil {
		t.Error("resolveUnderRoot(.hidden) 应当拒绝（隐藏目录会被扫描跳过）")
	}
	dir, err := resolveUnderRoot(root, "good-pkg")
	if err != nil {
		t.Fatalf("正常包名被拒绝：%v", err)
	}
	if !strings.HasPrefix(dir, root+string('/')) {
		t.Errorf("解析结果 %q 不在根 %q 之下", dir, root)
	}
}

// 判据：归档条目不能写到目标目录之外（ZIP Slip）。
func TestArchiveExtractionStaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := safeArchiveTarget(root, "../../x"); err == nil {
		t.Error("safeArchiveTarget(../../x) 应当拒绝")
	}
	if _, err := safeArchiveTarget(root, "/etc/cron.d/x"); err == nil {
		t.Error("safeArchiveTarget(/etc/...) 应当拒绝（绝对路径）")
	}
	p, err := safeArchiveTarget(root, "a/b.txt")
	if err != nil {
		t.Fatalf("正常条目被拒绝：%v", err)
	}
	if !strings.HasPrefix(p, root+string('/')) {
		t.Errorf("解析结果 %q 不在根 %q 之下", p, root)
	}
}
