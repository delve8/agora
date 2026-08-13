package runtime

import (
	"testing"

	"github.com/delve8/agora/internal/terminal"
)

func TestPTYObservationRecordsBoundedFramesAndSnapshot(t *testing.T) {
	observation := newPTYObservation()
	observation.capacity = 2
	if err := observation.emulator.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	observation.record([]byte("\x1b[2;1Hworld"))
	observation.record([]byte("\x1b[2Jok"))
	observation.record([]byte("!"))

	if got := len(observation.frames); got != 2 {
		t.Fatalf("expected bounded frame history, got %d", got)
	}
	if observation.frames[0].Sequence != 2 || observation.frames[1].Sequence != 3 {
		t.Fatalf("unexpected frame sequences: %d, %d", observation.frames[0].Sequence, observation.frames[1].Sequence)
	}
	snapshot := observation.snapshot()
	if snapshot.Sequence != 3 || !snapshot.Healthy {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Lines == nil {
		t.Fatal("expected screen lines")
	}
}

func TestPTYObservationSnapshotTypeIsStable(t *testing.T) {
	var _ terminal.Snapshot = terminal.Snapshot{}
}
