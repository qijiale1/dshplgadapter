# dshplgadapter 阶段 4：远程安装（2026-11-07）

用户 2026-11-07 拍板：开阶段 4。规划书 §四要求：从 npm / dsh 插件市场安装，
**必须带 sha256 校验，且默认关闭**。另见 §3.4 第三条：
阶段 1 只支持本地已安装包；远程安装推迟到阶段 4 并带 sha256。

## 用法

### 开关（默认关闭）

插件配置：

```json
{
  "remote_install_enabled": true,
  "npm_registry": "https://registry.npmjs.org"
}
```

- `remote_install_enabled`：默认 `false`。关着时 `dshplgadapter_install`
  直接拒绝，并说明如何开启；不设、设错类型都按关闭处理。
- `npm_registry`：可选，默认 `https://registry.npmjs.org`。
  改完配置要重启插件才生效。

### 管理工具 `dshplgadapter_install`

参数（都必填）：

| 参数 | 含义 |
| --- | --- |
| `package` | 包名，可带版本：`pkg`、`pkg@1.2.3`、`@scope/pkg`、`@scope/pkg@1.2.3`；不带版本装 `latest` |
| `sha256` | tarball 的期望 sha256（64 位十六进制）；**不提供直接拒绝** |

sha256 从哪来：

- 包作者发布时给出的值（README / release notes / 插件市场页）；
- 或 registry 的 packument 里对应版本的 `dist` 信息（本工具安装时也会
  把 registry 提供的 `integrity`（sha512）与 `shasum`（sha1）一并核对，
  但**不能只靠它们**——调用方提供的 sha256 是独立的第二方校验）；
- 自己先在可信机器上下载一次、算出 sha256 再填进来也可以。

### 安装成功后发生什么

1. 安装结果返回包名、版本、tarball URL、实测 sha256、integrity/shasum
   是否核对上、落盘目录。
2. 自动触发一次重扫（`rescan`）：新包进入 1–3 阶段已有的发现链路，
   `dshplgadapter_list` 里能看到它；纯 SKILL 包按阶段 3 的规则补暂存。
3. **不自动拉起 sidecar**：dsh 插件用 `dshplgadapter_launch` 手动试加载
   （与规则 3 候选一致）；纯 SKILL 包等下次 pluginmgr 启动重扫移交。

## 校验流程（按执行顺序，失败即停）

1. 开关检查：`remote_install_enabled` 为 false → 拒绝（不发任何请求）。
2. 解析 `包名[@版本]` 并校验包名（单段或 `@scope/段`；每段过
   `checkSafeName`，再过 npm 字符集；拒绝 `..`、隐藏段、大写）。
3. 取 packument（`GET {registry}/{包名}`），解析版本：
   `latest` 走 `dist-tags.latest`，指定版本必须在 `versions` 里；
   取该版本的 `dist.tarball` / `dist.shasum` / `dist.integrity`。
4. 下载 tarball（单包上限 200 MiB，HTTP 超时 120s）。
5. 先校验、后解压（tarball 只在内存里）：
   - 期望 sha256 未提供 → 拒绝（fail-closed）；格式不对 → 拒绝；
     与实测不一致 → 拒绝。
   - registry 的 `shasum`（sha1）与 `integrity`（sha512/sha1）有就核对，
     对不上 → 拒绝；不支持的 integrity 算法 → 拒绝。
6. 解压到安装根内的临时暂存目录（同一文件系统，便于 rename 原子落盘）：
   - 条目必须在 `package/` 前缀下；绝对路径、含 `..` 段 → 拒绝
     （走已有的 `safeArchiveTarget` / `resolveUnderRoot`）；
   - 只允许目录与普通文件，符号链接/硬链接/特殊文件 → 拒绝；
   - 单文件上限 100 MiB、解压总量上限 500 MiB。
7. 解出的 `package.json` 的 `name` 必须等于请求的包名，否则拒绝。
8. 目标已存在 → 拒绝（不覆盖，先移除再安装）。
9. 任何一步失败都删掉暂存目录，目标位置不留文件；成功才 rename 落盘。

每一步失败都记日志，写明哪一步、哪个校验、哪个 URL/包名版本。

## 安全声明（部署前必读，§9.5 同样适用）

**远程安装 = 从网上拉代码到本机执行。装 dsh 插件 = 给本机执行权限。**
sha256 只能证明"你拿到的是你以为的那个 tarball"，不能证明
"这个 tarball 里的代码是善意的"。只从你信任的来源安装；
来源不可信时，再正确的 sha256 也救不了。本项目不做沙箱限制。

## 实现位置

- `install.go`（新建）：`remoteInstaller`（开关、registry、HTTP、
  packument 解析、下载、校验、解压、落盘）与 `parsePackageSpec` /
  `validatePackageName` / `verifyDist` / `extractTarball`。
- `plugin.go`：`Plugin.installer` 字段；`NewPluginFactory` 读
  `remote_install_enabled` / `npm_registry`；`rescan()`（重跑
  `DiscoverPlugins` 并补暂存新 skill）；`installPackage()`（安装 →
  重扫 → 报告发现状态与 launch 提示）；注册管理工具
  `dshplgadapter_install`。
- `install_test.go`（新建）：8 项测试，全部用本地 httptest registry
  与自造 tarball，**不依赖外网**。

## 测试与变异验证（2026-11-07，本机实测）

```bash
export PATH=~/workspace/go/bin:$PATH
gofmt -l .          # 干净
go vet ./...        # 干净
go test -count=1 ./...   # 30 项全过（约 12.5s），0 失败 0 跳过
```

新测试（8 项）：

| 测试 | 判据 |
| --- | --- |
| TestInstallRejectsSHA256Mismatch | sha256 不一致拒绝，不落文件 |
| TestInstallRejectsMissingSHA256 | 未提供 sha256 拒绝（fail-closed），不落文件 |
| TestInstallRejectsWhenDisabled | 开关关闭拒绝，且零 registry 请求 |
| TestInstallRejectsPathTraversalTarball | tarball 含 `..` 拒绝，根外不写文件，暂存目录被清理 |
| TestInstallRejectsSymlinkEntry | tarball 含符号链接拒绝 |
| TestInstallRejectsIntegrityMismatch | registry integrity 对不上拒绝 |
| TestInstallSuccessWithLocalRegistry | 本地桩成功安装 → 落盘内容对 → 重扫发现为 dsh 插件 → 给出 launch 提示；重复安装拒绝（不覆盖） |
| TestParsePackageSpec | 包名/版本解析与校验（好坏各一组） |

变异验证（每组：改实现 → 对应判据必须变红 → 还原并校验和确认文件一致）：

| 变异 | 判据 | 结果 |
| --- | --- | --- |
| verifyDist 的 sha256 比对恒过 | TestInstallRejectsSHA256Mismatch | 变红 ✓ |
| 未提供 sha256 时直接放行（fail-open） | TestInstallRejectsMissingSHA256 | 变红 ✓ |
| Install 的 enabled 开关检查去掉 | TestInstallRejectsWhenDisabled | 变红 ✓ |
| extractTarball 绕过 safeArchiveTarget（直接 Join） | TestInstallRejectsPathTraversalTarball | 变红 ✓ |
| 解压前缀要求改错（正常 tarball 也解不出） | TestInstallSuccessWithLocalRegistry | 变红 ✓ |

附记：第一版变异 2（只删空值检查）**没红**——因为期望值格式检查
（64 位十六进制）把空串也拦住了。这是纵深防御在起作用，不是判据失效；
改用真正的错误形态（未提供时直接放行）后判据变红。

变异后全量复测：`go test -count=1 ./...` 30 项全过，gofmt、go vet 干净；
`install.go` / `plugin.go` / `install_test.go` 的 sha256 与变异前基线一致。

## 构建（2026-11-07，本机实测）

```bash
~/workspace/dshplgadapter-repos/hmapdev-bin/hmapdev build
# 产出 dist/dshplgadapter_bundle.hmap，12,702,479 字节
# sha256: 5dbe510c22cb078f1eb97a17eef66d3ff0ee9d8c073f56259e6520171478f112
# 内含 plugin.bin.linux.amd64（10,862,425 字节）+
# plugin.bin.darwin.amd64（11,070,928 字节）；两二进制均含
# remote_install_enabled / npm_registry / sha256 相关代码（已核对）。
```

附记：产物比阶段 1–3 的 5,912,638 字节大了一倍多，原因是 net/http +
crypto/tls + archive/tar 被编译进两个二进制；功能与平台构成未变
（仍是 linux + darwin，plg.json 写 linux+windows 的已知偏差未动）。

## 仍存在的限制（阶段 4 内）

- **没有做真实 npm 端到端安装**：成功路径只在本地 httptest registry
  上验证（自造 tarball + 本地 packument 桩）。真实 registry 的
  packument 结构、重定向、限速等未实测；第一次对真实包安装时，
  建议先用一个已知小包、拿作者公布的 sha256 试装一次。
- 只支持 npm registry 协议（packument + tarball）；dsh 插件市场若有
  自己的安装协议/鉴权，不在本次实现内，需另行对接。
- 不做版本范围解析（`^1.2.3`、`~1.2`、`>=1.0`）：只接受确切版本或
  `latest`；范围写法会被版本格式校验拒绝。
- 安装不更新 `<profile>/package.json` 的 dependencies 与
  `dsh.profile.bundles` 清单：包只落在 `node_modules` 下并进入本
  适配器的发现链路；dsh 自身的 profile 清单如需同步，由部署方
  在 dsh 侧处理（或下次 `dsh plugin add` 时统一登记）。
- 不提供卸载工具：移除已安装包请直接删对应目录（删前确认无 sidecar
  在跑，或先停插件），再用重扫刷新发现结果。
- 与 1–3 阶段相同的遗留问题仍在（见 PHASE1_NOTES.md）：
  approval/request 未接线、agents 占位、sessionQuery/userQuestions
  未提供、删掉的工具残留到重启、skill 交接在下次重扫完成、
  hook 查询 fail-open；`plg.json` 写 linux+windows 但实际产
  linux+darwin（本次未动，也不谎称修了）。

## 配置（plugin config，阶段 4 新增）

- `remote_install_enabled`（默认 false）
- `npm_registry`（默认 `https://registry.npmjs.org`）
