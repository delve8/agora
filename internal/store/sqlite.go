package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/delve8/agora/internal/coordination"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/message"
	"github.com/delve8/agora/internal/session"
)

type Store struct{ db *sql.DB }

type ObservationCursor struct {
	SessionID  string
	Path       string
	ByteOffset int64
	Line       int
	LastID     string
	UpdatedAt  time.Time
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = ".agora/agora.db"
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(context.Background(), `PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS coordinations (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, coordination_id TEXT NOT NULL, agent TEXT NOT NULL, external_id TEXT NOT NULL, workspace TEXT NOT NULL, display_name TEXT NOT NULL, role TEXT NOT NULL, state TEXT NOT NULL, capabilities_json TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, FOREIGN KEY (coordination_id) REFERENCES coordinations(id));
CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, kind TEXT NOT NULL, subtype TEXT NOT NULL, content TEXT NOT NULL, raw_json TEXT NOT NULL, created_at TEXT NOT NULL, FOREIGN KEY (session_id) REFERENCES sessions(id));
CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, coordination_id TEXT NOT NULL, sender_type TEXT NOT NULL, sender_id TEXT NOT NULL, recipient_type TEXT NOT NULL, recipient_id TEXT NOT NULL, content TEXT NOT NULL, reply_to TEXT NOT NULL, status TEXT NOT NULL, error TEXT NOT NULL, created_at TEXT NOT NULL, FOREIGN KEY (coordination_id) REFERENCES coordinations(id));
CREATE TABLE IF NOT EXISTS observation_cursors (session_id TEXT PRIMARY KEY, path TEXT NOT NULL, byte_offset INTEGER NOT NULL DEFAULT 0, line INTEGER NOT NULL DEFAULT 0, last_id TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL, FOREIGN KEY (session_id) REFERENCES sessions(id));
CREATE INDEX IF NOT EXISTS events_session_created ON events(session_id, created_at);
CREATE INDEX IF NOT EXISTS messages_coord_created ON messages(coordination_id, created_at);
UPDATE events SET session_id = (SELECT sessions.id FROM sessions WHERE sessions.external_id = events.session_id) WHERE EXISTS (SELECT 1 FROM sessions WHERE sessions.external_id = events.session_id);
`)
	if err != nil {
		return err
	}
	for _, statement := range []string{
		`ALTER TABLE sessions ADD COLUMN claude_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT 'managed'`,
		`ALTER TABLE sessions ADD COLUMN connection TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN process_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN session_meta_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN history_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN last_discovered_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN last_observed_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN external_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
	} {
		if _, alterErr := s.db.ExecContext(ctx, statement); alterErr != nil && !strings.Contains(strings.ToLower(alterErr.Error()), "duplicate column") {
			return alterErr
		}
	}
	_, err = s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS events_session_source_external ON events(session_id, source, external_id) WHERE external_id != ''`)
	return err
}

func (s *Store) withBusyRetry(ctx context.Context, fn func() error) error {
	const attempts = 8
	for attempt := 0; attempt < attempts; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "locked") && !strings.Contains(strings.ToLower(err.Error()), "busy") {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fn()
}

func (s *Store) CreateCoordination(ctx context.Context, value coordination.Coordination) error {
	return s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO coordinations(id,name,created_at) VALUES(?,?,?)`, value.ID, value.Name, value.CreatedAt.UTC().Format(timeFormat))
		return err
	})
}
func (s *Store) GetCoordination(ctx context.Context, id string) (coordination.Coordination, error) {
	var v coordination.Coordination
	var t string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,created_at FROM coordinations WHERE id=?`, id).Scan(&v.ID, &v.Name, &t)
	if err != nil {
		return v, err
	}
	v.CreatedAt, err = parseTime(t)
	return v, err
}
func (s *Store) ListCoordinations(ctx context.Context) ([]coordination.Coordination, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,created_at FROM coordinations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	result := make([]coordination.Coordination, 0)
	for rows.Next() {
		var v coordination.Coordination
		var t string
		if err := rows.Scan(&v.ID, &v.Name, &t); err != nil {
			_ = rows.Close()
			return nil, err
		}
		v.CreatedAt, err = parseTime(t)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) CreateSession(ctx context.Context, v session.Session) error {
	caps, err := json.Marshal(v.Capabilities)
	if err != nil {
		return err
	}
	if v.Source == "" {
		v.Source = session.SourceManaged
	}
	return s.withBusyRetry(ctx, func() error {
		_, err = s.db.ExecContext(ctx, `INSERT INTO sessions(id,coordination_id,agent,external_id,claude_session_id,workspace,display_name,role,state,source,connection,process_id,session_meta_path,history_path,last_discovered_at,last_observed_at,last_error,capabilities_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.CoordinationID, v.Agent, v.ExternalID, v.ClaudeSessionID, v.Workspace, v.DisplayName, v.Role, v.State, v.Source, v.Connection, v.ProcessID, v.SessionMetaPath, v.HistoryPath, formatOptionalTime(v.LastDiscoveredAt), formatOptionalTime(v.LastObservedAt), v.LastError, string(caps), v.CreatedAt.UTC().Format(timeFormat), v.UpdatedAt.UTC().Format(timeFormat))
		return err
	})
}
func (s *Store) GetSession(ctx context.Context, id string) (session.Session, error) {
	var v session.Session
	var caps, t, u, discovered, observed string
	err := s.db.QueryRowContext(ctx, `SELECT id,coordination_id,agent,external_id,claude_session_id,workspace,display_name,role,state,source,connection,process_id,session_meta_path,history_path,last_discovered_at,last_observed_at,last_error,capabilities_json,created_at,updated_at FROM sessions WHERE id=?`, id).Scan(&v.ID, &v.CoordinationID, &v.Agent, &v.ExternalID, &v.ClaudeSessionID, &v.Workspace, &v.DisplayName, &v.Role, &v.State, &v.Source, &v.Connection, &v.ProcessID, &v.SessionMetaPath, &v.HistoryPath, &discovered, &observed, &v.LastError, &caps, &t, &u)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal([]byte(caps), &v.Capabilities); err != nil {
		return v, err
	}
	v.LastDiscoveredAt, err = parseOptionalTime(discovered)
	if err != nil {
		return v, err
	}
	v.LastObservedAt, err = parseOptionalTime(observed)
	if err != nil {
		return v, err
	}
	v.CreatedAt, err = parseTime(t)
	if err != nil {
		return v, err
	}
	v.UpdatedAt, err = parseTime(u)
	return v, err
}
func (s *Store) ListSessions(ctx context.Context, cid string) ([]session.Session, error) {
	var rows *sql.Rows
	var err error
	if cid == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id FROM sessions ORDER BY created_at`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE coordination_id=? ORDER BY created_at`, cid)
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	result := make([]session.Session, 0, len(ids))
	for _, id := range ids {
		value, err := s.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}
func (s *Store) UpdateSessionState(ctx context.Context, id, state string) error {
	return s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE sessions SET state=?,updated_at=? WHERE id=?`, state, nowString(), id)
		return err
	})
}

func (s *Store) UpdateSessionObservation(ctx context.Context, v session.Session) error {
	return s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE sessions SET state=?,source=?,connection=?,process_id=?,session_meta_path=?,history_path=?,claude_session_id=?,last_discovered_at=?,last_observed_at=?,last_error=?,updated_at=? WHERE id=?`, v.State, v.Source, v.Connection, v.ProcessID, v.SessionMetaPath, v.HistoryPath, v.ClaudeSessionID, formatOptionalTime(v.LastDiscoveredAt), formatOptionalTime(v.LastObservedAt), v.LastError, nowString(), v.ID)
		return err
	})
}

func (s *Store) FindSessionByExternal(ctx context.Context, workspace, externalID string) (session.Session, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE workspace=? AND external_id=? LIMIT 1`, workspace, externalID).Scan(&id)
	if err != nil {
		return session.Session{}, err
	}
	return s.GetSession(ctx, id)
}

func (s *Store) HasEvent(ctx context.Context, sessionID, source, externalID string) (bool, error) {
	var value int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE session_id=? AND source=? AND external_id=? LIMIT 1`, sessionID, source, externalID).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) AppendEvent(ctx context.Context, v event.Event) error {
	return s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO events(id,session_id,kind,subtype,content,raw_json,external_id,source,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, v.ID, v.SessionID, v.Kind, v.Subtype, v.Content, v.RawJSON, v.ExternalID, v.Source, v.CreatedAt.UTC().Format(timeFormat))
		return err
	})
}

// CountEvents reports the number of persisted events for a session. Used to
// detect when a managed turn produced new output.
func (s *Store) CountEvents(ctx context.Context, sessionID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id=?`, sessionID).Scan(&count)
	return count, err
}
func (s *Store) ListEvents(ctx context.Context, sid string) ([]event.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,kind,subtype,content,raw_json,external_id,source,created_at FROM events WHERE session_id=? ORDER BY created_at`, sid)
	if err != nil {
		return nil, err
	}
	result := make([]event.Event, 0)
	for rows.Next() {
		var v event.Event
		var t string
		if err := rows.Scan(&v.ID, &v.SessionID, &v.Kind, &v.Subtype, &v.Content, &v.RawJSON, &v.ExternalID, &v.Source, &t); err != nil {
			_ = rows.Close()
			return nil, err
		}
		v.CreatedAt, err = parseTime(t)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) GetObservationCursor(ctx context.Context, sessionID string) (ObservationCursor, error) {
	var cursor ObservationCursor
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT session_id,path,byte_offset,line,last_id,updated_at FROM observation_cursors WHERE session_id=?`, sessionID).Scan(&cursor.SessionID, &cursor.Path, &cursor.ByteOffset, &cursor.Line, &cursor.LastID, &updated)
	if err != nil {
		return cursor, err
	}
	cursor.UpdatedAt, err = parseTime(updated)
	return cursor, err
}

func (s *Store) SaveObservationCursor(ctx context.Context, cursor ObservationCursor) error {
	if cursor.UpdatedAt.IsZero() {
		cursor.UpdatedAt = time.Now().UTC()
	}
	return s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO observation_cursors(session_id,path,byte_offset,line,last_id,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET path=excluded.path,byte_offset=excluded.byte_offset,line=excluded.line,last_id=excluded.last_id,updated_at=excluded.updated_at`, cursor.SessionID, cursor.Path, cursor.ByteOffset, cursor.Line, cursor.LastID, cursor.UpdatedAt.UTC().Format(timeFormat))
		return err
	})
}

func (s *Store) ListObservedSessions(ctx context.Context) ([]session.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE source=? ORDER BY created_at`, session.SourceExternal)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var values []session.Session
	for _, id := range ids {
		value, err := s.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (s *Store) CreateMessage(ctx context.Context, v message.Message) error {
	return s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO messages(id,coordination_id,sender_type,sender_id,recipient_type,recipient_id,content,reply_to,status,error,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.CoordinationID, v.Sender.Type, v.Sender.ID, v.Recipient.Type, v.Recipient.ID, v.Content, v.ReplyTo, v.Status, v.Error, v.CreatedAt.UTC().Format(timeFormat))
		return err
	})
}
func (s *Store) UpdateMessage(ctx context.Context, id string, status message.Status, e string) error {
	return s.withBusyRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE messages SET status=?,error=? WHERE id=?`, status, e, id)
		return err
	})
}
func (s *Store) ListMessages(ctx context.Context, cid string) ([]message.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,coordination_id,sender_type,sender_id,recipient_type,recipient_id,content,reply_to,status,error,created_at FROM messages WHERE coordination_id=? ORDER BY created_at`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]message.Message, 0)
	for rows.Next() {
		var v message.Message
		var t string
		if err := rows.Scan(&v.ID, &v.CoordinationID, &v.Sender.Type, &v.Sender.ID, &v.Recipient.Type, &v.Recipient.ID, &v.Content, &v.ReplyTo, &v.Status, &v.Error, &t); err != nil {
			return nil, err
		}
		v.CreatedAt, err = parseTime(t)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

const timeFormat = time.RFC3339Nano

func nowString() string                     { return time.Now().UTC().Format(timeFormat) }
func parseTime(v string) (time.Time, error) { return time.Parse(timeFormat, v) }
func formatOptionalTime(v *time.Time) string {
	if v == nil || v.IsZero() {
		return ""
	}
	return v.UTC().Format(timeFormat)
}
func parseOptionalTime(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	parsed, err := parseTime(v)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
