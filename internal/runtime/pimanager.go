package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/terminal"
)

const (
	defaultPiBinary   = "pi"
	defaultPiProvider = "anthropic"
	piEventBuffer     = 128
)

type PiConfig struct {
	Binary     string
	Provider   string
	Model      string
	SessionDir string
}

type PiEvent struct {
	Event    event.Event
	Response bool
	Done     bool
	Raw      []byte
}

type PiProcess struct {
	AgoraID    string
	PID        int
	NativeID   string
	Workspace  string
	Events     <-chan PiEvent
	Process    *os.Process
	SocketPath string

	observation  *ptyObservation
	master       *os.File
	listener     net.Listener
	cmd          *exec.Cmd
	mu           sync.Mutex
	closed       bool
	done         chan struct{}
	inputHandler func(string, string)
	replayMu     sync.Mutex
	replay       [][]byte
}

type PiManager struct {
	config           PiConfig
	binaryErr        error
	mu               sync.Mutex
	processes        map[string]*PiProcess
	intentionalStops map[string]bool
	onExit           func(PiExit)
	inputHandler     func(string, string)
}

type PiExit struct {
	AgoraID     string
	NativeID    string
	ExitCode    int
	Err         error
	Intentional bool
}

// PiManager runs Pi's normal interactive TUI under an Agora-owned PTY. Pi's
// RPC mode is deliberately not used here: RPC has pipe-oriented JSON output
// and cannot provide the native terminal UI users expect from `pi`.
func NewPiManager(config PiConfig) *PiManager {
	// Keep direct/local construction consistent with the daemon command. The
	// explicit PiConfig value wins, then the conventional environment names,
	// and finally the safe built-in defaults. If `pi` resolves to the generic
	// PATH wrapper, skip it and select the real Pi executable later in PATH.
	if config.Binary == "" {
		config.Binary = firstEnvValue("AGORA_PI_BINARY", "PI_BINARY")
	}
	var binaryErr error
	if resolved, err := resolveAgentBinary(config.Binary, defaultPiBinary); err == nil {
		config.Binary = resolved
	} else {
		binaryErr = err
		if config.Binary == "" {
			config.Binary = defaultPiBinary
		}
	}
	if config.Provider == "" {
		config.Provider = firstEnvValue("AGORA_PI_PROVIDER", "PI_PROVIDER")
	}
	if config.Provider == "" {
		config.Provider = defaultPiProvider
	}
	if config.Model == "" {
		config.Model = firstEnvValue("AGORA_PI_MODEL", "PI_MODEL")
	}
	if config.SessionDir == "" {
		config.SessionDir = firstEnvValue("AGORA_PI_SESSION_DIR", "PI_SESSION_DIR")
	}
	return &PiManager{config: config, binaryErr: binaryErr, processes: make(map[string]*PiProcess), intentionalStops: make(map[string]bool)}
}

func firstEnvValue(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func (m *PiManager) SetExitHandler(handler func(PiExit)) {
	m.mu.Lock()
	m.onExit = handler
	m.mu.Unlock()
}

// SetInputHandler observes submitted terminal lines without changing the bytes
// sent to Pi. It is used by the runtime to detect provider context commands.
func (m *PiManager) SetInputHandler(handler func(string, string)) {
	m.mu.Lock()
	m.inputHandler = handler
	m.mu.Unlock()
}
func (m *PiManager) SessionDir() string { return m.config.SessionDir }

// Command returns the exact provider command used for a managed Pi session.
// Session Host uses it so the Agent process is created outside the Daemon.
func (m *PiManager) Command(nativeID, historyPath string, agentArgs []string) []string {
	args := []string{m.config.Binary}
	if len(agentArgs) > 0 {
		return append(args, agentArgs...)
	}
	if m.config.Provider != "" {
		args = append(args, "--provider", m.config.Provider)
	}
	if m.config.Model != "" {
		args = append(args, "--model", m.config.Model)
	}
	if m.config.SessionDir != "" {
		args = append(args, "--session-dir", m.config.SessionDir)
	}
	if historyPath != "" {
		args = append(args, "--session", historyPath)
	} else if nativeID != "" {
		args = append(args, "--session-id", nativeID)
	}
	return args
}
func (m *PiManager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processes[id] != nil
}
func (m *PiManager) Start(id, workspace, nativeID string) (*PiProcess, error) {
	return m.StartWithArgs(id, workspace, nativeID, nil)
}

func (m *PiManager) StartWithArgs(id, workspace, nativeID string, agentArgs []string) (*PiProcess, error) {
	// The argument slice is owned by Pi and is passed through unchanged. We may
	// inspect an explicit session selector only for Agora's local bookkeeping;
	// it is never removed, reordered, or rewritten in the child argv.
	if nativeID == "" {
		nativeID = m.nativeIDFromArgs(agentArgs)
	}
	return m.start(id, workspace, nativeID, "", agentArgs)
}

func (m *PiManager) nativeIDFromArgs(args []string) string {
	candidate := ""
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--session" || arg == "--session-id" {
			if index+1 < len(args) {
				candidate = strings.TrimSpace(args[index+1])
			}
			index++
			continue
		}
		for _, flag := range []string{"--session=", "--session-id="} {
			if strings.HasPrefix(arg, flag) {
				candidate = strings.TrimSpace(strings.TrimPrefix(arg, flag))
			}
		}
	}
	if candidate == "" {
		return ""
	}
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		if nativeID := piSessionIDFromFile(candidate); nativeID != "" {
			return nativeID
		}
	}
	if strings.ContainsAny(candidate, `/\\`) {
		return ""
	}
	root := m.config.SessionDir
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".pi", "agent", "sessions")
	}
	if values, err := adapter.NewPiHistoryCatalog("", root).List(context.Background()); err == nil {
		for _, value := range values {
			if value.SessionID == candidate || (len(candidate) >= 8 && strings.HasPrefix(value.SessionID, candidate)) {
				return value.SessionID
			}
		}
	}
	// Keep the supplied value as a best-effort identity. Pi itself remains the
	// authority for whether this selector is valid.
	return candidate
}

func piSessionIDFromFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	var raw map[string]any
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &raw) != nil {
		return ""
	}
	for _, key := range []string{"id", "sessionId", "session_id"} {
		if value, ok := raw[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (m *PiManager) start(id, workspace, nativeID, historyPath string, agentArgs []string) (*PiProcess, error) {
	if m.binaryErr != nil {
		return nil, m.binaryErr
	}
	m.mu.Lock()
	if existing := m.processes[id]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	args := m.Command(nativeID, historyPath, agentArgs)
	// Command preserves caller-owned arguments and provider defaults exactly.
	log.Printf("agora: starting Pi session %s in %s: %s", id, workspace, strings.Join(args, " "))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = workspace
	// A daemon may itself have been launched from inside Pi. Do not leak the
	// parent's session markers into the child: they can make Pi reopen the
	// parent session or treat the child as a nested/headless agent. Provider
	// credentials and normal terminal variables are intentionally preserved.
	cmd.Env = cleanPiEnv()
	// Pi's TUI exits when the PTY starts with a 0x0 window size. Claude's
	// manager also uses a fixed initial viewport; resize the Pi PTY before the
	// process starts so its first terminal query returns a usable size.
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: defaultPTYCols, Rows: defaultPTYRows})
	if err != nil {
		return nil, fmt.Errorf("start Pi TUI: %w", err)
	}
	listener, err := net.Listen("unix", filepath.Join(os.TempDir(), fmt.Sprintf("agora-pi-%d.sock", time.Now().UnixNano())))
	if err != nil {
		_ = master.Close()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("listen Pi attach socket: %w", err)
	}
	m.mu.Lock()
	inputHandler := m.inputHandler
	m.mu.Unlock()
	process := &PiProcess{AgoraID: id, PID: cmd.Process.Pid, NativeID: nativeID, Workspace: workspace, Process: cmd.Process, SocketPath: listener.Addr().String(), observation: newPTYObservation(), master: master, listener: listener, cmd: cmd, done: make(chan struct{}), inputHandler: inputHandler}
	events := make(chan PiEvent, piEventBuffer)
	process.Events = events
	m.mu.Lock()
	m.processes[id] = process
	onExit := m.onExit
	m.mu.Unlock()
	go m.serveAttach(process)
	go m.discoverNativeID(process, historyPath)
	go func() {
		waitErr := cmd.Wait()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		m.mu.Lock()
		process.mu.Lock()
		currentID := process.AgoraID
		process.closed = true
		process.mu.Unlock()
		delete(m.processes, currentID)
		intentional := m.intentionalStops[currentID]
		delete(m.intentionalStops, currentID)
		m.mu.Unlock()
		close(process.done)
		_ = listener.Close()
		_ = master.Close()
		close(events)
		log.Printf("agora: Pi session %s process exited pid=%d code=%d err=%v", currentID, cmd.Process.Pid, code, waitErr)
		if onExit != nil {
			onExit(PiExit{AgoraID: currentID, NativeID: process.nativeID(), ExitCode: code, Err: waitErr, Intentional: intentional})
		}
	}()
	return process, nil
}

func (m *PiManager) discoverNativeID(process *PiProcess, historyPath string) {
	if process.nativeID() != "" {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if historyPath != "" {
			if summary, err := os.Stat(historyPath); err == nil && summary.Size() >= 0 {
				if values, err := adapter.NewPiHistoryCatalog("", filepath.Dir(filepath.Dir(historyPath))).List(context.Background()); err == nil {
					for _, value := range values {
						if value.Path == historyPath && value.SessionID != "" {
							process.setNativeID(value.SessionID)
							return
						}
					}
				}
			}
		} else {
			root := m.config.SessionDir
			if root == "" {
				home, _ := os.UserHomeDir()
				root = filepath.Join(home, ".pi", "agent", "sessions")
			}
			if values, err := adapter.NewPiHistoryCatalog("", root).List(context.Background()); err == nil {
				var best adapter.PiHistorySummary
				for _, value := range values {
					if value.Workspace == process.Workspace && value.ModifiedAt.After(time.Now().Add(-2*time.Minute)) && value.ModifiedAt.After(best.ModifiedAt) {
						best = value
					}
				}
				if best.SessionID != "" {
					process.setNativeID(best.SessionID)
					return
				}
			}
		}
		select {
		case <-process.done:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m *PiManager) Rekey(oldID, newID string) error {
	return m.Rebind(oldID, newID, "")
}

// Rebind changes both the Agora process key and, when supplied, the native Pi
// context identity discovered after an in-process /resume.
func (m *PiManager) Rebind(oldID, newID, nativeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.processes[oldID]
	if process == nil {
		return fmt.Errorf("Pi session %s is not running", oldID)
	}
	if oldID != newID {
		if _, ok := m.processes[newID]; ok {
			return fmt.Errorf("Pi session %s already exists", newID)
		}
		delete(m.processes, oldID)
	}
	process.mu.Lock()
	process.AgoraID = newID
	if nativeID != "" {
		process.NativeID = nativeID
	}
	process.mu.Unlock()
	m.processes[newID] = process
	return nil
}

func (m *PiManager) Resume(id, workspace, nativeID, historyPath string) (*PiProcess, error) {
	if nativeID == "" {
		return nil, errors.New("Pi session id is required")
	}
	if historyPath == "" {
		return nil, errors.New("Pi session history path is required")
	}
	return m.start(id, workspace, nativeID, historyPath, nil)
}
func (m *PiManager) NativeID(id string) string {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return ""
	}
	return p.nativeID()
}
func (m *PiManager) Events(id string) (<-chan PiEvent, error) {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("Pi session %s is not running", id)
	}
	return p.Events, nil
}
func (m *PiManager) Send(ctx context.Context, id, content string) error {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return fmt.Errorf("Pi session %s is not running", id)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("Pi process is closed")
	}
	select {
	case <-ctx.Done():
		p.mu.Unlock()
		return ctx.Err()
	default:
	}
	_, err := io.WriteString(p.master, content+"\r")
	handler := p.inputHandler
	agoraID := p.AgoraID
	p.mu.Unlock()
	if err == nil && handler != nil && strings.TrimSpace(content) != "" {
		handler(agoraID, content)
	}
	return err
}
func (m *PiManager) Interrupt(ctx context.Context, id string) error { return m.Send(ctx, id, "\x03") }
func (m *PiManager) Stop(id string) error {
	m.mu.Lock()
	p := m.processes[id]
	if p != nil {
		m.intentionalStops[id] = true
	}
	m.mu.Unlock()
	if p == nil || p.Process == nil {
		return fmt.Errorf("Pi session %s is not running", id)
	}
	if err := p.Process.Signal(syscall.SIGTERM); err != nil {
		m.mu.Lock()
		delete(m.intentionalStops, id)
		m.mu.Unlock()
		return err
	}
	return nil
}
func (m *PiManager) AttachAddr(id string) (string, error) {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil {
		return "", fmt.Errorf("Pi session %s is not running", id)
	}
	return p.SocketPath, nil
}

func (m *PiManager) Snapshot(id string) (terminal.Snapshot, error) {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil || p.observation == nil {
		return terminal.Snapshot{}, fmt.Errorf("Pi session %s is not running", id)
	}
	return p.observation.snapshot(), nil
}
func (m *PiManager) Close() {
	m.mu.Lock()
	values := make([]*PiProcess, 0, len(m.processes))
	for _, p := range m.processes {
		values = append(values, p)
	}
	m.mu.Unlock()
	for _, p := range values {
		_ = p.Process.Signal(syscall.SIGTERM)
	}
}

func (p *PiProcess) nativeID() string { p.mu.Lock(); defer p.mu.Unlock(); return p.NativeID }
func (p *PiProcess) setNativeID(id string) {
	p.mu.Lock()
	p.NativeID = id
	p.mu.Unlock()
}
func (p *PiProcess) recordOutput(data []byte) {
	p.replayMu.Lock()
	p.replay = append(p.replay, append([]byte(nil), data...))
	if len(p.replay) > 64 {
		p.replay = p.replay[len(p.replay)-64:]
	}
	p.replayMu.Unlock()
}
func (p *PiProcess) replayOutput() [][]byte {
	p.replayMu.Lock()
	defer p.replayMu.Unlock()
	values := make([][]byte, len(p.replay))
	for i, frame := range p.replay {
		values[i] = append([]byte(nil), frame...)
	}
	return values
}

func cleanPiEnv() []string {
	blocked := map[string]bool{
		"PI_SESSION_ID":      true,
		"PI_SESSION_FILE":    true,
		"PI_CODING_AGENT":    true,
		"PI_PROVIDER":        true,
		"PI_MODEL":           true,
		"PI_REASONING_LEVEL": true,
		"AI_AGENT":           true,
	}
	values := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if !ok || !blocked[key] {
			values = append(values, item)
		}
	}
	return values
}

// serveAttach keeps the PTY drained and broadcasts output to all attached
// clients. The first client receives a small replay of the current screen.
func (m *PiManager) serveAttach(p *PiProcess) {
	type client struct {
		conn net.Conn
		send chan []byte
	}
	var mu sync.Mutex
	clients := make(map[*client]bool)
	broadcast := func(data []byte) {
		mu.Lock()
		defer mu.Unlock()
		for c := range clients {
			select {
			case c.send <- append([]byte(nil), data...):
			default:
			}
		}
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.master.Read(buf)
			if n > 0 {
				p.recordOutput(buf[:n])
				if p.observation != nil {
					// Pi currently has no provider-independent approval protocol;
					// retain the terminal screen for observation without inferring
					// user intervention from arbitrary TUI text.
					p.observation.record(buf[:n])
				}
				broadcast(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		c := &client{conn: conn, send: make(chan []byte, 256)}
		mu.Lock()
		clients[c] = true
		for _, frame := range p.replayOutput() {
			select {
			case c.send <- frame:
			default:
			}
		}
		mu.Unlock()
		go func() {
			defer func() { mu.Lock(); delete(clients, c); mu.Unlock(); close(c.send); _ = conn.Close() }()
			go func() {
				for data := range c.send {
					if _, err := conn.Write(data); err != nil {
						return
					}
				}
			}()
			copyTerminalInput(p.master, conn, func() string { p.mu.Lock(); defer p.mu.Unlock(); return p.AgoraID }, p.inputHandler)
		}()
	}
}
