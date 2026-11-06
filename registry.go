package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 从 clawhubadapter/registry.go 移植（2026-11-04）。
//
// 移植时改动（转译结构与 default 宽容分支原样保留）：
//   ① 注册表收敛到一个 sdkAPI 接口（RegisterTool + 通道注册），而不是
//      直接依赖 *sdk.PluginSDK：PluginSDK 的构造器未导出，测试里建不出
//      带 fake 注册器的实例；接口让 TestRegistersToolFromPlugin 能用
//      fake 断言"工具真的进了表"，而不只是"日志打了"。生产侧传
//      *sdk.PluginSDK（它的方法集正好满足该接口），行为不变；
//   ② 日志前缀与 OC 字样 → dsh；
//   ③ 工具通知的 Parameters 被约定为**已经转好的 HA JSON Schema**
//      （Node 侧 host.js 用 dsh-tools 自带的 parameterSchemaSpecToJsonSchema
//      转完再通知，探针 0.3 已验证该方向无损）；Go 侧不再二次转译。
//
// 阶段 1 只接线 tool 分支；provider / channel / stage 的表结构先原样
// 移植（dsh 的 provider 类型表、通道与 hook 语义是阶段 2/3 的工作），
// 其 Dispatch 在骨架阶段同样可走，但 Start 并不拉起 sidecar，所以
// 骨架阶段不会有真实通知进来。

var (
	channelInputBuf   = map[string][]map[string]interface{}{}
	channelInputBufMu sync.Mutex
	channelStatus     map[string]map[string]interface{} // 通道运行状态（channel_status 通知）
	channelStatusMu   sync.Mutex
)

// sdkAPI 是注册表用到的 HA SDK 能力子集（生产侧由 *sdk.PluginSDK 满足）。
type sdkAPI interface {
	RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error
	RegisterInputChannel(name string, def sdk.ChannelDef) error
	RegisterOutputChannel(name string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error
	RegisterStage(stage sdk.Stage, handler sdk.StageHandler, scope ...sdk.StageScope)
}

// toolHAName 是 HA 工具名前缀规则（插件名_dsh工具名），
// ToolRegistry 与阶段 2 的工具归属表共用同一规则，不得各写一份。
func toolHAName(pluginName, dshToolName string) string {
	return fmt.Sprintf("%s_%s", pluginName, dshToolName)
}

type ToolRegistry struct{}

func (r *ToolRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s sdkAPI) {
	var d struct {
		Name        string                 `json:"name"`
		Label       string                 `json:"label"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
		Plugin      string                 `json:"plugin"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
		return
	}
	pn := pluginName
	if d.Plugin != "" {
		pn = d.Plugin
	}
	toolName := toolHAName(pn, d.Name)
	tDef := sdk.ToolDef{
		Name:        toolName,
		Description: d.Description,
		Parameters:  d.Parameters,
	}
	handler := func(sp *sidecarProcess, dshToolName string) sdk.ToolHandler {
		return func(args map[string]interface{}) (interface{}, error) {
			return sp.CallTool(dshToolName, args)
		}
	}(sp, d.Name)
	if err := s.RegisterTool(toolName, tDef, handler); err != nil {
		log.Printf("[dshplgadapter] register tool %s: %v", toolName, err)
	}
}

type providerDef struct {
	dshType    string
	toolSuffix string
	desc       string
}

// providerMap 暂沿用 clawhubadapter 的类型表；dsh 的 provider 类型表
// 是阶段 2 的工作，到时按 ctx 实际提供的 provider 形态替换。
var providerMap = []providerDef{
	{"image_generation", "generate_image", "根据文本描述生成图片，返回图片 URL"},
	{"music_generation", "generate_music", "根据描述生成音乐"},
	{"video_generation", "generate_video", "根据描述生成视频"},
	{"speech", "synthesize_speech", "将文本合成为语音"},
	{"web_search", "web_search", "搜索互联网信息"},
	{"web_fetch", "web_fetch", "获取指定网页的内容"},
	{"media_understanding", "analyze_media", "分析图片、音频或视频内容"},
	{"realtime_transcription", "transcribe_audio", "将音频转写为文字"},
	{"realtime_voice", "voice_io", "实时语音输入输出"},
}

type ProviderRegistry struct{}

func (r *ProviderRegistry) Dispatch(typeStr string, data json.RawMessage, pluginName string, sp *sidecarProcess, s sdkAPI) {
	var d struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Plugin      string `json:"plugin"`
	}
	json.Unmarshal(data, &d)

	pn := pluginName
	if d.Plugin != "" {
		pn = d.Plugin
	}

	// Strip _provider suffix: "image_generation_provider" -> "image_generation"
	lookupType := typeStr
	if lookupType != "provider" {
		lookupType = strings.TrimSuffix(lookupType, "_provider")
	}
	if lookupType == "" {
		return
	}

	for _, p := range providerMap {
		if p.dshType == lookupType {
			toolName := fmt.Sprintf("%s_%s", pn, p.toolSuffix)
			desc := p.desc
			if d.Name != "" {
				desc = fmt.Sprintf("[%s] %s", d.Name, desc)
			}
			props := map[string]interface{}{
				"prompt": map[string]interface{}{"type": "string", "description": "Prompt for generation or search query"},
			}
			switch p.dshType {
			case "image_generation":
				props["prompt"] = map[string]interface{}{"type": "string", "description": "Image description prompt"}
				props["size"] = map[string]interface{}{"type": "string", "description": "Image size (e.g. 1024x1024)", "enum": []interface{}{"256x256", "512x512", "1024x1024", "1792x1024", "1024x1792"}}
			case "web_search":
				props["query"] = map[string]interface{}{"type": "string", "description": "Search query"}
				delete(props, "prompt")
			case "web_fetch":
				props["url"] = map[string]interface{}{"type": "string", "description": "URL to fetch"}
				delete(props, "prompt")
			case "media_understanding":
				props["url"] = map[string]interface{}{"type": "string", "description": "Media URL to analyze"}
				props["media_type"] = map[string]interface{}{"type": "string", "description": "Media type", "enum": []interface{}{"image", "audio", "video"}}
			case "speech":
				props["text"] = map[string]interface{}{"type": "string", "description": "Text to synthesize"}
				props["voice"] = map[string]interface{}{"type": "string", "description": "Voice identifier"}
			case "music_generation":
				props["prompt"] = map[string]interface{}{"type": "string", "description": "Music description prompt"}
				props["duration"] = map[string]interface{}{"type": "number", "description": "Duration in seconds"}
			case "video_generation":
				props["prompt"] = map[string]interface{}{"type": "string", "description": "Video description prompt"}
				props["duration"] = map[string]interface{}{"type": "number", "description": "Duration in seconds"}
			case "realtime_transcription":
				props["audio_url"] = map[string]interface{}{"type": "string", "description": "Audio URL to transcribe"}
			case "realtime_voice":
				props["text"] = map[string]interface{}{"type": "string", "description": "Text to speak"}
				props["voice"] = map[string]interface{}{"type": "string", "description": "Voice identifier"}
			}
			tDef := sdk.ToolDef{
				Name:        toolName,
				Description: desc,
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": props,
				},
			}
			handler := func(sp *sidecarProcess, dshType string) sdk.ToolHandler {
				return func(args map[string]interface{}) (interface{}, error) {
					return sp.CallProvider(dshType, args)
				}
			}(sp, lookupType)
			if err := s.RegisterTool(toolName, tDef, handler); err != nil {
				log.Printf("[dshplgadapter] register provider tool %s: %v", toolName, err)
			}
			return
		}
	}

	if lookupType != typeStr {
		log.Printf("[dshplgadapter] provider %s -> lookup %s (plugin: %s)", typeStr, lookupType, pluginName)
	}
	switch lookupType {
	case "provider":
		log.Printf("[dshplgadapter] generic LLM provider from %s handled natively by HologramAgent", pluginName)
	case "embedding", "memory_embedding":
		log.Printf("[dshplgadapter] embedding provider %s from %s ignored (HologramAgent uses native embeddings)", lookupType, pluginName)
	default:
		log.Printf("[dshplgadapter] unknown provider type: %s (plugin: %s)", typeStr, pluginName)
	}
}

type ChannelRegistry struct{}

func (r *ChannelRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s sdkAPI) {
	var d struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Plugin string `json:"plugin"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
		return
	}
	chName := d.Name
	pn := pluginName
	if d.Plugin != "" {
		pn = d.Plugin
	}

	var caps int
	switch d.Type {
	case "file":
		caps = 2
	case "image":
		caps = 4
	case "audio":
		caps = 8
	default:
		caps = 1
	}
	desc := fmt.Sprintf("dsh channel %s (from %s)", chName, pn)
	// 通道名来自外部 dsh 配置（动态）：声明处**同时**登记 inputch ——
	// 该通道既收（InjectInputSync 见 plugin.go）又发（output_send__<通道>）。
	_ = s.RegisterInputChannel(chName, sdk.ChannelDef{})
	s.RegisterOutputChannel(chName, caps, desc, sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		return sp.CallTool(chName, args)
	})

	readToolName := fmt.Sprintf("%s_read_%s_input", pn, strings.ReplaceAll(chName, "-", "_"))
	s.RegisterTool(readToolName, sdk.ToolDef{
		Name:        readToolName,
		Description: fmt.Sprintf("读取 %s 通道的待处理输入消息", chName),
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		channelInputBufMu.Lock()
		buf := channelInputBuf[chName]
		if len(buf) == 0 {
			channelInputBufMu.Unlock()
			return map[string]interface{}{"messages": []interface{}{}}, nil
		}
		msgs := make([]interface{}, len(buf))
		for i, m := range buf {
			msgs[i] = m
		}
		channelInputBuf[chName] = nil
		channelInputBufMu.Unlock()
		return map[string]interface{}{"messages": msgs}, nil
	})
}

// StageRegistry 是阶段 2 的 HA 阶段接线。
//
// 语义（与 dsh waterfall 的对应关系，见 README）：
//   - before_toolcall：HA 问 dsh。evaluator 单向查询各 sidecar 的
//     tools/pre-execute 判定（先收集全部判定，再一次性提交）：
//     任一 deny ⇒ 设置 ctx.Response（内核 runStage 以 Response != nil
//     为准跳过执行），工具不执行；全部 allow / 查询出错 / 超时 ⇒
//     放行（fail-open + 大声日志，见 evaluator 的约定）。
//   - after_toolcall：只观测。notifier 把结果单向通知 sidecar
//     （不回放进 dsh 事件环——HA 阶段上下文与 dsh session/event
//     载荷形态不同，强行回放会误导插件）；不改 ToolResults。
//   - session/event 等只读订阅：外部插件拿不到 Events()（proc 模板
//     不注入 subscriber），无法实时订阅；hook 注册通知只记账，
//     由管理工具可见。这是外部形态的能力边界，不是 bug。
//
// Dispatch（通知路径）只做记录：阶段处理器在 Start 时注册一次，
// 不随每个 hook 通知重复注册。
type StageRegistry struct {
	evaluator func(toolName string, args map[string]interface{}) (deny bool, reason string)
	notifier  func(toolName string, result interface{})
}

func (r *StageRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s sdkAPI) {
	var d struct {
		Name  string `json:"name"`
		Event string `json:"event"`
	}
	json.Unmarshal(data, &d)
	log.Printf("[dshplgadapter] hook registered: %s (plugin: %s)", d.Event, pluginName)
}

// Register 把 before/after_toolcall 处理器注册到 HA（全局 scope：
// dsh 钩子可能 policing 任意工具，不只本插件自己的工具）。
func (r *StageRegistry) Register(s sdkAPI) {
	s.RegisterStage(sdk.StageBeforeToolcall, r.beforeToolcallHandler(), sdk.StageScopeGlobal)
	s.RegisterStage(sdk.StageAfterToolcall, r.afterToolcallHandler(), sdk.StageScopeGlobal)
}

func (r *StageRegistry) beforeToolcallHandler() sdk.StageHandler {
	return func(ctx *sdk.StageContext) error {
		if r.evaluator == nil {
			return nil
		}
		ctx.RLock()
		if len(ctx.ToolCalls) == 0 {
			ctx.RUnlock()
			return nil
		}
		call := ctx.ToolCalls[0]
		ctx.RUnlock()
		deny, reason := r.evaluator(call.Name, call.Arguments)
		if !deny {
			return nil
		}
		// 内核 runStage 以 Response != nil 为准跳过执行（见 core/stage.go）。
		msg := "denied by dsh hook"
		if reason != "" {
			msg += ": " + reason
		}
		ctx.Lock()
		ctx.Response = &msg
		ctx.Unlock()
		return nil
	}
}

func (r *StageRegistry) afterToolcallHandler() sdk.StageHandler {
	return func(ctx *sdk.StageContext) error {
		if r.notifier == nil {
			return nil
		}
		ctx.RLock()
		if len(ctx.ToolResults) == 0 {
			ctx.RUnlock()
			return nil
		}
		res := ctx.ToolResults[0]
		ctx.RUnlock()
		r.notifier(res.Name, res.Result)
		return nil
	}
}

type CapRecorder struct {
	mu   sync.Mutex
	caps []string
}

func (r *CapRecorder) Record(typeStr string, data json.RawMessage, pluginName string) {
	var d struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	json.Unmarshal(data, &d)
	capStr := fmt.Sprintf("[%s] capability: %s", pluginName, typeStr)
	if d.Name != "" {
		capStr += " (" + d.Name + ")"
	}
	if d.Description != "" {
		capStr += ": " + d.Description
	}
	r.mu.Lock()
	r.caps = append(r.caps, capStr)
	r.mu.Unlock()
}

func (r *CapRecorder) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.caps))
	copy(out, r.caps)
	return out
}

type RegistryDispatcher struct {
	toolReg     *ToolRegistry
	providerReg *ProviderRegistry
	channelReg  *ChannelRegistry
	stageReg    *StageRegistry
	capRecorder *CapRecorder
}

func NewDispatcher() *RegistryDispatcher {
	return &RegistryDispatcher{
		toolReg:     &ToolRegistry{},
		providerReg: &ProviderRegistry{},
		channelReg:  &ChannelRegistry{},
		stageReg:    &StageRegistry{},
		capRecorder: &CapRecorder{},
	}
}

func (d *RegistryDispatcher) Dispatch(typeStr string, data json.RawMessage, pluginName string, sp *sidecarProcess, s sdkAPI) {
	switch typeStr {
	case "tool":
		d.toolReg.Dispatch(data, pluginName, sp, s)
	case "tool_error", "tool_removed":
		// 工具注册失败 / 取消注册：HA 表侧无对应动作（SDK 1.4.0
		// 没有工具注销接口，tool_removed 的工具会在表里残留到重启），
		// 只记账，由管理工具可见。
		d.capRecorder.Record(typeStr, data, pluginName)
	case "channel":
		d.channelReg.Dispatch(data, pluginName, sp, s)
	case "provider", "image_generation_provider", "music_generation_provider",
		"video_generation_provider", "speech_provider",
		"realtime_transcription_provider", "realtime_voice_provider",
		"media_understanding_provider",
		"web_fetch_provider", "web_search_provider",
		"embedding_provider", "memory_embedding_provider":
		d.providerReg.Dispatch(typeStr, data, pluginName, sp, s)
		d.capRecorder.Record(typeStr, data, pluginName)
	case "hook", "runtime_lifecycle", "lifecycle",
		"agent_event_subscription", "agent_harness",
		"session_event", "conversation_binding_resolved",
		"interactive_handler",
		"cli", "cli_backend", "node_cli_feature",
		"command", "http_route", "service",
		"gateway_method", "gateway_discovery_service",
		"trusted_tool_policy", "tool_metadata",
		"context_engine",
		"memory_capability", "memory_prompt_section",
		"memory_flush_plan", "memory_runtime",
		"memory_prompt_supplement", "memory_corpus_supplement",
		"session_extension", "session_scheduler_job",
		"session_action", "control_ui_descriptor",
		"agent_tool_result_middleware":
		d.stageReg.Dispatch(data, pluginName, sp, s)
		d.capRecorder.Record(typeStr, data, pluginName)
	default:
		// ★ 宽容分支（dsh 注册类型比 OpenClaw 更多，这条必须保留）：
		// 未知类型不报错、不中断，只记录。将来 dsh 出新类型时，
		// 这里先收下来，再决定要不要接转译。
		d.capRecorder.Record(typeStr, data, pluginName)
	}
}

func (d *RegistryDispatcher) Capabilities() []string {
	return d.capRecorder.Snapshot()
}
