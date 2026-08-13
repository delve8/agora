package terminal

import "time"

// Snapshot is a read-only view of the virtual terminal screen.
type Snapshot struct {
	Sequence        uint64    `json:"sequence"`
	CapturedAt      time.Time `json:"captured_at"`
	Cols            int       `json:"cols"`
	Rows            int       `json:"rows"`
	Lines           []string  `json:"lines"`
	CursorCol       int       `json:"cursor_col"`
	CursorRow       int       `json:"cursor_row"`
	CursorVisible   bool      `json:"cursor_visible"`
	AlternateScreen bool      `json:"alternate_screen"`
	Healthy         bool      `json:"healthy"`
}

// Emulator consumes terminal output and exposes the current screen state.
type Emulator interface {
	Write([]byte) error
	Resize(cols, rows int)
	Snapshot() Snapshot
}
