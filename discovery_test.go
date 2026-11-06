package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 判据：带 apply 标记的包必须被发现（规则 1 dsh 字段、规则 2 cordis.patch.yml、
// 作用域包），普通包不被发现，有入口无标记的包只记为规则 3 候选（不加载）。
func TestDiscoversDSHPlugin(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pkg-a", "package.json"), `{"name":"pkg-a","dsh":{}}`)
	writeFile(t, filepath.Join(root, "pkg-b", "package.json"), `{"name":"pkg-b"}`)
	writeFile(t, filepath.Join(root, "pkg-b", "cordis.patch.yml"), `insert: []`)
	writeFile(t, filepath.Join(root, "@scope", "pkg-c", "package.json"), `{"name":"@scope/pkg-c","dsh":{"x":1}}`)
	writeFile(t, filepath.Join(root, "ordinary", "package.json"), `{"name":"ordinary"}`)
	writeFile(t, filepath.Join(root, "entry-only", "package.json"), `{"name":"entry-only","main":"lib/index.js"}`)
	writeFile(t, filepath.Join(root, "entry-only", "lib", "index.js"), `export function apply(ctx) {}`)

	found := DiscoverPlugins(root)
	byName := map[string]DiscoveredPackage{}
	for _, d := range found {
		byName[d.Name] = d
	}

	cases := []struct{ name, kind string }{
		{"pkg-a", KindDSHPlugin},
		{"pkg-b", KindDSHPlugin},
		{"@scope/pkg-c", KindDSHPlugin},
		{"entry-only", KindMaybePlugin},
	}
	for _, c := range cases {
		d, ok := byName[c.name]
		if !ok {
			t.Errorf("包 %s 未被发现", c.name)
			continue
		}
		if d.Kind != c.kind {
			t.Errorf("包 %s kind = %q，想要 %q（reason: %s）", c.name, d.Kind, c.kind, d.Reason)
		}
	}
	if _, ok := byName["ordinary"]; ok {
		t.Error("普通包 ordinary 不应被发现")
	}
}

// 判据：一个坏包不能中断整轮扫描（与 clawhubadapter Start 的 continue 语义一致）。
func TestPluginLoadFailureDoesNotAbortScan(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "broken", "package.json"), `{invalid json`)
	writeFile(t, filepath.Join(root, "good", "package.json"), `{"name":"good","dsh":{}}`)
	writeFile(t, filepath.Join(root, "also-good", "package.json"), `{"name":"also-good"}`)
	writeFile(t, filepath.Join(root, "also-good", "cordis.patch.yml"), `insert: []`)

	found := DiscoverPlugins(root)
	byName := map[string]bool{}
	for _, d := range found {
		byName[d.Name] = true
	}
	if !byName["good"] || !byName["also-good"] {
		t.Errorf("坏包中断了扫描，实际发现：%v", byName)
	}
	if byName["broken"] {
		t.Error("坏包 broken 不应被发现")
	}
}
