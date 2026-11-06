package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// 纯 SKILL 交接（阶段 3，外部形态）。
//
// 外部插件拿不到 Events()（proc 模板不注入 subscriber），
// 所以不能像内置插件那样 Publish("skill_detected")。
// 外部形态的等价路径是规划书结论 3 的机制：pluginmgr 在安装时与
// 每次启动时重扫 <插件目录>/skill/（skill_handout.go 的
// handOutPluginSkills / reHandOutAllPluginSkills），自己发 skill_detected。
//
// 于是适配器的工作是**暂存（stage）**：把发现的纯 SKILL 包
// （SKILL.md/skill.json）复制到适配器自己的插件目录 skill/ 下
// （默认 = plugin.bin 所在目录的 skill/ 子目录，即 pluginmgr
// 重扫的位置），下次启动时 pluginmgr 把它们移交给 skillmgr。
// 交接完成的时点是"下次启动重扫"，不是暂存当时——管理工具
// 如实报告 staged（已暂存，待重扫移交），不谎称已注册。

// skillNameForStage 把包名映射成 skill 目录名。
// 包名可能含 scope（@scope/pkg）与斜杠，不能直接当目录名，
// 也不得含 checkSafeName 拒绝的形态。
func skillNameForStage(pkgName string) (string, error) {
	name := strings.ReplaceAll(pkgName, "@", "")
	name = strings.ReplaceAll(name, "/", "__")
	name = strings.Trim(name, " ")
	if name == "" {
		return "", fmt.Errorf("包名 %q 无法映射成 skill 目录名", pkgName)
	}
	if strings.HasPrefix(name, ".") {
		name = "skill-" + name
	}
	if err := checkSafeName(name); err != nil {
		return "", err
	}
	return name, nil
}

// stageSkill 把一个纯 SKILL 包暂存到 stageDir 下。
// 只复制非隐藏文件（递归），跳过符号链接与 node_modules，
// 单文件超过 10MB 跳过并记日志；每个目标路径都过 safeArchiveTarget。
// 目标已存在时先整个删掉再复制（重复暂存幂等）。
func stageSkill(pkg DiscoveredPackage, stageDir string) (string, error) {
	name, err := skillNameForStage(pkg.Name)
	if err != nil {
		return "", err
	}
	dest, err := resolveUnderRoot(stageDir, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return "", fmt.Errorf("创建 skill 暂存根失败: %w", err)
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", fmt.Errorf("清理旧暂存失败: %w", err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("创建 skill 暂存目录失败: %w", err)
	}
	err = filepath.WalkDir(pkg.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(pkg.Dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if base == "node_modules" {
				return filepath.SkipDir
			}
			target, err := safeArchiveTarget(dest, rel)
			if err != nil {
				return err
			}
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			log.Printf("[dshplgadapter] skill stage: 跳过符号链接 %s", rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > 10<<20 {
			log.Printf("[dshplgadapter] skill stage: 跳过超大文件 %s (%d bytes)", rel, info.Size())
			return nil
		}
		target, err := safeArchiveTarget(dest, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(p, target)
	})
	if err != nil {
		os.RemoveAll(dest)
		return "", fmt.Errorf("暂存 skill %s 失败: %w", pkg.Name, err)
	}
	// 暂存结果必须仍是 skillmgr 认得的形态（SKILL.md 或 skill.json），
	// 否则 pluginmgr 重扫时会跳过——暂存一个"看起来不像 skill"的目录
	// 等于没暂存，必须在这里拦住。
	hasSkill := false
	for _, f := range []string{"SKILL.md", "skill.json"} {
		if st, err := os.Stat(filepath.Join(dest, f)); err == nil && !st.IsDir() {
			hasSkill = true
			break
		}
	}
	if !hasSkill {
		os.RemoveAll(dest)
		return "", fmt.Errorf("暂存后 %s 缺 SKILL.md/skill.json，skillmgr 不会认", dest)
	}
	return dest, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
