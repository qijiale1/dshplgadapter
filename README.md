# dshplgadapter

dshplgadapter plugin

## Build

```bash
hmapdev build
```

## Channels

入站与出站是分开登记的两件事：

| 方向 | API | 用途 |
|---|---|---|
| 入站 inputch | RegisterInputChannel(name, def) | 声明「谁会往这个通道注入输入」。**凡是用 InjectText*/InjectInput*/InjectInterrupt*(source, "<name>", ...) 注入的通道名都要登记** |
| 出站 output | RegisterOutputChannel(name, caps, desc, def, handler) | 声明 output_send__<name> 的回复发给谁；handler 收到 {payload,type,meta} |

def（ChannelDef）描述该通道在记忆计算层的行为：NoMemory: true = 该通道输入不进记忆；
Cleaner = 计算层清洗后再向量化/提关键词（原文不改）。

> 只登记出站通道、却用同名通道注入输入时，内核会兜底登记同名 inputch 并在日志里告警。
> 兜底只为兼容老插件 —— 请显式登记，让「这是入站通道」成为插件的明确意图。

## Install

Upload the .hmap file through the Plugin Manager API.

## dshplgadapter：dsh 插件接入（阶段 1–4）

把 DeepSeek Harness（dsh）的 Cordis 插件生态接入 HologramAgent 的外部插件。
一个 sidecar 只带一个 dsh 插件，协议走 fd 3（stdout 只当日志）。

### 安全声明（部署前必读，用户 2026-11-04 已定）

**允许 dsh 插件跑任意命令。装 dsh 插件 = 给本机执行权限。**
本项目不做沙箱限制。只安装你信任的插件。

### 管理工具

- `dshplgadapter_list`：发现结果＋运行中 sidecar＋已暂存 skill＋capabilities
- `dshplgadapter_skills`：已暂存 skill 列表＋stageDir＋“下次重扫移交”说明
- `dshplgadapter_launch`：手动试加载一个已发现包（参数 plugin 必填）
- `dshplgadapter_reload`：热重载运行中插件
- `dshplgadapter_install`：阶段 4 远程安装（默认关闭，需 config
  `remote_install_enabled=true`；参数 package、sha256 都必填；
  先校验 sha256 后解压，不一致拒绝且不落文件；装完重扫发现链路，
  不自动拉起）。用法与校验流程见 `PHASE4_NOTES.md`

### 限制（部署方须知）

- 只读本地已安装包（默认 `~/.dsh/profiles/web/node_modules`），
  阶段 4（远程安装）已做但**默认关闭**（config `remote_install_enabled`
  默认 false）；开启后也须提供 sha256 才装，校验流程见 `PHASE4_NOTES.md`。
  远程安装 = 从网上拉代码到本机执行，只从信任的来源安装。
- 规则 3 候选（有入口、无 dsh 标记的包）在 Start 时只标记、不自动拉起，
  须用 `dshplgadapter_launch` 手动试加载 —— 这是对比规划书 §3.3 字面的
  有意偏离（自动拉起会把 node_modules 里任何带入口的包都 import）。
- agents 是占位服务（只满足 inject 门禁，调用打警告、返回 null/[]）；
  sessionQuery/userQuestions 未提供（调用即抛明确错误）。
- after_toolcall 只通知、不回放结果；session/event 只记录、不实时订阅
  （外部插件形态下无法 Publish/Subscribe）；approval/request 未接线。
- 钩子判定 fail-open：查询出错/超时/无 sidecar 一律放行（大声日志），
  但任一 deny 即 deny，拒绝胜出。
- 热重载同名覆盖；删掉的工具会在 HA 表里残留到重启
  （SDK 1.4.0 无工具注销接口）。
- 纯 SKILL 包只暂存到 skill/ 目录，交接在下次 pluginmgr 启动重扫时完成，
  暂存当时不算已注册；管理工具如实报告 staged。

详细改动与验证见 `PHASE1_NOTES.md`（阶段 1–3）与 `PHASE4_NOTES.md`
（阶段 4 远程安装），探针证据见
`docs/zh/dshplgadapter-probe.md`（已验证的探针结论不要改写）。
