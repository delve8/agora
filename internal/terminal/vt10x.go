package terminal

import (
	"strings"
	"sync"
	"time"

	"github.com/hinshun/vt10x"
)

const (
	defaultCols = 80
	defaultRows = 24
)

type vt10xEmulator struct {
	mu      sync.Mutex
	term    vt10x.Terminal
	seq     uint64
	stamp   time.Time
	healthy bool
}

// NewVT10x returns an ANSI/VT terminal emulator backed by vt10x.
func NewVT10x(cols, rows int) Emulator {
	if cols < 1 {
		cols = defaultCols
	}
	if rows < 1 {
		rows = defaultRows
	}
	return &vt10xEmulator{
		term:    vt10x.New(vt10x.WithSize(cols, rows)),
		healthy: true,
	}
}

func (e *vt10xEmulator) Write(data []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.term.Write(data)
	if err != nil {
		e.healthy = false
		return err
	}
	e.seq++
	e.stamp = time.Now().UTC()
	return nil
}

func (e *vt10xEmulator) Resize(cols, rows int) {
	if cols < 1 || rows < 1 {
		return
	}
	e.mu.Lock()
	e.term.Resize(cols, rows)
	e.seq++
	e.stamp = time.Now().UTC()
	e.mu.Unlock()
}

func (e *vt10xEmulator) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()

	cols, rows := e.term.Size()
	lines := make([]string, rows)
	for y := 0; y < rows; y++ {
		var line strings.Builder
		line.Grow(cols)
		for x := 0; x < cols; x++ {
			line.WriteRune(e.term.Cell(x, y).Char)
		}
		lines[y] = strings.TrimRight(line.String(), " ")
	}
	cursor := e.term.Cursor()
	return Snapshot{
		Sequence:        e.seq,
		CapturedAt:      e.stamp,
		Cols:            cols,
		Rows:            rows,
		Lines:           lines,
		CursorCol:       cursor.X,
		CursorRow:       cursor.Y,
		CursorVisible:   e.term.CursorVisible(),
		AlternateScreen: e.term.Mode()&vt10x.ModeAltScreen != 0,
		Healthy:         e.healthy,
	}
}
