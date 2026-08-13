package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
	store   *store.Store
	adapter *adapter.ClaudeCodeAdapter
	pty     *PTYManager
	homeDir string

	mu        sync.Mutex
	active    map[string]bool
	observers map[string]context.CancelFunc
	subs      map[string]map[chan event.Event]struct{}
	closed    bool
}

func NewManager(db *store.Store, agentAdapter *adapter.ClaudeCodeAdapter, ptyManager *PTYManager) *Manager {
	homeDir, _ := os.UserHomeDir()
	return &Manager{store: db, adapter: agentAdapter, pty: ptyManager, homeDir: homeDir, active: make(map[string]bool), observers: make(map[string]context.CancelFunc), subs: make(map[string]map[chan event.Event]struct{})}
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

// Send runs one turn against a managed session: the print-session manager
// executes `claude --resume <id> --print <msg>`, which appends to the
// session's JSONL. Send returns once the turn produced output; the observer
// ingests the new JSONL content asynchronously.
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
	before, _ := m.store.CountEvents(context.Background(), value.ID)
	go func() {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				m.clearActive(value.ID)
				_ = m.store.UpdateSessionState(context.Background(), value.ID, session.StateWaiting)
				return
			default:
			}
			after, err := m.store.CountEvents(context.Background(), value.ID)
			if err == nil && after > before {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		m.clearActive(value.ID)
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
		displayName = "Claude Code"
	}
	now := time.Now().UTC()
	value := session.Session{
		ID:             fmt.Sprintf("sess-%d", now.UnixNano()),
		CoordinationID: coordinationID,
		Agent:          "claude-code",
		Workspace:      workspace,
		DisplayName:    displayName,
		Role:           role,
		State:          session.StateStarting,
		Source:         session.SourceManaged,
		Connection:     session.ConnectionUnavailable,
		Capabilities:   session.Capabilities{CanStart: true, CanSendInput: true, CanStream: true, CanResume: true, CanReadHistory: true, CanObserve: true},
		CreatedAt:      now,
		UpdatedAt:      now,
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
		displayName = "Claude Code"
	}
	now := time.Now().UTC()
	value := session.Session{ID: id, CoordinationID: coordinationID, Agent: "claude-code", Workspace: workspace, DisplayName: displayName, Role: role, State: session.StateStarting, Source: session.SourceManaged, Connection: session.ConnectionUnavailable, Capabilities: session.Capabilities{CanStart: true, CanSendInput: true, CanStream: true, CanResume: true, CanReadHistory: true, CanObserve: true}, CreatedAt: now, UpdatedAt: now}
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

// ResumeManagedSessions relaunches every persisted managed session that has a
// recorded Claude session id, so an Agora restart continues the same
// conversations (claude --resume). Each session is re-held under an
// Agora-owned PTY; the JSONL observer starts immediately.
func (m *Manager) ResumeManagedSessions(ctx context.Context) error {
	values, err := m.store.ListSessions(ctx, "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.Source != session.SourceManaged || value.ClaudeSessionID == "" || value.Workspace == "" {
			continue
		}
		if m.pty == nil {
			return fmt.Errorf("session manager is unavailable")
		}
		launched, err := m.pty.Launch(value.ID, value.Workspace, value.ClaudeSessionID)
		if err != nil {
			value.LastError = err.Error()
			value.State = session.StateStale
			value.Connection = session.ConnectionStale
			_ = m.store.UpdateSessionObservation(ctx, value)
			continue
		}
		value.ProcessID = launched.Process.Pid
		value.SessionMetaPath = filepath.Join(m.homeDir, ".claude", "sessions", fmt.Sprintf("%d.json", launched.Process.Pid))
		value.State = session.StateRunning
		value.Connection = session.ConnectionObserved
		value.LastError = ""
		_ = m.store.UpdateSessionObservation(ctx, value)
		if err := m.StartObserver(value); err != nil {
			value.LastError = err.Error()
			_ = m.store.UpdateSessionObservation(ctx, value)
		}
		log.Printf("agora: resumed managed session %s (claude %s)", value.ID, value.ClaudeSessionID)
	}
	return nil
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
	values, err := m.store.ListSessions(ctx, "")
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.HistoryPath == "" {
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
	if err == sql.ErrNoRows {
		cursor = store.ObservationCursor{SessionID: value.ID, Path: value.HistoryPath}
	} else if err != nil {
		m.markObservationError(value, err)
		return
	}
	if cursor.Path == "" {
		cursor.Path = value.HistoryPath
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
			if record.Event.ExternalID != "" {
				exists, err := m.store.HasEvent(ctx, value.ID, record.Event.Source, record.Event.ExternalID)
				if err != nil {
					m.markObservationError(value, err)
					return
				}
				if exists {
					cursor.ByteOffset = record.Cursor.ByteOffset
					cursor.Line = record.Cursor.Line
					cursor.LastID = record.Event.ExternalID
					continue
				}
			}
			if err := m.store.AppendEvent(ctx, record.Event); err != nil {
				m.markObservationError(value, err)
				return
			}
			cursor.ByteOffset = record.Cursor.ByteOffset
			cursor.Line = record.Cursor.Line
			cursor.LastID = record.Event.ExternalID
			_ = m.store.SaveObservationCursor(ctx, store.ObservationCursor{SessionID: value.ID, Path: cursor.Path, ByteOffset: cursor.ByteOffset, Line: cursor.Line, LastID: cursor.LastID})
			m.publish(value.CoordinationID, record.Event)
		}
		if len(records) > 0 {
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

func (m *Manager) PublishEvent(ctx context.Context, sessionID string, value event.Event) error {
	current, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	value.SessionID = sessionID
	m.publish(current.CoordinationID, value)
	return nil
}

func (m *Manager) clearActive(id string) {
	m.mu.Lock()
	delete(m.active, id)
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
	_ = m.store.AppendEvent(context.Background(), e)
	m.publish(value.CoordinationID, e)
}
