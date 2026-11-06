package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 工具名前缀规则必须只有一份（ToolRegistry 与归属表共用），钉住它。
func TestToolHAName(t *testing.T) {
	if got := toolHAName("myplug", "read_file"); got != "myplug_read_file" {
		t.Errorf("toolHAName = %q，想要 myplug_read_file", got)
	}
}

// 阶段处理器单元测试（不用 sidecar）：deny 设 Response、allow 不设、
// 空调用不 panic；after 处理器把结果交给 notifier。
func TestStageHandlersDenyAllow(t *testing.T) {
	var notified []string
	r := &StageRegistry{
		evaluator: func(name string, args map[string]interface{}) (bool, string) {
			if name == "bad_tool" {
				return true, "test policy"
			}
			return false, ""
		},
		notifier: func(name string, result interface{}) {
			notified = append(notified, name)
		},
	}
	fake := &fakeSDK{}
	r.Register(fake)
	before, ok := fake.stages[sdk.StageBeforeToolcall]
	if !ok {
		t.Fatal("before_toolcall 未注册")
	}
	after, ok := fake.stages[sdk.StageAfterToolcall]
	if !ok {
		t.Fatal("after_toolcall 未注册")
	}

	denyCtx := &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: "bad_tool"}}}
	if err := before(denyCtx); err != nil {
		t.Fatalf("before handler 报错: %v", err)
	}
	if denyCtx.Response == nil || !strings.Contains(*denyCtx.Response, "test policy") {
		t.Errorf("deny 应设置带理由的 Response，实际：%v", denyCtx.Response)
	}

	allowCtx := &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: "good_tool"}}}
	if err := before(allowCtx); err != nil {
		t.Fatalf("before handler 报错: %v", err)
	}
	if allowCtx.Response != nil {
		t.Errorf("allow 不应设置 Response，实际：%q", *allowCtx.Response)
	}

	emptyCtx := &sdk.StageContext{}
	if err := before(emptyCtx); err != nil {
		t.Fatalf("空调用不应报错: %v", err)
	}

	resCtx := &sdk.StageContext{ToolResults: []sdk.ToolResult{{Name: "good_tool", Result: "ok"}}}
	if err := after(resCtx); err != nil {
		t.Fatalf("after handler 报错: %v", err)
	}
	if len(notified) != 1 || notified[0] != "good_tool" {
		t.Errorf("after 应通知一次 good_tool，实际：%v", notified)
	}
}

// 判据（阶段 3）：造一个只有 SKILL.md 的目录，暂存后仍是
// skillmgr 认得的形态（SKILL.md 在位、内容一致），且管理侧可见。
func TestSkillHandoffToSkillmgr(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "my-skill")
	writeFile(t, filepath.Join(skillDir, "SKILL.md"), "# My Skill\n\nDo things.\n")
	writeFile(t, filepath.Join(skillDir, "notes.txt"), "helper notes")
	// 一个指向根外的符号链接：暂存必须跳过它，不能跟出去。
	if err := os.Symlink("/etc/hostname", filepath.Join(skillDir, "evil-link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "plain", "package.json"), `{"name":"plain"}`)

	found := DiscoverPlugins(root)
	var skillPkg *DiscoveredPackage
	for i := range found {
		if found[i].Name == "my-skill" {
			skillPkg = &found[i]
		}
	}
	if skillPkg == nil {
		t.Fatalf("my-skill 未被发现，发现结果：%v", found)
	}
	if skillPkg.Kind != KindSkill {
		t.Errorf("my-skill kind = %q，想要 %q", skillPkg.Kind, KindSkill)
	}

	stageDir := t.TempDir()
	dest, err := stageSkill(*skillPkg, stageDir)
	if err != nil {
		t.Fatalf("stageSkill 失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatalf("暂存后 SKILL.md 不在：%v", err)
	}
	if string(data) != "# My Skill\n\nDo things.\n" {
		t.Errorf("暂存后 SKILL.md 内容变了：%q", string(data))
	}
	if _, err := os.Stat(filepath.Join(dest, "notes.txt")); err != nil {
		t.Errorf("暂存后 notes.txt 丢了：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "evil-link")); err == nil {
		t.Error("符号链接不应被跟进暂存")
	}

	// 重复暂存幂等（旧目录先清再复制，结果一致）。
	dest2, err := stageSkill(*skillPkg, stageDir)
	if err != nil {
		t.Fatalf("重复暂存失败: %v", err)
	}
	if dest2 != dest {
		t.Errorf("重复暂存目标变了：%q vs %q", dest2, dest)
	}
}

// skill 名映射：scope 与斜杠不能逃出暂存根。
func TestSkillNameForStage(t *testing.T) {
	cases := map[string]string{
		"my-skill":     "my-skill",
		"@scope/pkg-c": "scope__pkg-c",
		"a/b":          "a__b",
		// ".." 不会逃出：它先被改写成单层目录名 skill-..，
		// 再过 checkSafeName + resolveUnderRoot 双保险，仍在暂存根内。
		"..": "skill-..",
	}
	for in, want := range cases {
		got, err := skillNameForStage(in)
		if err != nil {
			t.Errorf("skillNameForStage(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("skillNameForStage(%q) = %q，想要 %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "@", "   "} {
		if _, err := skillNameForStage(bad); err == nil {
			t.Errorf("skillNameForStage(%q) 应报错", bad)
		}
	}
}
