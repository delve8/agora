package terminal

import (
	"strconv"
	"strings"
)

// Render returns the current screen as ANSI escape sequences, so a client that
// attaches to a session which has already produced output sees the screen
// instead of a blank terminal.
//
// The repaint is plain text: the emulator keeps no character attributes, so
// colours return with the Agent's next repaint. An empty screen renders as an
// empty string, because a fresh client already shows one.
func (s Snapshot) Render() string {
	if s.blank() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l") // keep the cursor out of the way while repainting
	if s.AlternateScreen {
		// The Agent is on the alternate screen, so the client has to switch too,
		// otherwise the repaint lands in the wrong buffer.
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[2J\x1b[H")
	for i, line := range s.Lines {
		if i > 0 {
			// Separators only between lines: a trailing newline on the last row
			// would scroll the screen and shift the whole repaint up by one.
			b.WriteString("\r\n")
		}
		b.WriteString(strings.TrimRight(line, " "))
	}
	b.WriteString("\x1b[" + strconv.Itoa(clamp(s.CursorRow, 0, len(s.Lines)-1)+1) +
		";" + strconv.Itoa(clamp(s.CursorCol, 0, s.Cols-1)+1) + "H")
	if s.CursorVisible {
		b.WriteString("\x1b[?25h")
	}
	return b.String()
}

func (s Snapshot) blank() bool {
	for _, line := range s.Lines {
		if strings.TrimRight(line, " ") != "" {
			return false
		}
	}
	return true
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if high >= low && value > high {
		return high
	}
	return value
}
