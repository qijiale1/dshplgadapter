// dshplgadapter host.js — Node 侧 Cordis 宿主（一个 sidecar 只带一个 dsh 插件）。
//
// 运行形态：Go 侧以 `node host.js <插件目录>` 拉起，协议走 fd 3（net.Socket），
// stdout 从不使用（日志一律走 stderr），插件的 console.log 再多也不会坏帧。
//
// 宿主提供的 ctx 面（探针 0.2/0.4 的结论）：
//   - tools：真实服务（register / guard / list / get）。register 收到的是
//     defineTool 已编译好的 JSON Schema（直接透传 + 断言）；只有手写 def
//     的作者 spec 才用 dsh-tools 自带的 parameterSchemaSpecToJsonSchema 转换；
//     非法 spec 直接抛错（fail-loud），工具不注册、错误记日志并通知 Go 侧。
//   - agents：占位服务，只满足 inject 门禁与方法存在性；create/get/list/
//     resume 调用时打警告并返回空结果（真正驱动 agent 的插件在阶段 1–3
//     不可用，见 README 限制）。不是静默伪造：每次调用都有警告。
//   - logger：stderr 实现（裸 Context 的 logger 默认不输出，插件日志会丢）。
//   - sessionQuery / userQuestions：未提供，调用即抛明确错误（fail-loud）。
//   - ctx.on / ctx.waterfall / ctx.effect：Cordis 原生。
//
// 插件加载必须走 ctx.plugin（探针 0.2：直接调 apply 不经过 fiber，
// ctx.effect 等 fiber 能力不可用）；inject 缺服务时 fiber 停 PENDING，
// 宿主 30s 后直接退出（探针 0.4 的静默故障不能留：装上了但没反应
// 是最难查的形态，宁可大声死掉）。
//
// 安全声明（用户 2026-11-04 已定）：允许 dsh 插件跑任意命令。
// 装 dsh 插件 = 给本机执行权限。见 README。

'use strict';

const fs = require('fs');
const net = require('net');
const path = require('path');
const { pathToFileURL } = require('url');

const PLUGIN_DIR = process.argv[2];
const PENDING_TIMEOUT_MS = 30000;
const PENDING_WARN_AT_MS = [5000, 10000, 20000];
const HOOK_EVENTS = new Set([
  'tools/pre-execute', 'tools/execute', 'tools/post-execute',
  'session/event', 'agent/status', 'approval/request',
]);

function log(...args) {
  process.stderr.write('[dshplgadapter-host] ' + args.join(' ') + '\n');
}

function fatal(msg) {
  log('FATAL:', msg);
  process.exit(1);
}

if (!PLUGIN_DIR) fatal('usage: node host.js <plugin-dir>');
if (!fs.existsSync(PLUGIN_DIR)) fatal('plugin dir missing: ' + PLUGIN_DIR);

// dsh 包解析：优先 NODE_PATH（Go 侧指向 profile 的 node_modules），
// 其次全局 dsh 安装的嵌套 node_modules；都找不到就大声退出。
function resolvePkg(name) {
  try {
    return require(name);
  } catch (e) { /* fall through */ }
  const fallbacks = ['/usr/lib/node_modules/@deepseek-ai/dsh/node_modules'];
  for (const base of fallbacks) {
    try {
      return require(path.join(base, name));
    } catch (e) { /* fall through */ }
  }
  return null;
}

const cordisPkg = resolvePkg('@deepseek-ai/cordis');
if (!cordisPkg) fatal('cannot resolve @deepseek-ai/cordis (Go side must set NODE_PATH to the dsh profile node_modules)');
const { Context, Service } = cordisPkg;

const dshTools = resolvePkg('@deepseek-ai/dsh-tools');
if (!dshTools) fatal('cannot resolve @deepseek-ai/dsh-tools');
const { parameterSchemaSpecToJsonSchema, validateJsonSchemaValue, assertSupportedJsonSchema } = dshTools;
if (typeof parameterSchemaSpecToJsonSchema !== 'function' || typeof validateJsonSchemaValue !== 'function') {
  fatal('@deepseek-ai/dsh-tools does not export parameterSchemaSpecToJsonSchema/validateJsonSchemaValue');
}

// ---- 状态（一个 sidecar 一个插件）----
const state = {
  pluginName: '',
  pluginDir: PLUGIN_DIR,
  mainFile: '',
  inject: [],
  fiber: null,
  pluginState: 'STARTING', // STARTING | ACTIVE | PENDING | FAILED | DISPOSED
  pluginError: '',
  tools: new Map(), // dshName -> { def, jsonSchema }
  guards: [],
  hooks: new Set(), // 插件注册过的 hook 事件名
  recentNotifies: [], // {event, tool, at}，仅 status 用，不存参数与结果
  callSeq: 0,
  hookSeq: 0,
};

// ---- fd 3 协议通道 ----
const sock = new net.Socket({ fd: 3, readable: true, writable: true });
let sockBuf = '';

function sendFrame(frame) {
  try {
    sock.write(JSON.stringify(frame) + '\n');
  } catch (e) {
    log('ERROR: fd3 write failed:', e.message);
  }
}

// Go 侧通知（无 id 的 JSON-RPC notification）。
function notify(method, params) {
  sendFrame({ jsonrpc: '2.0', method, params });
}

function rpcError(code, message) {
  const err = new Error(message);
  err.code = code;
  return err;
}

// ---- Cordis Context 与服务 ----
const ctx = new Context();

// logger：stderr 实现（裸 Context 的 logger 默认不输出）。
(function installLogger() {
  const logger = {
    debug: (...a) => log('[plugin debug]', ...a),
    info: (...a) => log('[plugin]', ...a),
    warn: (...a) => log('[plugin WARN]', ...a),
    error: (...a) => log('[plugin ERROR]', ...a),
  };
  try {
    ctx.logger = logger;
  } catch (e) { /* ignore, try defineProperty below */ }
  if (!ctx.logger || typeof ctx.logger.info !== 'function') {
    try {
      Object.defineProperty(ctx, 'logger', { value: logger, writable: true, configurable: true });
    } catch (e) {
      log('WARN: cannot install stderr logger, plugin ctx.logger stays native');
    }
  }
})();

// 未提供的 ctx 面：调用即抛明确错误（fail-loud，不伪造）。
if (typeof ctx.sessionQuery === 'undefined') {
  ctx.sessionQuery = () => { throw new Error('ctx.sessionQuery is not provided by dshplgadapter (phases 1-3)'); };
}
if (typeof ctx.userQuestions === 'undefined') {
  ctx.userQuestions = () => { throw new Error('ctx.userQuestions is not provided by dshplgadapter (phases 1-3)'); };
}

// tools 服务：register / guard / list / get。
class ToolsService extends Service {
  constructor(c) {
    super(c, 'tools');
  }

  register(def) {
    if (!def || typeof def.name !== 'string' || !def.name) {
      throw new Error('tool definition needs a non-empty name');
    }
    if (typeof def.execute !== 'function') {
      throw new Error(`tool ${def.name} needs execute()`);
    }
    // schema 形态（实测 dsh-tools 源码确认，勿二次转换）：
    // defineTool() 内部已经调 parameterSchemaSpecToJsonSchema 编译过，
    // 所以 register 收到的是已编译 JSON Schema（{type:'object', properties}），
    // 再调一次转换会把 {type, properties} 当属性表读，报
    // "parameters.type must be a value schema object"。因此：
    // 已编译形态（type 为字符串）直接透传 + assertSupportedJsonSchema 断言；
    // 只有手写 def（无 defineTool、parameters 为作者 spec 属性表）才走转换。
    // 非法 spec 在哪一层都直接抛错、工具不注册 —— 不存在静默丢字段的路径。
    let jsonSchema;
    try {
      const p = def.parameters || {};
      if (typeof p.type === 'string') {
        if (p.type !== 'object' || typeof p.properties !== 'object') {
          throw new Error(`parameters is not an object JSON schema (type=${p.type})`);
        }
        if (typeof assertSupportedJsonSchema === 'function') assertSupportedJsonSchema(p);
        jsonSchema = p;
      } else {
        jsonSchema = parameterSchemaSpecToJsonSchema(p);
      }
    } catch (e) {
      const msg = `tool ${def.name} schema rejected: ${e.message}`;
      log('ERROR:', msg);
      notify('register', { type: 'tool_error', data: { name: def.name, plugin: state.pluginName, error: msg } });
      throw new Error(msg);
    }
    state.tools.set(def.name, { def, jsonSchema });
    notify('register', {
      type: 'tool',
      data: { name: def.name, description: def.description || '', parameters: jsonSchema, plugin: state.pluginName },
    });
    return () => {
      state.tools.delete(def.name);
      notify('register', { type: 'tool_removed', data: { name: def.name, plugin: state.pluginName } });
    };
  }

  // 单调守卫：返回理由字符串即拒绝，falsy 即允许；一旦拒绝不可撤销。
  guard(fn) {
    if (typeof fn !== 'function') throw new Error('ctx.tools.guard needs a function');
    state.guards.push(fn);
    return () => {
      const i = state.guards.indexOf(fn);
      if (i >= 0) state.guards.splice(i, 1);
    };
  }

  list() {
    return [...state.tools.values()].map((t) => t.def);
  }

  get(name) {
    const t = state.tools.get(name);
    return t ? t.def : undefined;
  }
}

// agents 服务：占位实现，只满足 inject 门禁；调用打警告、返回空结果。
class AgentsService extends Service {
  constructor(c) {
    super(c, 'agents');
  }

  create() {
    log('WARN: ctx.agents.create is a placeholder in dshplgadapter phases 1-3, returning null');
    return null;
  }

  get() {
    log('WARN: ctx.agents.get is a placeholder in dshplgadapter phases 1-3, returning null');
    return null;
  }

  list() {
    log('WARN: ctx.agents.list is a placeholder in dshplgadapter phases 1-3, returning []');
    return [];
  }

  resume() {
    log('WARN: ctx.agents.resume is a placeholder in dshplgadapter phases 1-3, returning null');
    return null;
  }
}

ctx.plugin(ToolsService);
ctx.plugin(AgentsService);

// ctx.on 包装：只记录 hook 事件的注册（通知 Go 侧记账与管理工具可见），
// 其余事件原样透传。waterfall 评估时直接跑 Cordis 原生链，不依赖这层记录。
const origOn = ctx.on.bind(ctx);
ctx.on = (...args) => {
  const event = args[0];
  if (typeof event === 'string' && HOOK_EVENTS.has(event) && !state.hooks.has(event)) {
    state.hooks.add(event);
    notify('register', { type: 'hook', data: { event, plugin: state.pluginName } });
  }
  return origOn(...args);
};

// ---- 工具执行 ----
const neverController = new AbortController();

async function callTool(name, args) {
  const t = state.tools.get(name);
  if (!t) throw rpcError(-32601, `tool not found: ${name}`);
  const safeArgs = args || {};
  // 用 register 时已归一化的编译 schema 校验（defineTool 的 execute 内部
  // 也会再校验一次；这里只做前置 fail-loud，不做二次 spec 转换）。
  const problems = validateJsonSchemaValue(t.jsonSchema, safeArgs, '');
  if (problems && problems.length) {
    throw rpcError(-32602, `invalid arguments for ${name}: ${problems.join('; ')}`);
  }
  const execCtx = {
    callId: `ha-${++state.callSeq}`,
    name,
    arguments: safeArgs,
    signal: neverController.signal,
  };
  const value = await t.def.execute(safeArgs, execCtx);
  let blocks;
  if (t.def.output && typeof t.def.output.render === 'function') {
    blocks = t.def.output.render(safeArgs, value);
  } else {
    blocks = [{ type: 'text', text: typeof value === 'string' ? value : JSON.stringify(value) }];
  }
  if (!Array.isArray(blocks)) blocks = [{ type: 'text', text: String(blocks) }];
  return { content: blocks };
}

// ---- hook 判定（阶段 2，HA 阶段单向查询）----
// 只跑 tools/pre-execute 的 waterfall：它是 dsh 的决策点（{kind:'deny'}）。
// tools/execute 是环绕（超时/重试/指标），在 HA 阶段无允许/拒绝之外的
// 对应语义，不参与判定；tools/post-execute 是结果变换，HA 的
// after_toolcall 不回放（见 README），也不参与判定。
async function evaluateHook(event, tool, args) {
  if (event !== 'tools/pre-execute') {
    return { decision: 'error', reason: `unsupported hook event for evaluation: ${event}` };
  }
  const exec = {
    callId: `ha-hook-${++state.hookSeq}`,
    name: tool,
    arguments: args || {},
    signal: neverController.signal,
  };
  let decision;
  try {
    decision = await ctx.waterfall('tools/pre-execute', exec, async () => ({ kind: 'allow' }));
  } catch (e) {
    return { decision: 'error', reason: String((e && e.message) || e) };
  }
  if (decision && decision.kind === 'deny') {
    return { decision: 'deny', reason: decision.reason || '' };
  }
  for (const g of state.guards) {
    let r;
    try {
      r = g(exec);
    } catch (e) {
      return { decision: 'error', reason: `guard threw: ${e.message}` };
    }
    if (r) return { decision: 'deny', reason: String(r) };
  }
  return { decision: 'allow' };
}

// ---- 插件加载 ----
function resolveMainFile() {
  const pkgPath = path.join(PLUGIN_DIR, 'package.json');
  let main = '';
  try {
    const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));
    if (typeof pkg.main === 'string' && pkg.main) main = pkg.main;
    state.pluginName = typeof pkg.name === 'string' && pkg.name ? pkg.name : '';
  } catch (e) {
    fatal(`cannot read ${pkgPath}: ${e.message}`);
  }
  const candidates = [];
  if (main) candidates.push(main);
  candidates.push('lib/index.js', 'index.js');
  for (const c of candidates) {
    const full = path.join(PLUGIN_DIR, c);
    if (fs.existsSync(full)) return full;
  }
  fatal(`no loadable entry in ${PLUGIN_DIR} (tried: ${candidates.join(', ')})`);
  return '';
}

const FIBER_STATE = { 0: 'PENDING', 1: 'LOADING', 2: 'ACTIVE', 3: 'FAILED', 4: 'DISPOSED', 5: 'UNLOADING' };

async function waitFiber(fiber, what) {
  const started = Date.now();
  const warned = new Set();
  for (;;) {
    const st = FIBER_STATE[fiber.state] || `?(${fiber.state})`;
    if (st === 'ACTIVE') return true;
    if (st === 'FAILED' || st === 'DISPOSED') return false;
    const elapsed = Date.now() - started;
    for (const at of PENDING_WARN_AT_MS) {
      if (elapsed >= at && !warned.has(at)) {
        warned.add(at);
        log(`WARN: plugin ${what} still ${st} after ${at}ms (inject: [${state.inject.join(', ')}])`);
      }
    }
    if (elapsed >= PENDING_TIMEOUT_MS) {
      log(`ERROR: plugin ${what} stuck ${st} after ${PENDING_TIMEOUT_MS}ms (inject: [${state.inject.join(', ')}]); refusing to idle silently`);
      return false;
    }
    await new Promise((r) => setTimeout(r, 200));
  }
}

async function loadPlugin(mainFile, cacheBust) {
  let href = pathToFileURL(mainFile).href;
  if (cacheBust) href += `?t=${Date.now()}`;
  const entry = await import(href);
  let mod = entry;
  if (entry && entry.default && typeof entry.default.apply === 'function') mod = entry.default;
  if (!mod || typeof mod.apply !== 'function') {
    throw new Error(`plugin entry must export apply() (got: ${Object.keys(entry || {}).join(', ')})`);
  }
  if (typeof mod.name === 'string' && mod.name) state.pluginName = mod.name;
  if (Array.isArray(mod.inject)) state.inject = mod.inject.filter((x) => typeof x === 'string');
  const fiber = ctx.plugin(mod);
  state.fiber = fiber;
  const ok = await waitFiber(fiber, state.pluginName || path.basename(PLUGIN_DIR));
  state.pluginState = ok ? 'ACTIVE' : (FIBER_STATE[fiber.state] || `?(${fiber.state})`);
  if (!ok) {
    state.pluginError = `fiber ended ${state.pluginState} (inject: [${state.inject.join(', ')}])`;
    return false;
  }
  return true;
}

async function initialLoad() {
  state.mainFile = resolveMainFile();
  try {
    const ok = await loadPlugin(state.mainFile, false);
    if (!ok) fatal(`plugin failed to activate: ${state.pluginError}`);
    log(`plugin active: ${state.pluginName || '(unnamed)'} (inject: [${state.inject.join(', ')}])`);
  } catch (e) {
    fatal(`plugin load failed: ${e.message}`);
  }
}

// ---- RPC 处理 ----
async function handleRequest(method, params) {
  const p = params || {};
  switch (method) {
    case 'ping':
      return { ok: true, plugin: { name: state.pluginName, state: state.pluginState } };
    case 'tools/list': {
      const tools = [...state.tools.entries()].map(([name, t]) => ({
        name, description: t.def.description || '', inputSchema: t.jsonSchema,
      }));
      return { tools };
    }
    case 'tools/call':
      return callTool(p.name, p.arguments);
    case 'hooks/evaluate':
      return evaluateHook(p.event, p.tool, p.arguments);
    case 'hooks/notify': {
      // 单向观测：只记录，不回放进 dsh 事件环（HA 阶段上下文与 dsh
      // session/event 载荷形态不同，强行回放会误导插件）。
      state.recentNotifies.push({ event: p.event, tool: p.tool, at: Date.now() });
      if (state.recentNotifies.length > 20) state.recentNotifies.shift();
      return { ok: true };
    }
    case 'plugin/status':
      return {
        name: state.pluginName,
        state: state.pluginState,
        inject: state.inject,
        tools: [...state.tools.keys()],
        hooks: [...state.hooks],
        recentNotifies: state.recentNotifies,
      };
    case 'plugin/dispose': {
      if (state.fiber && state.pluginState !== 'DISPOSED') {
        await state.fiber.dispose();
        state.pluginState = 'DISPOSED';
      }
      return { disposed: true };
    }
    case 'plugin/reload': {
      if (state.fiber && state.pluginState !== 'DISPOSED') {
        await state.fiber.dispose();
        state.pluginState = 'DISPOSED';
      }
      state.tools.clear();
      state.guards = [];
      state.hooks.clear();
      state.recentNotifies = [];
      try {
        const ok = await loadPlugin(state.mainFile, true);
        if (!ok) return { name: state.pluginName, state: state.pluginState, error: state.pluginError };
        return { name: state.pluginName, state: state.pluginState };
      } catch (e) {
        state.pluginState = 'FAILED';
        state.pluginError = e.message;
        log('ERROR: reload failed:', e.message);
        return { name: state.pluginName, state: state.pluginState, error: e.message };
      }
    }
    default:
      throw rpcError(-32601, `method not found: ${method}`);
  }
}

function onLine(line) {
  if (!line.trim()) return;
  let frame;
  try {
    frame = JSON.parse(line);
  } catch (e) {
    log('WARN: dropping non-JSON frame on fd3:', line.slice(0, 200));
    return;
  }
  // 无 id = Go 侧单向通知（目前只有 hooks/notify），不回包。
  if (frame.id === undefined || frame.id === null) {
    if (frame.method === 'hooks/notify') {
      const p = frame.params || {};
      state.recentNotifies.push({ event: p.event, tool: p.tool, at: Date.now() });
      if (state.recentNotifies.length > 20) state.recentNotifies.shift();
    } else if (frame.method) {
      log('WARN: ignoring unknown notification:', frame.method);
    }
    return;
  }
  handleRequest(frame.method, frame.params)
    .then((result) => sendFrame({ jsonrpc: '2.0', id: frame.id, result }))
    .catch((e) => sendFrame({
      jsonrpc: '2.0', id: frame.id,
      error: { code: e.code || -32603, message: e.message || String(e) },
    }));
}

sock.on('data', (chunk) => {
  sockBuf += chunk.toString('utf8');
  let idx;
  while ((idx = sockBuf.indexOf('\n')) >= 0) {
    const line = sockBuf.slice(0, idx);
    sockBuf = sockBuf.slice(idx + 1);
    onLine(line);
  }
});

sock.on('error', (e) => {
  log('ERROR: fd3 socket error:', e.message);
  process.exit(1);
});

sock.on('close', () => {
  log('fd3 closed, exiting');
  process.exit(0);
});

process.on('uncaughtException', (e) => {
  log('FATAL: uncaught exception:', (e && e.stack) || e);
  process.exit(1);
});

process.on('SIGTERM', () => {
  log('SIGTERM, disposing and exiting');
  if (state.fiber) state.fiber.dispose().catch(() => {}).finally(() => process.exit(0));
  else process.exit(0);
});
process.on('SIGINT', () => {
  log('SIGINT, disposing and exiting');
  if (state.fiber) state.fiber.dispose().catch(() => {}).finally(() => process.exit(0));
  else process.exit(0);
});

// 先起 RPC，再异步加载插件：Go 侧 waitReady(ping) 不会被插件加载卡住，
// 而 waitPluginActive 会等到 ACTIVE/FAILED 再放行。
initialLoad();
