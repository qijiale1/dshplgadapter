package main

// 外部插件形态判据（从 clawhubadapter/external_form_test.go 移植，2026-11-04）。
//
// ## 这些判据钉的是「它真的能当外部插件用」
//
// dshplgadapter 生来就是外部插件（`package main` + `NewPluginFactory` +
// `plugin.bin`），风险不在业务逻辑，而在**外部插件的能力边界**：
//
//	① 入口名必须是 NewPluginFactory（proc 运行时就找这个名字）
//	② 不能依赖任何 internal/* 包（外部插件拿不到）
//	③ 不能用 only-builtin 能力（尤其 Events().Publish）
//
// 前两条错了是编译期红，第三条错了是**运行期静默失效**（`Events()` 返回 nil，
// 调 `.Publish` 直接 panic 或什么也不做）。所以第 ③ 条必须有判据守着。
// 纯 SKILL 移交（阶段 3）不走事件发布：走 .hmap 内 bundle 的 skill/ 目录，
// 由 pluginmgr 在安装与每次启动重扫时移交 skillmgr（规划书结论 3）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 判据：入口函数名必须是 NewPluginFactory。
//
// hmapdev 生成的 proc 运行时按**名字**调用
// （`z_proc_gen.go` 里 `NewPluginFactory(...)`）。写错成 NewPlugin 时
// 报的是 `undefined: NewPluginFactory` —— 但那是**生成代码**报的错，
// 读者只看得到自己的文件名对不上，第一次遇到很难反应过来。
func TestEntryPointIsNewPluginFactory(t *testing.T) {
	found := false
	for _, f := range goFilesInDir(t, ".") {
		fs := token.NewFileSet()
		af, err := parser.ParseFile(fs, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue // 只看包级函数
			}
			if fn.Name.Name != "NewPluginFactory" {
				continue
			}
			found = true
			// 签名必须是 (string, map[string]interface{}) (sdk.Plugin, error)
			params := fn.Type.Params.List
			if len(params) != 2 {
				t.Errorf("NewPluginFactory 必须收两个参数，实际 %d 个", len(params))
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 2 {
				t.Errorf("NewPluginFactory 必须返回两个值 (sdk.Plugin, error)")
			}
		}
	}
	if !found {
		t.Fatal("找不到 NewPluginFactory —— proc 运行时按此名调用，缺失则外部形态不可用")
	}
}

// 判据：不得 import 内核 internal/* 包。
//
// 外部插件编译时用的是公版 SDK（github.com/JianFeeeee/hologramagentsdk），
// 内核仓的 internal/* 对它**不存在**。这条在编译期自然成立，
// 但判据仍有价值：它把"迁移时漏改某个 import"变成一个明确的错误信息，
// 而不是让 `hmapdev build` 吐一段难读的 go 编译输出。
func TestNoKernelInternalImports(t *testing.T) {
	const forbidden = "HologramAgent/internal"
	for _, f := range goFilesInDir(t, ".") {
		fs := token.NewFileSet()
		af, err := parser.ParseFile(fs, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, forbidden) {
				t.Errorf("%s 仍 import 内核内部包 %q —— 外部插件拿不到", filepath.Base(f), path)
			}
		}
	}
}

// 判据：不得调用 Events().Publish。
//
// 内置版用 `s.Events().Publish("skill_detected", ...)` 把纯 SKILL 条目
// 移交 skillmgr。外部插件做不到，三层证据：
//
//	① capability_test.go 把 events.publish 列进 forbiddenPrefixes
//	   —— 内核刻意不提供，且有测试守着不许加回；
//	② proc 协议表只有 events.subscribe / events.unsubscribe，没有 publish；
//	③ 外部插件模板不注入 subscriber ⇒ Events() 恒为 nil。
//
// 真实后果：`Events()` 返回 nil，调 `.Publish` **直接 nil pointer panic**。
// 而这条路径只在"扫到纯 SKILL 条目"时才走 —— 启动时不报错，
// 真到用户装了个纯 SKILL 才崩，是最难查的一类。
func TestNoEventPublish(t *testing.T) {
	for _, f := range goFilesInDir(t, ".") {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		text := string(src)
		if strings.Contains(text, "Events().Publish") {
			// 注释里提到是允许的（本文件的说明就要提到它）。
			// 判据只看**代码行**：去掉行注释后再判断。
			for i, line := range strings.Split(text, "\n") {
				code := line
				if idx := strings.Index(code, "//"); idx >= 0 {
					code = code[:idx]
				}
				if strings.Contains(code, "Events().Publish") {
					t.Errorf("%s:%d 调了 Events().Publish —— 外部插件 Events() 恒为 nil，会 panic",
						filepath.Base(f), i+1)
				}
			}
		}
	}
}

// 判据：safeClose 对两种 channel 类型都能用（泛型真的覆盖了调用点）。
//
// 移植时真实踩过：第一版把 safeClose 写成 `chan struct{}` 专用，
// 而 pendingResp.ch 是 `chan []byte` ⇒ 编译期红。
// 这条钉住"不许窄化回去"。
func TestSafeCloseCoversAllChannelKinds(t *testing.T) {
	// chan struct{}
	var o1 onceFlag
	c1 := make(chan struct{})
	safeClose(&o1.o, c1)
	if _, ok := <-c1; ok {
		t.Error("chan struct{} 未关闭")
	}

	// chan []byte（pendingResp 的形态）
	var o2 onceFlag
	c2 := make(chan []byte, 1)
	safeClose(&o2.o, c2)
	if _, ok := <-c2; ok {
		t.Error("chan []byte 未关闭")
	}

	// 二次调用不得 panic（这正是 safeClose 存在的理由）
	safeClose(&o1.o, c1)
	safeClose(&o2.o, c2)
}

// ── 工具 ────────────────────────────────────────────────────

// onceFlag 把 sync.Once 与它的 channel 配对，避免重复写 var o sync.Once。
type onceFlag struct{ o sync.Once }

func goFilesInDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue // 判据不检查自己，避免自指假阳性
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}
