# dshplgadapter 阶段 0 可行性探针

日期：2026-11-04。结论先说：**四个探针全部通过**，阶段 1 骨架已按探针结论建成。
但骨架不执行任何插件代码 —— 是否允许 dsh 插件跑任意代码（规划书 §9.5），
仍需用户定夺，端到端验证等决策后再做。

探针代码：`~/workspace/dshplgadapter-probes/`（p01–p04）。
阶段 1 骨架：`~/workspace/dshplgadapter/`（说明见 `PHASE1_NOTES.md`）。

## 0. 环境自检（逐条实测）

| 项目 | 结果 |
|---|---|
| 磁盘 | /home/hatch 150G，剩余 145G；/tmp 只有 512M，持久工作放 ~/workspace，不放 /tmp |
| 内存 | 共 7934M，可用 462M，无 swap。Go 构建全程限流（GOFLAGS=-p=2/-p=1，GOMAXPROCS=2/1），未 OOM |
| Node / npm | v24.22.1 / 10.9.4；npm 全局 prefix 是 /usr（不是 /usr/lib/node_modules/.bin） |
| Go | 本机原本没有；从 go.dev 下载 go1.27.1，装在 ~/workspace/go（ survives VM 重置），`go version` = go1.27.1 linux/amd64；gcc 13.3.0 在 |
| dsh | 2026-11-04 新装 @deepseek-ai/dsh 0.2.1-rc.2（617 包）；`dsh --version` = 0.2.1-rc.2；二进制在 /usr/bin/dsh |
| cordis | 嵌套在 /usr/lib/node_modules/@deepseek-ai/dsh/node_modules/@deepseek-ai/cordis，版本 4.0.5（与规划书一致）；**不在全局顶层**，规划书 §9.2 的原命令需加 `NODE_PATH=/usr/lib/node_modules/@deepseek-ai/dsh/node_modules` 或用绝对路径 require，否则报 Cannot find module —— 这是命令文本偏差，不是 Context 故障 |
| dsh-tools | 同样嵌套存在；`defineTool`、`parameterSchemaSpecToJsonSchema` 运行时可用（见探针 0.3） |
| hmapdev | 本机原本没有；从 SDK 仓源码编译（tools/hmapdev），二进制存 ~/workspace/dshplgadapter-repos/hmapdev-bin/hmapdev；`hmapdev version` = 1.4.0，SDK 模块 hologramagentsdk，构建 Go go1.27.1 |
| hmapdev SDK | 初装时 `sdk current/list` 均为空；用 `hmapdev sdk install --from <克隆的 SDK 仓> v1.4.0` 装好并激活，现存 ~/.hologram/hmapdev/sdk/v1.4.0（注意：新路径是 ~/.hologram，规划书写的 ~/.homeagent 是改名前的旧路径） |
| 内核仓编译 | `go build ./...`（HologramAgent，-p=1）退出码 0、零输出，通过 |
| 参考仓 | 全部 --depth 1 浅克隆到 ~/workspace/dshplgadapter-repos/：HologramAgent 129M、clawhubadapter 452K、hologramagentsdk 9.1M、deepseek-harness 235M、dsh-mail-bridge 2.4M；内核仓 internal/plugins/ 下确实无 clawhubadapter（已迁出，与规划书一致） |
| 本机缺失项（均已用替代源解决，不阻塞） | ~/.dsh/profiles/ 不存在；/opt/agentmail 的 dsh-mail-bridge 生产副本不存在；dsh-mobile-nav 未在 npm 发布（npm view 404）；harness 仓内也无 dsh-mobile-nav |

## 1. 探针 0.1：fd3 协议通道 —— 通过

**问题**：dsh 插件爱打 console.log，stdout 一旦混入日志，JSON-RPC 帧就坏
（内核 readLoop 报非法帧并丢行；clawhubadapter 生产上为此付过 1038 次错）。
规划书 §2.3 推荐方案 A：协议走 fd 3，stdout/stderr 只当日志。

**做法**：Go 父进程建 Unix socketpair，把一端以 `ExtraFiles[0]` 传给子进程
（子进程里即 fd 3）；Node 子进程用 `fs.readSync(3)` / `fs.writeSync(3)` 收发，
同时往 stdout 打一行普通日志。
代码：`dshplgadapter-probes/p01-fd3/main.go`、`child.js`。

**实测输出**：

```
fd3 response: "PONG\n"
stdout captured: "[child] stdout log, not protocol\n"
stderr captured: ""
PASS: protocol on fd3 works, stdout stays separate
```

**结论**：通过。协议帧走 fd 3 到达，stdout 只有日志、没有协议帧，
两者彻底分开。socketpair 是无特权操作，容器内可用。

**对设计的影响**：阶段 1 的 Node 侧 host.js 落地时，传输层改走 fd 3
（stdout 留给插件日志）；Go 侧届时把 stdin/stdout 换成 ExtraFiles 的
socketpair 端。骨架里的 sidecar.go 仍先按 stdio 移植（与 clawhubadapter
相同），切 fd 3 是 host.js 落地时的一次性改动，已记在 PHASE1_NOTES.md。

## 2. 探针 0.2：裸 new Context() 承载插件 —— 通过

**问题**：规划书 §9.6 的推测之一 —— dsh 插件能否跑在裸 `new Context()` 里。
优先源 dsh-mobile-nav 不可用（未发布、无本地副本），改用官方教程的
标准最小插件（01-first-plugin.zh.md 的 hello 插件）+ 仿 mobile-nav 的
`ctx.effect` 插件（02-lifecycle-and-effects.zh.md 的形态）。

**做法**：`NODE_PATH` 指向 dsh 嵌套 node_modules；四种情形实测。
代码：`dshplgadapter-probes/p02-context/probe.mjs` 及其 `plugins/`。

**实测输出**：

```
bare ctx.tools: undefined
bare ctx.on: function | ctx.plugin: function | ctx.effect: function
hello from my first plugin
(a) direct apply(hello): OK, apply ran = true
(b) direct apply(effect): OK (no error)
hello from my first plugin
(c) ctx.plugin(hello): status = ACTIVE | apply ran = true
(d) ctx.plugin(effect): status = ACTIVE | apply ran = true
(d) after fiber.dispose(): status = DISPOSED | disposer ran = true
PASS: bare Context hosts plugins via ctx.plugin
```

（注：初版探针用 `fiber.status` 取不到状态，查 fiber.d.ts 确认字段名是
`state`（PENDING=0、ACTIVE=2、DISPOSED=4），修正后通过。）

**结论**：通过，但有两个必须写进设计的约束：

1. **必须走 `ctx.plugin` 加载路径，不能直接调 `apply()`**。直接调虽然
   hello 能跑，但 fiber 生命周期、effect 归属、inject 门控都依赖 loader；
   只有 loader 路径能保证 `fiber.dispose()` 逆序跑清理函数
   （(d) 已验证 disposer 确实跑了）。
2. **裸 Context 没有 tools/agents 服务**（`ctx.tools` 是 undefined）。
   这是 cordis 核心与 dsh 服务的分界：tools/agents 是 dsh 部署提供的服务，
   不是 cordis 自带的。宿主必须自己实现并提供这些服务（见探针 0.4），
   否则插件永远到不了 ACTIVE。

## 3. 探针 0.3：defineTool 的 schema 往返 —— 通过

**问题**：dsh 工具的 schema 能否无损转成 HA 的 ToolDef.Parameters（JSON Schema），
再转回来。规划书引用的 dsh-mail-bridge 源**作废**：公开镜像
（QIKO0721/dsh-mail-bridge）不是 cordis 插件 —— src/ 是 CJS 守护进程
（dsh CLI 包装器），无 defineTool、无 inject、无 apply，连 package.json
的 main（src/index.js）都不存在；本地生产副本也不存在。
改用官方教程 adding-a-tool.zh.md 的标准工具形态（read_file，真实 defineTool 调用）。

**做法**：用 dsh-tools 自带的 `parameterSchemaSpecToJsonSchema` 做正向；
自己写反向转换器（JSON Schema 子集 → dsh ParameterSchemaSpec）；
两个工具做往返深比较。HA 侧的 ToolDef.Parameters 是
`map[string]interface{}`（sdk/plugin.go），即一个 JSON Schema 对象。
代码：`dshplgadapter-probes/p03-schema/probe.mjs`。

**实测输出**：

```
defineTool(read_file): OK, name = read_file
--- read_file: forward (HA JSON Schema) ---
{"type":"object","properties":{"path":{"type":"string","description":"Absolute path"},"limit":{"type":"number"}},"required":["path"]}
--- read_file: round-trip equal = true ---
--- search_files: forward (HA JSON Schema) ---
{"type":"object","properties":{"query":{"type":"string","description":"Search text"},"paths":{"type":"array","items":{"type":"string"}},"maxResults":{"type":"integer"},"options":{"type":"object","additionalProperties":false,"properties":{"caseSensitive":{"type":"boolean"}}}},"required":["query"]}
--- search_files: round-trip equal = true ---
PASS: schema round-trips losslessly for the tested subset
```

**结论**：通过。正向转换已有官方内置函数，dshplgadapter 不用自己写；
反向在 string/number/integer/boolean/array/object/required/description/
additionalProperties 子集上无损。dsh 侧 `required` 是逐属性标记，
HA 侧是顶层 required 数组 —— 转换器已处理这个形态差。

**未覆盖、需阶段 1 明确处理**：oneOf/nullable/json 等超集形态未验证；
阶段 1 的转换器要么扩展覆盖，要么对超集工具报明确错误，**不能静默丢字段**。
另外注意：规划书引用的 `@deepseek-ai/dsh-tools/lib/types/schema.d.ts:248`
在本机版本不存在（类型在 index.d.ts 里从 ./schema.ts 重导出），
运行时 API 以实测为准。

## 4. 探针 0.4：inject 缺服务行为 —— 通过（答案是第三种）

**问题**：声明 `inject: ['agents','tools']` 但不提供服务时，是报错退出还是降级？
答案实测是**两者都不是：挂起**。

**做法**：一个声明 inject 且在 apply 里调 `ctx.tools.register` 的插件，
四种情形实测。代码：`dshplgadapter-probes/p04-inject/probe.mjs` 及其 `plugins/`。

**实测输出**：

```
(a) direct apply: THREW TypeError - Cannot read properties of undefined (reading 'register')
(b) ctx.plugin, no services: status = PENDING | apply ran = false
(c1) tools provided, agents missing: status = PENDING | apply ran = false
(c2) tools+agents provided: status = ACTIVE | apply ran = true | registered tool = probe_tool
PASS: inject gates activation (PENDING until services provided); direct apply fails loud
```

**结论**：通过，且结论比规划书 §七的"fail-loud"更精确，分三层：

1. **直接调 apply**：大声失败 —— TypeError（`ctx.tools` 是 undefined）。
   这是宿主 bug 的快速信号，不是静默降级。
2. **走 loader 但服务缺失**：fiber 停在 PENDING，apply 根本不执行，
   也不报错。只提供 tools 仍不够（agents 缺失照样 PENDING）；
   两个服务都提供后才 ACTIVE，register 正常调用。
3. **设计落点**：宿主（Node host.js）必须实现插件 inject 的服务
   （tools 至少，agents 按需），否则插件永不激活；同时宿主必须对
   PENDING 设超时告警 —— 否则"插件装上了但没反应"会成为最难查的静默故障。
   这与规划书"只提供声明的服务、其余缺失 fail-loud、不伪造"一致，
   只是 fail-loud 的形态是"宿主侧超时告警"，不是 Cordis 替你报错。

## 5. 阶段 1 骨架状态（探针全过后的产物）

位置：`~/workspace/dshplgadapter/`。`hmapdev init` 生成骨架后移植与新建：

| 文件 | 来源 / 内容 |
|---|---|
| plugin.go | init 生成后改写：Start 只做静态发现 + 注册管理工具 `dshplgadapter_list`；不启动 sidecar、不执行插件代码 |
| sidecar.go | clawhubadapter 移植：JSON-RPC over stdio、pending-id 配对、NotifyChan、waitReady 30s、Close；入口改 host.js、scanner 上限 1MB（见 PHASE1_NOTES.md） |
| registry.go | clawhubadapter 移植：转译表 + default 宽容分支；注册表收敛到 sdkAPI 接口以便测试用 fake 断言 |
| discovery.go | 新建：§3.3 规则 1/2 静态发现；规则 3 只标记 MaybePlugin、不加载；本地包 only，无任何下载/安装代码 |
| pathsafe.go / safeclose.go | clawhubadapter 移植：resolveUnderRoot、safeArchiveTarget、泛型 safeClose |
| 4 个测试文件 | 10 项判据，见下表 |

测试（`go test ./...`，10 项全过；`gofmt`、`go vet` 干净）：

- TestDiscoversDSHPlugin（dsh 字段 / cordis.patch.yml / 作用域包 / 普通包排除 / 入口包记 Maybe）
- TestPluginLoadFailureDoesNotAbortScan（坏包不中断扫描）
- TestRegistersToolFromPlugin（工具真的进了表，不只是日志；plugin 字段覆盖前缀）
- TestUnknownRegisterTypeDoesNotCrash（未知类型只记录、不崩溃）
- TestEntryPointIsNewPluginFactory / TestNoKernelInternalImports /
  TestNoEventPublish / TestSafeCloseCoversAllChannelKinds（外部形态四判据）
- TestPackageNameCannotEscapeRoot / TestArchiveExtractionStaysInsideRoot（路径安全）

构建：`hmapdev build` 产出 `dist/dshplgadapter_bundle.hmap`
（plugin.bin.linux.amd64 + plugin.bin.darwin.amd64；plg.json 的 name_en
已由生成器的 `Dshplgadapter` 改为 `dshplgadapter`，避免产物文件名 mismatch。
一个观察：plg.json targets 写 linux+windows，但实际产出是 linux+darwin，
安装时以 .hmap 内实际内容为准。）

## 6. 待用户定的唯一决策（§9.5）

**dsh 插件是 Node 代码，`apply()` 里能执行任意命令**
（例如 import('node:child_process')）。阶段 1 开工前必须由用户定：

> **允不允许 dsh 插件跑任意代码？**

骨架不默认放行：发现是纯静态读文件，规则 3 候选只标记不加载，
sidecar 不拉起，端到端验证不跑。决策下来后，再接 Node 宿主与工具调用链。
除此之外没有其他待定项。
