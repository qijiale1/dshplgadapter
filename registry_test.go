package main

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// fakeSDK 记录注册调用，代替无法在测试里构造的 *sdk.PluginSDK
// （PluginSDK 的构造器未导出；生产侧传 *sdk.PluginSDK，其方法集满足 sdkAPI）。
type fakeSDK struct {
	tools    map[string]sdk.ToolDef
	handlers map[string]sdk.ToolHandler
	inputs   []string
	outputs  []string
	stages   map[sdk.Stage]sdk.StageHandler
}

func (f *fakeSDK) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	if f.tools == nil {
		f.tools = map[string]sdk.ToolDef{}
		f.handlers = map[string]sdk.ToolHandler{}
	}
	f.tools[name] = def
	f.handlers[name] = handler
	return nil
}

func (f *fakeSDK) RegisterInputChannel(name string, def sdk.ChannelDef) error {
	f.inputs = append(f.inputs, name)
	return nil
}

func (f *fakeSDK) RegisterOutputChannel(name string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error {
	f.outputs = append(f.outputs, name)
	return nil
}

func (f *fakeSDK) RegisterStage(stage sdk.Stage, handler sdk.StageHandler, scope ...sdk.StageScope) {
	if f.stages == nil {
		f.stages = map[sdk.Stage]sdk.StageHandler{}
	}
	f.stages[stage] = handler
}

// 判据：插件注册的工具必须出现在 HA 工具表里（不只是日志）。
// 通知里的 Parameters 已是 Node 侧转好的 HA JSON Schema（探针 0.3）。
func TestRegistersToolFromPlugin(t *testing.T) {
	d := NewDispatcher()
	fake := &fakeSDK{}
	notif := `{"name":"read_file","description":"Read a file from disk.","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`
	d.Dispatch("tool", json.RawMessage(notif), "myplug", nil, fake)

	def, ok := fake.tools["myplug_read_file"]
	if !ok {
		t.Fatalf("工具 myplug_read_file 未进表，实际：%v", fake.tools)
	}
	if def.Description != "Read a file from disk." {
		t.Errorf("description = %q", def.Description)
	}
	if def.Parameters["type"] != "object" {
		t.Errorf("parameters.type = %v", def.Parameters["type"])
	}

	// 通知自带 plugin 字段时，工具名前缀以它为准。
	fake2 := &fakeSDK{}
	notif2 := `{"name":"read_file","description":"x","plugin":"otherplug","parameters":{"type":"object","properties":{}}}`
	d.Dispatch("tool", json.RawMessage(notif2), "myplug", nil, fake2)
	if _, ok := fake2.tools["otherplug_read_file"]; !ok {
		t.Errorf("plugin 覆盖未生效，实际：%v", fake2.tools)
	}
}

// 判据：未知注册类型只记录、不崩溃（dsh 注册类型比 OpenClaw 多，
// 这条宽容分支必须保留；未来 dsh 出新类型时先收下来再决定）。
func TestUnknownRegisterTypeDoesNotCrash(t *testing.T) {
	d := NewDispatcher()
	fake := &fakeSDK{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("未知类型导致 panic: %v", r)
		}
	}()
	d.Dispatch("some_future_dsh_type_xyz", json.RawMessage(`{"name":"x","description":"y"}`), "myplug", nil, fake)
	if len(fake.tools) != 0 {
		t.Errorf("未知类型不应注册工具，实际：%v", fake.tools)
	}
	found := false
	for _, c := range d.Capabilities() {
		if strings.Contains(c, "some_future_dsh_type_xyz") {
			found = true
		}
	}
	if !found {
		t.Errorf("未知类型未被记录，实际 capabilities：%v", d.Capabilities())
	}
}
