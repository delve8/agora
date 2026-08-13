package terminal

import (
	"strings"
	"testing"
)

func TestVT10xAppliesANSIAndPreservesChunkBoundaries(t *testing.T) {
	e := NewVT10x(12, 3)
	for _, chunk := range [][]byte{
		[]byte("hello\x1b[2;1Hwo"),
		[]byte("rld\x1b[2J"),
		[]byte("ok"),
	} {
		if err := e.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}

	snapshot := e.Snapshot()
	if snapshot.Cols != 12 || snapshot.Rows != 3 {
		t.Fatalf("unexpected size: %dx%d", snapshot.Cols, snapshot.Rows)
	}
	if got := strings.TrimSpace(strings.Join(snapshot.Lines, "\n")); got != "ok" {
		t.Fatalf("unexpected screen: %q", got)
	}
	if !snapshot.Healthy {
		t.Fatal("expected healthy emulator")
	}
}

func TestVT10xTracksCursorAndAlternateScreen(t *testing.T) {
	e := NewVT10x(8, 2)
	if err := e.Write([]byte("x\x1b[?1049hY")); err != nil {
		t.Fatal(err)
	}
	snapshot := e.Snapshot()
	if !snapshot.AlternateScreen {
		t.Fatal("expected alternate screen")
	}
	if snapshot.CursorCol != 2 || snapshot.CursorRow != 0 {
		t.Fatalf("unexpected cursor: (%d,%d)", snapshot.CursorCol, snapshot.CursorRow)
	}
	if err := e.Write([]byte("\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().AlternateScreen {
		t.Fatal("expected primary screen")
	}
}

func TestVT10xResize(t *testing.T) {
	e := NewVT10x(4, 2)
	e.Resize(6, 3)
	snapshot := e.Snapshot()
	if snapshot.Cols != 6 || snapshot.Rows != 3 {
		t.Fatalf("unexpected resized terminal: %dx%d", snapshot.Cols, snapshot.Rows)
	}
}
