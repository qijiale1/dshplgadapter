package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 本地 registry 桩（单测不依赖外网） ----------

type fakeDist struct {
	Tarball   string `json:"tarball"`
	Shasum    string `json:"shasum"`
	Integrity string `json:"integrity"`
}

type fakeVersion struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Dist    fakeDist `json:"dist"`
}

type fakePackument struct {
	Name     string                 `json:"name"`
	DistTags map[string]string      `json:"dist-tags"`
	Versions map[string]fakeVersion `json:"versions"`
}

// fakeRegistry 用 httptest 模拟 npm registry：
// GET /{包名} → packument；GET /t/{文件} → tarball。
// requests 计数用于断言"开关关闭时一次请求都没发"。
type fakeRegistry struct {
	server     *httptest.Server
	packuments map[string]fakePackument
	tarballs   map[string][]byte
	requests   int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{
		packuments: map[string]fakePackument{},
		tarballs:   map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fr.requests++
		if strings.HasPrefix(r.URL.Path, "/t/") {
			data, ok := fr.tarballs[strings.TrimPrefix(r.URL.Path, "/t/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		p, ok := fr.packuments[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	})
	fr.server = httptest.NewServer(mux)
	t.Cleanup(fr.server.Close)
	return fr
}

// publish 把一个 tarball 登记进桩 registry，返回它的 sha256 / shasum / integrity。
func (fr *fakeRegistry) publish(name, version string, tarball []byte) (sha256hex, shasum, integrity string) {
	s256 := sha256.Sum256(tarball)
	sha256hex = hex.EncodeToString(s256[:])
	s1 := sha1.Sum(tarball)
	shasum = hex.EncodeToString(s1[:])
	s512 := sha512.Sum512(tarball)
	integrity = "sha512-" + base64.StdEncoding.EncodeToString(s512[:])
	file := fmt.Sprintf("%s-%s.tgz", strings.ReplaceAll(strings.TrimPrefix(name, "@"), "/", "-"), version)
	fr.tarballs[file] = tarball
	p, ok := fr.packuments[name]
	if !ok {
		p = fakePackument{Name: name, DistTags: map[string]string{}, Versions: map[string]fakeVersion{}}
	}
	p.Versions[version] = fakeVersion{
		Name: name, Version: version,
		Dist: fakeDist{Tarball: fr.server.URL + "/t/" + file, Shasum: shasum, Integrity: integrity},
	}
	if p.DistTags["latest"] == "" {
		p.DistTags["latest"] = version
	}
	fr.packuments[name] = p
	return sha256hex, shasum, integrity
}

// ---------- tarball 自造 ----------

type tarEntry struct {
	name     string // 归档内完整名，如 "package/package.json"
	content  string
	typeflag byte // 0 = 普通文件
	linkname string
}

func makeTarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(e.content)),
			Typeflag: tf,
			Linkname: e.linkname,
		}
		if tf == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			if _, err := io.WriteString(tw, e.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testPluginTarball 造一个最小 dsh 插件 tarball（package.json 带 dsh 字段）。
func testPluginTarball(t *testing.T, pkgName string) []byte {
	t.Helper()
	return makeTarball(t, []tarEntry{
		{name: "package/", typeflag: tar.TypeDir},
		{name: "package/package.json", content: fmt.Sprintf(`{"name":%q,"type":"module","main":"lib/index.js","dsh":{}}`, pkgName)},
		{name: "package/lib/index.js", content: `export function apply(ctx) {}`},
	})
}

func newTestInstaller(root string, enabled bool, reg *fakeRegistry) *remoteInstaller {
	return newRemoteInstaller(root, enabled, reg.server.URL)
}

func assertNoFilesLanded(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读安装根失败: %v", err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("拒绝安装后仍有文件落盘: %v", names)
	}
}

// 判据：sha256 不一致拒绝安装，且目标位置不留任何文件。
func TestInstallRejectsSHA256Mismatch(t *testing.T) {
	root := t.TempDir()
	reg := newFakeRegistry(t)
	tarball := testPluginTarball(t, "@scope/test-pkg")
	reg.publish("@scope/test-pkg", "1.0.0", tarball)
	in := newTestInstaller(root, true, reg)

	wrong := strings.Repeat("0", 64) // 格式合法、值不对
	_, err := in.Install(context.Background(), "@scope/test-pkg@1.0.0", wrong)
	if err == nil {
		t.Fatal("sha256 不一致应当拒绝")
	}
	if !strings.Contains(err.Error(), "sha256 不一致") {
		t.Errorf("拒绝原因应说明 sha256 不一致，实际：%v", err)
	}
	assertNoFilesLanded(t, root)
	if _, err := os.Stat(filepath.Join(root, "@scope", "test-pkg")); !os.IsNotExist(err) {
		t.Error("目标目录不应存在")
	}
}

// 判据：未提供期望 sha256 直接拒绝（fail-closed），且不落文件。
func TestInstallRejectsMissingSHA256(t *testing.T) {
	root := t.TempDir()
	reg := newFakeRegistry(t)
	tarball := testPluginTarball(t, "plain-pkg")
	reg.publish("plain-pkg", "2.0.0", tarball)
	in := newTestInstaller(root, true, reg)

	for _, missing := range []string{"", "   "} {
		_, err := in.Install(context.Background(), "plain-pkg@2.0.0", missing)
		if err == nil {
			t.Fatalf("未提供 sha256（%q）应当拒绝", missing)
		}
		if !strings.Contains(err.Error(), "sha256") {
			t.Errorf("拒绝原因应提到 sha256，实际：%v", err)
		}
	}
	assertNoFilesLanded(t, root)
}

// 判据：开关关闭时直接拒绝，且一次 registry 请求都不发。
func TestInstallRejectsWhenDisabled(t *testing.T) {
	root := t.TempDir()
	reg := newFakeRegistry(t)
	tarball := testPluginTarball(t, "plain-pkg")
	sha, _, _ := reg.publish("plain-pkg", "2.0.0", tarball)
	in := newTestInstaller(root, false, reg)

	_, err := in.Install(context.Background(), "plain-pkg@2.0.0", sha)
	if err == nil {
		t.Fatal("开关关闭应当拒绝")
	}
	if !strings.Contains(err.Error(), "未开启") {
		t.Errorf("拒绝原因应说明开关未开启，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "remote_install_enabled") {
		t.Errorf("拒绝原因应说明如何开启（remote_install_enabled），实际：%v", err)
	}
	if reg.requests != 0 {
		t.Errorf("开关关闭时不应发任何 registry 请求，实际发了 %d 次", reg.requests)
	}
	assertNoFilesLanded(t, root)
}

// 判据：tarball 内含穿越路径（..）拒绝安装，且根外不写文件、
// 暂存目录被清理。
func TestInstallRejectsPathTraversalTarball(t *testing.T) {
	root := t.TempDir()
	reg := newFakeRegistry(t)
	evil := makeTarball(t, []tarEntry{
		{name: "package/", typeflag: tar.TypeDir},
		{name: "package/package.json", content: `{"name":"evil-pkg","dsh":{}}`},
		{name: "package/../../evil.txt", content: "pwned"},
	})
	sha, _, _ := reg.publish("evil-pkg", "1.0.0", evil)
	in := newTestInstaller(root, true, reg)

	_, err := in.Install(context.Background(), "evil-pkg@1.0.0", sha)
	if err == nil {
		t.Fatal("含穿越路径的 tarball 应当拒绝")
	}
	// 根外（TempDir 的父目录）不应出现 evil.txt。
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); !os.IsNotExist(err) {
		t.Error("穿越文件写到了根外")
	}
	assertNoFilesLanded(t, root)
}

// 判据：tarball 内含符号链接拒绝安装（归档不得在盘上留出指向根外的链接）。
func TestInstallRejectsSymlinkEntry(t *testing.T) {
	root := t.TempDir()
	reg := newFakeRegistry(t)
	withLink := makeTarball(t, []tarEntry{
		{name: "package/", typeflag: tar.TypeDir},
		{name: "package/package.json", content: `{"name":"link-pkg","dsh":{}}`},
		{name: "package/evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	sha, _, _ := reg.publish("link-pkg", "1.0.0", withLink)
	in := newTestInstaller(root, true, reg)

	_, err := in.Install(context.Background(), "link-pkg@1.0.0", sha)
	if err == nil {
		t.Fatal("含符号链接的 tarball 应当拒绝")
	}
	assertNoFilesLanded(t, root)
}

// 判据：registry 的 integrity/shasum 与 tarball 对不上也拒绝
// （交叉核对不是摆设）。
func TestInstallRejectsIntegrityMismatch(t *testing.T) {
	root := t.TempDir()
	reg := newFakeRegistry(t)
	tarball := testPluginTarball(t, "plain-pkg")
	sha, _, _ := reg.publish("plain-pkg", "2.0.0", tarball)
	// 事后篡改 registry 侧的 integrity（tarball 不变，期望 sha256 不变）。
	p := reg.packuments["plain-pkg"]
	v := p.Versions["2.0.0"]
	v.Dist.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	p.Versions["2.0.0"] = v
	reg.packuments["plain-pkg"] = p
	in := newTestInstaller(root, true, reg)

	_, err := in.Install(context.Background(), "plain-pkg@2.0.0", sha)
	if err == nil {
		t.Fatal("registry integrity 不一致应当拒绝")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Errorf("拒绝原因应提到 integrity，实际：%v", err)
	}
	assertNoFilesLanded(t, root)
}

// 判据：本地 packument 桩 + 自造 tarball 的成功安装路径，
// 随后 rescan 能发现新包（进入 1–3 阶段的发现链路），
// 且重复安装同一包被拒绝（不覆盖）。
func TestInstallSuccessWithLocalRegistry(t *testing.T) {
	root := t.TempDir()
	stageDir := t.TempDir()
	reg := newFakeRegistry(t)
	tarball := testPluginTarball(t, "@scope/test-pkg")
	sha, _, _ := reg.publish("@scope/test-pkg", "1.0.0", tarball)
	p := &Plugin{
		name:          "dshplgadapter",
		discoveryRoot: root,
		skillStageDir: stageDir,
		stagedSkills:  map[string]stagedSkill{},
		installer:     newTestInstaller(root, true, reg),
	}

	out, err := p.installPackage("@scope/test-pkg@1.0.0", sha)
	if err != nil {
		t.Fatalf("本地 registry 成功安装失败：%v", err)
	}
	if out["package"] != "@scope/test-pkg" || out["version"] != "1.0.0" {
		t.Errorf("安装结果包名/版本不对：%v", out)
	}
	if out["sha256"] != sha {
		t.Errorf("安装结果 sha256 不对：%v", out)
	}
	if out["integrityChecked"] != true || out["shasumChecked"] != true {
		t.Errorf("registry 交叉核对应记为已核对：%v", out)
	}
	if out["discovered"] != true || out["discoveredKind"] != KindDSHPlugin {
		t.Errorf("安装后重扫应发现新包为 dsh 插件：%v", out)
	}
	if h, _ := out["launchHint"].(string); h == "" {
		t.Errorf("dsh 插件应给出 launch 提示：%v", out)
	}
	// 落盘内容核对。
	data, err := os.ReadFile(filepath.Join(root, "@scope", "test-pkg", "package.json"))
	if err != nil {
		t.Fatalf("安装后 package.json 不在：%v", err)
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil || pkg.Name != "@scope/test-pkg" {
		t.Errorf("安装后 package.json 内容不对：%s / %v", data, err)
	}
	if _, err := os.ReadFile(filepath.Join(root, "@scope", "test-pkg", "lib", "index.js")); err != nil {
		t.Errorf("安装后 lib/index.js 不在：%v", err)
	}
	// 再次安装同一包：拒绝，不覆盖。
	_, err = p.installPackage("@scope/test-pkg@1.0.0", sha)
	if err == nil {
		t.Error("重复安装同一包应当拒绝（不覆盖）")
	}
}

// 判据：包名/版本解析与校验（名字来自调用方，先校验、后联网）。
func TestParsePackageSpec(t *testing.T) {
	good := []struct{ spec, name, version string }{
		{"pkg", "pkg", "latest"},
		{"pkg@2.0.0", "pkg", "2.0.0"},
		{"@scope/pkg", "@scope/pkg", "latest"},
		{"@scope/pkg@1.2.3-beta.1", "@scope/pkg", "1.2.3-beta.1"},
	}
	for _, c := range good {
		name, version, err := parsePackageSpec(c.spec)
		if err != nil {
			t.Errorf("parsePackageSpec(%q) 不应报错：%v", c.spec, err)
			continue
		}
		if name != c.name || version != c.version {
			t.Errorf("parsePackageSpec(%q) = (%q,%q)，想要 (%q,%q)", c.spec, name, version, c.name, c.version)
		}
	}
	bad := []string{"", "..", "a/b/c", ".hidden", "@scope/", "pkg@", "pkg@1/2", "Pkg", "@Scope/pkg"}
	for _, spec := range bad {
		if _, _, err := parsePackageSpec(spec); err == nil {
			t.Errorf("parsePackageSpec(%q) 应当拒绝", spec)
		}
	}
}
