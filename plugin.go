package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ★★ 插件里**绝不能往 stdout 打印任何东西**（踩坑记录，2026-10-16）。
//
// 子进程插件的 **stdout 是 JSON-RPC 协议通道**（与内核的帧都走它），
// stderr 才是日志通道（SDK 侧 log.SetOutput(os.Stderr)）。
//
// 用 fmt.Printf 打一行日志 ⇒ 内核 readLoop 收到一段非 JSON 文本，
// 报「收到非法 JSON 帧（N 字节）」并**丢弃**。日志本身丢了，还刷了一条
// 看起来像协议故障的报错（极难定位到「是我自己 printf 的」）。
//
// ⇒ 一律用 log.Printf（默认走 stderr）。
//
// dsh 侧同理：Node sidecar 与 dsh 插件的 stdout 不再是协议通道
// （阶段 1 起协议走 fd 3），但 host.js 仍全程只写 stderr——
// stdout 全程不用，传输层将来再怎么换都不会被日志污染。

type toolOwner struct {
	sidecar *sidecarProcess
	dshName string
	plugin  string
}

type stagedSkill struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

type notifyItem struct {
	sp    *sidecarProcess
	notif DSHNotification
}

type Plugin struct {
	name          string
	sdk           *sdk.PluginSDK
	dispatcher    *RegistryDispatcher
	discoveryRoot string
	discovered    []DiscoveredPackage

	skillStageDir   string
	hookEvalTimeout time.Duration

	mu           sync.Mutex
	sidecars     map[string]*sidecarProcess // key: 插件名
	sidecarOrder []string                   // 拉起顺序（Stop 时逆序释放）
	toolOwners   map[string]toolOwner       // HA 工具名 -> 归属
	stagedSkills map[string]stagedSkill     // skill 目录名 -> 暂存信息
	installer    *remoteInstaller           // 阶段 4 远程安装（默认关闭）

	notifyCh   chan notifyItem
	notifyStop chan struct{}
	notifyWg   sync.WaitGroup
	stopOnce   sync.Once
}

func (p *Plugin) Name() string { return p.name }

// NewPluginFactory 是 hmapdev 生成的 proc 运行时调用的入口。
//
// ★ 名字必须是 NewPluginFactory（不是 NewPlugin）—— proc 运行时按此名调用，
// 拼错会报 `undefined: NewPluginFactory`（实测）。
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	root := ""
	if v, ok := config["dsh_profile_node_modules"].(string); ok && v != "" {
		root = v
	} else if home, err := os.UserHomeDir(); err == nil {
		// 规划书 §2.3：<profile> 一般是 ~/.dsh/profiles/<name>，默认 web。
		root = filepath.Join(home, ".dsh", "profiles", "web", "node_modules")
	}
	stageDir := ""
	if v, ok := config["skill_stage_dir"].(string); ok && v != "" {
		stageDir = v
	} else if exe, err := os.Executable(); err == nil {
		// 默认 = plugin.bin 所在目录的 skill/ 子目录：pluginmgr 在安装时
		// 与每次启动时重扫 <插件目录>/skill/，正好是这个位置。
		stageDir = filepath.Join(filepath.Dir(exe), "skill")
	} else {
		log.Printf("[dshplgadapter] cannot locate executable, skill staging falls back to temp dir (pluginmgr will NOT rescan it)")
		stageDir = filepath.Join(os.TempDir(), "dshplgadapter-skill")
	}
	hookTimeout := 5 * time.Second
	if v, ok := config["hook_eval_timeout_ms"].(float64); ok && v > 0 {
		hookTimeout = time.Duration(v) * time.Millisecond
	}
	// 阶段 4：远程安装默认关闭。开启须在插件配置里设
	// remote_install_enabled=true（并重启插件）；npm_registry 可改上游。
	remoteEnabled := false
	if v, ok := config["remote_install_enabled"].(bool); ok {
		remoteEnabled = v
	}
	registry := ""
	if v, ok := config["npm_registry"].(string); ok {
		registry = v
	}
	return &Plugin{
		name:            name,
		discoveryRoot:   root,
		skillStageDir:   stageDir,
		hookEvalTimeout: hookTimeout,
		installer:       newRemoteInstaller(root, remoteEnabled, registry),
		sidecars:        map[string]*sidecarProcess{},
		toolOwners:      map[string]toolOwner{},
		stagedSkills:    map[string]stagedSkill{},
	}, nil
}

// rescan 重新跑一遍静态发现并更新发现结果，同时为新出现的纯 SKILL
// 包补暂存（与 Start 里的暂存逻辑一致）。只更新发现结果，
// 不自动拉起 sidecar——新装的包由操作员用 dshplgadapter_launch
// 手动试加载。返回发现到的包总数。
func (p *Plugin) rescan() int {
	found := DiscoverPlugins(p.discoveryRoot)
	p.mu.Lock()
	p.discovered = found
	p.mu.Unlock()
	log.Printf("[%s] rescan: discovered %d dsh package(s) under %s", p.name, len(found), p.discoveryRoot)
	for _, d := range found {
		if d.Kind != KindSkill && !d.HasSkill {
			continue
		}
		stageName, err := skillNameForStage(d.Name)
		if err != nil {
			continue
		}
		p.mu.Lock()
		_, already := p.stagedSkills[stageName]
		p.mu.Unlock()
		if already {
			continue
		}
		dest, err := stageSkill(d, p.skillStageDir)
		if err != nil {
			log.Printf("[%s] rescan: stage skill %s failed: %v", p.name, d.Name, err)
			continue
		}
		p.mu.Lock()
		p.stagedSkills[stageName] = stagedSkill{Source: d.Dir, Dest: dest}
		p.mu.Unlock()
		log.Printf("[%s] rescan: staged skill %s -> %s (pluginmgr hands off on next startup rescan)", p.name, d.Name, dest)
	}
	return len(found)
}

// installPackage 是 dshplgadapter_install 的处理函数：远程安装 →
// 重扫发现链路 → 在结果里报告新包的发现状态与拉起方式。
func (p *Plugin) installPackage(spec, expectedSHA256 string) (map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	res, err := p.installer.Install(ctx, spec, expectedSHA256)
	if err != nil {
		return nil, err
	}
	count := p.rescan()
	res.RescannedCount = count
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.discovered {
		if d.Name == res.Package {
			res.Discovered = true
			res.DiscoveredKind = d.Kind
			break
		}
	}
	out := map[string]interface{}{
		"package": res.Package, "version": res.Version, "tarball": res.Tarball,
		"sha256": res.SHA256, "integrityChecked": res.IntegrityOK, "shasumChecked": res.ShasumOK,
		"dir": res.Dir, "discovered": res.Discovered, "discoveredKind": res.DiscoveredKind,
		"rescanCount": res.RescannedCount,
		"note":        "installed package is now in the discovery chain (see dshplgadapter_list); it is NOT auto-launched",
	}
	if res.DiscoveredKind == KindDSHPlugin {
		res.LaunchHint = "use " + p.name + "_launch with plugin=" + res.Package + " to trial-load it"
		out["launchHint"] = res.LaunchHint
	} else if res.DiscoveredKind == KindSkill {
		out["note"] = "installed package is a pure skill, staged for handoff on the next pluginmgr startup rescan"
	} else if res.DiscoveredKind == KindMaybePlugin {
		res.LaunchHint = "rule-3 candidate: use " + p.name + "_launch with plugin=" + res.Package + " to trial-load it"
		out["launchHint"] = res.LaunchHint
	} else if !res.Discovered {
		out["note"] = "installed, but not recognized as a dsh plugin/skill by discovery (no dsh field, no cordis.patch.yml, no SKILL.md)"
	}
	return out, nil
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	p.dispatcher = NewDispatcher()
	s.RegisterStopHandler(func() { log.Printf("[%s] stop handler running", p.name) })

	// 静态发现（读 package.json / cordis.patch.yml / SKILL.md），不执行代码。
	p.discovered = DiscoverPlugins(p.discoveryRoot)
	log.Printf("[%s] discovered %d dsh package(s) under %s", p.name, len(p.discovered), p.discoveryRoot)
	for _, d := range p.discovered {
		log.Printf("[%s] - %s (%s) %s", p.name, d.Name, d.Kind, d.Dir)
	}

	// 阶段 3：纯 SKILL 暂存（失败只记日志，不阻断启动；交接完成
	// 在下次 pluginmgr 启动重扫，不在暂存当时）。
	for _, d := range p.discovered {
		if d.Kind == KindSkill || d.HasSkill {
			dest, err := stageSkill(d, p.skillStageDir)
			if err != nil {
				log.Printf("[%s] stage skill %s failed: %v", p.name, d.Name, err)
				continue
			}
			stageName, _ := skillNameForStage(d.Name)
			p.mu.Lock()
			p.stagedSkills[stageName] = stagedSkill{Source: d.Dir, Dest: dest}
			p.mu.Unlock()
			log.Printf("[%s] staged skill %s -> %s (pluginmgr hands off on next startup rescan)", p.name, d.Name, dest)
		}
	}

	// 阶段 1：为已声明的 dsh 插件拉起 sidecar。
	// 一个坏插件的启动失败只记日志跳过，不中断其余插件。
	p.notifyCh = make(chan notifyItem, 1024)
	p.notifyStop = make(chan struct{})
	p.notifyWg.Add(1)
	go p.dispatchLoop(s)
	for _, d := range p.discovered {
		if d.Kind != KindDSHPlugin {
			continue
		}
		sp, err := launchSidecar(d.Name, d.Dir, p.discoveryRoot)
		if err != nil {
			log.Printf("[%s] launch sidecar %s failed: %v (skipping, other plugins unaffected)", p.name, d.Name, err)
			continue
		}
		p.mu.Lock()
		p.sidecars[d.Name] = sp
		p.sidecarOrder = append(p.sidecarOrder, d.Name)
		p.mu.Unlock()
		go p.forwardLoop(sp)
		log.Printf("[%s] sidecar started: %s", p.name, d.Name)
	}

	// 阶段 2：阶段处理器（全局 scope）。
	p.dispatcher.stageReg.evaluator = p.evaluateHooks
	p.dispatcher.stageReg.notifier = p.notifyToolResult
	p.dispatcher.stageReg.Register(s)

	// 管理工具。
	toolName := p.name + "_list"
	_ = s.RegisterTool(toolName, sdk.ToolDef{
		Name:        toolName,
		Description: "列出本机已安装的 dsh 插件（静态发现结果）与运行中 sidecar、已暂存 skill",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.listReport(), nil
	})

	skillsTool := p.name + "_skills"
	_ = s.RegisterTool(skillsTool, sdk.ToolDef{
		Name:        skillsTool,
		Description: "列出已暂存的纯 SKILL（下次 pluginmgr 启动重扫时移交给 skillmgr）",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		out := map[string]interface{}{}
		for name, sk := range p.stagedSkills {
			out[name] = sk
		}
		return map[string]interface{}{
			"stageDir": p.skillStageDir,
			"skills":   out,
			"note":     "staged skills are handed off by pluginmgr on the next startup rescan, not at stage time",
		}, nil
	})

	launchTool := p.name + "_launch"
	_ = s.RegisterTool(launchTool, sdk.ToolDef{
		Name:        launchTool,
		Description: "手动试加载一个已发现的包（含规则 3 候选）：加载失败即跳过，不中断其他插件",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"plugin": map[string]interface{}{"type": "string", "description": "包名（_list 输出里的 name）"},
		}, "required": []interface{}{"plugin"}},
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["plugin"].(string)
		if name == "" {
			return nil, errMissingPluginArg
		}
		return p.launchOne(name)
	})

	reloadTool := p.name + "_reload"
	_ = s.RegisterTool(reloadTool, sdk.ToolDef{
		Name:        reloadTool,
		Description: "热重载一个运行中的 dsh 插件（释放旧 fiber 后重新 import，工具同名覆盖）",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"plugin": map[string]interface{}{"type": "string", "description": "插件名"},
		}, "required": []interface{}{"plugin"}},
	}, func(args map[string]interface{}) (interface{}, error) {
		name, _ := args["plugin"].(string)
		if name == "" {
			return nil, errMissingPluginArg
		}
		p.mu.Lock()
		sp := p.sidecars[name]
		p.mu.Unlock()
		if sp == nil {
			return nil, errPluginNotRunning
		}
		st, err := sp.Reload()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"plugin": name, "state": st.State, "error": st.Error}, nil
	})

	// 阶段 4：远程安装（默认关闭）。sha256 必填：不提供直接拒绝，
	// 不一致拒绝且不落文件。安装成功后重扫发现链路，不自动拉起。
	installTool := p.name + "_install"
	_ = s.RegisterTool(installTool, sdk.ToolDef{
		Name:        installTool,
		Description: "从 npm registry 远程安装一个 dsh 插件（默认关闭，需 config remote_install_enabled=true）。sha256 必填：先校验后解压，不一致拒绝且不落文件。远程安装 = 从网上拉代码到本机执行，只从信任的来源安装。",
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"package": map[string]interface{}{"type": "string", "description": "包名，可带版本，如 @scope/pkg 或 @scope/pkg@1.2.3；不带版本装 latest"},
			"sha256":  map[string]interface{}{"type": "string", "description": "tarball 的期望 sha256（64 位十六进制，从包作者或 registry 的 dist 信息取）；必填，不提供直接拒绝"},
		}, "required": []interface{}{"package", "sha256"}},
	}, func(args map[string]interface{}) (interface{}, error) {
		spec, _ := args["package"].(string)
		sha, _ := args["sha256"].(string)
		if spec == "" {
			return nil, errString("package 参数缺失")
		}
		return p.installPackage(spec, sha)
	})

	log.Printf("[%s] started", p.name)
	return nil
}

var errMissingPluginArg = errString("plugin 参数缺失")
var errPluginNotRunning = errString("插件未运行")

type errString string

func (e errString) Error() string { return string(e) }

func (p *Plugin) listReport() map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(p.discovered))
	for _, d := range p.discovered {
		out = append(out, map[string]interface{}{
			"name": d.Name, "dir": d.Dir, "kind": d.Kind, "reason": d.Reason, "hasSkill": d.HasSkill,
		})
	}
	p.mu.Lock()
	running := make([]string, 0, len(p.sidecarOrder))
	for _, name := range p.sidecarOrder {
		if _, ok := p.sidecars[name]; ok {
			running = append(running, name)
		}
	}
	staged := make([]string, 0, len(p.stagedSkills))
	for name := range p.stagedSkills {
		staged = append(staged, name)
	}
	p.mu.Unlock()
	return map[string]interface{}{
		"plugins":      out,
		"running":      running,
		"capabilities": p.dispatcher.Capabilities(),
		"stagedSkills": staged,
	}
}

// launchOne 手动试加载一个已发现的包（规则 3 的操作员入口）。
func (p *Plugin) launchOne(name string) (map[string]interface{}, error) {
	p.mu.Lock()
	if _, ok := p.sidecars[name]; ok {
		p.mu.Unlock()
		return map[string]interface{}{"plugin": name, "alreadyRunning": true}, nil
	}
	var found *DiscoveredPackage
	for i := range p.discovered {
		if p.discovered[i].Name == name {
			found = &p.discovered[i]
			break
		}
	}
	p.mu.Unlock()
	if found == nil {
		return nil, errPluginNotFound
	}
	sp, err := launchSidecar(found.Name, found.Dir, p.discoveryRoot)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.sidecars[found.Name] = sp
	p.sidecarOrder = append(p.sidecarOrder, found.Name)
	p.mu.Unlock()
	go p.forwardLoop(sp)
	return map[string]interface{}{"plugin": found.Name, "state": "ACTIVE"}, nil
}

var errPluginNotFound = errString("包未在发现结果里")

// forwardLoop 把一个 sidecar 的通知扇入分发队列；Stop 关闭
// notifyStop 后自行退出（不依赖 NotifyChan 被关闭——它从不关闭）。
func (p *Plugin) forwardLoop(sp *sidecarProcess) {
	for {
		select {
		case notif := <-sp.NotifyChan():
			select {
			case p.notifyCh <- notifyItem{sp: sp, notif: notif}:
			case <-p.notifyStop:
				return
			}
		case <-p.notifyStop:
			return
		}
	}
}

func (p *Plugin) dispatchLoop(s sdkAPI) {
	defer p.notifyWg.Done()
	for {
		select {
		case item := <-p.notifyCh:
			p.handleNotification(item, s)
		case <-p.notifyStop:
			return
		}
	}
}

func (p *Plugin) handleNotification(item notifyItem, s sdkAPI) {
	var params struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(item.notif.Params, &params); err != nil {
		log.Printf("[%s] bad notification params: %v", p.name, err)
		return
	}
	// 工具归属表（阶段 2 的 before_toolcall 查表用；命名规则与
	// ToolRegistry 共用 toolHAName，不得各写一份）。
	switch params.Type {
	case "tool":
		var d struct {
			Name   string `json:"name"`
			Plugin string `json:"plugin"`
		}
		if json.Unmarshal(params.Data, &d) == nil && d.Name != "" {
			pn := d.Plugin
			if pn == "" {
				pn = item.sp.name
			}
			p.mu.Lock()
			p.toolOwners[toolHAName(pn, d.Name)] = toolOwner{sidecar: item.sp, dshName: d.Name, plugin: pn}
			p.mu.Unlock()
		}
	case "tool_removed":
		var d struct {
			Name   string `json:"name"`
			Plugin string `json:"plugin"`
		}
		if json.Unmarshal(params.Data, &d) == nil && d.Name != "" {
			pn := d.Plugin
			if pn == "" {
				pn = item.sp.name
			}
			p.mu.Lock()
			delete(p.toolOwners, toolHAName(pn, d.Name))
			p.mu.Unlock()
		}
	}
	p.dispatcher.Dispatch(params.Type, params.Data, item.sp.name, item.sp, s)
}

// evaluateHooks 单向查询 dsh 钩子对一次工具调用的判定。
//
// 方向是 HA → dsh（不是 dsh waterfall 等 HA）：先收集全部判定，
// 再一次性提交；任一 deny 即 deny；查询出错 / 超时 / 无 sidecar
// 一律放行（fail-open），但大声记日志——挂掉的钩子不能把全部
// 工具调用拖死（TestHookChainDoesNotDeadlock）。
//
// 归属规则：HA 工具名在归属表里（即 dsh 工具）→ 只问归属 sidecar，
// 用 dsh 原名问；否则（其他 HA 工具）→ 问全部 sidecar，用 HA 名问。
func (p *Plugin) evaluateHooks(haToolName string, args map[string]interface{}) (deny bool, reason string) {
	p.mu.Lock()
	owner, ok := p.toolOwners[haToolName]
	var sidecars []*sidecarProcess
	if ok {
		sidecars = append(sidecars, owner.sidecar)
	} else {
		for _, name := range p.sidecarOrder {
			if sp, ok := p.sidecars[name]; ok {
				sidecars = append(sidecars, sp)
			}
		}
	}
	p.mu.Unlock()
	if len(sidecars) == 0 {
		return false, ""
	}
	type hookResult struct {
		deny   bool
		reason string
	}
	results := make(chan hookResult, len(sidecars))
	var wg sync.WaitGroup
	for _, sp := range sidecars {
		wg.Add(1)
		go func(sp *sidecarProcess) {
			defer wg.Done()
			toolName := haToolName
			if ok && sp == owner.sidecar {
				toolName = owner.dshName
			}
			data, err := sp.callWithTimeout("hooks/evaluate", map[string]interface{}{
				"event": "tools/pre-execute", "tool": toolName, "arguments": args,
			}, p.hookEvalTimeout)
			if err != nil {
				log.Printf("[%s] hook evaluate %s via %s failed (fail-open): %v", p.name, toolName, sp.name, err)
				results <- hookResult{deny: false}
				return
			}
			var r struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(data, &r); err != nil {
				log.Printf("[%s] hook evaluate %s via %s bad response (fail-open): %v", p.name, toolName, sp.name, err)
				results <- hookResult{deny: false}
				return
			}
			switch r.Decision {
			case "deny":
				results <- hookResult{deny: true, reason: r.Reason}
			case "error":
				log.Printf("[%s] hook evaluate %s via %s error (fail-open): %s", p.name, toolName, sp.name, r.Reason)
				results <- hookResult{deny: false}
			default:
				results <- hookResult{deny: false}
			}
		}(sp)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timeout := time.After(p.hookEvalTimeout + 2*time.Second)
	denyReason := ""
	for i := 0; i < len(sidecars); i++ {
		select {
		case r := <-results:
			if r.deny && !deny {
				deny = true
				denyReason = r.reason
			}
		case <-timeout:
			log.Printf("[%s] hook evaluate %s: global timeout, proceeding fail-open", p.name, haToolName)
			return deny, denyReason
		}
	}
	<-done
	return deny, denyReason
}

// notifyToolResult 把一次工具执行结果单向通知各 sidecar（观测用，
// 不回放进 dsh 事件环；写失败只记日志，绝不阻塞 HA 阶段）。
func (p *Plugin) notifyToolResult(haToolName string, result interface{}) {
	p.mu.Lock()
	var sidecars []*sidecarProcess
	for _, name := range p.sidecarOrder {
		if sp, ok := p.sidecars[name]; ok {
			sidecars = append(sidecars, sp)
		}
	}
	p.mu.Unlock()
	for _, sp := range sidecars {
		sp.notify("hooks/notify", map[string]interface{}{
			"event": "tools/post-execute", "tool": haToolName, "result": result,
		})
	}
}

func (p *Plugin) Stop() error {
	p.stopOnce.Do(func() {
		if p.notifyStop != nil {
			close(p.notifyStop)
		}
		p.notifyWg.Wait()
	})
	// 逆序释放 sidecar：先 dispose（fiber 内 ctx.effect 清理函数逆序跑），
	// 再杀进程。dispose 失败只记日志，不阻断其余 sidecar 的释放。
	p.mu.Lock()
	order := make([]string, len(p.sidecarOrder))
	copy(order, p.sidecarOrder)
	p.mu.Unlock()
	for i := len(order) - 1; i >= 0; i-- {
		p.mu.Lock()
		sp := p.sidecars[order[i]]
		p.mu.Unlock()
		if sp == nil {
			continue
		}
		if err := sp.Dispose(); err != nil {
			log.Printf("[%s] dispose sidecar %s failed: %v", p.name, order[i], err)
		}
		sp.Close()
	}
	log.Printf("[%s] stopped", p.name)
	return nil
}
