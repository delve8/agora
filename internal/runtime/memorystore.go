package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

var (
	errSessionNotFound = errors.New("session not found")
	errCursorNotFound  = errors.New("observation cursor not found")
)

// StateStore is the small runtime state contract used by Manager. The Server
// supplies the SQLite-backed implementation; a Daemon supplies memoryStore.
type StateStore interface {
	CreateSession(context.Context, session.Session) error
	DeleteSession(context.Context, string) error
	RekeySession(context.Context, string, session.Session) error
	GetSession(context.Context, string) (session.Session, error)
	ListSessions(context.Context, string) ([]session.Session, error)
	UpdateSessionDisplayName(context.Context, string, string, string) error
	UpdateSessionState(context.Context, string, string) error
	UpdateSessionObservation(context.Context, session.Session) error
	GetObservationCursor(context.Context, string) (store.ObservationCursor, error)
	SaveObservationCursor(context.Context, store.ObservationCursor) error
	CreateMessage(context.Context, message.Message) error
	UpdateMessage(context.Context, string, message.Status, string) error
}

type memoryStore struct {
	mu       sync.RWMutex
	sessions map[string]session.Session
	cursors  map[string]store.ObservationCursor
}

func NewMemoryStore() StateStore {
	return newMemoryStore()
}

func newMemoryStore() *memoryStore {
	return &memoryStore{sessions: make(map[string]session.Session), cursors: make(map[string]store.ObservationCursor)}
}

func (s *memoryStore) CreateSession(_ context.Context, value session.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value.DisplayNameSource == "" {
		value.DisplayNameSource = session.InitialDisplayNameSource(value.DisplayName)
	}
	s.sessions[value.ID] = value
	return nil
}
func (s *memoryStore) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	delete(s.cursors, id)
	return nil
}
func (s *memoryStore) RekeySession(_ context.Context, oldID string, value session.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.sessions[oldID]; ok {
		if value.DisplayName == "" {
			value.DisplayName = current.DisplayName
		}
		if value.CreatedAt.IsZero() {
			value.CreatedAt = current.CreatedAt
		}
	}
	delete(s.sessions, oldID)
	delete(s.sessions, value.ID)
	s.sessions[value.ID] = value
	delete(s.cursors, value.ID)
	if cursor, ok := s.cursors[oldID]; ok {
		delete(s.cursors, oldID)
		cursor.SessionID = value.ID
		s.cursors[value.ID] = cursor
	}
	return nil
}
func (s *memoryStore) GetSession(_ context.Context, id string) (session.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.sessions[id]
	if !ok {
		return session.Session{}, errSessionNotFound
	}
	return value, nil
}
func (s *memoryStore) ListSessions(_ context.Context, coordinationID string) ([]session.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]session.Session, 0, len(s.sessions))
	for _, value := range s.sessions {
		if coordinationID == "" || value.CoordinationID == coordinationID {
			values = append(values, value)
		}
	}
	return values, nil
}
func (s *memoryStore) UpdateSessionDisplayName(_ context.Context, id, name, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[id]
	if !ok {
		return errSessionNotFound
	}
	value.DisplayName, value.DisplayNameSource, value.UpdatedAt = name, source, nowUTC()
	s.sessions[id] = value
	return nil
}
func (s *memoryStore) UpdateSessionState(_ context.Context, id, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[id]
	if !ok {
		return errSessionNotFound
	}
	value.State, value.UpdatedAt = state, nowUTC()
	s.sessions[id] = value
	return nil
}
func (s *memoryStore) UpdateSessionObservation(_ context.Context, value session.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[value.ID]
	if current.ID == "" {
		current = value
	}
	value.DisplayName = firstNonEmpty(value.DisplayName, current.DisplayName)
	value.DisplayNameSource = firstNonEmpty(value.DisplayNameSource, current.DisplayNameSource)
	value.CoordinationID = firstNonEmpty(value.CoordinationID, current.CoordinationID)
	value.Workspace = firstNonEmpty(value.Workspace, current.Workspace)
	value.Agent = firstNonEmpty(value.Agent, current.Agent)
	value.Role = firstNonEmpty(value.Role, current.Role)
	value.CreatedAt = firstTime(value.CreatedAt, current.CreatedAt)
	value.UpdatedAt = nowUTC()
	s.sessions[value.ID] = value
	return nil
}
func (s *memoryStore) GetObservationCursor(_ context.Context, id string) (store.ObservationCursor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.cursors[id]
	if !ok {
		return store.ObservationCursor{}, errCursorNotFound
	}
	return value, nil
}
func (s *memoryStore) SaveObservationCursor(_ context.Context, cursor store.ObservationCursor) error {
	s.mu.Lock()
	s.cursors[cursor.SessionID] = cursor
	s.mu.Unlock()
	return nil
}
func (s *memoryStore) CreateMessage(context.Context, message.Message) error { return nil }
func (s *memoryStore) UpdateMessage(context.Context, string, message.Status, string) error {
	return nil
}

func nowUTC() time.Time { return time.Now().UTC() }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func firstTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}
