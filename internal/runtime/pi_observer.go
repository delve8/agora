package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/session"
	"github.com/delve8/agora/internal/store"
)

func (m *Manager) StartPiObserver(value session.Session) error {
	if value.Agent != "pi" {
		return fmt.Errorf("session %s is not a Pi session", value.ID)
	}
	m.mu.Lock()
	if _, exists := m.piObservers[value.ID]; exists {
		m.mu.Unlock()
		return nil
	}
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("session manager is closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.piObservers[value.ID] = cancel
	m.piObserverTokens[value.ID]++
	token := m.piObserverTokens[value.ID]
	m.mu.Unlock()
	go m.observePi(ctx, value, token)
	return nil
}

func (m *Manager) StopPiObserver(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopPiObserverLocked(id)
}

func (m *Manager) stopPiObserverLocked(id string) {
	if cancel := m.piObservers[id]; cancel != nil {
		cancel()
		delete(m.piObservers, id)
	}
}

func (m *Manager) stopPiObserverIfCurrent(id string, token uint64) {
	m.mu.Lock()
	if m.piObserverTokens[id] == token {
		m.stopPiObserverLocked(id)
	}
	m.mu.Unlock()
}

func (m *Manager) observePi(ctx context.Context, value session.Session, token uint64) {
	defer m.stopPiObserverIfCurrent(value.ID, token)
	cursor, err := m.store.GetObservationCursor(ctx, value.ID)
	if isCursorNotFound(err) {
		cursor = store.ObservationCursor{SessionID: value.ID, Path: value.HistoryPath}
	} else if err != nil {
		m.markObservationError(value, err)
		return
	}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if fresh, getErr := m.store.GetSession(ctx, value.ID); getErr == nil {
			value = fresh
		}
		if cursor.Path == "" {
			cursor.Path = value.HistoryPath
		}
		if cursor.Path == "" {
			nativeID := strings.TrimPrefix(value.AgentSessionID, "pi://")
			if nativeID == "" && m.pi != nil {
				nativeID = m.pi.NativeID(value.ID)
				if nativeID != "" {
					value.AgentSessionID = "pi://" + nativeID
				}
			}
			if nativeID != "" {
				cursor.Path = adapter.FindPiHistoryBySessionID(m.piHistoryRoot(), nativeID)
				if cursor.Path != "" {
					value.HistoryPath = cursor.Path
					_ = m.store.UpdateSessionObservation(ctx, value)
					m.notifySessionUpdate(value)
				}
			}
		}
		if cursor.Path != "" {
			records, readErr := adapter.ReadPiHistory(ctx, adapter.PiHistoryCursor{Path: cursor.Path, ByteOffset: cursor.ByteOffset, Line: cursor.Line, LastID: cursor.LastID}, value.ID)
			if readErr != nil {
				// A new Pi session can expose its native id before its JSONL
				// file is created. Keep polling instead of marking it stale.
				if !os.IsNotExist(readErr) {
					m.markObservationError(value, readErr)
				}
			} else {
				for _, record := range records {
					// Pi persists an explicit name as a session_info entry. It is
					// authoritative and must win over the first prompt fallback.
					if record.SessionName != "" {
						value.DisplayName = record.SessionName
						value.DisplayNameSource = session.DisplayNameSourceCustom
						_ = m.store.UpdateSessionDisplayName(ctx, value.ID, record.SessionName, session.DisplayNameSourceCustom)
						m.notifySessionUpdate(value)
					} else if record.Event.Kind == event.KindUser && session.CanApplyFirstUserName(value) && !adapter.IsPiBootstrapPrompt(record.Event.Content) {
						name := session.DescribeMessage(record.Event.Content)
						if name != "" {
							value.DisplayName = name
							value.DisplayNameSource = session.DisplayNameSourceFirstUser
							_ = m.store.UpdateSessionDisplayName(ctx, value.ID, name, session.DisplayNameSourceFirstUser)
							m.notifySessionUpdate(value)
						}
					}
					cursor.ByteOffset = record.Cursor.ByteOffset
					cursor.Line = record.Cursor.Line
					cursor.LastID = record.Event.ExternalID
					_ = m.store.SaveObservationCursor(ctx, store.ObservationCursor{SessionID: value.ID, Path: cursor.Path, ByteOffset: cursor.ByteOffset, Line: cursor.Line, LastID: cursor.LastID})
					m.advanceObservation(value.ID)
					m.publish(value.CoordinationID, record.Event)
				}
				if len(records) > 0 {
					now := time.Now().UTC()
					value.LastObservedAt = &now
					value.State = session.StateWaiting
					value.Connection = session.ConnectionObserved
					_ = m.store.UpdateSessionObservation(ctx, value)
				}
			}
		}
		if m.pi != nil {
			if events, eventErr := m.pi.Events(value.ID); eventErr == nil {
				select {
				case item, ok := <-events:
					if !ok {
						return
					}
					if item.Event.ID != "" {
						m.publish(value.CoordinationID, item.Event)
					}
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *Manager) PiHistoryForSession(ctx context.Context, value session.Session, limit int) ([]event.Event, error) {
	if value.HistoryPath == "" {
		return []event.Event{}, nil
	}
	records, err := adapter.ReadPiHistory(ctx, adapter.PiHistoryCursor{Path: value.HistoryPath}, value.ID)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(records) > limit {
		records = records[len(records)-limit:]
	}
	values := make([]event.Event, 0, len(records))
	for _, record := range records {
		values = append(values, record.Event)
	}
	return values, nil
}
