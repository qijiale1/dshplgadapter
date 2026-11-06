package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 从 clawhubadapter/sidecar.go 移植（2026-11-04），阶段 1 改为 fd 3 传输。
//
// 移植时改动（其余结构、超时、关闭语义原样保留）：
//   ① OCNotification/OCPTool/OCCallResult → DSH*（dsh 侧通知与工具形态）；
//   ② 入口由 main.js 改为 host.js —— Node 侧是 Cordis 宿主（本工程
//      host.js，go:embed 进 plugin.bin），不是 OpenClaw 模拟器；
//   ③ launchProcessIn 不再把 NODE_PATH 当作 node 二进制目录（原版 quirk，
//      对 dsh 不适用）；node 走 PATH，NODE_PATH 由调用方设为 profile 的
//      node_modules（cordis/dsh-tools 解析用），透传给子进程；
//   ④ scanner 上限由 64KB 放宽到 1MB（初始仍 64KB）：工具结果可能很长，
//      64KB 上限会让 readLoop 在长行上直接退出，等于 sidecar 死亡；
//   ⑤ 日志前缀 [clawhubadapter] → [dshplgadapter]。
//
// ★ 传输层（阶段 1，探针 0.1 已验证）：JSON-RPC over fd 3，不是 stdio。
//
// dsh 插件爱打 console.log，stdout 一旦混入日志帧，readLoop 就报
// 「非法 JSON 帧」并丢行（clawhubadapter 生产上为此付过 1038 次错）。
// 所以 Go 侧建 Unix socketpair，把一端以 ExtraFiles[0] 传给子进程
// （子进程里即 fd 3），Node 侧用 net.Socket 包起 fd 3 做异步收发；
// 子进程的 stdout/stderr 直接并入本进程 stderr（即内核日志），
// 不再是协议通道。host.js 自身也只写 stderr，stdout 全程不用。

//go:embed host.js
var hostJSBytes []byte

var (
	hostJSCacheOnce sync.Once
	hostJSCachePath string
	hostJSCacheErr  error
)

// materializeHostJS 把内嵌的 host.js 写到临时文件，供 node 执行。
// 同一进程内只写一次；文件随插件进程生命周期存在（几十 KB，
// /tmp 由系统回收，无需主动清理）。
func materializeHostJS() (string, error) {
	hostJSCacheOnce.Do(func() {
		dir, err := os.MkdirTemp("", "dshplgadapter-host-")
		if err != nil {
			hostJSCacheErr = err
			return
		}
		p := filepath.Join(dir, "host.js")
		if err := os.WriteFile(p, hostJSBytes, 0o644); err != nil {
			hostJSCacheErr = err
			return
		}
		hostJSCachePath = p
	})
	return hostJSCachePath, hostJSCacheErr
}

type sidecarRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type sidecarResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int              `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type DSHTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type DSHCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content"`
}

// DSHNotification 是 Node 侧 Cordis 宿主主动推送的通知
type DSHNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// pendingResp 是待应答通道 + 它自己的 close 守卫。
//
// 为什么需要 once：Close() 可能被多条路径调用（Stop / 重载 / 停机），
// 而 `close(ch)` 二次即 panic。这些 channel 是**按请求创建**的，
// 不能共用一个全局 Once ⇒ 每个自带一个。
type pendingResp struct {
	ch   chan []byte
	once sync.Once
}

func (p *pendingResp) closeOnce() { safeClose(&p.once, p.ch) }

type sidecarProcess struct {
	readerReadyOnce sync.Once
	name            string
	dir             string
	cmd             *exec.Cmd
	protoConn       *os.File
	protoW          *bufio.Writer
	mu              sync.Mutex
	nextID          int
	closed          bool
	stopped         bool

	// 异步 reader
	pending     map[int]*pendingResp
	notifyCh    chan DSHNotification
	readerStop  chan struct{}
	readerWg    sync.WaitGroup
	readerReady chan struct{}
}

func newSidecarProcess(name, dir string, cmd *exec.Cmd, protoConn *os.File) *sidecarProcess {
	sp := &sidecarProcess{
		name:        name,
		dir:         dir,
		cmd:         cmd,
		protoConn:   protoConn,
		protoW:      bufio.NewWriter(protoConn),
		pending:     make(map[int]*pendingResp),
		notifyCh:    make(chan DSHNotification, 1024),
		readerStop:  make(chan struct{}),
		readerReady: make(chan struct{}),
	}
	sp.readerWg.Add(1)
	go sp.readLoop(protoConn)
	<-sp.readerReady
	return sp
}

func (s *sidecarProcess) readLoop(r io.Reader) {
	defer s.readerWg.Done()
	scanner := bufio.NewScanner(bufio.NewReader(r))
	// 初始 64KB，上限 1MB：工具结果长行不能让 readLoop 退出。
	scanner.Buffer(make([]byte, 0, 1024*64), 1024*1024)
	safeClose(&s.readerReadyOnce, s.readerReady)

	for {
		select {
		case <-s.readerStop:
			return
		default:
		}

		if !scanner.Scan() {
			if scanner.Err() != nil {
				log.Printf("[dshplgadapter] sidecar %s read error: %v", s.name, scanner.Err())
			}
			return
		}
		line := scanner.Text()

		var base struct {
			ID     *int             `json:"id"`
			Method string           `json:"method,omitempty"`
			Error  *json.RawMessage `json:"error,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &base); err != nil {
			continue
		}

		if base.ID != nil {
			s.mu.Lock()
			pr, ok := s.pending[*base.ID]
			delete(s.pending, *base.ID)
			s.mu.Unlock()
			if ok {
				pr.ch <- []byte(line)
			}
		} else if base.Method != "" {
			var notif DSHNotification
			if err := json.Unmarshal([]byte(line), &notif); err == nil {
				select {
				case s.notifyCh <- notif:
				default:
					log.Printf("[dshplgadapter] sidecar %s notify channel full, dropping: %s", s.name, notif.Method)
				}
			}
		}
	}
}

func (s *sidecarProcess) NotifyChan() <-chan DSHNotification {
	return s.notifyCh
}

// launchSidecar 为一个 dsh 插件拉起 Node Cordis 宿主。
//
// host.js 内嵌在 plugin.bin 里，不存在"入口缺失返回 nil"的情况；
// 插件加载失败由 waitPluginActive 在启动时一次性判定（见下），
// 失败的 sidecar 不会留在运行表里。
func launchSidecar(pluginName, pluginDir, nodeModulesDir string) (*sidecarProcess, error) {
	hostJS, err := materializeHostJS()
	if err != nil {
		return nil, fmt.Errorf("materialize host.js: %w", err)
	}
	return launchProcessIn("node", hostJS, pluginDir, pluginName, nodeModulesDir)
}

// launchProcessIn 以 dir 作为工作目录拉起子进程，协议走 fd 3。
func launchProcessIn(bin, arg, dir, name, nodeModulesDir string) (*sidecarProcess, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "dshplgadapter-fd3-parent")
	childSock := os.NewFile(uintptr(fds[1]), "dshplgadapter-fd3-child")

	cmd := exec.Command(bin, arg, dir)
	cmd.Dir = dir
	// 子进程 stdout 不再是协议通道：日志直接并入本进程 stderr（内核日志）。
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{childSock}

	// NODE_PATH 供 host.js 与插件 require dsh 包（Cordis/dsh-tools）。
	// ESM 插件用 createRequire 时同样走 NODE_PATH；真实部署里 profile 的
	// node_modules 本就含这些包，NODE_PATH 只是把解析锚点钉在 profile 上。
	env := os.Environ()
	if nodeModulesDir != "" {
		nodePath := nodeModulesDir
		for i, e := range env {
			if strings.HasPrefix(e, "NODE_PATH=") {
				nodePath = nodeModulesDir + string(os.PathListSeparator) + e[len("NODE_PATH="):]
				env[i] = "NODE_PATH=" + nodePath
				nodePath = ""
				break
			}
		}
		if nodePath != "" {
			env = append(env, "NODE_PATH="+nodePath)
		}
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		parentSock.Close()
		childSock.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	childSock.Close() // 父进程的副本；子进程里它就是 fd 3

	sp := newSidecarProcess(name, dir, cmd, parentSock)

	if err := sp.waitReady(); err != nil {
		sp.Close()
		return nil, err
	}
	if err := sp.waitPluginActive(30 * time.Second); err != nil {
		sp.Close()
		return nil, err
	}

	return sp, nil
}

// sidecarReadyTimeout 是等 sidecar 回第一个 ping 的上限。
//
// ★ 从固定 5s 放宽到 30s（2026-10-25 修 CI 误报）。
//
// 原来的 5s 是**按开发机估算**的：node 冷启动 + 模块加载在快盘上空载时
// 约 0.2s。而 CI 的共享 runner（以及真实部署里的低配机器）上，一次
// fork+exec+node 引导遇到 CPU 争抢就会到 5s 以上，实测 CI 报
// `sidecar echoplugin ping timeout` 而本机全绿。
//
// 这不是"测试环境太慢所以放宽测试"——它是**产品行为**：sidecar 起不来
// 等于该 skill 整体不可用。给足时间是正确取舍；判断"真的挂了"应由进程
// 退出信号负责（readLoop 会感知），而不是由引导耗时猜。
//
// 30s 的取法：比 systemd 的 TimeoutStopSec(25s) 宽，比用户可感知的
// "卡住"阈值(约 30s)持平——再长就该让用户看到明确失败。
const sidecarReadyTimeout = 30 * time.Second

func (s *sidecarProcess) waitReady() error {
	done := make(chan error, 1)
	go func() {
		_, err := s.call("ping", nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(sidecarReadyTimeout):
		return fmt.Errorf("sidecar %s ping timeout（%v 内未响应；慢机器上 node 冷启动可能更久，"+
			"若持续失败请检查 node 是否可用）", s.name, sidecarReadyTimeout)
	}
}

// waitPluginActive 轮询 plugin/status，直到 ACTIVE（成功）、FAILED
// （插件坏了，大声失败）或超时。host.js 自身对 PENDING 也有 30s 退出，
// 两边都不让"装上了但没反应"静默存在。
func (s *sidecarProcess) waitPluginActive(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		data, err := s.callWithTimeout("plugin/status", nil, 5*time.Second)
		lastErr = err
		if err == nil && data != nil {
			var st struct {
				Name  string `json:"name"`
				State string `json:"state"`
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &st) == nil {
				switch st.State {
				case "ACTIVE":
					return nil
				case "FAILED":
					return fmt.Errorf("sidecar %s plugin failed: %s", s.name, st.Error)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sidecar %s plugin not ACTIVE within %v (last err: %v)", s.name, timeout, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// writeFrameLocked 在 protoW 上写一帧（调用方必须持有 s.mu，
// 否则并发写会把两帧交错成非法 JSON）。
func (s *sidecarProcess) writeFrameLocked(data []byte) error {
	if _, err := s.protoW.Write(data); err != nil {
		return err
	}
	if _, err := s.protoW.Write([]byte("\n")); err != nil {
		return err
	}
	return s.protoW.Flush()
}

func (s *sidecarProcess) call(method string, params interface{}) ([]byte, error) {
	return s.callWithTimeout(method, params, 30*time.Second)
}

func (s *sidecarProcess) callWithTimeout(method string, params interface{}, timeout time.Duration) ([]byte, error) {
	ch := make(chan []byte, 1)

	s.mu.Lock()
	if s.closed || s.stopped {
		s.mu.Unlock()
		return nil, fmt.Errorf("sidecar %s closed", s.name)
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = &pendingResp{ch: ch}

	req := sidecarRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	if err := s.writeFrameLocked(data); err != nil {
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	select {
	case raw := <-ch:
		if raw == nil {
			return nil, fmt.Errorf("sidecar %s error: nil response", s.name)
		}
		var resp sidecarResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("sidecar %s unmarshal: %w", s.name, err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("sidecar %s error: %s", s.name, resp.Error.Message)
		}
		if resp.Result == nil {
			return nil, nil
		}
		return []byte(*resp.Result), nil
	case <-time.After(timeout):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("sidecar %s call %s timeout", s.name, method)
	}
}

// notify 单向发一帧（无 id、不等回包）：after_toolcall 的观测通知用它，
// 绝不能阻塞 HA 阶段；写失败只记日志。
func (s *sidecarProcess) notify(method string, params interface{}) {
	frame, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stopped {
		return
	}
	if err := s.writeFrameLocked(frame); err != nil {
		log.Printf("[dshplgadapter] sidecar %s notify %s failed: %v", s.name, method, err)
	}
}

func (s *sidecarProcess) ListTools() ([]DSHTool, error) {
	data, err := s.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var result struct {
		Tools []DSHTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (s *sidecarProcess) CallTool(name string, args map[string]interface{}) (string, error) {
	data, err := s.call("tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", nil
	}
	var result DSHCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	var sb string
	for _, c := range result.Content {
		if c.Type == "text" {
			sb += c.Text
		}
	}
	return sb, nil
}

func (s *sidecarProcess) CallProvider(providerType string, args map[string]interface{}) (string, error) {
	data, err := s.call("provider/call", map[string]interface{}{
		"type":   providerType,
		"action": "execute",
		"args":   args,
	})
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", nil
	}
	var result DSHCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	var sb string
	for _, c := range result.Content {
		if c.Type == "text" {
			sb += c.Text
		}
	}
	return sb, nil
}

// PluginStatus 是 host.js plugin/status 的回包。
type PluginStatus struct {
	Name  string   `json:"name"`
	State string   `json:"state"`
	Error string   `json:"error"`
	Tools []string `json:"tools"`
	Hooks []string `json:"hooks"`
}

func (s *sidecarProcess) Status() (*PluginStatus, error) {
	data, err := s.call("plugin/status", nil)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var st PluginStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Dispose 让 host.js 释放插件 fiber（ctx.effect 清理函数按注册逆序跑，
// 实测 Cordis dispose 即逆序，见 TestEffectsRunInReverseOnStop）。
// 随后调用方仍需 Close() 杀进程。
func (s *sidecarProcess) Dispose() error {
	_, err := s.callWithTimeout("plugin/dispose", nil, 10*time.Second)
	return err
}

// Reload 让 host.js 释放旧 fiber 后重新 import 插件（带 cache-bust），
// 工具随注册通知重新进表（同名覆盖；SDK 1.4.0 没有工具注销接口，
// v2 删掉的工具会在 HA 表里残留到重启，见 README 限制）。
func (s *sidecarProcess) Reload() (*PluginStatus, error) {
	data, err := s.callWithTimeout("plugin/reload", nil, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var st PluginStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *sidecarProcess) Close() {
	s.mu.Lock()
	if s.closed || s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()

	// 先杀进程（子进程 fd 3 随之关闭），然后关父端 socket，
	// reader 的 Scan() 会 EOF 退出。
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}
	if s.protoConn != nil {
		s.protoConn.Close()
	}

	// 等 reader 循环结束
	s.readerWg.Wait()

	s.mu.Lock()
	for id, pr := range s.pending {
		pr.closeOnce()
		delete(s.pending, id)
	}
	s.mu.Unlock()

	log.Printf("[dshplgadapter] sidecar %s stopped", s.name)
}
