package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 集成测试：真实 sidecar（node host.js + Cordis）+ 最小 dsh 测试插件。
//
// 跳过条件（只在确实缺环境时跳过，本机 node + dsh 俱全，必须跑）：
// node 不在 PATH，或 dsh 的 node_modules 里没有 cordis/dsh-tools。
// 跳过时用 t.Skipf 大声说明，不静默。

func dshNodeModules(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("DSH_NODE_MODULES"); v != "" {
		return v
	}
	cand := "/usr/lib/node_modules/@deepseek-ai/dsh/node_modules"
	if st, err := os.Stat(filepath.Join(cand, "@deepseek-ai", "cordis")); err == nil && st.IsDir() {
		if st2, err := os.Stat(filepath.Join(cand, "@deepseek-ai", "dsh-tools")); err == nil && st2.IsDir() {
			return cand
		}
	}
	t.Skip("dsh node_modules not found (need @deepseek-ai/cordis + dsh-tools)")
	return ""
}

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not in PATH")
	}
}

func writeTestPlugin(t *testing.T, dir, name, indexJS string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "package.json"), fmt.Sprintf(`{"name":%q,"type":"module","main":"lib/index.js"}`, name))
	writeFile(t, filepath.Join(dir, "lib", "index.js"), indexJS)
}

func launchTestSidecar(t *testing.T, pluginName, pluginDir string) *sidecarProcess {
	t.Helper()
	requireNode(t)
	nm := dshNodeModules(t)
	sp, err := launchSidecar(pluginName, pluginDir, nm)
	if err != nil {
		t.Fatalf("launchSidecar %s: %v", pluginName, err)
	}
	t.Cleanup(func() { sp.Close() })
	return sp
}

// drainUntilTool 从 sidecar 通知里分发，直到 fake 表里出现 wantHAName，
// 或超时。通知本身可能乱序到达（hook 注册等），一律先分发再判断。
func drainUntilTool(t *testing.T, sp *sidecarProcess, d *RegistryDispatcher, fake *fakeSDK, wantHAName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, ok := fake.tools[wantHAName]; ok {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for tool %s; table: %v; capabilities: %v", wantHAName, toolNames(fake), d.Capabilities())
		}
		select {
		case notif := <-sp.NotifyChan():
			dispatchNotif(t, sp, d, fake, notif)
		case <-time.After(remaining):
			t.Fatalf("timed out waiting for tool %s; table: %v", wantHAName, toolNames(fake))
		}
	}
}

func dispatchNotif(t *testing.T, sp *sidecarProcess, d *RegistryDispatcher, fake *fakeSDK, notif DSHNotification) {
	t.Helper()
	var params struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(notif.Params, &params); err != nil {
		t.Fatalf("bad notification params: %v", err)
	}
	d.Dispatch(params.Type, params.Data, sp.name, sp, fake)
}

func toolNames(fake *fakeSDK) []string {
	out := make([]string, 0, len(fake.tools))
	for name := range fake.tools {
		out = append(out, name)
	}
	return out
}

const echoPluginJS = `
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { defineTool } = require('@deepseek-ai/dsh-tools');

export const name = 'test-echo';
export const inject = ['tools'];

export function apply(ctx) {
  ctx.tools.register(defineTool({
    name: 'echo',
    description: 'Echo back the text.',
    parameters: {
      text: { type: 'string', required: true, description: 'Text to echo' },
    },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args) { return 'echo:' + args.text; },
  }));
}
`

// 判据（阶段 1）：装了 → 工具进了 HA 工具表 → 能调。
func TestSidecarRegistersToolEndToEnd(t *testing.T) {
	plugDir := filepath.Join(t.TempDir(), "test-echo")
	writeTestPlugin(t, plugDir, "test-echo", echoPluginJS)
	sp := launchTestSidecar(t, "test-echo", plugDir)

	d := NewDispatcher()
	fake := &fakeSDK{}
	drainUntilTool(t, sp, d, fake, "test-echo_echo", 30*time.Second)

	def, ok := fake.tools["test-echo_echo"]
	if !ok {
		t.Fatalf("工具未进表")
	}
	if def.Description != "Echo back the text." {
		t.Errorf("description = %q", def.Description)
	}
	props, _ := def.Parameters["properties"].(map[string]interface{})
	if props == nil || props["text"] == nil {
		t.Errorf("parameters 缺 text 属性，实际：%v", def.Parameters)
	}

	// 能调：走工具 handler（即 sidecar CallTool）跑一次。
	h := fake.handlers["test-echo_echo"]
	out, err := h(map[string]interface{}{"text": "hello"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out != "echo:hello" {
		t.Errorf("调用结果 = %v，想要 echo:hello", out)
	}

	// tools/list 也能看到它（第二条独立证据）。
	tools, err := sp.ListTools()
	if err != nil {
		t.Fatalf("tools/list 失败: %v", err)
	}
	found := false
	for _, tl := range tools {
		if tl.Name == "echo" {
			found = true
		}
	}
	if !found {
		t.Errorf("tools/list 未见 echo，实际：%v", tools)
	}
}

// 判据（§八）：插件狂打日志时协议仍通。插件在 apply 与 execute 里
// 打大量日志（含伪造的 JSON-RPC 帧与垃圾行）；协议走 fd 3，
// 这些日志只能进 stdout/stderr，绝不能坏帧。
const noisyPluginJS = `
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { defineTool } = require('@deepseek-ai/dsh-tools');

export const name = 'test-noisy';
export const inject = ['tools'];

export function apply(ctx) {
  console.log('{"jsonrpc":"2.0","id":999,"result":{"fake":true}}');
  console.log('garbage line, not json at all {{{');
  for (let i = 0; i < 50; i++) console.log('[noisy-plugin] log line ' + i + ' {"method":"register","params":{}}');
  ctx.tools.register(defineTool({
    name: 'echo',
    description: 'Echo back the text.',
    parameters: {
      text: { type: 'string', required: true },
    },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args) {
      console.log('executing echo, more noise');
      console.log('{"jsonrpc":"2.0","id":998,"result":null}');
      return 'echo:' + args.text;
    },
  }));
}
`

func TestProtocolSurvivesPluginLogging(t *testing.T) {
	plugDir := filepath.Join(t.TempDir(), "test-noisy")
	writeTestPlugin(t, plugDir, "test-noisy", noisyPluginJS)
	sp := launchTestSidecar(t, "test-noisy", plugDir)

	d := NewDispatcher()
	fake := &fakeSDK{}
	drainUntilTool(t, sp, d, fake, "test-noisy_echo", 30*time.Second)

	h := fake.handlers["test-noisy_echo"]
	out, err := h(map[string]interface{}{"text": "hello"})
	if err != nil {
		t.Fatalf("日志干扰下调用失败: %v", err)
	}
	if out != "echo:hello" {
		t.Errorf("日志干扰下调用结果 = %v，想要 echo:hello", out)
	}
}

const gatePluginJS = `
import { createRequire } from 'node:module';
import fs from 'node:fs';
const require = createRequire(import.meta.url);
const { defineTool } = require('@deepseek-ai/dsh-tools');

export const name = 'test-gate';
export const inject = ['tools'];

export function apply(ctx) {
  ctx.tools.register(defineTool({
    name: 'danger_tool',
    description: 'A tool the hook denies.',
    parameters: { x: { type: 'string' } },
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args) {
      fs.appendFileSync(process.env.DSH_TEST_BODY_LOG, 'danger_tool executed\n');
      return 'ran';
    },
  }));
  ctx.on('tools/pre-execute', async (exec, next) => {
    if (exec.name === 'danger_tool') return { kind: 'deny', reason: 'test policy denies danger_tool' };
    return next();
  });
}
`

// newHookPlugin 搭一个带 sidecar 的 Plugin（evaluator 直连真实 sidecar），
// 并返回 before_toolcall 处理器（从 fakeSDK 里取注册结果）。
func newHookPlugin(t *testing.T, hookTimeout time.Duration) (*Plugin, *fakeSDK, sdk.StageHandler) {
	t.Helper()
	p := &Plugin{
		name:            "test",
		hookEvalTimeout: hookTimeout,
		sidecars:        map[string]*sidecarProcess{},
		toolOwners:      map[string]toolOwner{},
		stagedSkills:    map[string]stagedSkill{},
	}
	reg := &StageRegistry{evaluator: p.evaluateHooks, notifier: p.notifyToolResult}
	fake := &fakeSDK{}
	reg.Register(fake)
	h, ok := fake.stages[sdk.StageBeforeToolcall]
	if !ok {
		t.Fatal("before_toolcall 处理器未注册")
	}
	return p, fake, h
}

func addHookSidecar(p *Plugin, sp *sidecarProcess, pluginName string, owned map[string]string) {
	if p.sidecars == nil {
		p.sidecars = map[string]*sidecarProcess{}
	}
	p.sidecars[pluginName] = sp
	p.sidecarOrder = append(p.sidecarOrder, pluginName)
	for haName, dshName := range owned {
		p.toolOwners[haName] = toolOwner{sidecar: sp, dshName: dshName, plugin: pluginName}
	}
}

// 判据（阶段 2）：插件拒绝后 HA 不执行该工具。
func TestToolExecuteHookCanDeny(t *testing.T) {
	bodyLog := filepath.Join(t.TempDir(), "body.log")
	t.Setenv("DSH_TEST_BODY_LOG", bodyLog)

	plugDir := filepath.Join(t.TempDir(), "test-gate")
	writeTestPlugin(t, plugDir, "test-gate", gatePluginJS)
	sp := launchTestSidecar(t, "test-gate", plugDir)

	// 先让工具注册完成（归属表才有 test-gate_danger_tool）。
	d := NewDispatcher()
	fake := &fakeSDK{}
	drainUntilTool(t, sp, d, fake, "test-gate_danger_tool", 30*time.Second)

	p, _, handler := newHookPlugin(t, 5*time.Second)
	addHookSidecar(p, sp, "test-gate", map[string]string{"test-gate_danger_tool": "danger_tool"})

	// 拒绝：HA 工具名 test-gate_danger_tool → 归属 sidecar、dsh 原名 danger_tool → 拒。
	denyCtx := &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: "test-gate_danger_tool", Arguments: map[string]interface{}{"x": "1"}}}}
	if err := handler(denyCtx); err != nil {
		t.Fatalf("handler 报错: %v", err)
	}
	if denyCtx.Response == nil {
		t.Fatal("拒绝判定未设置 Response（HA 会继续执行，判据失败）")
	}
	if !strings.Contains(*denyCtx.Response, "test policy denies danger_tool") {
		t.Errorf("Response = %q，未带 hook 的拒绝理由", *denyCtx.Response)
	}
	// 内核按 Response != nil 跳过执行：测试里模拟这一步——不调 CallTool，
	// 断言 body 从未跑（side-effect 文件不存在）。
	if _, err := os.Stat(bodyLog); err == nil {
		t.Error("被拒绝的工具 body 居然跑了（body.log 已生成）")
	}

	// 放行：非归属工具名 → 问全部 sidecar，hook 对非 danger_tool 放行。
	allowCtx := &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: "other_tool", Arguments: map[string]interface{}{}}}}
	if err := handler(allowCtx); err != nil {
		t.Fatalf("handler 报错: %v", err)
	}
	if allowCtx.Response != nil {
		t.Errorf("放行判定不应设置 Response，实际：%q", *allowCtx.Response)
	}
}

const hangPluginJS = `
export const name = 'test-hang';
export function apply(ctx) {
  ctx.on('tools/pre-execute', async (exec, next) => {
    await new Promise(() => {}); // 永不 settle：模拟挂死的钩子
  });
}
`

const allowPluginJS = `
export const name = 'test-allow';
export function apply(ctx) {
  ctx.on('tools/pre-execute', async (exec, next) => next());
}
`

// 判据（阶段 2）：多插件串联不卡死（带超时）。挂死的钩子 + 正常钩子
// 并存时，判定必须在有界时间内完成；挂死的钩子超时后 fail-open，
// 且 sidecar 不被拖死（第二次判定仍能正常完成）。
func TestHookChainDoesNotDeadlock(t *testing.T) {
	hangDir := filepath.Join(t.TempDir(), "test-hang")
	writeTestPlugin(t, hangDir, "test-hang", hangPluginJS)
	hangSp := launchTestSidecar(t, "test-hang", hangDir)

	allowDir := filepath.Join(t.TempDir(), "test-allow")
	writeTestPlugin(t, allowDir, "test-allow", allowPluginJS)
	allowSp := launchTestSidecar(t, "test-allow", allowDir)

	p, _, handler := newHookPlugin(t, 2*time.Second)
	addHookSidecar(p, hangSp, "test-hang", nil)
	addHookSidecar(p, allowSp, "test-allow", nil)

	run := func(toolName string) (*sdk.StageContext, time.Duration) {
		ctx := &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: toolName, Arguments: map[string]interface{}{}}}}
		start := time.Now()
		if err := handler(ctx); err != nil {
			t.Fatalf("handler 报错: %v", err)
		}
		return ctx, time.Since(start)
	}

	// 子测试 1：挂死 + 放行 → 放行，且有界（2s 超时 + 余量，15s 为上限）。
	ctx1, el1 := run("some_tool")
	if el1 > 15*time.Second {
		t.Errorf("判定耗时 %v，超过 15s 上限（链疑似卡死）", el1)
	}
	if ctx1.Response != nil {
		t.Errorf("挂死+放行应放行，实际 Response：%q", *ctx1.Response)
	}
	// 第二次判定仍能完成：sidecar 没被第一次的超时拖死。
	ctx2, el2 := run("some_tool")
	if el2 > 15*time.Second {
		t.Errorf("第二次判定耗时 %v，sidecar 疑似被拖死", el2)
	}
	if ctx2.Response != nil {
		t.Errorf("第二次判定应放行，实际 Response：%q", *ctx2.Response)
	}
}

// 挂死 + 拒绝 → 拒绝胜出（deny wins），同样有界。
func TestHookChainDenyWinsOverHang(t *testing.T) {
	hangDir := filepath.Join(t.TempDir(), "test-hang")
	writeTestPlugin(t, hangDir, "test-hang", hangPluginJS)
	hangSp := launchTestSidecar(t, "test-hang", hangDir)

	gateDir := filepath.Join(t.TempDir(), "test-gate")
	writeTestPlugin(t, gateDir, "test-gate", gatePluginJS)
	gateSp := launchTestSidecar(t, "test-gate", gateDir)

	p, _, handler := newHookPlugin(t, 2*time.Second)
	addHookSidecar(p, hangSp, "test-hang", nil)
	addHookSidecar(p, gateSp, "test-gate", nil)

	// 非归属名 danger_tool：问全部 sidecar，gate 以 dsh 原名判定拒绝。
	ctx := &sdk.StageContext{ToolCalls: []sdk.ToolCall{{Name: "danger_tool", Arguments: map[string]interface{}{}}}}
	start := time.Now()
	if err := handler(ctx); err != nil {
		t.Fatalf("handler 报错: %v", err)
	}
	el := time.Since(start)
	if el > 15*time.Second {
		t.Errorf("判定耗时 %v，超过 15s 上限", el)
	}
	if ctx.Response == nil {
		t.Fatal("挂死+拒绝应拒绝，Response 未设置")
	}
	if !strings.Contains(*ctx.Response, "test policy denies danger_tool") {
		t.Errorf("Response = %q，未带拒绝理由", *ctx.Response)
	}
}

const effectsPluginJS = `
import fs from 'node:fs';
export const name = 'test-effects';
export function apply(ctx) {
  ctx.effect(() => () => { fs.appendFileSync(process.env.DSH_TEST_EFFECT_LOG, 'first\n'); }, 'first');
  ctx.effect(() => () => { fs.appendFileSync(process.env.DSH_TEST_EFFECT_LOG, 'second\n'); }, 'second');
}
`

// 判据（阶段 3）：清理函数在 Stop 时逆序调用。
func TestEffectsRunInReverseOnStop(t *testing.T) {
	effectLog := filepath.Join(t.TempDir(), "effects.log")
	t.Setenv("DSH_TEST_EFFECT_LOG", effectLog)

	plugDir := filepath.Join(t.TempDir(), "test-effects")
	writeTestPlugin(t, plugDir, "test-effects", effectsPluginJS)
	sp := launchTestSidecar(t, "test-effects", plugDir)

	if err := sp.Dispose(); err != nil {
		t.Fatalf("dispose 失败: %v", err)
	}
	data, err := os.ReadFile(effectLog)
	if err != nil {
		t.Fatalf("effect 日志未生成（清理函数没跑？）: %v", err)
	}
	if string(data) != "second\nfirst\n" {
		t.Errorf("清理顺序 = %q，想要 second\\nfirst\\n（逆序）", string(data))
	}
}

const reloadV1JS = `
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { defineTool } = require('@deepseek-ai/dsh-tools');
export const name = 'test-reload';
export const inject = ['tools'];
export function apply(ctx) {
  ctx.tools.register(defineTool({
    name: 'versioned',
    description: 'v1',
    parameters: {},
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute() { return 'v1'; },
  }));
}
`

const reloadV2JS = `
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { defineTool } = require('@deepseek-ai/dsh-tools');
export const name = 'test-reload';
export const inject = ['tools'];
export function apply(ctx) {
  ctx.tools.register(defineTool({
    name: 'versioned',
    description: 'v2',
    parameters: {},
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute() { return 'v2'; },
  }));
}
`

// 判据（阶段 3）：热重载——改文件后 reload，新工具定义覆盖旧的，且能调。
func TestHotReload(t *testing.T) {
	plugDir := filepath.Join(t.TempDir(), "test-reload")
	writeTestPlugin(t, plugDir, "test-reload", reloadV1JS)
	sp := launchTestSidecar(t, "test-reload", plugDir)

	d := NewDispatcher()
	fake := &fakeSDK{}
	drainUntilTool(t, sp, d, fake, "test-reload_versioned", 30*time.Second)
	if fake.tools["test-reload_versioned"].Description != "v1" {
		t.Fatalf("重载前 description = %q，想要 v1", fake.tools["test-reload_versioned"].Description)
	}

	writeFile(t, filepath.Join(plugDir, "lib", "index.js"), reloadV2JS)
	st, err := sp.Reload()
	if err != nil {
		t.Fatalf("reload 失败: %v", err)
	}
	if st.State != "ACTIVE" {
		t.Fatalf("重载后状态 = %s，想要 ACTIVE", st.State)
	}

	// 等新注册通知落地（同名覆盖），再断言。
	deadline := time.Now().Add(30 * time.Second)
	for fake.tools["test-reload_versioned"].Description != "v2" {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("重载后 description 未变 v2，实际：%q", fake.tools["test-reload_versioned"].Description)
		}
		select {
		case notif := <-sp.NotifyChan():
			dispatchNotif(t, sp, d, fake, notif)
		case <-time.After(remaining):
			t.Fatalf("重载后 description 未变 v2，实际：%q", fake.tools["test-reload_versioned"].Description)
		}
	}
	out, err := fake.handlers["test-reload_versioned"](map[string]interface{}{})
	if err != nil {
		t.Fatalf("重载后调用失败: %v", err)
	}
	if out != "v2" {
		t.Errorf("重载后调用结果 = %v，想要 v2", out)
	}
}

// 判据（阶段 1，规则 3 的操作员入口）：手动试加载一个规则 3 候选，
// 成功则工具进表；加载失败则返回错误、包被跳过。
func TestLaunchRule3Candidate(t *testing.T) {
	plugDir := filepath.Join(t.TempDir(), "entry-only")
	writeTestPlugin(t, plugDir, "entry-only", echoPluginJS)
	// 注意：echoPluginJS 的 name 是 test-echo；这里改写包名以外的部分：
	// 包发现按 package.json 的 name 认，这里故意让包名与模块名不一致，
	// 验证 launchOne 按发现名找包、按模块名注册工具。
	// （为简单起见，模块名仍为 test-echo，HA 工具名以通知的 plugin 为准。）

	p := &Plugin{
		name:          "test",
		discoveryRoot: filepath.Dir(plugDir),
		dispatcher:    NewDispatcher(),
		sidecars:      map[string]*sidecarProcess{},
		toolOwners:    map[string]toolOwner{},
		stagedSkills:  map[string]stagedSkill{},
		notifyCh:      make(chan notifyItem, 1024),
		notifyStop:    make(chan struct{}),
	}
	p.discovered = DiscoverPlugins(p.discoveryRoot)
	found := false
	for _, dd := range p.discovered {
		if dd.Name == "entry-only" && dd.Kind == KindMaybePlugin {
			found = true
		}
	}
	if !found {
		t.Fatalf("entry-only 未被发现为规则 3 候选，发现结果：%v", p.discovered)
	}

	requireNode(t)
	nm := dshNodeModules(t)
	// launchOne 用 discoveryRoot 当 NODE_PATH；测试里发现根是临时目录，
	// 没有 dsh 包，所以把真实 dsh node_modules 追加进进程 NODE_PATH，
	// launchSidecar 会把它前置的发现根与这里的 NODE_PATH 拼起来解析。
	t.Setenv("NODE_PATH", nm)
	res, err := p.launchOne("entry-only")
	if err != nil {
		t.Fatalf("launchOne 失败: %v", err)
	}
	if res["state"] != "ACTIVE" {
		t.Fatalf("launchOne 状态 = %v，想要 ACTIVE", res)
	}
	sp := p.sidecars["entry-only"]
	if sp == nil {
		t.Fatal("launchOne 后 sidecar 未登记")
	}
	t.Cleanup(func() {
		close(p.notifyStop)
		sp.Close()
	})

	fake := &fakeSDK{}
	// 通知经 forwarder 扇入 p.notifyCh，测试从这里分发。
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := fake.tools["test-echo_echo"]; ok {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("手动加载后工具未进表，实际：%v", toolNames(fake))
		}
		select {
		case item := <-p.notifyCh:
			p.handleNotification(item, fake)
		case <-time.After(remaining):
			t.Fatalf("手动加载后工具未进表，实际：%v", toolNames(fake))
		}
	}
}
