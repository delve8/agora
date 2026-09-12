// Package sessionhost implements the process boundary that owns one managed
// Agent session. It intentionally has no dependency on the Agora Server or
// Daemon: a Host can keep its child and PTY alive while its controller exits.
package sessionhost

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/delve8/agora/internal/terminal"
)

const MetadataVersion = 1

type Config struct {
	HostID            string   `json:"host_id"`
	SessionID         string   `json:"session_id"`
	CoordinationID    string   `json:"coordination_id,omitempty"`
	DaemonID          string   `json:"daemon_id,omitempty"`
	Agent             string   `json:"agent"`
	AgentSessionID    string   `json:"agent_session_id,omitempty"`
	Workspace         string   `json:"workspace"`
	DisplayName       string   `json:"display_name,omitempty"`
	DisplayNameSource string   `json:"display_name_source,omitempty"`
	HistoryPath       string   `json:"history_path,omitempty"`
	Command           []string `json:"command"`
	Env               []string `json:"env,omitempty"`
	RuntimeDir        string   `json:"runtime_dir"`
}

type Metadata struct {
	SchemaVersion     int       `json:"schema_version"`
	HostID            string    `json:"host_id"`
	SessionID         string    `json:"session_id"`
	CoordinationID    string    `json:"coordination_id,omitempty"`
	DaemonID          string    `json:"daemon_id,omitempty"`
	Agent             string    `json:"agent"`
	AgentSessionID    string    `json:"agent_session_id,omitempty"`
	Workspace         string    `json:"workspace"`
	DisplayName       string    `json:"display_name,omitempty"`
	DisplayNameSource string    `json:"display_name_source,omitempty"`
	HistoryPath       string    `json:"history_path,omitempty"`
	HostPID           int       `json:"host_pid"`
	HostStartedAt     time.Time `json:"host_started_at"`
	AgentPID          int       `json:"agent_pid"`
	AgentStartedAt    time.Time `json:"agent_started_at"`
	ControlSocket     string    `json:"control_socket"`
	AttachSocket      string    `json:"attach_socket,omitempty"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	TokenFile         string    `json:"token_file"`
}

type request struct {
	Version   int             `json:"version"`
	RequestID string          `json:"request_id,omitempty"`
	Token     string          `json:"token,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type response struct {
	Version   int    `json:"version"`
	RequestID string `json:"request_id,omitempty"`
	Type      string `json:"type"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Payload   any    `json:"payload,omitempty"`
}

type statePayload struct {
	Metadata
	PID int `json:"pid"`
}

type rebindPayload struct {
	SessionID      string `json:"session_id,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	HistoryPath    string `json:"history_path,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
}

type Host struct {
	config      Config
	meta        Metadata
	cmd         *exec.Cmd
	pty         *os.File
	control     net.Listener
	attach      net.Listener
	controlPath string
	attachPath  string
	token       string
	observation terminal.Emulator
	events      map[net.Conn]struct{}

	mu       sync.Mutex
	inputMu  sync.Mutex
	clients  map[net.Conn]struct{}
	input    *inputObserver
	stopping bool
	closed   bool
	done     chan struct{}
}

// New starts no process. Run starts the configured Agent and blocks until the
// child exits or Stop is called.
func New(config Config) (*Host, error) {
	if strings.TrimSpace(config.HostID) == "" {
		config.HostID = newID()
	}
	if strings.TrimSpace(config.SessionID) == "" || strings.TrimSpace(config.Agent) == "" {
		return nil, errors.New("host session_id and agent are required")
	}
	if len(config.Command) == 0 || strings.TrimSpace(config.Command[0]) == "" {
		return nil, errors.New("host command is required")
	}
	if strings.TrimSpace(config.Workspace) == "" {
		return nil, errors.New("host workspace is required")
	}
	workspace, err := filepath.Abs(config.Workspace)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("host workspace must be an existing directory")
	}
	config.Workspace = workspace
	if config.RuntimeDir == "" {
		home, _ := os.UserHomeDir()
		config.RuntimeDir = filepath.Join(home, ".agora", "runtime", "sessions", config.HostID)
	}
	if err := os.MkdirAll(config.RuntimeDir, 0o700); err != nil {
		return nil, err
	}
	return &Host{config: config, clients: make(map[net.Conn]struct{}), events: make(map[net.Conn]struct{}), done: make(chan struct{})}, nil
}

// Run owns the child process and all local sockets until the Agent exits.
func (h *Host) Run() error {
	if err := h.prepareSockets(); err != nil {
		return err
	}
	cmd := exec.Command(h.config.Command[0], h.config.Command[1:]...)
	cmd.Dir = h.config.Workspace
	env := h.config.Env
	if env == nil {
		env = os.Environ()
	}
	// The injected Agent extension identifies its Host through this variable.
	// The Host ID is stable across rebinds, unlike the canonical Session ID.
	cmd.Env = withEnv(env, "AGORA_HOST_ID", h.config.HostID)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: terminal.DefaultCols, Rows: terminal.DefaultRows})
	if err != nil {
		h.cleanupFiles()
		return fmt.Errorf("start agent: %w", err)
	}
	h.mu.Lock()
	h.cmd, h.pty = cmd, master
	now := time.Now().UTC()
	h.meta.AgentPID = cmd.Process.Pid
	h.meta.AgentStartedAt = now
	h.meta.State = "running"
	h.meta.UpdatedAt = now
	meta := h.meta
	h.mu.Unlock()
	if err := writeMetadata(filepath.Join(h.config.RuntimeDir, "metadata.json"), meta); err != nil {
		_ = cmd.Process.Kill()
		_ = master.Close()
		h.cleanupFiles()
		return err
	}

	go h.readOutput()
	go h.serveControl()
	go h.serveAttach()
	waitErr := cmd.Wait()

	h.mu.Lock()
	h.stopping = true
	h.meta.State = "exited"
	h.meta.UpdatedAt = time.Now().UTC()
	h.mu.Unlock()
	_ = waitErr // the exit code is available through the final state/metadata log
	h.close()
	return waitErr
}

// SocketDir is where Host control and attach sockets live. It is a short path
// because Unix socket names are length limited, and it is shared so a Daemon can
// clean up sockets left behind by a Host that was killed.
func SocketDir() string {
	return filepath.Join(os.TempDir(), "agora-host")
}

func (h *Host) prepareSockets() error {
	// Unix-domain socket path limits are small on macOS. Keep the long-lived
	// metadata under ~/.agora, but place the sockets in a short system temp
	// path and remove them explicitly during cleanup.
	socketDir := SocketDir()
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return err
	}
	controlPath := filepath.Join(socketDir, h.config.HostID+"-c.sock")
	attachPath := filepath.Join(socketDir, h.config.HostID+"-a.sock")
	for _, path := range []string{controlPath, attachPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	control, err := net.Listen("unix", controlPath)
	if err != nil {
		return fmt.Errorf("listen control socket: %w", err)
	}
	attach, err := net.Listen("unix", attachPath)
	if err != nil {
		_ = control.Close()
		return fmt.Errorf("listen attach socket: %w", err)
	}
	_ = os.Chmod(controlPath, 0o600)
	_ = os.Chmod(attachPath, 0o600)
	token, err := randomToken()
	if err != nil {
		_ = control.Close()
		_ = attach.Close()
		return err
	}
	tokenPath := filepath.Join(h.config.RuntimeDir, "token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		_ = control.Close()
		_ = attach.Close()
		return err
	}
	now := time.Now().UTC()
	h.mu.Lock()
	h.control, h.attach, h.controlPath, h.attachPath, h.token = control, attach, controlPath, attachPath, token
	h.observation = terminal.NewVT10x(terminal.DefaultCols, terminal.DefaultRows)
	h.input = &inputObserver{onLine: h.broadcastInput}
	h.meta = Metadata{SchemaVersion: MetadataVersion, HostID: h.config.HostID, SessionID: h.config.SessionID, CoordinationID: h.config.CoordinationID, DaemonID: h.config.DaemonID, Agent: h.config.Agent, AgentSessionID: h.config.AgentSessionID, Workspace: h.config.Workspace, DisplayName: h.config.DisplayName, DisplayNameSource: h.config.DisplayNameSource, HistoryPath: h.config.HistoryPath, HostPID: os.Getpid(), HostStartedAt: now, ControlSocket: controlPath, AttachSocket: attachPath, State: "starting", CreatedAt: now, UpdatedAt: now, TokenFile: tokenPath}
	meta := h.meta
	h.mu.Unlock()
	return writeMetadata(filepath.Join(h.config.RuntimeDir, "metadata.json"), meta)
}

func (h *Host) readOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := h.pty.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			h.mu.Lock()
			if h.observation != nil {
				_ = h.observation.Write(data)
			}
			clients := make([]net.Conn, 0, len(h.clients))
			for conn := range h.clients {
				clients = append(clients, conn)
			}
			h.mu.Unlock()
			for _, conn := range clients {
				_, _ = conn.Write(data)
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *Host) serveAttach() {
	for {
		conn, err := h.attach.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			_ = conn.Close()
			return
		}
		h.clients[conn] = struct{}{}
		// Replay the current screen before releasing the lock: readOutput takes
		// the same lock to hand bytes to the clients, so a client that attaches
		// to a session which already painted sees the screen first and live
		// output after it, with nothing lost in between.
		if h.observation != nil {
			if screen := h.observation.Snapshot().Render(); screen != "" {
				_, _ = conn.Write([]byte(screen))
			}
		}
		h.mu.Unlock()
		go func() {
			defer func() {
				h.mu.Lock()
				delete(h.clients, conn)
				h.mu.Unlock()
				_ = conn.Close()
			}()
			_, _ = io.Copy(inputWriter{host: h}, terminal.NewAttachStream(conn, h.resize))
		}()
	}
}

func (h *Host) serveControl() {
	for {
		conn, err := h.control.Accept()
		if err != nil {
			return
		}
		go h.handleControl(conn)
	}
}

func (h *Host) handleControl(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(bufio.NewReader(conn))
	encoder := json.NewEncoder(conn)
	var req request
	if err := decoder.Decode(&req); err != nil {
		return
	}
	if req.Version != 0 && req.Version != MetadataVersion {
		_ = encoder.Encode(response{Version: MetadataVersion, RequestID: req.RequestID, Type: req.Type + ".result", Error: "unsupported protocol version"})
		return
	}
	h.mu.Lock()
	token := h.token
	h.mu.Unlock()
	if token == "" || req.Token != token {
		_ = encoder.Encode(response{Version: MetadataVersion, RequestID: req.RequestID, Type: req.Type + ".result", Error: "invalid host token"})
		return
	}
	var result response
	result.Version, result.RequestID, result.Type, result.OK = MetadataVersion, req.RequestID, req.Type+".result", true
	switch req.Type {
	case "ping":
	case "state", "hello":
		h.mu.Lock()
		result.Payload = statePayload{Metadata: h.meta, PID: h.meta.AgentPID}
		h.mu.Unlock()
	case "subscribe":
		h.mu.Lock()
		h.events[conn] = struct{}{}
		h.mu.Unlock()
		_ = encoder.Encode(result)
		<-h.done
		return
	case "snapshot":
		h.mu.Lock()
		if h.observation == nil {
			result.OK, result.Error = false, "terminal snapshot is unavailable"
		} else {
			result.Payload = h.observation.Snapshot()
		}
		h.mu.Unlock()
	case "input":
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(req.Payload, &payload); err != nil || payload.Content == "" {
			result.OK, result.Error = false, "input content is required"
			break
		}
		if h.pty == nil {
			result.OK, result.Error = false, "agent is not ready"
			break
		}
		if _, err := io.WriteString(h.pty, payload.Content+"\r"); err != nil {
			result.OK, result.Error = false, err.Error()
		} else {
			h.processInput([]byte(payload.Content + "\r"))
		}
	case "rebind":
		var payload rebindPayload
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			result.OK, result.Error = false, err.Error()
			break
		}
		h.mu.Lock()
		if payload.SessionID != "" {
			h.meta.SessionID = payload.SessionID
		}
		if payload.AgentSessionID != "" {
			h.meta.AgentSessionID = payload.AgentSessionID
		}
		// A rebind defines the binding, so an empty path clears a stale one
		// (for example when the Agent moved to a session with no transcript yet).
		h.meta.HistoryPath = payload.HistoryPath
		if payload.DisplayName != "" {
			h.meta.DisplayName = payload.DisplayName
		}
		h.meta.UpdatedAt = time.Now().UTC()
		meta := h.meta
		h.mu.Unlock()
		if err := writeMetadata(filepath.Join(h.config.RuntimeDir, "metadata.json"), meta); err != nil {
			result.OK, result.Error = false, err.Error()
		} else {
			result.Payload = statePayload{Metadata: meta, PID: meta.AgentPID}
		}
	case "stop", "shutdown":
		h.mu.Lock()
		h.stopping = true
		cmd := h.cmd
		h.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				result.OK, result.Error = false, err.Error()
			}
		}
	default:
		result.OK, result.Error = false, "unknown host command"
	}
	if result.Error != "" {
		result.OK = false
	}
	_ = encoder.Encode(result)
}

// withEnv sets key in an environment slice, replacing an existing entry.
func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, prefix+value)
}

func (h *Host) resize(cols, rows int) {
	cols, rows, ok := terminal.ClampSize(cols, rows)
	if !ok {
		return
	}
	h.mu.Lock()
	master, observation := h.pty, h.observation
	h.mu.Unlock()
	if master != nil {
		_ = pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
	if observation != nil {
		observation.Resize(cols, rows)
	}
}

func (h *Host) processInput(data []byte) {
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	if h.input != nil {
		h.input.process(data)
	}
}

func (h *Host) broadcastInput(line string) {
	h.mu.Lock()
	connections := make([]net.Conn, 0, len(h.events))
	for conn := range h.events {
		connections = append(connections, conn)
	}
	h.mu.Unlock()
	message := response{Version: MetadataVersion, Type: "input", OK: true, Payload: map[string]string{"content": line}}
	for _, conn := range connections {
		if err := json.NewEncoder(conn).Encode(message); err != nil {
			h.mu.Lock()
			delete(h.events, conn)
			h.mu.Unlock()
			_ = conn.Close()
		}
	}
}

type inputWriter struct{ host *Host }

func (w inputWriter) Write(data []byte) (int, error) {
	if w.host == nil || w.host.pty == nil {
		return 0, errors.New("agent is not ready")
	}
	n, err := w.host.pty.Write(data)
	if err == nil {
		w.host.processInput(data[:n])
	}
	return n, err
}

func (h *Host) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	control, attach, master := h.control, h.attach, h.pty
	for conn := range h.clients {
		_ = conn.Close()
	}
	for conn := range h.events {
		_ = conn.Close()
	}
	h.clients = make(map[net.Conn]struct{})
	h.events = make(map[net.Conn]struct{})
	h.mu.Unlock()
	if control != nil {
		_ = control.Close()
	}
	if attach != nil {
		_ = attach.Close()
	}
	if master != nil {
		_ = master.Close()
	}
	close(h.done)
	h.cleanupFiles()
}

func (h *Host) cleanupFiles() {
	controlPath, attachPath := h.controlPath, h.attachPath
	if controlPath != "" {
		_ = os.Remove(controlPath)
	}
	if attachPath != "" {
		_ = os.Remove(attachPath)
	}
	if h.control != nil {
		_ = h.control.Close()
	}
	if h.attach != nil {
		_ = h.attach.Close()
	}
	_ = os.RemoveAll(h.config.RuntimeDir)
}

func writeMetadata(path string, value Metadata) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newID() string {
	value, err := randomToken()
	if err != nil {
		return fmt.Sprintf("host-%d", time.Now().UnixNano())
	}
	return "host-" + value[:24]
}

// LoadConfig reads a JSON launch request used by the standalone
// `agora session-host --config` entry point. The config contains only launch
// metadata and provider command arguments; it never contains transcript data.
func LoadConfig(path string) (Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var value Config
	if err := json.Unmarshal(body, &value); err != nil {
		return Config{}, err
	}
	return value, nil
}

// LoadMetadata reads a Host metadata file for a reconnecting Daemon.
func LoadMetadata(path string) (Metadata, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, err
	}
	var value Metadata
	if err := json.Unmarshal(body, &value); err != nil {
		return Metadata{}, err
	}
	if value.SchemaVersion != MetadataVersion || value.HostID == "" || value.ControlSocket == "" {
		return Metadata{}, errors.New("invalid session-host metadata")
	}
	return value, nil
}
