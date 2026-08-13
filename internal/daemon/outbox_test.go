package daemon

import (
	"testing"

	"github.com/delve8/agora/internal/protocol"
)

func TestOutboxIsBoundedAndReportsGap(t *testing.T) {
	o := newOutbox(2)
	for i := 0; i < 3; i++ {
		frame, err := protocol.NewEnvelope(protocol.SessionUpdate, map[string]int{"i": i})
		if err != nil {
			t.Fatal(err)
		}
		o.Add(frame)
	}
	if len(o.Items()) != 2 {
		t.Fatalf("expected bounded outbox, got %d", len(o.Items()))
	}
	if !o.Gap() {
		t.Fatal("expected gap marker")
	}
}
