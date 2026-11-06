# dshplgadapter 阶段 1 骨架说明

骨架已建成并通过构建与测试。本文件只记"骨架是什么、改了什么、还没做什么"，
详细探针证据见 `docs/zh/dshplgadapter-probe.md`。

## 骨架包含什么

- `plugin.go`：`NewPluginFactory` + Start/Stop。Start 只做**静态发现**
  （`DiscoverPlugins`），并注册一个管理工具 `dshplgadapter_list`
  （列出发现结果）。**不启动 sidecar、不执行任何插件代码。**
- `sidecar.go`：从 clawhubadapter 移植的 JSON-RPC over stdio 子进程管理
  （pending-id 配对、NotifyChan、waitReady 30s、Close、ListTools/CallTool/CallProvider）。
- `registry.go`：从 clawhubadapter 移植的转译表（Tool/Provider/Channel/Stage
  + CapRecorder + Dispatcher，含 default 宽容分支：未知类型只记录、不报错）。
- `discovery.go`：规划书 §3.3 的包发现（规则 1 dsh 字段、规则 2 cordis.patch.yml；
  规则 3 只标记为 MaybePlugin，**不加载**——加载即执行代码，等用户决策）。
- `pathsafe.go` / `safeclose.go`：从 clawhubadapter 移植的安全与关闭纪律。
- 10 个测试全过（`go test ./...`），`hmapdev build` 产出
  `dist/dshplgadapter_bundle.hmap`（plugin.bin.linux.amd64 + plugin.bin.darwin.amd64）。

## 移植时改动（非静默漂移，逐条有据）

1. sidecar 入口由 `main.js` 改为 `host.js`：Node 侧是 Cordis 宿主
   （阶段 1 实现），不是 OpenClaw 模拟器；骨架阶段 host.js 不存在，
   `launchSidecar` 返回 (nil, nil)，与原版"无 sidecar"语义一致。
2. scanner 上限 64KB → 1MB（初始仍 64KB）：工具结果长行不能让 readLoop 退出。
3. 删掉原版"NODE_PATH 当 node 二进制目录"的 quirk：node 走 PATH，
   子进程 env 原样继承，调用方设置的 NODE_PATH（cordis 解析用）会透传。
4. 注册表收敛到 `sdkAPI` 接口（RegisterTool + 通道注册），而非直接依赖
   `*sdk.PluginSDK`：PluginSDK 构造器未导出，测试里建不出实例；
   生产侧传 `*sdk.PluginSDK`（方法集正好满足接口），行为不变。
   这让 `TestRegistersToolFromPlugin` 能用 fake 断言"工具真的进了表"。
5. `plg.json` 的 `name_en` 由生成器的 `Dshplgadapter` 改为 `dshplgadapter`
   （与 name 一致，避免产物文件名 mismatch，规划书 §5.5 的坑）。
6. 观察（非阻塞）：`plg.json` 的 targets 写的是 `linux/amd64,windows/amd64`，
   但 `hmapdev build` 实际产出的是 linux + darwin 两个 bin。
   安装时以 .hmap 内实际内容为准，windows 目标如需再确认。

## 骨架阶段明确不做的事

- Node 侧 Cordis 宿主 `host.js`（提供 tools/agents 服务、接 JSON-RPC/fd3、
  调 `ctx.plugin` 加载插件）：阶段 1 实现工作，不在骨架里。
- 任何插件代码的执行：发现是纯静态读文件；规则 3 候选只标记。
- 远程安装：骨架里没有任何下载/安装/解压逻辑，阶段 1 只支持本地已安装包。
- 不可信插件进 HA 的端到端验证：等 §9.5 用户决策后再做。

## 构建与测试（本机实测，2026-11-04，阶段 1–3 完成后）

```bash
export PATH=~/workspace/go/bin:$PATH
gofmt -l .          # 干净
go vet ./...        # 干净
go test ./...       # 22 项全过（12.6s，含 8 组变异验证，见上）
~/workspace/dshplgadapter-repos/hmapdev-bin/hmapdev build   # 产出 dist/dshplgadapter_bundle.hmap
```

## §9.5 决策已定（2026-11-04，用户定，不再问）

dsh 插件是 Node 代码，`apply()` 里能执行任意命令（例如 import('node:child_process')）。
用户已定：**允许 dsh 插件跑任意命令。含义：装 dsh 插件 = 给本机执行权限。**
部署方须知悉此条后再安装；本项目不做沙箱限制方案。

## 阶段 1–3（2026-11-04，骨架之后续写）

### 阶段 1：最小闭环（只工具）

- `host.js`（新建）：Node 侧 Cordis 宿主，一个 sidecar 只带一个 dsh 插件。
  协议走 fd 3（net.Socket），stdout 从不使用（日志一律 stderr，插件的
  console.log 再多也不会坏帧）；插件加载必须走 `ctx.plugin`（探针 0.2）；
  提供 tools 真实服务（register/guard/list/get）与 agents 占位服务
  （只满足 inject 门禁，调用打警告、返回 null/[]）；stderr logger；
  sessionQuery/userQuestions 未提供（调用即抛明确错误）；
  inject 缺服务导致 PENDING 时 5s/10s/20s 警告、30s 后直接退出（探针 0.4）。
- `sidecar.go`（重写）：传输从 stdio 改为 fd 3 socketpair（Go 建 socketpair，
  ExtraFiles[0] 传给子进程即 fd 3；子进程 stdout/stderr 并入本进程 stderr）。
  host.js 用 go:embed 内嵌进 plugin.bin，启动时物化到临时文件（同一进程只写一次）。
  新增 waitPluginActive（轮询 plugin/status 至 ACTIVE，FAILED 大声失败，30s 超时）、
  callWithTimeout、notify（单向无 id）、Status()、Dispose()（先让 fiber 释放，
  ctx.effect 逆序跑，再由调用方 Close 杀进程）、Reload()、写帧持锁。
- schema 转译（实测修正，见 host.js 注释）：`defineTool()` 内部已经调
  `parameterSchemaSpecToJsonSchema` 编译过，所以 register 收到的是已编译
  JSON Schema（直接透传 + `assertSupportedJsonSchema` 断言）；只有手写 def
  的作者 spec 才走转换。二次转换会报
  `parameters.type must be a value schema object`，是 host 侧的旧 bug，已修。
  非法 spec 在哪一层都直接抛错、工具不注册 —— 不存在静默丢字段的路径。
- `plugin.go`（重写 Start）：静态发现 → 暂存 skill（失败只记日志，不阻断启动）
  → 为已声明 dsh 插件拉 sidecar（一个坏插件失败只记日志跳过）
  → 注册阶段处理器（全局 scope）→ 注册四个管理工具：
  `dshplgadapter_list`（发现结果＋运行中 sidecar＋已暂存 skill＋capabilities）、
  `dshplgadapter_skills`（已暂存 skill 列表＋stageDir＋“下次重扫移交”说明）、
  `dshplgadapter_launch`（手动试加载一个已发现包，参数 plugin 必填）、
  `dshplgadapter_reload`（热重载运行中插件）。
- 阶段 1 判据（规划书 §四，全过）：TestDiscoversDSHPlugin、
  TestRegistersToolFromPlugin、TestUnknownRegisterTypeDoesNotCrash、
  TestPluginLoadFailureDoesNotAbortScan；另有端到端
  TestSidecarRegistersToolEndToEnd（装了 → 工具进了 HA 工具表 → 能调，
  另以 tools/list 为第二证据）与 TestProtocolSurvivesPluginLogging
  （插件在 apply 与 execute 里狂打日志，含伪造 JSON-RPC 帧与垃圾行，协议仍通）。

### 阶段 2：事件与钩子

- 语义定稿：deny 的唯一决策点是 `tools/pre-execute` waterfall＋单调 guards；
  `tools/execute` 是环绕（超时/重试/指标），HA 阶段无对应语义，不参与判定；
  `tools/post-execute` 是结果变换，HA 的 after_toolcall 不回放，只通知。
- `registry.go`：StageRegistry 从 stub 换成真正处理器：Register() 注册全局 scope
  的 before/after_toolcall；before 处理器调 evaluator，deny 时设置
  `ctx.Response`（内核以此跳过执行）；after 处理器调 notifier（只观测，
  不改 ToolResults）。工具名前缀抽成唯一 helper `toolHAName(plugin, dshName)`。
- `plugin.go` 的 evaluateHooks（HA → dsh 单向查询）：先收集全部判定再一次性提交；
  归属规则：HA 工具名在归属表里 → 只问归属 sidecar、用 dsh 原名问，
  否则问全部 sidecar、用 HA 名问；任一 deny 即 deny；查询出错/超时/无 sidecar
  一律 fail-open（大声日志），挂掉的钩子不能拖死全部工具调用；
  单 sidecar 超时默认 5s（config `hook_eval_timeout_ms` 可配），
  全局超时为单超时＋2s。
- session/event 与 agent/status 等只读订阅在外部形态下无法实时订阅
  （proc 模板 `proc_main.go.tmpl` 的 `buildPluginSDK` 里没有注入事件订阅器，
  外部插件 Events() 恒为 nil，不能 Publish/Subscribe；另见 TestNoEventPublish）；
  hook 注册通知只记账（`hooks/notify` 只记录最近 20 条，不回放进 dsh 事件环）。
  approval/request 在规划书 §3.2 中标“待定（HA 无直接对应）”，阶段 2 未接线。
- 阶段 2 判据（全过）：TestToolExecuteHookCanDeny（gate 插件对 danger_tool 返回
  deny，HA 设 Response 且 body 副作用文件未生成；非归属工具放行）、
  TestHookChainDoesNotDeadlock（永不 settle 的挂死钩子＋放行钩子并存，
  判定有界 15s 内完成且 fail-open，第二次判定仍能完成）、
  TestHookChainDenyWinsOverHang（挂死＋拒绝并存，拒绝胜出）、
  TestStageHandlersDenyAllow（deny 设带理由 Response、allow 不设、
  空调用不报错、after 通知一次）。

### 阶段 3：纯 SKILL 交接 + 生命周期

- `skill.go`（新建）：`skillNameForStage` 把包名映射成单层目录名（去 @、斜杠→__、
  以. 开头则加 `skill-` 前缀，再过 checkSafeName）；`stageSkill` 把纯 SKILL 包
  复制到 stageDir（默认 plugin.bin 所在目录的 skill/ 子目录，即 pluginmgr
  重扫位置；config 可配 `skill_stage_dir`），只复制非隐藏文件（递归）、
  跳过符号链接与 node_modules、单文件超 10MB 跳过并记日志、
  每个目标路径过 safeArchiveTarget、目标已存在先删再复制（幂等）、
  暂存后必须仍含 SKILL.md 或 skill.json 否则删掉并报错。
  **交接完成时点是“下次 pluginmgr 启动重扫”，不是暂存当时；**
  管理工具须如实报告 staged，不谎称已注册。
- `discovery.go`：加 KindSkill（纯 SKILL 条目）与 HasSkill。判定顺序：
  dsh 字段 → cordis.patch.yml → 纯 SKILL → 规则 3 候选。
  **有意偏离规划书 §3.3 字面：** 规则 3 候选在 Start 时只标记、不自动拉起
  （自动拉起会把 node_modules 里任何带入口的包都 import，顶层代码随即执行；
  用户的决策是“允许 dsh 插件跑任意命令”，不是“允许所有带入口的包跑
  任意命令”）；操作员可用管理工具手动试加载一个候选（失败即跳过，
  与规划书原意一致）。另修：无 package.json 的裸 SKILL 目录也算纯 SKILL 条目。
- 生命周期：`Stop` 先关分发循环，再按拉起逆序对各 sidecar 先 Dispose 再 Close；
  dispose 失败只记日志，不阻断其余释放。`Reload` 带 cache-bust 重新 import；
  同名覆盖，删掉的工具因 SDK 1.4.0 无工具注销接口会在 HA 表里残留到重启。
- 阶段 3 判据（全过）：TestSkillHandoffToSkillmgr（只有 SKILL.md 的目录暂存后
  SKILL.md 内容一致、notes.txt 在位、符号链接未被跟进、重复暂存幂等）、
  TestSkillNameForStage（scope/斜杠映射；“..” 映射为 `skill-..` 后仍在暂存根内，
  不逃逸；空/“@”/空白应报错）、TestEffectsRunInReverseOnStop（两个 effect
  的清理顺序为 second、first）、TestHotReload（v1→改文件→reload→description
  变 v2 且能调出 v2）、TestLaunchRule3Candidate（手动加载 entry-only 候选，
  工具进表）。

### 变异验证（2026-11-04，判据不是摆设）

每组变异：改实现 → 对应判据必须变红 → 还原并 diff 确认文件一致：

| 变异 | 判据 | 结果 |
|---|---|---|
| host.js 工具注册通知 type 改名 | TestSidecarRegistersToolEndToEnd | 变红（30s 排水超时）✓ |
| host.js hooks/evaluate 强制 allow | TestToolExecuteHookCanDeny | 变红 ✓ |
| plugin.go hook 出错改 fail-closed | TestHookChainDoesNotDeadlock | 变红 ✓ |
| registry.go toolHAName 去前缀 | TestToolHAName | 变红 ✓ |
| sidecar.go Reload 变空操作 | TestHotReload | 变红 ✓ |
| registry.go before 处理器不设 Response | TestStageHandlersDenyAllow | 变红 ✓ |
| sidecar.go Dispose 变空操作 | TestEffectsRunInReverseOnStop | 变红 ✓ |
| skill.go stageSkill 变空操作 | TestSkillHandoffToSkillmgr | 变红 ✓ |
| discovery.go 裸 SKILL 识别去掉 | TestSkillHandoffToSkillmgr | 变红（未被发现）✓ |

变异后全量复测：`go test ./...` 22 项全过（12.6s），gofmt、go vet 干净。

### 配置（plugin config）

- `dsh_profile_node_modules`（默认 `~/.dsh/profiles/web/node_modules`）
- `skill_stage_dir`（默认 plugin.bin 所在目录的 skill/ 子目录）
- `hook_eval_timeout_ms`（默认 5000）

### 遗留问题（阶段 4 以外）

- approval/request 未接线（规划书 §3.2 标“待定”，HA 无直接对应）。
- agents 是占位服务（create/get/list/resume 只满足 inject 门禁，调用打警告、
  返回 null/[]）；真正驱动 agent 的插件在阶段 1–3 不可用。
- sessionQuery/userQuestions 未提供（调用即抛明确错误）。
- after_toolcall 只通知不回放；session/event 只记录不实时订阅
  （proc 模板不注入 subscriber，外部插件无法 Publish/Subscribe）。
- 热重载删掉的工具残留到重启（SDK 1.4.0 无工具注销接口）。
- skill 交接在下次 pluginmgr 启动重扫时才完成，暂存当时不算注册。
- hook 查询 fail-open：出错/超时/无 sidecar 一律放行（大声日志）。
- `plg.json` targets 写 linux+windows，但构建实际产出 linux+darwin
  （见骨架说明第 6 条，安装时以 .hmap 内实际内容为准）。
- 阶段 4（远程安装）：2026-11-07 已做，默认关闭，见 PHASE4_NOTES.md。
