package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	store    StateStore
	adapter  *adapter.ClaudeCodeAdapter
	pty      *PTYManager
	daemonID string
	homeDir  string
	history  *adapter.HistoryCatalog

	mu            sync.Mutex
	active        map[string]bool
	generation    map[string]uint64
	observers     map[string]context.CancelFunc
	subs          map[string]map[chan event.Event]struct{}
	sessionOnExit func(session.Session, PTYExit)
	closed        bool
}

func NewManager(db StateStore, agentAdapter *adapter.ClaudeCodeAdapter, ptyManager *PTYManager) *Manager {
	homeDir, _ := os.UserHomeDir()
	if ptyManager != nil && ptyManager.homeDir != "" {
		homeDir = ptyManager.homeDir
	}
	manager := &Manager{store: db, adapter: agentAdapter, pty: ptyManager, homeDir: homeDir, history: adapter.NewHistoryCatalog(homeDir), active: make(map[string]bool), generation: make(map[string]uint64), observers: make(map[string]context.CancelFunc), subs: make(map[string]map[chan event.Event]struct{})}
	if ptyManager != nil {
		ptyManager.SetExitHandler(manager.handlePTYExit)
	}
	return manager
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
	if m == nil || m.pty == nil {
		return "", fmt.Errorf("session manager is unavailable")
	}
	return m.pty.WaitClaudeSessionID(ctx, id)
}
func (m *Manager) AgentSessionID(id string) string {
	if m == nil || m.pty == nil {
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
	}
	value.HistoryPath = adapter.FindHistoryBySessionID(m.homeDir, strings.TrimPrefix(agentSessionID, agent+"://"))
	if err := m.store.UpdateSessionObservation(ctx, value); err != nil {
		return session.Session{}, err
	}
	return value, nil
}

func (m *Manager) RekeySession(ctx context.Context, oldID, newID string) (session.Session, error) {
	value, err := m.store.GetSession(ctx, oldID)
	if err != nil {
		return session.Session{}, err
	}
	if m.pty != nil {
		if err := m.pty.Rekey(oldID, newID); err != nil {
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
	if value.HistoryPath != "" {
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
}

// Send writes one turn into a managed session's PTY and returns after the input
// is accepted. A manager-owned watcher keeps the session active until the JSONL
// observer sees new output or the turn times out.
func (m *Manager) Send(ctx context.Context, value session.Session, msg message.Message) error {
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

// ResumeManagedSessions is retained for startup compatibility. Sessions are no
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

func (m *Manager) StopSession(value session.Session) error {
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
	go m.observe(ctx, value)
	return nil
}

func (m *Manager) StopObserver(id string) {
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

func (m *Manager) IsRunning(id string) bool {
	return m != nil && m.pty != nil && m.pty.IsRunning(id)
}

func (m *Manager) EffectiveSession(value session.Session) session.Session {
	if value.Source == session.SourceHistory {
		value.Capabilities = session.Capabilities{CanReadHistory: true, CanResume: value.ClaudeSessionID != "" && value.Workspace != ""}
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
		value.Capabilities = session.Capabilities{CanStart: true, CanAttach: true, CanObserve: true, CanSendInput: true, CanStream: true, CanInterrupt: true, CanResume: true, CanReadHistory: true, CanReadTerminal: true}
		return value
	}
	value.State = session.StateStopped
	value.Connection = session.ConnectionUnavailable
	value.ProcessID = 0
	value.Capabilities = session.Capabilities{CanReadHistory: value.HistoryPath != "" || value.ClaudeSessionID != "", CanResume: value.ClaudeSessionID != "" && value.Workspace != ""}
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
	return session.Session{
		ID:                historySessionID(owner, summary.SessionID),
		CoordinationID:    coordinationID,
		Agent:             "claude-code",
		ExternalID:        summary.SessionID,
		ClaudeSessionID:   summary.SessionID,
		Workspace:         workspace,
		DisplayName:       name,
		DisplayNameSource: nameSource,
		Role:              "history",
		State:             session.StateStopped,
		Source:            session.SourceHistory,
		Connection:        session.ConnectionUnavailable,
		HistoryPath:       summary.Path,
		Capabilities:      session.Capabilities{CanReadHistory: true, CanResume: summary.SessionID != "" && workspace != ""},
		CreatedAt:         createdAt,
		UpdatedAt:         updatedAt,
	}
}

func historySessionID(owner, claudeSessionID string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + claudeSessionID))
	return fmt.Sprintf("history-%x", sum[:12])
}

// CanManageSessions reports whether this Manager owns a PTY and local Claude
// processes. Standalone Server mode has a Manager only for shared services.
func (m *Manager) CanManageSessions() bool { return m != nil && m.pty != nil }

func (m *Manager) History(ctx context.Context, id string, limit int) ([]event.Event, error) {
	value, err := m.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
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
	path := value.HistoryPath
	if path == "" && value.ClaudeSessionID != "" {
		path = adapter.FindHistoryBySessionID(m.homeDir, value.ClaudeSessionID)
	}
	if path == "" {
		return event.Event{}, false, nil
	}
	first, ok, err := adapter.FirstUserHistoryEvent(ctx, path, id)
	if os.IsNotExist(err) {
		return event.Event{}, false, nil
	}
	return first, ok, err
}

func (m *Manager) DerivedDisplayName(ctx context.Context, id string) (string, string, bool, error) {
	values, err := m.History(ctx, id, 0)
	if err != nil {
		return "", "", false, err
	}
	name, source := session.DerivedDisplayName(values)
	return name, source, name != "", nil
}

// Snapshot returns a read-only view of a managed session's terminal screen.
func (m *Manager) Snapshot(id string) (terminal.Snapshot, error) {
	if m.pty == nil {
		return terminal.Snapshot{}, fmt.Errorf("session manager is unavailable")
	}
	return m.pty.Snapshot(id)
}

// AttachAddr returns the Unix socket where `claude-wrapper` can attach to a
// managed session's PTY.
func (m *Manager) AttachAddr(sessionID string) (string, error) {
	if m.pty == nil {
		return "", fmt.Errorf("session manager is unavailable")
	}
	return m.pty.AttachAddr(sessionID)
}

func (m *Manager) clearActive(id string) {
	m.mu.Lock()
	delete(m.active, id)
	m.mu.Unlock()
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *Manager) observationGeneration(id string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation[id]
}

func (m *Manager) advanceObservation(id string) {
	m.mu.Lock()
	m.generation[id]++
	m.mu.Unlock()
}

func (m *Manager) publish(coordinationID string, value event.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs[coordinationID] {
		select {
		case ch <- value:
		default:
		}
	}
}

func (m *Manager) publishError(value session.Session, err error) {
	e := event.Event{ID: fmt.Sprintf("evt-%d", time.Now().UnixNano()), SessionID: value.ID, Kind: event.KindError, Source: event.SourceStream, Content: err.Error(), CreatedAt: time.Now().UTC()}
	m.publish(value.CoordinationID, e)
}
