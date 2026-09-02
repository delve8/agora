package runtime

import (
	"context"
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
	"github.com/delve8/agora/internal/terminal"
)

// PTYManager owns Claude Code subprocesses launched under PTYs that Agora
// controls. Each managed session gets a PTY whose master fd stays inside this
// process (macOS /dev/ptmx cannot be re-opened by another process), served to
// `claude-wrapper` / `agora attach` clients over a Unix socket, and written to
// by Input() when a message arrives from the web UI or IM.
type PTYManager struct {
	binary    string
	binaryErr error
	homeDir   string
	socketDir string

	mu               sync.Mutex
	sessions         map[string]*PTYSession // keyed by Agora session id
	intentionalStops map[string]bool
	onExit           func(PTYExit)
	onAttention      func(string, string)
	inputHandler     func(string, string)
}

type PTYExit struct {
	AgoraID       string
	ClaudeSession string
	ExitCode      int
	Err           error
	Intentional   bool
}

type PTYSession struct {
	AgoraID       string
	ClaudeSession string // Claude's own session id (from ~/.claude/sessions after launch)
	Workspace     string
	Process       *os.Process
	Master        *os.File
	Listener      net.Listener
	SocketPath    string
	StartedAt     time.Time

	observation  *ptyObservation
	inputHandler func(string, string)
	mu           sync.Mutex
}

type ptyObservation struct {
	mu       sync.RWMutex
	emulator terminal.Emulator
	frames   []rawFrame
	capacity int
	sequence uint64
	approval bool
}

type rawFrame struct {
	Sequence   uint64
	CapturedAt time.Time
	Data       []byte
}

const (
	defaultPTYCols       = 120
	defaultPTYRows       = 40
	defaultRawFrameLimit = 32
)

func looksLikeApprovalPrompt(snapshot terminal.Snapshot) bool {
	text := strings.ToLower(strings.Join(snapshot.Lines, "\n"))
	return strings.Contains(text, "do you want to allow claude") || (strings.Contains(text, "claude wants to") && strings.Contains(text, "1."))
}

func newPTYObservation() *ptyObservation {
	return &ptyObservation{
		emulator: terminal.NewVT10x(defaultPTYCols, defaultPTYRows),
		capacity: defaultRawFrameLimit,
	}
}

func (o *ptyObservation) record(data []byte) bool {
	copyData := append([]byte(nil), data...)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sequence++
	capturedAt := time.Now().UTC()
	_ = o.emulator.Write(copyData)
	o.frames = append(o.frames, rawFrame{Sequence: o.sequence, CapturedAt: capturedAt, Data: copyData})
	if len(o.frames) > o.capacity {
		o.frames = o.frames[len(o.frames)-o.capacity:]
	}
	approval := looksLikeApprovalPrompt(o.emulator.Snapshot())
	changed := approval && !o.approval
	o.approval = approval
	return changed
}

func (o *ptyObservation) snapshot() terminal.Snapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	value := o.emulator.Snapshot()
	value.Sequence = o.sequence
	return value
}

func NewPTYManager(binary, homeDir string) *PTYManager {
	var binaryErr error
	if resolved, err := resolveAgentBinary(binary, "claude"); err == nil {
		binary = resolved
	} else {
		binaryErr = err
		if strings.TrimSpace(binary) == "" {
			// Preserve the historical constructor contract; Launch reports the
			// useful exec error if the real binary is not installed.
			binary = "claude"
		}
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	return &PTYManager{
		binary:           binary,
		binaryErr:        binaryErr,
		homeDir:          homeDir,
		socketDir:        filepath.Join(os.TempDir(), "agora-pty"),
		sessions:         make(map[string]*PTYSession),
		intentionalStops: make(map[string]bool),
	}
}

func (m *PTYManager) SetExitHandler(handler func(PTYExit)) {
	m.mu.Lock()
	m.onExit = handler
	m.mu.Unlock()
}

func (m *PTYManager) SetAttentionHandler(handler func(string, string)) {
	m.mu.Lock()
	m.onAttention = handler
	m.mu.Unlock()
}

// SetInputHandler observes submitted terminal lines without changing the bytes
// sent to Claude. It is used for provider context-switch detection.
func (m *PTYManager) SetInputHandler(handler func(string, string)) {
	m.mu.Lock()
	m.inputHandler = handler
	m.mu.Unlock()
}

func (m *PTYManager) IsRunning(agoraID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[agoraID] != nil
}

// Launch starts a Claude Code process for the given Agora session under a PTY.
// If claudeSession is non-empty it resumes that conversation (--resume);
// otherwise it starts fresh and lets Claude choose its native session id.
func (m *PTYManager) Command(claudeSession string) []string {
	args := []string{m.binary}
	if claudeSession != "" {
		args = append(args, "--resume", claudeSession)
	}
	return args
}

func (m *PTYManager) FreshCommand(claudeSession string) []string {
	return []string{m.binary, "--session-id", claudeSession}
}

func (m *PTYManager) Launch(agoraID, workspace, claudeSession string) (*PTYSession, error) {
	return m.launch(agoraID, workspace, claudeSession, false)
}

// LaunchWithSessionID starts a fresh Claude conversation with a caller-selected
// native id. This is used by split Daemon creation so the Server can receive a
// canonical session identity immediately instead of waiting for Claude's
// metadata file (which may not be written until after project trust is handled).
func (m *PTYManager) LaunchWithSessionID(agoraID, workspace, sessionID string) (*PTYSession, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("Claude session id is required")
	}
	return m.launch(agoraID, workspace, sessionID, true)
}

func (m *PTYManager) launch(agoraID, workspace, claudeSession string, freshSessionID bool) (*PTYSession, error) {
	if m.binaryErr != nil {
		return nil, m.binaryErr
	}
	m.mu.Lock()
	if existing := m.sessions[agoraID]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	args := []string{m.binary}
	if freshSessionID {
		args = append(args, "--session-id", claudeSession)
	} else if claudeSession != "" {
		args = append(args, "--resume", claudeSession)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = workspace
	cmd.Env = cleanClaudeEnv()

	file, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start claude PTY: %w", err)
	}
	if err := os.MkdirAll(m.socketDir, 0o700); err != nil {
		_ = file.Close()
		return nil, err
	}
	socketPath := filepath.Join(m.socketDir, fmt.Sprintf("sess-%d.sock", time.Now().UnixNano()))
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("listen %s: %w", socketPath, err)
	}

	m.mu.Lock()
	inputHandler := m.inputHandler
	m.mu.Unlock()
	session := &PTYSession{
		AgoraID:       agoraID,
		Workspace:     workspace,
		Process:       cmd.Process,
		Master:        file,
		Listener:      listener,
		SocketPath:    socketPath,
		StartedAt:     time.Now().UTC(),
		ClaudeSession: claudeSession,
		observation:   newPTYObservation(),
		inputHandler:  inputHandler,
	}
	go m.serveAttach(session)

	m.mu.Lock()
	m.sessions[agoraID] = session
	m.mu.Unlock()

	// Read the Claude session id this process records (it appears shortly after
	// launch in ~/.claude/sessions/<pid>.json). A fresh launch may not record it
	// until the workspace trust prompt is accepted, so poll in the background
	// until it appears or the process exits; callers that need the id re-check
	// via ClaudeSessionID.
	if claudeSession == "" {
		metaPath := filepath.Join(m.homeDir, ".claude", "sessions", fmt.Sprintf("%d.json", cmd.Process.Pid))
		go func() {
			for {
				if sid, _, err := adapter.ReadSessionMetadata(metaPath); err == nil && sid != "" {
					m.mu.Lock()
					session.ClaudeSession = sid
					m.mu.Unlock()
					log.Printf("agora: managed session %s has Claude session %s", agoraID, sid)
					return
				}
				if cmd.ProcessState != nil {
					return
				}
				time.Sleep(300 * time.Millisecond)
			}
		}()
	}

	go func() {
		waitErr := cmd.Wait()
		exitCode := 0
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		m.mu.Lock()
		session.mu.Lock()
		currentID := session.AgoraID
		claudeSession := session.ClaudeSession
		session.mu.Unlock()
		delete(m.sessions, currentID)
		intentional := m.intentionalStops[currentID]
		delete(m.intentionalStops, currentID)
		handler := m.onExit
		m.mu.Unlock()
		_ = listener.Close()
		_ = file.Close()
		if handler != nil {
			handler(PTYExit{AgoraID: currentID, ClaudeSession: claudeSession, ExitCode: exitCode, Err: waitErr, Intentional: intentional})
		}
		log.Printf("agora: managed session %s process exited", currentID)
	}()

	return session, nil
}

// Input writes a line into the PTY master, which the Claude TUI reads as typed
// input followed by Enter (\r submits in the Claude TUI; \n alone only moves
// the cursor). Concurrent sends are serialized by the caller (Manager.Send
// holds the per-session active lock).
func (m *PTYManager) Input(agoraID, content string) error {
	m.mu.Lock()
	session := m.sessions[agoraID]
	m.mu.Unlock()
	if session == nil {
		return fmt.Errorf("managed session %s is not running", agoraID)
	}
	_, err := io.WriteString(session.Master, content+"\r")
	if err == nil && session.inputHandler != nil && strings.TrimSpace(content) != "" {
		session.inputHandler(session.AgoraID, content)
	}
	return err
}

func (m *PTYManager) Stop(agoraID string) error {
	m.mu.Lock()
	session := m.sessions[agoraID]
	if session != nil {
		m.intentionalStops[agoraID] = true
	}
	m.mu.Unlock()
	if session == nil || session.Process == nil {
		return fmt.Errorf("managed session %s is not running", agoraID)
	}
	if err := session.Process.Signal(syscall.SIGTERM); err != nil {
		m.mu.Lock()
		delete(m.intentionalStops, agoraID)
		m.mu.Unlock()
		return fmt.Errorf("stop managed session %s: %w", agoraID, err)
	}
	return nil
}

// Snapshot returns a read-only view of the current terminal screen.
func (m *PTYManager) Snapshot(agoraID string) (terminal.Snapshot, error) {
	m.mu.Lock()
	session := m.sessions[agoraID]
	m.mu.Unlock()
	if session == nil || session.observation == nil {
		return terminal.Snapshot{}, fmt.Errorf("managed session %s is not running", agoraID)
	}
	return session.observation.snapshot(), nil
}

// AttachAddr returns the Unix socket a client connects to for `claude-wrapper`.
func (m *PTYManager) AttachAddr(agoraID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[agoraID]
	if session == nil {
		return "", fmt.Errorf("managed session %s is not running", agoraID)
	}
	return session.SocketPath, nil
}

func (m *PTYManager) WaitClaudeSessionID(ctx context.Context, agoraID string) (string, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if id := m.ClaudeSessionID(agoraID); id != "" {
			return id, nil
		}
		m.mu.Lock()
		running := m.sessions[agoraID] != nil
		m.mu.Unlock()
		if !running {
			return "", fmt.Errorf("managed session %s exited before reporting agent session id", agoraID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}
func (m *PTYManager) Rekey(oldID, newID string) error {
	return m.Rebind(oldID, newID, "")
}

// Rebind changes the logical Agora key and optionally the provider-native
// identity while keeping the same running PTY process.
func (m *PTYManager) Rebind(oldID, newID, nativeID string) error {
	if oldID == "" || newID == "" {
		return fmt.Errorf("invalid PTY session rekey")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	value := m.sessions[oldID]
	if value == nil {
		return fmt.Errorf("managed session %s is not running", oldID)
	}
	if oldID != newID {
		if m.sessions[newID] != nil {
			return fmt.Errorf("managed session %s already exists", newID)
		}
		delete(m.sessions, oldID)
	}
	value.AgoraID = newID
	if nativeID != "" {
		value.ClaudeSession = nativeID
	}
	m.sessions[newID] = value
	return nil
}

// ClaudeSessionID returns the recorded Claude session id for a managed session.
func (m *PTYManager) ClaudeSessionID(agoraID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session := m.sessions[agoraID]; session != nil {
		return session.ClaudeSession
	}
	return ""
}

// Close stops all managed PTY sessions.
func (m *PTYManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		_ = session.Listener.Close()
		_ = session.Master.Close()
		if session.Process != nil {
			_ = session.Process.Kill()
		}
	}
	m.sessions = make(map[string]*PTYSession)
}

// serveAttach is a hub for a session's PTY: one goroutine reads master output
// and broadcasts it to every attached client; each client's keyboard input is
// written back into the master. This guarantees the PTY output is always
// consumed (so claude never blocks on a full buffer) even when no client is
// attached, and multiple clients (terminal wrapper + web) never fight over the
// master.
func (m *PTYManager) serveAttach(session *PTYSession) {
	listener := session.Listener
	master := session.Master
	type client struct {
		conn net.Conn
		send chan []byte
	}
	var clientsMu sync.Mutex
	clients := make(map[*client]bool)
	broadcast := func(data []byte) {
		clientsMu.Lock()
		for c := range clients {
			select {
			case c.send <- data:
			default: // slow client: drop this frame rather than block the hub
			}
		}
		clientsMu.Unlock()
	}
	unregister := func(c *client) {
		clientsMu.Lock()
		delete(clients, c)
		clientsMu.Unlock()
		close(c.send)
		_ = c.conn.Close()
	}

	// Always consume master output, even with zero clients, so the PTY never
	// back-pressures claude.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				if session.observation != nil && session.observation.record(data) {
					m.mu.Lock()
					handler := m.onAttention
					m.mu.Unlock()
					if handler != nil {
						handler(session.AgoraID, "approval_required")
					}
				}
				broadcast(data)
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		c := &client{conn: conn, send: make(chan []byte, 256)}
		clientsMu.Lock()
		clients[c] = true
		clientsMu.Unlock()

		go func(c *client) {
			defer unregister(c)
			// Master output -> client.
			go func() {
				for data := range c.send {
					if _, err := c.conn.Write(data); err != nil {
						return
					}
				}
			}()
			// Client keyboard -> master, while observing submitted lines.
			copyTerminalInput(master, c.conn, func() string { m.mu.Lock(); defer m.mu.Unlock(); return session.AgoraID }, session.inputHandler)
		}(c)
	}
}

// cleanClaudeEnv returns the process environment with all inherited Claude
// Code "child session" variables stripped. These markers (set by Cursor/VS
// Code extensions or a parent claude) tell claude it is a sub-session, which
// disables transcript/JSONL persistence. Without them, a fresh interactive
// claude writes its history JSONL normally.
func cleanClaudeEnv() []string {
	blocked := map[string]bool{
		"CLAUDE_CODE_CHILD_SESSION":                 true,
		"CLAUDE_CODE_SESSION_ID":                    true,
		"CLAUDE_PID":                                true,
		"CLAUDE_CODE_ENTRYPOINT":                    true,
		"CLAUDE_AGENT_SDK_VERSION":                  true,
		"CLAUDE_CODE_EXECPATH":                      true,
		"CURSOR_SPAWNED_BY_EXTENSION_ID":            true,
		"CURSOR_SPAWN_CHAIN":                        true,
		"AI_AGENT":                                  true,
		"CLAUDE_CODE_ENABLE_TASKS":                  true,
		"CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING": true,
		"CLAUDE_CODE_SUBAGENT_MODEL":                true,
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if !blocked[key] {
			env = append(env, entry)
		}
	}
	return env
}
