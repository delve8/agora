package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
	"github.com/delve8/agora/internal/terminal"
)

type Manager struct {
	store     StateStore
	adapter   *adapter.ClaudeCodeAdapter
	pty       *PTYManager
	pi        *PiManager
	daemonID  string
	homeDir   string
	history   *adapter.HistoryCatalog
	piHistory *adapter.PiHistoryCatalog

	mu               sync.Mutex
	active           map[string]bool
	generation       map[string]uint64
	observers        map[string]context.CancelFunc
	piObservers      map[string]context.CancelFunc
	subs             map[string]map[chan event.Event]struct{}
	sessionOnExit    func(session.Session, PTYExit)
	eventHandler     func(event.Event)
	attentionHandler func(string, string)
	closed           bool
}

func NewManager(db StateStore, agentAdapter *adapter.ClaudeCodeAdapter, ptyManager *PTYManager) *Manager {
	homeDir, _ := os.UserHomeDir()
	if ptyManager != nil && ptyManager.homeDir != "" {
		homeDir = ptyManager.homeDir
	}
	manager := &Manager{store: db, adapter: agentAdapter, pty: ptyManager, homeDir: homeDir, history: adapter.NewHistoryCatalog(homeDir), piHistory: adapter.NewPiHistoryCatalog(homeDir, ""), active: make(map[string]bool), generation: make(map[string]uint64), observers: make(map[string]context.CancelFunc), piObservers: make(map[string]context.CancelFunc), subs: make(map[string]map[chan event.Event]struct{})}
	if ptyManager != nil {
		ptyManager.SetExitHandler(manager.handlePTYExit)
		ptyManager.SetAttentionHandler(func(id, attention string) {
			manager.mu.Lock()
			handler := manager.attentionHandler
			manager.mu.Unlock()
			if handler != nil {
				handler(id, attention)
			}
		})
	}
	return manager
}

func NewPiManagerRuntime(db StateStore, daemonID string, config PiConfig) *Manager {
	manager := NewManager(db, nil, nil)
	manager.daemonID = daemonID
	manager.pi = NewPiManager(config)
	manager.pi.SetExitHandler(manager.handlePiExit)
	return manager
}

func (m *Manager) AttachPi(pi *PiManager) {
	m.pi = pi
	if pi != nil {
		pi.SetExitHandler(m.handlePiExit)
	}
}

func NewDaemonManager(db StateStore, daemonID string, agentAdapter *adapter.ClaudeCodeAdapter, ptyManager *PTYManager) *Manager {
	manager := NewManager(db, agentAdapter, ptyManager)
	manager.daemonID = daemonID
	return manager
}
func (m *Manager) ListSessions(ctx context.Context, coordinationID string) ([]session.Session, error) {
	return m.store.ListSessions(ctx, coordinationID)
}

func (m *Manager) GetSession(ctx context.Context, id string) (session.Session, error) {
	if m == nil || m.store == nil {
		return session.Session{}, errSessionNotFound
	}
	return m.store.GetSession(ctx, id)
}

func (m *Manager) WaitAgentSessionID(ctx context.Context, id string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("session manager is unavailable")
	}
	if value, err := m.store.GetSession(ctx, id); err == nil && value.Agent == "pi" && m.pi != nil {
		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		for {
			if native := m.pi.NativeID(id); native != "" {
				return native, nil
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-deadline.C:
				return "", fmt.Errorf("Pi session %s did not report session id", id)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if m.pty == nil {
		return "", fmt.Errorf("session manager is unavailable")
	}
	return m.pty.WaitClaudeSessionID(ctx, id)
}
func (m *Manager) AgentSessionID(id string) string {
	if m == nil {
		return ""
	}
	if value, err := m.store.GetSession(context.Background(), id); err == nil && value.Agent == "pi" && m.pi != nil {
		return m.pi.NativeID(id)
	}
	if m.pty == nil {
		return ""
	}
	return m.pty.ClaudeSessionID(id)
}

func (m *Manager) SetAgentIdentity(ctx context.Context, id, agent, agentSessionID string) (session.Session, error) {
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	value.Agent = agent
	value.AgentSessionID = agentSessionID
	if agent == "claude" {
		value.ClaudeSessionID = strings.TrimPrefix(agentSessionID, "claude://")
		value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
	} else if agent == "pi" {
		nativeID := strings.TrimPrefix(agentSessionID, "pi://")
		value.HistoryPath = adapter.FindPiHistoryBySessionID(m.piHistoryRoot(), nativeID)
	}
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	return value, nil
}

func (m *Manager) piHistoryRoot() string {
	if m.piHistory == nil {
		return ""
	}
	if m.pi != nil && m.pi.SessionDir() != "" {
		return m.pi.SessionDir()
	}
	return m.piHistory.Root()
}

func (m *Manager) RekeySession(ctx context.Context, oldID, newID string) (session.Session, error) {
	value, err := m.store.GetSession(ctx, oldID)
	if err != nil {
		return session.Session{}, err
	}
	if m.pty != nil && value.Agent != "pi" {
		if err := m.pty.Rekey(oldID, newID); err != nil {
			return session.Session{}, err
		}
	}
	if m.pi != nil && value.Agent == "pi" {
		if err := m.pi.Rekey(oldID, newID); err != nil {
			return session.Session{}, err
		}
	}
	m.StopObserver(oldID)
	value.ID = newID
	if err := m.store.RekeySession(ctx, oldID, value); err != nil {
		return session.Session{}, err
	}
	m.mu.Lock()
	if active := m.active[oldID]; active {
		m.active[newID] = active
		delete(m.active, oldID)
	}
	if generation := m.generation[oldID]; generation != 0 {
		m.generation[newID] = generation
		delete(m.generation, oldID)
	}
	m.mu.Unlock()
	if value.HistoryPath != "" || value.Agent == "pi" {
		_ = m.StartObserver(value)
	}
	return value, nil
}

func (m *Manager) Subscribe(coordinationID string) (<-chan event.Event, func()) {
	ch := make(chan event.Event, 64)
	m.mu.Lock()
	if m.closed {
		close(ch)
		m.mu.Unlock()
		return ch, func() {}
	}
	if m.subs[coordinationID] == nil {
		m.subs[coordinationID] = make(map[chan event.Event]struct{})
	}
	m.subs[coordinationID][ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			return
		}
		if subscribers := m.subs[coordinationID]; subscribers != nil {
			delete(subscribers, ch)
			if len(subscribers) == 0 {
				delete(m.subs, coordinationID)
			}
		}
		close(ch)
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, cancel := range m.observers {
		cancel()
	}
	m.observers = make(map[string]context.CancelFunc)
	for coordinationID, subscribers := range m.subs {
		for ch := range subscribers {
			close(ch)
		}
		delete(m.subs, coordinationID)
	}
	m.mu.Unlock()
	if m.pty != nil {
		m.pty.Close()
	}
	if m.pi != nil {
		m.pi.Close()
	}
}

// Send writes one turn into a managed session's PTY and returns after the input
// is accepted. A manager-owned watcher keeps the session active until the JSONL
// observer sees new output or the turn times out.
func (m *Manager) Send(ctx context.Context, value session.Session, msg message.Message) error {
	if value.Agent == "pi" && m.pi != nil {
		if !value.Capabilities.CanSendInput {
			return fmt.Errorf("session does not accept input")
		}
		if err := m.store.UpdateMessage(ctx, msg.ID, message.StatusSent, ""); err != nil {
			return err
		}
		if err := m.pi.Send(ctx, value.ID, msg.Content); err != nil {
			_ = m.store.UpdateMessage(context.Background(), msg.ID, message.StatusFailed, err.Error())
			m.publishError(value, err)
			return err
		}
		return nil
	}
	if !value.Capabilities.CanSendInput {
		return fmt.Errorf("session does not accept input")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("session manager is closed")
	}
	if m.active[value.ID] {
		m.mu.Unlock()
		return fmt.Errorf("session is already processing a turn")
	}
	m.active[value.ID] = true
	m.mu.Unlock()

	if err := m.store.UpdateSessionState(ctx, value.ID, session.StateRunning); err != nil {
		m.clearActive(value.ID)
		return err
	}
	if err := m.store.UpdateMessage(ctx, msg.ID, message.StatusSent, ""); err != nil {
		m.clearActive(value.ID)
		return err
	}

	// Snapshot observation state before input so even a very fast JSONL append is
	// observed as completion.
	before := m.observationGeneration(value.ID)

	// Write into the PTY master; the Claude TUI reads it as typed input and the
	// observer picks up the resulting history from JSONL.
	if err := m.pty.Input(value.ID, msg.Content); err != nil {
		m.clearActive(value.ID)
		_ = m.store.UpdateMessage(context.Background(), msg.ID, message.StatusFailed, err.Error())
		_ = m.store.UpdateSessionState(context.Background(), value.ID, session.StateFailed)
		m.publishError(value, err)
		return err
	}

	// The turn completes when the observer sees new history land in the JSONL.
	go func() {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if m.isClosed() {
				m.clearActive(value.ID)
				return
			}
			if m.observationGeneration(value.ID) > before {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		m.clearActive(value.ID)
		if !m.IsRunning(value.ID) {
			return
		}
		_ = m.store.UpdateSessionState(context.Background(), value.ID, session.StateWaiting)
	}()

	_ = m.store.UpdateMessage(ctx, msg.ID, message.StatusSent, "")
	return nil
}

// CreateManagedSession persists a new managed session and launches its Claude
// process under an Agora-owned PTY. The user drives it from the terminal
// (claude-wrapper) or the web UI; its history JSONL is observed.
func (m *Manager) CreateManagedSession(ctx context.Context, coordinationID, workspace, displayName, role string) (session.Session, error) {
	if m.pty == nil {
		return session.Session{}, fmt.Errorf("session manager is unavailable")
	}
	if displayName == "" {
		displayName = "New session"
	}
	now := time.Now().UTC()
	value := session.Session{
		ID:                fmt.Sprintf("sess-%d", now.UnixNano()),
		CoordinationID:    coordinationID,
		Agent:             "claude-code",
		Workspace:         workspace,
		DisplayName:       displayName,
		DisplayNameSource: session.InitialDisplayNameSource(displayName),
		Role:              role,
		State:             session.StateStarting,
		Source:            session.SourceManaged,
		Connection:        session.ConnectionUnavailable,
		Capabilities:      session.Capabilities{CanStart: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true, CanObserve: true},
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := m.store.CreateSession(ctx, value); err != nil {
		return session.Session{}, err
	}

	// Launch the Claude process under an Agora-owned PTY; Launch reads back the
	// recorded Claude session id so history can be located.
	started, err := m.pty.Launch(value.ID, workspace, "")
	if err != nil {
		value.LastError = err.Error()
		value.State = session.StateFailed
		value.Connection = session.ConnectionStale
		_ = m.store.UpdateSessionObservation(ctx, value)
		return m.store.GetSession(ctx, value.ID)
	}
	value.ClaudeSessionID = started.ClaudeSession
	value.ProcessID = started.Process.Pid
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.SessionMetaPath = filepath.Join(m.homeDir, ".claude", "sessions", fmt.Sprintf("%d.json", started.Process.Pid))
	if value.HistoryPath == "" {
		value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, started.ClaudeSession)
	}
	// Fall back: the history file name is the Claude session id. Guarantees a
	// stable id for --resume recovery even if the metadata file was missed.
	if value.ClaudeSessionID == "" && value.HistoryPath != "" {
		value.ClaudeSessionID = strings.TrimSuffix(filepath.Base(value.HistoryPath), ".jsonl")
	}
	_ = m.store.UpdateSessionObservation(ctx, value)

	// Persist the Claude session id once the background reader in PTYManager
	// learns it (a fresh launch records it only after trust is accepted).
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			sid := m.pty.ClaudeSessionID(value.ID)
			if sid != "" {
				value.ClaudeSessionID = sid
				value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, sid)
				_ = m.store.UpdateSessionObservation(context.Background(), value)
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()

	// Start the observer even before the history JSONL appears: claude writes
	// it only after accepting the workspace trust prompt, so the path may not
	// exist yet. The observer re-resolves it when empty.
	if err := m.StartObserver(value); err != nil {
		value.LastError = err.Error()
		value.Connection = session.ConnectionStale
		value.State = session.StateStale
		_ = m.store.UpdateSessionObservation(ctx, value)
	}
	return m.store.GetSession(ctx, value.ID)
}

func (m *Manager) CreateManagedSessionWithID(ctx context.Context, id, coordinationID, workspace, displayName, role string) (session.Session, error) {
	return m.CreateManagedSessionWithAgent(ctx, id, coordinationID, workspace, displayName, role, "claude")
}

func (m *Manager) CreateManagedSessionWithAgent(ctx context.Context, id, coordinationID, workspace, displayName, role, agent string) (session.Session, error) {
	if agent == "pi" && m.pi != nil {
		return m.createPiSession(ctx, id, coordinationID, workspace, displayName, role)
	}
	if id == "" {
		return session.Session{}, fmt.Errorf("session id is required")
	}
	if m.pty == nil {
		return session.Session{}, fmt.Errorf("session manager is unavailable")
	}
	if displayName == "" {
		displayName = "New session"
	}
	now := time.Now().UTC()
	value := session.Session{ID: id, CoordinationID: coordinationID, Agent: "claude-code", Workspace: workspace, DisplayName: displayName, DisplayNameSource: session.InitialDisplayNameSource(displayName), Role: role, State: session.StateStarting, Source: session.SourceManaged, Connection: session.ConnectionUnavailable, Capabilities: session.Capabilities{CanStart: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true, CanObserve: true}, CreatedAt: now, UpdatedAt: now}
	if err := m.store.CreateSession(ctx, value); err != nil {
		return session.Session{}, err
	}
	started, err := m.pty.Launch(value.ID, workspace, "")
	if err != nil {
		value.LastError = err.Error()
		value.State = session.StateFailed
		value.Connection = session.ConnectionStale
		_ = m.store.UpdateSessionObservation(ctx, value)
		return m.store.GetSession(ctx, value.ID)
	}
	value.ClaudeSessionID = started.ClaudeSession
	value.ProcessID = started.Process.Pid
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.SessionMetaPath = filepath.Join(m.homeDir, ".claude", "sessions", fmt.Sprintf("%d.json", started.Process.Pid))
	value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, started.ClaudeSession)
	if value.ClaudeSessionID == "" && value.HistoryPath != "" {
		value.ClaudeSessionID = strings.TrimSuffix(filepath.Base(value.HistoryPath), ".jsonl")
	}
	_ = m.store.UpdateSessionObservation(ctx, value)
	if err := m.StartObserver(value); err != nil {
		value.LastError = err.Error()
		value.Connection = session.ConnectionStale
		value.State = session.StateStale
		_ = m.store.UpdateSessionObservation(ctx, value)
	}
	return m.store.GetSession(ctx, value.ID)
}

func (m *Manager) createPiSession(ctx context.Context, id, coordinationID, workspace, displayName, role string) (session.Session, error) {
	if id == "" {
		return session.Session{}, fmt.Errorf("session id is required")
	}
	if m.pi == nil {
		return session.Session{}, fmt.Errorf("Pi session manager is unavailable")
	}
	if displayName == "" {
		displayName = "New session"
	}
	now := time.Now().UTC()
	value := session.Session{ID: id, CoordinationID: coordinationID, Agent: "pi", Workspace: workspace, DisplayName: displayName, DisplayNameSource: session.InitialDisplayNameSource(displayName), Role: role, State: session.StateStarting, Source: session.SourceManaged, Connection: session.ConnectionUnavailable, Capabilities: piCapabilities(), CreatedAt: now, UpdatedAt: now}
	if err := m.store.CreateSession(ctx, value); err != nil {
		return session.Session{}, err
	}
	nativeID, err := newPiNativeID()
	if err != nil {
		value.State = session.StateFailed
		value.Connection = session.ConnectionStale
		value.LastError = err.Error()
		_ = m.store.UpdateSessionObservation(ctx, value)
		return m.store.GetSession(ctx, id)
	}
	value.AgentSessionID = "pi://" + nativeID
	process, err := m.pi.Start(id, workspace, nativeID)
	if err != nil {
		value.State = session.StateFailed
		value.Connection = session.ConnectionStale
		value.LastError = err.Error()
		_ = m.store.UpdateSessionObservation(ctx, value)
		return m.store.GetSession(ctx, id)
	}
	value.HistoryPath = adapter.FindPiHistoryBySessionID(m.piHistoryRoot(), nativeID)
	value.ProcessID = process.PID
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.Capabilities = piCapabilities()
	_ = m.store.UpdateSessionObservation(ctx, value)
	// The Pi history file may be created only after the first prompt. Start
	// the observer immediately; it will resolve the path once Pi creates it.
	_ = m.StartPiObserver(value)
	return m.store.GetSession(ctx, id)
}

func piCapabilities() session.Capabilities {
	// Pi runs its native TUI under an Agora-owned PTY. The same VT emulator
	// used by Claude exposes a read-only screen snapshot to the Web UI.
	return session.Capabilities{CanStart: true, CanDiscover: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true}
}

func newPiNativeID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Pi session id: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(value[0:4]), hex.EncodeToString(value[4:6]), hex.EncodeToString(value[6:8]), hex.EncodeToString(value[8:10]), hex.EncodeToString(value[10:16])), nil
}

// longer relaunched automatically: persisted state is reconciled against the
// actual PTY table and users resume inactive conversations explicitly.
func (m *Manager) ResumeManagedSessions(ctx context.Context) error {
	values, err := m.store.ListSessions(ctx, "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Source != session.SourceManaged || m.IsRunning(value.ID) {
			continue
		}
		effective := m.EffectiveSession(value)
		if err := m.store.UpdateSessionObservation(ctx, effective); err != nil {
			return err
		}
	}
	return nil
}

func isSessionNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, errSessionNotFound)
}

func isCursorNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, errCursorNotFound)
}

func managedRunningCapabilities() session.Capabilities {
	return session.Capabilities{CanStart: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true}
}

func (m *Manager) ResumeSession(ctx context.Context, value session.Session) (session.Session, error) {
	if value.Agent == "pi" && m.pi != nil {
		return m.resumePiSession(ctx, value)
	}
	if m.pty == nil {
		return session.Session{}, fmt.Errorf("session manager is unavailable")
	}
	if strings.TrimSpace(value.ClaudeSessionID) == "" {
		return session.Session{}, fmt.Errorf("Claude session id is required")
	}
	workspace, err := filepath.Abs(strings.TrimSpace(value.Workspace))
	if err != nil {
		return session.Session{}, err
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return session.Session{}, fmt.Errorf("workspace must be an existing directory")
	}
	value.Workspace = workspace
	if value.AgentSessionID == "" && value.ClaudeSessionID != "" {
		value.AgentSessionID = "claude://" + value.ClaudeSessionID
	}
	if m.IsRunning(value.ID) {
		if stored, getErr := m.store.GetSession(ctx, value.ID); getErr == nil {
			return m.EffectiveSession(stored), nil
		}
		return m.EffectiveSession(value), nil
	}
	_, err = m.store.GetSession(ctx, value.ID)
	if isSessionNotFound(err) {
		value.Source = session.SourceManaged
		value.State = session.StateStarting
		value.Connection = session.ConnectionUnavailable
		value.ProcessID = 0
		value.Capabilities = session.Capabilities{CanReadHistory: true, CanResume: true}
		if value.CreatedAt.IsZero() {
			value.CreatedAt = time.Now().UTC()
		}
		value.UpdatedAt = time.Now().UTC()
		if err := m.store.CreateSession(ctx, value); err != nil {
			return session.Session{}, err
		}
	} else if err != nil {
		return session.Session{}, err
	}
	launched, err := m.pty.Launch(value.ID, value.Workspace, value.ClaudeSessionID)
	if err != nil {
		value.State = session.StateFailed
		value.Connection = session.ConnectionStale
		value.LastError = err.Error()
		_ = m.store.UpdateSessionObservation(ctx, value)
		return session.Session{}, err
	}
	value.Source = session.SourceManaged
	value.ProcessID = launched.Process.Pid
	value.SessionMetaPath = filepath.Join(m.homeDir, ".claude", "sessions", fmt.Sprintf("%d.json", launched.Process.Pid))
	value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.Capabilities = managedRunningCapabilities()
	value.LastError = ""
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	if err := m.StartObserver(value); err != nil {
		return session.Session{}, err
	}
	stored, err := m.store.GetSession(ctx, value.ID)
	if err != nil {
		return session.Session{}, err
	}
	return m.EffectiveSession(stored), nil
}

func (m *Manager) resumePiSession(ctx context.Context, value session.Session) (session.Session, error) {
	if m.pi == nil {
		return session.Session{}, fmt.Errorf("Pi session manager is unavailable")
	}
	nativeID := strings.TrimPrefix(value.AgentSessionID, "pi://")
	if nativeID == "" {
		return session.Session{}, fmt.Errorf("Pi session id is required")
	}
	workspace, err := filepath.Abs(strings.TrimSpace(value.Workspace))
	if err != nil {
		return session.Session{}, err
	}
	if info, statErr := os.Stat(workspace); statErr != nil || !info.IsDir() {
		return session.Session{}, fmt.Errorf("workspace must be an existing directory")
	}
	value.Workspace = workspace
	if m.IsRunning(value.ID) {
		return m.store.GetSession(ctx, value.ID)
	}
	if _, err := m.store.GetSession(ctx, value.ID); isSessionNotFound(err) {
		value.Source = session.SourceManaged
		value.State = session.StateStarting
		value.Connection = session.ConnectionUnavailable
		value.Capabilities = piCapabilities()
		if value.CreatedAt.IsZero() {
			value.CreatedAt = time.Now().UTC()
		}
		if err := m.store.CreateSession(ctx, value); err != nil {
			return session.Session{}, err
		}
	} else if err != nil {
		return session.Session{}, err
	}
	historyPath := value.HistoryPath
	if historyPath == "" {
		historyPath = adapter.FindPiHistoryBySessionID(m.piHistoryRoot(), nativeID)
	}
	process, err := m.pi.Resume(value.ID, value.Workspace, nativeID, historyPath)
	if err != nil {
		return session.Session{}, err
	}
	value.ProcessID = process.PID
	value.State = session.StateRunning
	value.Connection = session.ConnectionObserved
	value.Capabilities = piCapabilities()
	value.HistoryPath = historyPath
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	if err := m.StartPiObserver(value); err != nil {
		return session.Session{}, err
	}
	return m.store.GetSession(ctx, value.ID)
}

func (m *Manager) StopSession(value session.Session) error {
	if value.Agent == "pi" && m.pi != nil {
		return m.pi.Stop(value.ID)
	}
	if m.pty == nil {
		return fmt.Errorf("session manager is unavailable")
	}
	if !m.IsRunning(value.ID) {
		return fmt.Errorf("session is not running")
	}
	m.clearActive(value.ID)
	return m.pty.Stop(value.ID)
}

func (m *Manager) StartObserver(value session.Session) error {
	if value.Agent == "pi" {
		return m.StartPiObserver(value)
	}
	m.mu.Lock()
	if _, exists := m.observers[value.ID]; exists {
		m.mu.Unlock()
		return nil
	}
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("session manager is closed")
	}
	// Observers are manager-owned and live until Close; they must not derive
	// from a request context, which is cancelled when the HTTP call returns.
	ctx, cancel := context.WithCancel(context.Background())
	m.observers[value.ID] = cancel
	m.mu.Unlock()
	// Establish the initial end-of-file cursor before returning to the caller.
	// Otherwise a very fast append immediately after ResumeSession can race
	// with the observer goroutine's first stat and get skipped as "old".
	m.initializeObserverCursor(value.ID, value.HistoryPath)
	go m.observe(ctx, value)
	return nil
}

func (m *Manager) StopObserver(id string) {
	if value, err := m.store.GetSession(context.Background(), id); err == nil && value.Agent == "pi" {
		m.StopPiObserver(id)
		return
	}
	m.mu.Lock()
	if cancel := m.observers[id]; cancel != nil {
		cancel()
		delete(m.observers, id)
	}
	m.mu.Unlock()
}

// ReconcileObservers starts observers for all persisted sessions that have a
// history path (both fresh managed launches and sessions restored after an
// Agora restart). It does not launch Claude — that is PTYManager's job.
func (m *Manager) ReconcileObservers(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	values, err := m.store.ListSessions(ctx, "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.HistoryPath == "" || !m.IsRunning(value.ID) {
			continue
		}
		if err := m.StartObserver(value); err != nil {
			value.State = session.StateStale
			value.Connection = session.ConnectionStale
			value.LastError = err.Error()
			_ = m.store.UpdateSessionObservation(ctx, value)
		}
	}
	return nil
}

func (m *Manager) observe(ctx context.Context, value session.Session) {
	defer m.StopObserver(value.ID)
	cursor, err := m.store.GetObservationCursor(ctx, value.ID)
	if isCursorNotFound(err) {
		cursor = store.ObservationCursor{SessionID: value.ID, Path: value.HistoryPath}
	} else if err != nil {
		m.markObservationError(value, err)
		return
	}
	if cursor.Path == "" {
		cursor.Path = value.HistoryPath
	}
	if cursor.Path != "" {
		if info, statErr := os.Stat(cursor.Path); statErr == nil && cursor.ByteOffset == 0 && cursor.Line == 0 {
			cursor.ByteOffset = info.Size()
			_ = m.store.SaveObservationCursor(ctx, store.ObservationCursor{SessionID: value.ID, Path: cursor.Path, ByteOffset: cursor.ByteOffset})
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		// Refresh the session from the DB each loop: claude_session_id and
		// history_path may be back-filled after launch (the metadata/JSONL only
		// appear once claude accepts the workspace trust prompt), so the
		// observer must not rely on the launch-time snapshot.
		if fresh, err := m.store.GetSession(ctx, value.ID); err == nil {
			value = fresh
		}
		// The history JSONL may appear only after claude accepts the workspace
		// trust prompt. Re-resolve it from the session's Claude id when empty.
		if cursor.Path == "" && value.ClaudeSessionID != "" {
			cursor.Path = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
			if cursor.Path != "" {
				value.HistoryPath = cursor.Path
				_ = m.store.UpdateSessionObservation(ctx, value)
			}
		}
		records, readErr := adapter.ReadHistory(ctx, adapter.HistoryCursor{Path: cursor.Path, ByteOffset: cursor.ByteOffset, Line: cursor.Line, LastID: cursor.LastID}, value.ID)
		if readErr != nil {
			m.markObservationError(value, readErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for _, record := range records {
			name := ""
			nameSource := ""
			switch record.Event.Kind {
			case event.KindAITitle:
				if session.CanApplyAITitle(value) {
					name = session.DescribeMessage(record.Event.Content)
					nameSource = session.DisplayNameSourceAITitle
				}
			case event.KindUser:
				if session.CanApplyFirstUserName(value) {
					name = session.DescribeMessage(record.Event.Content)
					nameSource = session.DisplayNameSourceFirstUser
				}
			}
			if name != "" {
				value.DisplayName = name
				value.DisplayNameSource = nameSource
				_ = m.store.UpdateSessionDisplayName(ctx, value.ID, name, nameSource)
			}
			cursor.ByteOffset = record.Cursor.ByteOffset
			cursor.Line = record.Cursor.Line
			cursor.LastID = record.Event.ExternalID
			_ = m.store.SaveObservationCursor(ctx, store.ObservationCursor{SessionID: value.ID, Path: cursor.Path, ByteOffset: cursor.ByteOffset, Line: cursor.Line, LastID: cursor.LastID})
			m.advanceObservation(value.ID)
			m.publish(value.CoordinationID, record.Event)
		}
		if len(records) > 0 && m.IsRunning(value.ID) {
			now := time.Now().UTC()
			value.LastObservedAt = &now
			value.Connection = session.ConnectionObserved
			value.State = session.StateWaiting
			value.LastError = ""
			_ = m.store.UpdateSessionObservation(ctx, value)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *Manager) initializeObserverCursor(sessionID, historyPath string) {
	if m == nil || m.store == nil || strings.TrimSpace(historyPath) == "" {
		return
	}
	_, err := m.store.GetObservationCursor(context.Background(), sessionID)
	if err == nil {
		return
	}
	if !isCursorNotFound(err) {
		return
	}
	info, err := os.Stat(historyPath)
	if err != nil {
		return
	}
	_ = m.store.SaveObservationCursor(context.Background(), store.ObservationCursor{
		SessionID: sessionID, Path: historyPath, ByteOffset: info.Size(),
	})
}

func (m *Manager) markObservationError(value session.Session, err error) {
	value.Connection = session.ConnectionStale
	value.State = session.StateStale
	value.LastError = err.Error()
	_ = m.store.UpdateSessionObservation(context.Background(), value)
}

func (m *Manager) SetSessionExitHandler(handler func(session.Session, PTYExit)) {
	m.mu.Lock()
	m.sessionOnExit = handler
	m.mu.Unlock()
}

func (m *Manager) SetEventHandler(handler func(event.Event)) {
	m.mu.Lock()
	m.eventHandler = handler
	m.mu.Unlock()
}

func (m *Manager) SetAttentionHandler(handler func(string, string)) {
	m.mu.Lock()
	m.attentionHandler = handler
	m.mu.Unlock()
}

func (m *Manager) handlePTYExit(exited PTYExit) {
	m.clearActive(exited.AgoraID)
	m.StopObserver(exited.AgoraID)
	if m.isClosed() {
		return
	}
	value, err := m.store.GetSession(context.Background(), exited.AgoraID)
	if err != nil {
		return
	}
	if value.ClaudeSessionID == "" {
		value.ClaudeSessionID = exited.ClaudeSession
	}
	if value.HistoryPath == "" && value.ClaudeSessionID != "" {
		value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
	}
	value.ProcessID = 0
	value.SessionMetaPath = ""
	value.State = session.StateStopped
	value.Connection = session.ConnectionUnavailable
	value.LastError = ""
	if exited.Err != nil && exited.ExitCode != 0 {
		value.LastError = exited.Err.Error()
	}
	_ = m.store.UpdateSessionObservation(context.Background(), value)
	m.mu.Lock()
	handler := m.sessionOnExit
	m.mu.Unlock()
	if handler != nil {
		handler(value, exited)
	}
}

func (m *Manager) handlePiExit(exited PiExit) {
	m.clearActive(exited.AgoraID)
	m.StopObserver(exited.AgoraID)
	if m.isClosed() || m.store == nil {
		return
	}
	value, err := m.store.GetSession(context.Background(), exited.AgoraID)
	if err != nil {
		return
	}
	value.ProcessID = 0
	value.State = session.StateStopped
	value.Connection = session.ConnectionUnavailable
	value.LastError = ""
	if exited.Err != nil && exited.ExitCode != 0 {
		value.LastError = exited.Err.Error()
	}
	_ = m.store.UpdateSessionObservation(context.Background(), value)
	m.mu.Lock()
	handler := m.sessionOnExit
	m.mu.Unlock()
	if handler != nil {
		handler(value, PTYExit{AgoraID: exited.AgoraID, ExitCode: exited.ExitCode, Err: exited.Err, Intentional: exited.Intentional})
	}
}

func (m *Manager) IsRunning(id string) bool {
	if m == nil {
		return false
	}
	if m.pi != nil {
		if value, err := session.ParseSessionID(id); err == nil && value.Agent == "pi" {
			return m.pi.IsRunning(id)
		}
	}
	return m.pty != nil && m.pty.IsRunning(id)
}

// LiveSessions returns the sessions the local manager is currently running
// (serve mode). The manager owns these PTYs, so the live set is read from
// memory rather than persisted rows.
func (m *Manager) LiveSessions(ctx context.Context, coordinationID string) ([]session.Session, error) {
	if m == nil {
		return nil, nil
	}
	type livePTY struct {
		id            string
		claudeSession string
		workspace     string
		pid           int
		started       time.Time
	}
	lives := make([]livePTY, 0)
	if m.pty != nil {
		m.pty.mu.Lock()
		lives = make([]livePTY, 0, len(m.pty.sessions))
		for id, ps := range m.pty.sessions {
			item := livePTY{id: id, claudeSession: ps.ClaudeSession, workspace: ps.Workspace, started: ps.StartedAt}
			if ps.Process != nil {
				item.pid = ps.Process.Pid
			}
			lives = append(lives, item)
		}
		m.pty.mu.Unlock()
	}

	// Enrich live sessions with names and history paths from the observed
	// history catalog, so the list is readable without consulting the store.
	var summaries []adapter.HistorySummary
	if m.history != nil {
		summaries, _ = m.history.List(ctx)
	}
	named := make(map[string]session.Session, len(summaries))
	for _, summary := range summaries {
		named[summary.SessionID] = historySession(summary, coordinationID, m.daemonID)
	}
	values := make([]session.Session, 0, len(lives))
	for _, live := range lives {
		value := session.Session{
			ID: live.id, CoordinationID: coordinationID, DaemonID: m.daemonID,
			Agent: "claude-code", Workspace: live.workspace, Role: "managed",
			Source: session.SourceManaged, State: session.StateRunning, Connection: session.ConnectionObserved,
			ProcessID: live.pid, CreatedAt: live.started, UpdatedAt: live.started,
			Capabilities: session.Capabilities{CanStart: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true},
		}
		if live.claudeSession != "" {
			value.AgentSessionID = "claude://" + live.claudeSession
			value.ClaudeSessionID = live.claudeSession
			if history, ok := named[live.claudeSession]; ok {
				value.DisplayName = history.DisplayName
				value.DisplayNameSource = history.DisplayNameSource
				value.HistoryPath = history.HistoryPath
			}
		}
		values = append(values, value)
	}

	// Pi has its own PTY table, so enumerate it separately from the Claude PTY
	// table while exposing the same live-session metadata.
	if m.pi != nil {
		m.pi.mu.Lock()
		piLives := make([]struct {
			id, nativeID, workspace string
			pid                     int
		}, 0, len(m.pi.processes))
		for id, process := range m.pi.processes {
			process.mu.Lock()
			nativeID := process.NativeID
			workspace := process.Workspace
			process.mu.Unlock()
			piLives = append(piLives, struct {
				id, nativeID, workspace string
				pid                     int
			}{id: id, nativeID: nativeID, workspace: workspace, pid: process.PID})
		}
		m.pi.mu.Unlock()
		for _, live := range piLives {
			value, err := m.store.GetSession(ctx, live.id)
			if err != nil {
				continue
			}
			if coordinationID != "" && value.CoordinationID != coordinationID {
				continue
			}
			value.ProcessID = live.pid
			value.Workspace = firstNonEmpty(value.Workspace, live.workspace)
			value.Agent = "pi"
			if value.AgentSessionID == "" && live.nativeID != "" {
				value.AgentSessionID = "pi://" + live.nativeID
			}
			value.State = session.StateRunning
			value.Connection = session.ConnectionObserved
			value.Capabilities = piCapabilities()
			values = append(values, value)
		}
	}
	return values, nil
}

func (m *Manager) EffectiveSession(value session.Session) session.Session {
	if value.Source == session.SourceHistory {
		value.Capabilities = session.Capabilities{CanReadHistory: true, CanResume: value.NativeSessionURI() != "" && value.Workspace != ""}
		value.State = session.StateStopped
		value.Connection = session.ConnectionUnavailable
		value.ProcessID = 0
		return value
	}
	if value.Source != session.SourceManaged {
		return value
	}
	if m.IsRunning(value.ID) {
		if value.State != session.StateWaiting && value.State != session.StateStarting {
			value.State = session.StateRunning
		}
		value.Connection = session.ConnectionObserved
		if value.Agent == "pi" {
			value.Capabilities = piCapabilities()
		} else {
			value.Capabilities = managedRunningCapabilities()
		}
		return value
	}
	value.State = session.StateStopped
	value.Connection = session.ConnectionUnavailable
	value.ProcessID = 0
	value.Capabilities = session.Capabilities{CanReadHistory: value.HistoryPath != "" || value.NativeSessionURI() != "", CanResume: value.NativeSessionURI() != "" && value.Workspace != ""}
	return value
}

func (m *Manager) DiscoverHistorySessions(ctx context.Context, coordinationID, daemonID string) ([]session.Session, error) {
	summaries, err := m.history.List(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]session.Session, 0, len(summaries))
	for _, summary := range summaries {
		if summary.SessionID == "" {
			continue
		}
		uri := "claude://" + summary.SessionID
		id, err := session.NewSessionID(daemonID, "claude", uri)
		if err != nil {
			continue
		}
		name := session.DescribeMessage(summary.LatestAITitle)
		nameSource := session.DisplayNameSourceAITitle
		if name == "" {
			name = session.DescribeMessage(summary.FirstUser)
			nameSource = session.DisplayNameSourceFirstUser
		}
		if name == "" {
			name = summary.SessionID
			nameSource = session.DisplayNameSourceInitial
		}
		workspace := strings.TrimSpace(summary.Workspace)
		if workspace == "" {
			workspace = summary.ProjectDirectory
		}
		updatedAt := summary.LastEventAt
		if updatedAt.IsZero() {
			updatedAt = summary.ModifiedAt
		}
		createdAt := summary.FirstEventAt
		if createdAt.IsZero() {
			createdAt = updatedAt
		}
		values = append(values, session.Session{ID: id, CoordinationID: coordinationID, DaemonID: daemonID, Agent: "claude", AgentSessionID: uri, ClaudeSessionID: summary.SessionID, Workspace: workspace, DisplayName: name, DisplayNameSource: nameSource, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, HistoryPath: summary.Path, Capabilities: session.Capabilities{CanReadHistory: true, CanResume: workspace != ""}, CreatedAt: createdAt, UpdatedAt: updatedAt})
	}

	piSummaries, piErr := adapter.NewPiHistoryCatalog(m.homeDir, m.piHistoryRoot()).List(ctx)
	if piErr != nil {
		return nil, piErr
	}
	for _, summary := range piSummaries {
		value, ok := piHistorySession(summary, coordinationID, daemonID)
		if ok {
			values = append(values, value)
		}
	}
	return values, nil
}
func (m *Manager) HistorySessions(ctx context.Context, coordinationID, owner string) ([]session.Session, error) {
	summaries, err := m.history.List(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]session.Session, 0, len(summaries))
	for _, summary := range summaries {
		values = append(values, historySession(summary, coordinationID, owner))
	}
	piSummaries, err := adapter.NewPiHistoryCatalog(m.homeDir, m.piHistoryRoot()).List(ctx)
	if err != nil {
		return nil, err
	}
	for _, summary := range piSummaries {
		if value, ok := piHistorySession(summary, coordinationID, owner); ok {
			values = append(values, value)
		}
	}
	return values, nil
}

func (m *Manager) HistorySession(ctx context.Context, id, coordinationID, owner string) (session.Session, bool, error) {
	values, err := m.HistorySessions(ctx, coordinationID, owner)
	if err != nil {
		return session.Session{}, false, err
	}
	for _, value := range values {
		if value.ID == id {
			return value, true, nil
		}
	}
	return session.Session{}, false, nil
}

func (m *Manager) ResolveHistory(ctx context.Context, id, coordinationID, owner string, limit int) ([]event.Event, error) {
	if value, err := m.store.GetSession(ctx, id); err == nil {
		return m.HistoryForSession(ctx, value, limit)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	value, ok, err := m.HistorySession(ctx, id, coordinationID, owner)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, sql.ErrNoRows
	}
	return m.HistoryForSession(ctx, value, limit)
}

func (m *Manager) HistoryForSession(ctx context.Context, value session.Session, limit int) ([]event.Event, error) {
	if value.Agent == "pi" {
		return m.PiHistoryForSession(ctx, value, limit)
	}
	if value.Source == session.SourceHistory {
		if value.HistoryPath == "" {
			return []event.Event{}, nil
		}
		values, err := adapter.ReadAllHistory(ctx, value.HistoryPath, value.ID, limit)
		if os.IsNotExist(err) {
			return []event.Event{}, nil
		}
		return values, err
	}
	return m.History(ctx, value.ID, limit)
}

func historySession(summary adapter.HistorySummary, coordinationID, owner string) session.Session {
	name := session.DescribeMessage(summary.LatestAITitle)
	source := session.DisplayNameSourceAITitle
	if name == "" {
		name = session.DescribeMessage(summary.FirstUser)
		source = session.DisplayNameSourceFirstUser
	}
	if name == "" {
		name = summary.SessionID
		source = session.DisplayNameSourceInitial
	}
	workspace := strings.TrimSpace(summary.Workspace)
	if workspace == "" {
		workspace = summary.ProjectDirectory
	}
	updated := summary.LastEventAt
	if updated.IsZero() {
		updated = summary.ModifiedAt
	}
	created := summary.FirstEventAt
	if created.IsZero() {
		created = updated
	}
	return session.Session{ID: historySessionID(owner, summary.SessionID), CoordinationID: coordinationID, Agent: "claude-code", ExternalID: summary.SessionID, AgentSessionID: "claude://" + summary.SessionID, ClaudeSessionID: summary.SessionID, Workspace: workspace, DisplayName: name, DisplayNameSource: source, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, HistoryPath: summary.Path, Capabilities: session.Capabilities{CanReadHistory: true, CanResume: summary.SessionID != "" && workspace != ""}, CreatedAt: created, UpdatedAt: updated}
}
func piHistorySession(summary adapter.PiHistorySummary, coordinationID, owner string) (session.Session, bool) {
	if summary.SessionID == "" {
		return session.Session{}, false
	}
	uri := "pi://" + summary.SessionID
	id, err := session.NewSessionID(owner, "pi", uri)
	if err != nil {
		return session.Session{}, false
	}
	name := session.DescribeMessage(summary.SessionName)
	nameSource := session.DisplayNameSourceCustom
	if name == "" {
		name = session.DescribeMessage(summary.FirstUser)
		nameSource = session.DisplayNameSourceFirstUser
	}
	if name == "" {
		name = piFallbackDisplayName(summary.Workspace)
		nameSource = session.DisplayNameSourceInitial
	}
	createdAt := summary.FirstEventAt
	if createdAt.IsZero() {
		createdAt = summary.ModifiedAt
	}
	updatedAt := summary.LastEventAt
	if updatedAt.IsZero() {
		updatedAt = summary.ModifiedAt
	}
	return session.Session{ID: id, CoordinationID: coordinationID, DaemonID: owner, Agent: "pi", AgentSessionID: uri, Workspace: summary.Workspace, DisplayName: name, DisplayNameSource: nameSource, Role: "history", State: session.StateStopped, Source: session.SourceHistory, Connection: session.ConnectionUnavailable, HistoryPath: summary.Path, Capabilities: piCapabilitiesForHistory(summary.Workspace), CreatedAt: createdAt, UpdatedAt: updatedAt}, true
}

func piFallbackDisplayName(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace != "" {
		if base := filepath.Base(filepath.Clean(workspace)); base != "" && base != "." && base != string(filepath.Separator) {
			return "Pi · " + base
		}
	}
	return "Pi session"
}

func piCapabilitiesForHistory(workspace string) session.Capabilities {
	return session.Capabilities{CanReadHistory: true, CanResume: workspace != ""}
}

func historySessionID(owner, nativeID string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + nativeID))
	return fmt.Sprintf("history-%x", sum[:12])
}
func (m *Manager) CanManageSessions() bool { return m != nil && (m.pty != nil || m.pi != nil) }
func (m *Manager) History(ctx context.Context, id string, limit int) ([]event.Event, error) {
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if value.Agent == "pi" {
		return m.PiHistoryForSession(ctx, value, limit)
	}
	path := value.HistoryPath
	if path == "" && value.ClaudeSessionID != "" {
		path = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
		if path != "" {
			value.HistoryPath = path
			_ = m.store.UpdateSessionObservation(ctx, value)
		}
	}
	if path == "" {
		return []event.Event{}, nil
	}
	values, err := adapter.ReadAllHistory(ctx, path, id, limit)
	if os.IsNotExist(err) {
		return []event.Event{}, nil
	}
	return values, err
}
func (m *Manager) FirstUserEvent(ctx context.Context, id string) (event.Event, bool, error) {
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return event.Event{}, false, err
	}
	if value.Agent == "pi" {
		values, err := m.PiHistoryForSession(ctx, value, 0)
		if err != nil {
			return event.Event{}, false, err
		}
		for _, item := range values {
			if item.Kind == event.KindUser && strings.TrimSpace(item.Content) != "" {
				return item, true, nil
			}
		}
		return event.Event{}, false, nil
	}
	path := value.HistoryPath
	if path == "" && value.ClaudeSessionID != "" {
		path = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
	}
	if path == "" {
		return event.Event{}, false, nil
	}
	return adapter.FirstUserHistoryEvent(ctx, path, id)
}
func (m *Manager) DerivedDisplayName(ctx context.Context, id string) (string, string, bool, error) {
	values, err := m.History(ctx, id, 0)
	if err != nil {
		return "", "", false, err
	}
	name, source := session.DerivedDisplayName(values)
	return name, source, name != "", nil
}
func (m *Manager) Snapshot(id string) (terminal.Snapshot, error) {
	if m == nil {
		return terminal.Snapshot{}, fmt.Errorf("session manager is unavailable")
	}
	if value, err := m.store.GetSession(context.Background(), id); err == nil && value.Agent == "pi" && m.pi != nil {
		return m.pi.Snapshot(id)
	}
	if m.pty == nil {
		return terminal.Snapshot{}, fmt.Errorf("session manager is unavailable")
	}
	return m.pty.Snapshot(id)
}
func (m *Manager) AttachAddr(id string) (string, error) {
	if m.pty == nil {
		return "", fmt.Errorf("session manager is unavailable")
	}
	return m.pty.AttachAddr(id)
}

func (m *Manager) AttachPiAddr(id string) (string, error) {
	if m.pi == nil {
		return "", fmt.Errorf("Pi session manager is unavailable")
	}
	return m.pi.AttachAddr(id)
}
func (m *Manager) clearActive(id string) { m.mu.Lock(); delete(m.active, id); m.mu.Unlock() }
func (m *Manager) isClosed() bool        { m.mu.Lock(); defer m.mu.Unlock(); return m.closed }
func (m *Manager) observationGeneration(id string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation[id]
}
func (m *Manager) advanceObservation(id string) { m.mu.Lock(); m.generation[id]++; m.mu.Unlock() }

func (m *Manager) publish(coordinationID string, value event.Event) {
	m.mu.Lock()
	for ch := range m.subs[coordinationID] {
		select {
		case ch <- value:
		default:
		}
	}
	handler := m.eventHandler
	m.mu.Unlock()
	if handler != nil {
		handler(value)
	}
}

func (m *Manager) publishError(value session.Session, err error) {
	m.publish(value.CoordinationID, event.Event{ID: fmt.Sprintf("evt-%d", time.Now().UnixNano()), SessionID: value.ID, Kind: event.KindError, Source: event.SourceStream, Content: err.Error(), CreatedAt: time.Now().UTC()})
}
