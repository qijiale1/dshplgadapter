package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// 包发现（规划书 §3.3，阶段 1 骨架版）。
//
// 扫描 <profile>/node_modules/（含 @scope/ 作用域目录与无作用域目录）。
// 一个目录算 dsh 插件，当且仅当：
//
//	(1) package.json 能解析，并且
//	(2) package.json 有 dsh 字段，或者 cordis.patch.yml 存在。
//
// 规则 (3)（入口能加载且导出 apply 才算插件）是**动态**判定 ——
// "试加载，加载失败就当普通包跳过"，静态扫描（ESM 正则）两头误判。
// 但自动试加载的 blast radius 太大：node_modules 里任何带入口的包
// 都会被 import（顶层代码随即执行），而不只是"已声明的 dsh 插件"。
// 用户 2026-11-04 的决策是"允许 dsh 插件跑任意命令"，不是
// "允许所有带入口的包跑任意命令"。所以规则 (3) 候选在 Start 时
// 只标记、不自动拉起；操作员可用 dshplgadapter_launch 手动试加载
// 一个候选（失败即跳过，与规划书原意一致）。这是对比规划书
// §3.3 字面的有意偏离，见 README。
//
// 安全与范围：
//   - 只读本地已装包：本文件没有任何下载、安装、解压逻辑，
//     阶段 1 不支持远程安装（规划书 §七，远程安装是阶段 4 且默认关闭）；
//   - 目录名逐段过 resolveUnderRoot（pathsafe.go），".." 逃不出扫描根；
//   - 单个包坏了（package.json 解析失败、目录不可读）只记日志跳过，
//     绝不中断整轮扫描（与 clawhubadapter Start 的 continue 语义一致）；
//   - 隐藏目录（. 开头）跳过。

// KindDSHPlugin 是规则 (1)+(2) 确认的 dsh 插件。
const KindDSHPlugin = "dsh"

// KindMaybePlugin 是规则 (3) 候选：有入口、无 dsh 标记，尚未试加载。
const KindMaybePlugin = "maybe"

// KindSkill 是纯 SKILL 条目：有 SKILL.md/skill.json，但无插件标记。
// （阶段 3：暂存到适配器 skill/ 下，等 pluginmgr 启动重扫移交。）
const KindSkill = "skill"

type DiscoveredPackage struct {
	Name     string // package.json 的 name；缺失时用目录名
	Dir      string // 包目录绝对路径
	Kind     string // KindDSHPlugin、KindMaybePlugin 或 KindSkill
	Reason   string // 判定的依据（dsh 字段 / cordis.patch.yml / 可加载入口 / SKILL.md）
	HasSkill bool   // 包内是否带 SKILL.md/skill.json（插件包也可能带 skill）
}

// DiscoverPlugins 静态扫描 nodeModulesDir，返回发现的包。
// 根目录缺失或不可读时记日志并返回空 —— 启动不因此失败。
func DiscoverPlugins(nodeModulesDir string) []DiscoveredPackage {
	var out []DiscoveredPackage
	entries, err := os.ReadDir(nodeModulesDir)
	if err != nil {
		log.Printf("[dshplgadapter] discovery root unreadable: %s: %v", nodeModulesDir, err)
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if strings.HasPrefix(e.Name(), "@") {
			scopeDir, err := resolveUnderRoot(nodeModulesDir, e.Name())
			if err != nil {
				log.Printf("[dshplgadapter] skip scope %q: %v", e.Name(), err)
				continue
			}
			subs, err := os.ReadDir(scopeDir)
			if err != nil {
				log.Printf("[dshplgadapter] scope unreadable: %s: %v", scopeDir, err)
				continue
			}
			for _, sub := range subs {
				if !sub.IsDir() || strings.HasPrefix(sub.Name(), ".") {
					continue
				}
				pkgDir, err := resolveUnderRoot(scopeDir, sub.Name())
				if err != nil {
					log.Printf("[dshplgadapter] skip package %q: %v", sub.Name(), err)
					continue
				}
				if d, ok := checkPackage(pkgDir); ok {
					out = append(out, d)
				}
			}
			continue
		}
		pkgDir, err := resolveUnderRoot(nodeModulesDir, e.Name())
		if err != nil {
			log.Printf("[dshplgadapter] skip package %q: %v", e.Name(), err)
			continue
		}
		if d, ok := checkPackage(pkgDir); ok {
			out = append(out, d)
		}
	}
	return out
}

func checkPackage(dir string) (DiscoveredPackage, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err == nil {
		var pkg struct {
			Name string          `json:"name"`
			Main string          `json:"main"`
			DSH  json.RawMessage `json:"dsh"`
		}
		if err := json.Unmarshal(data, &pkg); err != nil {
			// package.json 坏了：先看是不是裸 SKILL 目录（下文），
			// 不是才记日志跳过，不中断扫描（TestPluginLoadFailureDoesNotAbortScan）。
			log.Printf("[dshplgadapter] %s: package.json 解析失败: %v", dir, err)
		} else {
			name := pkg.Name
			if name == "" {
				name = filepath.Base(dir)
			}
			// 包内是否带 skill（SKILL.md 或 skill.json，skillmgr 的双判据）。
			hasSkill := false
			skillFile := ""
			for _, f := range []string{"SKILL.md", "skill.json"} {
				if st, err := os.Stat(filepath.Join(dir, f)); err == nil && !st.IsDir() {
					hasSkill = true
					skillFile = f
					break
				}
			}
			if len(pkg.DSH) > 0 {
				return DiscoveredPackage{Name: name, Dir: dir, Kind: KindDSHPlugin, Reason: "package.json dsh field", HasSkill: hasSkill}, true
			}
			if _, err := os.Stat(filepath.Join(dir, "cordis.patch.yml")); err == nil {
				return DiscoveredPackage{Name: name, Dir: dir, Kind: KindDSHPlugin, Reason: "cordis.patch.yml", HasSkill: hasSkill}, true
			}
			// 纯 SKILL 条目（阶段 3）：有 SKILL.md/skill.json 但无插件标记。
			if hasSkill {
				return DiscoveredPackage{Name: name, Dir: dir, Kind: KindSkill, Reason: "pure skill (" + skillFile + ")", HasSkill: true}, true
			}
			// 规则 (3) 候选：只标记，不自动拉起。
			//
			// 规划书 §3.3 原文是"试着加载，加载失败就当普通包跳过"。
			// 但自动拉起的 blast radius 太大：node_modules 里任何带 index.js
			// 的包都会被 import（顶层代码随即执行），而不只是"已声明的 dsh 插件"。
			// 用户 2026-11-04 的决策是"允许 dsh 插件跑任意命令"，不是
			// "允许所有带入口的包跑任意命令"。所以规则 3 候选只标记、
			// 由 dshplgadapter_launch 手动试加载（失败即跳过，与原意一致），
			// 不在 Start 时自动拉起。这是对比规划书字面的有意偏离，见 README。
			candidates := []string{}
			if pkg.Main != "" {
				candidates = append(candidates, pkg.Main)
			}
			candidates = append(candidates, "lib/index.js", "index.js")
			for _, c := range candidates {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(c))); err == nil {
					return DiscoveredPackage{Name: name, Dir: dir, Kind: KindMaybePlugin, Reason: "loadable entry, no dsh marker (rule-3 candidate)"}, true
				}
			}
			return DiscoveredPackage{}, false
		}
	}
	// 无 package.json（或解析失败）：裸 SKILL 目录也算纯 SKILL 条目
	// （只有 SKILL.md/skill.json 的目录，阶段 3 同样暂存移交）。
	// 既无 package.json 又无 skill 文件的目录不算发现。
	for _, f := range []string{"SKILL.md", "skill.json"} {
		if st, err := os.Stat(filepath.Join(dir, f)); err == nil && !st.IsDir() {
			return DiscoveredPackage{Name: filepath.Base(dir), Dir: dir, Kind: KindSkill, Reason: "pure skill (" + f + ", no package.json)", HasSkill: true}, true
		}
	}
	return DiscoveredPackage{}, false
}
