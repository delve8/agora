package terminal

import (
	"strings"
	"testing"
)

// A client that attaches to a session which already painted must be told what is
// on the screen, otherwise it stares at an empty terminal until the Agent
// happens to repaint.
func TestRenderRepaintsTheCurrentScreen(t *testing.T) {
	snapshot := Snapshot{
		Sequence: 12, Cols: 20, Rows: 3,
		Lines:         []string{"READY", "> hello", ""},
		CursorCol:     7,
		CursorRow:     1,
		CursorVisible: true,
	}
	want := "\x1b[?25l\x1b[2J\x1b[HREADY\r\n> hello\r\n\x1b[2;8H\x1b[?25h"
	if got := snapshot.Render(); got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

// The Agent may be on the alternate screen with the cursor hidden, and a client
// that attaches late has to follow it there.
func TestRenderFollowsTheAlternateScreen(t *testing.T) {
	snapshot := Snapshot{
		Sequence: 3, Cols: 10, Rows: 2,
		Lines:           []string{"full screen", ""},
		AlternateScreen: true,
	}
	got := snapshot.Render()
	for _, want := range []string{"\x1b[?1049h", "\x1b[?25l", "\x1b[2J\x1b[H", "full screen", "\x1b[1;1H"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Render() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "\x1b[?25h") {
		t.Fatalf("Render() = %q enabled a hidden cursor", got)
	}
}

// Nothing to repaint: a fresh client already shows an empty screen, and a cursor
// position outside the screen must not produce an out-of-range escape.
func TestRenderSkipsAnEmptyScreen(t *testing.T) {
	empty := Snapshot{Cols: 80, Rows: 24, Lines: make([]string, 24), CursorVisible: true}
	if got := empty.Render(); got != "" {
		t.Fatalf("Render() = %q, want nothing to repaint", got)
	}
	offscreen := Snapshot{Cols: 10, Rows: 2, Lines: []string{"x", ""}, CursorRow: 9, CursorCol: 42}
	if got := offscreen.Render(); !strings.Contains(got, "\x1b[2;10H") {
		t.Fatalf("Render() = %q, want the cursor clamped onto the screen", got)
	}
}
