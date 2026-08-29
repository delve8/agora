package runtime

import (
	"context"
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

	observation *ptyObservation
	master      *os.File
	listener    net.Listener
	cmd         *exec.Cmd
	mu          sync.Mutex
	closed      bool
	done        chan struct{}
	replayMu    sync.Mutex
	replay      [][]byte
}

type PiManager struct {
	config    PiConfig
	mu        sync.Mutex
	processes map[string]*PiProcess
	onExit    func(PiExit)
}

type PiExit struct {
	AgoraID  string
	NativeID string
	ExitCode int
	Err      error
}

// PiManager runs Pi's normal interactive TUI under an Agora-owned PTY. Pi's
// RPC mode is deliberately not used here: RPC has pipe-oriented JSON output
// and cannot provide the native terminal UI users expect from `pi`.
func NewPiManager(config PiConfig) *PiManager {
	// Keep direct/local construction consistent with the daemon command. The
	// explicit PiConfig value wins, then the conventional environment names,
	// and finally the safe built-in defaults.
	if config.Binary == "" {
		config.Binary = firstEnvValue("AGORA_PI_BINARY", "PI_BINARY")
	}
	if config.Binary == "" {
		config.Binary = defaultPiBinary
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
	return &PiManager{config: config, processes: make(map[string]*PiProcess)}
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
func (m *PiManager) SessionDir() string { return m.config.SessionDir }
func (m *PiManager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processes[id] != nil
}
func (m *PiManager) Start(id, workspace, nativeID string) (*PiProcess, error) {
	return m.start(id, workspace, nativeID, "")
}

func (m *PiManager) start(id, workspace, nativeID, historyPath string) (*PiProcess, error) {
	m.mu.Lock()
	if existing := m.processes[id]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	args := []string{m.config.Binary, "--provider", m.config.Provider}
	if m.config.Model != "" {
		args = append(args, "--model", m.config.Model)
	}
	if m.config.SessionDir != "" {
		args = append(args, "--session-dir", m.config.SessionDir)
	}
	if historyPath != "" {
		// Resume with the concrete JSONL path. --resume opens Pi's interactive
		// session picker and is not suitable for a managed process.
		args = append(args, "--session", historyPath)
	} else if nativeID != "" {
		// A fresh session can use a stable native id immediately, avoiding a
		// race between the visible TUI and history catalog discovery.
		args = append(args, "--session-id", nativeID)
	}
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
	process := &PiProcess{AgoraID: id, PID: cmd.Process.Pid, NativeID: nativeID, Workspace: workspace, Process: cmd.Process, SocketPath: listener.Addr().String(), observation: newPTYObservation(), master: master, listener: listener, cmd: cmd, done: make(chan struct{})}
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
		delete(m.processes, id)
		m.mu.Unlock()
		process.mu.Lock()
		process.closed = true
		process.mu.Unlock()
		close(process.done)
		_ = listener.Close()
		_ = master.Close()
		close(events)
		log.Printf("agora: Pi session %s process exited pid=%d code=%d err=%v", id, cmd.Process.Pid, code, waitErr)
		if onExit != nil {
			onExit(PiExit{AgoraID: id, NativeID: process.nativeID(), ExitCode: code, Err: waitErr})
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
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.processes[oldID]
	if process == nil {
		return fmt.Errorf("Pi session %s is not running", oldID)
	}
	if _, ok := m.processes[newID]; ok {
		return fmt.Errorf("Pi session %s already exists", newID)
	}
	delete(m.processes, oldID)
	process.mu.Lock()
	process.AgoraID = newID
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
	return m.start(id, workspace, nativeID, historyPath)
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
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("Pi process is closed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	_, err := io.WriteString(p.master, content+"\r")
	return err
}
func (m *PiManager) Interrupt(ctx context.Context, id string) error { return m.Send(ctx, id, "\x03") }
func (m *PiManager) Stop(id string) error {
	m.mu.Lock()
	p := m.processes[id]
	m.mu.Unlock()
	if p == nil || p.Process == nil {
		return fmt.Errorf("Pi session %s is not running", id)
	}
	return p.Process.Signal(syscall.SIGTERM)
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

func (p *PiProcess) nativeID() string      { p.mu.Lock(); defer p.mu.Unlock(); return p.NativeID }
func (p *PiProcess) setNativeID(id string) { p.mu.Lock(); p.NativeID = id; p.mu.Unlock() }
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
			_, _ = io.Copy(p.master, conn)
		}()
	}
}
