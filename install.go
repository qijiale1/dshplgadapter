package main

// 远程安装（阶段 4，规划书 §四，可选、需单独决策——用户 2026-11-07 已决策通过）。
//
// 流程（全部硬要求）：
//  1. 开关默认关闭：config `remote_install_enabled` 为 false 时直接拒绝，
//     并说明如何开启；不设开关、开关关着，都装不了。
//  2. 先读 packument（GET {registry}/{name}）取指定版本的 dist.tarball，
//     再下载 tarball；下载完先算 sha256，与调用方提供的期望值比对，
//     一致才解压；不一致拒绝且不落任何文件。
//  3. 没有提供期望 sha256 时直接拒绝（fail-closed，绝不默认放行）。
//     registry 同时提供的 integrity（sha512）/ shasum（sha1）一并核对并记录。
//  4. 解压走已有的 safeArchiveTarget / resolveUnderRoot（pathsafe.go）：
//     归档条目含 ".."、绝对路径、符号链接/硬链接一律拒绝；
//     先解到 root 内的临时暂存目录，校验通过才 rename 到目标位置，
//     任何一步失败都删掉暂存目录，不留文件。
//  5. 安装成功后由调用方触发 rescan（plugin.go 的 rescan），
//     新包进入 1–3 阶段已有的发现链路；不绕过发现逻辑，
//     也不在这里自动拉起 sidecar（拉起走 dshplgadapter_launch，
//     与规则 3 候选的手动试加载一致）。
//
// 安全声明（§9.5 同样适用）：远程安装 = 从网上拉代码到本机执行。
// 装插件 = 给本机执行权限。只从你信任的来源安装。

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
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// defaultNPMRegistry 是默认的上游 registry；config `npm_registry` 可改。
const defaultNPMRegistry = "https://registry.npmjs.org"

// maxTarballBytes 是单个 tarball 的下载上限；超过即拒绝（防内存撑爆）。
const maxTarballBytes = 200 << 20 // 200 MiB

// maxExtractedBytes 是解压后总量的上限；超过即拒绝（防解压炸弹）。
const maxExtractedBytes = 500 << 20 // 500 MiB

// maxExtractedFileBytes 是解压后单个文件的上限。
const maxExtractedFileBytes = 100 << 20 // 100 MiB

const installUserAgent = "dshplgadapter/0.1.0"

var (
	// npm 包名每段只允许小写字母、数字与 . _ ~ -，且不以 . 开头
	//（checkSafeName 另行拒绝 "."、".." 与隐藏段）。
	npmNameSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._~-]*$`)
	// 版本只允许数字、字母与 . - +，且不含路径分隔符与 ".."。
	npmVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)
	hex64      = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// remoteInstaller 是阶段 4 的安装器：纯逻辑，不依赖 PluginSDK，
// 测试可直接用本地 httptest registry 驱动。
type remoteInstaller struct {
	root       string // 安装根 = <profile>/node_modules
	enabled    bool
	registry   string // registry 根 URL（无尾部斜杠）
	httpClient *http.Client
}

func newRemoteInstaller(root string, enabled bool, registry string) *remoteInstaller {
	registry = strings.TrimSuffix(strings.TrimSpace(registry), "/")
	if registry == "" {
		registry = defaultNPMRegistry
	}
	return &remoteInstaller{
		root:       root,
		enabled:    enabled,
		registry:   registry,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// InstallResult 是安装成功后返回的报告（hash 与校验结果全写进结果，
// 调用方与日志都能定位到"哪一步、哪个校验、哪个 URL/包名版本"）。
type InstallResult struct {
	Package        string `json:"package"`
	Version        string `json:"version"`
	Tarball        string `json:"tarball"`
	SHA256         string `json:"sha256"`
	IntegrityOK    bool   `json:"integrityChecked"`
	ShasumOK       bool   `json:"shasumChecked"`
	Dir            string `json:"dir"`
	DiscoveredKind string `json:"discoveredKind,omitempty"`
	Discovered     bool   `json:"discovered"`
	LaunchHint     string `json:"launchHint,omitempty"`
	RescannedCount int    `json:"rescannedCount"`
}

func (r *InstallResult) logLine(prefix string) string {
	return fmt.Sprintf("%s package=%s version=%s tarball=%s sha256=%s integrityChecked=%v shasumChecked=%v dir=%s",
		prefix, r.Package, r.Version, r.Tarball, r.SHA256, r.IntegrityOK, r.ShasumOK, r.Dir)
}

// parsePackageSpec 解析 "name[@version]"。
// 作用域包形如 "@scope/name@1.2.3"：版本分隔符是首字符之后的最后一个 "@"。
func parsePackageSpec(spec string) (name, version string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", fmt.Errorf("包名为空")
	}
	version = "latest"
	name = spec
	if idx := strings.LastIndex(spec[1:], "@"); idx >= 0 {
		// idx 是 spec[1:] 里的下标，换算回 spec 下标要 +1。
		at := idx + 1
		if at == 0 {
			return "", "", fmt.Errorf("包名格式不对: %q", spec)
		}
		name = spec[:at]
		version = spec[at+1:]
		if version == "" {
			return "", "", fmt.Errorf("包名 %q 的版本为空", spec)
		}
	}
	if err := validatePackageName(name); err != nil {
		return "", "", err
	}
	if version != "latest" {
		if !npmVersion.MatchString(version) || strings.Contains(version, "..") {
			return "", "", fmt.Errorf("版本号格式不对: %q", version)
		}
	}
	return name, version, nil
}

// validatePackageName 校验 npm 包名（不含版本）。
// 规则：单段或 "@scope/段"；每段过 checkSafeName（挡 ".."、
// 隐藏段、分隔符），再过 npm 字符集（小写字母数字与 . _ ~ -）。
func validatePackageName(name string) error {
	if name == "" {
		return fmt.Errorf("包名为空")
	}
	segs := strings.Split(name, "/")
	if len(segs) > 2 {
		return fmt.Errorf("包名格式不对（最多一个斜杠）: %q", name)
	}
	for i, seg := range segs {
		if i == 0 && len(segs) == 2 {
			if !strings.HasPrefix(seg, "@") || seg == "@" {
				return fmt.Errorf("作用域包名格式不对: %q", name)
			}
			seg = seg[1:]
		}
		if err := checkSafeName(seg); err != nil {
			return fmt.Errorf("包名不合法 %q: %w", name, err)
		}
		if strings.Contains(seg, "..") {
			return fmt.Errorf("包名不合法（含 ..）: %q", name)
		}
		if !npmNameSegment.MatchString(seg) {
			return fmt.Errorf("包名不合法（只允许小写字母、数字与 . _ ~ -）: %q", name)
		}
	}
	return nil
}

type packument struct {
	Name     string                 `json:"name"`
	DistTags map[string]string      `json:"dist-tags"`
	Versions map[string]packVersion `json:"versions"`
}

type packVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Shasum    string `json:"shasum"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

// fetchPackument 从 registry 取包元数据（GET {registry}/{escaped-name}）。
func (in *remoteInstaller) fetchPackument(ctx context.Context, name string) (*packument, error) {
	u := in.registry + "/" + url.PathEscape(name)
	log.Printf("[dshplgadapter] install: fetch packument package=%s url=%s", name, u)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 packument 请求失败 package=%s url=%s: %w", name, u, err)
	}
	req.Header.Set("User-Agent", installUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := in.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("取 packument 失败 package=%s url=%s: %w", name, u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("包不存在 package=%s url=%s (registry 返回 404)", name, u)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("取 packument 失败 package=%s url=%s: registry 返回 %s", name, u, resp.Status)
	}
	var p packument
	dec := json.NewDecoder(io.LimitReader(resp.Body, 10<<20))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("解析 packument 失败 package=%s url=%s: %w", name, u, err)
	}
	return &p, nil
}

// resolveVersion 把 "latest" 或指定版本解析成 packument 里的具体版本条目。
func resolveVersion(p *packument, want string) (packVersion, error) {
	version := want
	if want == "latest" {
		version = p.DistTags["latest"]
		if version == "" {
			return packVersion{}, fmt.Errorf("包 %s 没有 latest dist-tag", p.Name)
		}
	}
	v, ok := p.Versions[version]
	if !ok {
		return packVersion{}, fmt.Errorf("包 %s 没有版本 %s", p.Name, version)
	}
	if v.Dist.Tarball == "" {
		return packVersion{}, fmt.Errorf("包 %s 版本 %s 的 packument 缺 dist.tarball", p.Name, version)
	}
	return v, nil
}

// downloadTarball 下载 tarball（上限 maxTarballBytes，超了直接拒绝）。
func (in *remoteInstaller) downloadTarball(ctx context.Context, tarballURL string) ([]byte, error) {
	log.Printf("[dshplgadapter] install: download tarball url=%s", tarballURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tarballURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 tarball 请求失败 url=%s: %w", tarballURL, err)
	}
	req.Header.Set("User-Agent", installUserAgent)
	resp, err := in.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 tarball 失败 url=%s: %w", tarballURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 tarball 失败 url=%s: 返回 %s", tarballURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTarballBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读 tarball 失败 url=%s: %w", tarballURL, err)
	}
	if len(data) > maxTarballBytes {
		return nil, fmt.Errorf("tarball 超过上限 %d 字节 url=%s", maxTarballBytes, tarballURL)
	}
	return data, nil
}

// verifyDist 核对全部校验和，返回算出的 sha256。
//
// 顺序：先核对调用方提供的期望 sha256（不提供直接拒绝，
// fail-closed）；再核对 registry 提供的 shasum（sha1）与
// integrity（sha512），有就核对、核对不上就拒绝。
// 任何一步失败都返回错误，调用方不得落盘。
func verifyDist(v packVersion, data []byte, expectedSHA256 string) (computed string, integrityOK, shasumOK bool, err error) {
	if strings.TrimSpace(expectedSHA256) == "" {
		return "", false, false, fmt.Errorf("未提供期望 sha256：远程安装必须提供 sha256 才装（fail-closed），不提供直接拒绝")
	}
	expected := strings.ToLower(strings.TrimSpace(expectedSHA256))
	if !hex64.MatchString(expected) {
		return "", false, false, fmt.Errorf("期望 sha256 格式不对（应为 64 位十六进制）: %q", expectedSHA256)
	}
	sum := sha256.Sum256(data)
	computed = hex.EncodeToString(sum[:])
	if computed != expected {
		return "", false, false, fmt.Errorf("sha256 不一致 package=%s version=%s：期望 %s，实际 %s", v.Name, v.Version, expected, computed)
	}
	if v.Dist.Shasum != "" {
		s1 := sha1.Sum(data)
		if hex.EncodeToString(s1[:]) != strings.ToLower(v.Dist.Shasum) {
			return "", false, false, fmt.Errorf("registry shasum（sha1）不一致 package=%s version=%s", v.Name, v.Version)
		}
		shasumOK = true
	}
	if v.Dist.Integrity != "" {
		algo, b64, ok := strings.Cut(v.Dist.Integrity, "-")
		if !ok {
			return "", false, false, fmt.Errorf("registry integrity 格式不对 package=%s version=%s: %q", v.Name, v.Version, v.Dist.Integrity)
		}
		want, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", false, false, fmt.Errorf("registry integrity 解码失败 package=%s version=%s: %w", v.Name, v.Version, err)
		}
		switch algo {
		case "sha512":
			s := sha512.Sum512(data)
			if !bytes.Equal(s[:], want) {
				return "", false, false, fmt.Errorf("registry integrity（sha512）不一致 package=%s version=%s", v.Name, v.Version)
			}
		case "sha1":
			s := sha1.Sum(data)
			if !bytes.Equal(s[:], want) {
				return "", false, false, fmt.Errorf("registry integrity（sha1）不一致 package=%s version=%s", v.Name, v.Version)
			}
		default:
			return "", false, false, fmt.Errorf("registry integrity 算法不支持 package=%s version=%s: %q", v.Name, v.Version, algo)
		}
		integrityOK = true
	}
	return computed, integrityOK, shasumOK, nil
}

// targetDirFor 把包名解析成 root 之下的目标目录（作用域包两段解析）。
func (in *remoteInstaller) targetDirFor(name string) (string, error) {
	segs := strings.Split(name, "/")
	if len(segs) == 2 {
		scopeDir, err := resolveUnderRoot(in.root, segs[0])
		if err != nil {
			return "", err
		}
		return resolveUnderRoot(scopeDir, segs[1])
	}
	return resolveUnderRoot(in.root, name)
}

// extractTarball 把已校验的 tarball 解到 dest（dest 须已存在且为空）。
// npm tarball 的条目都在 "package/" 前缀下；前缀外的条目拒绝。
// 符号链接/硬链接/特殊文件一律拒绝；每条目标路径过 safeArchiveTarget。
func extractTarball(data []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("tarball 不是合法 gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读 tarball 失败: %w", err)
		}
		if !strings.HasPrefix(hdr.Name, "package/") {
			return fmt.Errorf("tarball 条目不在 package/ 前缀下: %q", hdr.Name)
		}
		rel := strings.TrimPrefix(hdr.Name, "package/")
		if rel == "" {
			continue // 顶层 package/ 目录本身
		}
		target, err := safeArchiveTarget(dest, rel)
		if err != nil {
			return fmt.Errorf("tarball 条目 %q 被拒绝: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("创建目录 %q 失败: %w", rel, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size > maxExtractedFileBytes {
				return fmt.Errorf("tarball 内文件 %q 超过单文件上限 %d 字节", rel, maxExtractedFileBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("创建父目录失败 %q: %w", rel, err)
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return fmt.Errorf("写文件 %q 失败: %w", rel, err)
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxExtractedFileBytes+1))
			cerr := f.Close()
			if err != nil {
				return fmt.Errorf("写文件 %q 失败: %w", rel, err)
			}
			if cerr != nil {
				return fmt.Errorf("关闭文件 %q 失败: %w", rel, cerr)
			}
			if n > maxExtractedFileBytes {
				return fmt.Errorf("tarball 内文件 %q 超过单文件上限 %d 字节", rel, maxExtractedFileBytes)
			}
			total += n
			if total > maxExtractedBytes {
				return fmt.Errorf("tarball 解压总量超过上限 %d 字节", maxExtractedBytes)
			}
		default:
			// 符号链接、硬链接、字符/块设备、FIFO：一律拒绝，
			// 宁可装不上，也不让归档在盘上留出指向根外的链接。
			return fmt.Errorf("tarball 条目类型不支持（只允许目录与普通文件）: %q type=%c", hdr.Name, hdr.Typeflag)
		}
	}
	// 解出的包必须有 package.json，且 name 必须与请求的包名一致
	//（防 tarball 内容与 packument 声明的包名对不上）。
	raw, err := os.ReadFile(filepath.Join(dest, "package.json"))
	if err != nil {
		return fmt.Errorf("解出的包缺 package.json: %w", err)
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return fmt.Errorf("解出的包 package.json 解析失败: %w", err)
	}
	return nil
}

// Install 执行一次远程安装：开关 → 解析 → 取 packument →
// 下载 → 校验（sha256 必备 + registry 交叉核对）→ 解压到暂存目录
// → 校验包名 → rename 落盘。校验失败在落盘之前返回，
// 目标位置不留任何文件。
func (in *remoteInstaller) Install(ctx context.Context, spec, expectedSHA256 string) (*InstallResult, error) {
	if !in.enabled {
		return nil, fmt.Errorf("远程安装未开启（默认关闭）：在插件配置里设 remote_install_enabled=true 后重启插件再安装")
	}
	name, version, err := parsePackageSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("安装请求无效 spec=%q: %w", spec, err)
	}
	log.Printf("[dshplgadapter] install: start package=%s version=%s registry=%s", name, version, in.registry)

	p, err := in.fetchPackument(ctx, name)
	if err != nil {
		return nil, err
	}
	v, err := resolveVersion(p, version)
	if err != nil {
		return nil, fmt.Errorf("安装请求无效 package=%s version=%s: %w", name, version, err)
	}
	log.Printf("[dshplgadapter] install: resolved package=%s version=%s tarball=%s", v.Name, v.Version, v.Dist.Tarball)

	data, err := in.downloadTarball(ctx, v.Dist.Tarball)
	if err != nil {
		return nil, err
	}
	computed, integrityOK, shasumOK, err := verifyDist(v, data, expectedSHA256)
	if err != nil {
		// 校验失败：tarball 只在内存里，不落盘。
		log.Printf("[dshplgadapter] install: REJECTED package=%s version=%s: %v", v.Name, v.Version, err)
		return nil, err
	}
	log.Printf("[dshplgadapter] install: checksum ok package=%s version=%s sha256=%s integrityChecked=%v shasumChecked=%v", v.Name, v.Version, computed, integrityOK, shasumOK)

	target, err := in.targetDirFor(v.Name)
	if err != nil {
		return nil, fmt.Errorf("目标目录不合法 package=%s: %w", v.Name, err)
	}
	if _, err := os.Stat(target); err == nil {
		return nil, fmt.Errorf("包 %s 已安装在 %s（先移除再安装，不覆盖）", v.Name, target)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("检查目标目录失败 %s: %w", target, err)
	}
	if err := os.MkdirAll(in.root, 0o755); err != nil {
		return nil, fmt.Errorf("创建安装根失败 %s: %w", in.root, err)
	}
	// 先解到 root 内的暂存目录（同一文件系统，rename 原子落盘）；
	// 任何失败都删掉暂存目录，目标位置不留文件。
	staging, err := os.MkdirTemp(in.root, ".dshplgadapter-install-*")
	if err != nil {
		return nil, fmt.Errorf("创建暂存目录失败: %w", err)
	}
	// 暂存目录本身是隐藏目录，discovery 会跳过；失败路径上清理它。
	cleanup := func() { os.RemoveAll(staging) }
	if err := extractTarball(data, staging); err != nil {
		cleanup()
		log.Printf("[dshplgadapter] install: REJECTED package=%s version=%s: 解压失败: %v", v.Name, v.Version, err)
		return nil, fmt.Errorf("解压 tarball 失败 package=%s version=%s: %w", v.Name, v.Version, err)
	}
	// 包名二次核对：解出的 package.json 的 name 必须等于请求的包名。
	raw, err := os.ReadFile(filepath.Join(staging, "package.json"))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("解出的包缺 package.json package=%s: %w", v.Name, err)
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		cleanup()
		return nil, fmt.Errorf("解出的包 package.json 解析失败 package=%s: %w", v.Name, err)
	}
	if pkg.Name != v.Name {
		cleanup()
		return nil, fmt.Errorf("tarball 内包名 %q 与请求包名 %q 不一致，拒绝安装", pkg.Name, v.Name)
	}
	// 作用域包：先建好 scope 目录（rename 不会自己建父目录）。
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		cleanup()
		return nil, fmt.Errorf("创建目标父目录失败 %s: %w", filepath.Dir(target), err)
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		cleanup()
		return nil, fmt.Errorf("设置暂存目录权限失败: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		cleanup()
		return nil, fmt.Errorf("落盘失败 package=%s target=%s: %w", v.Name, target, err)
	}
	res := &InstallResult{
		Package:     v.Name,
		Version:     v.Version,
		Tarball:     v.Dist.Tarball,
		SHA256:      computed,
		IntegrityOK: integrityOK,
		ShasumOK:    shasumOK,
		Dir:         target,
	}
	log.Printf("[dshplgadapter] install: installed %s", res.logLine("installed"))
	return res, nil
}
