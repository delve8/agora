package runtime

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestCopyTerminalInputPreservesBytesAndReportsSubmittedLine(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var master bytes.Buffer
	got := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		copyTerminalInput(&master, left, func() string { return "sess-1" }, func(id, text string) {
			got <- id + ":" + text
		})
		close(done)
	}()
	input := []byte("/resume\r")
	if _, err := right.Write(input); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-got:
		if value != "sess-1:/resume" {
			t.Fatalf("reported input = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("input was not reported")
	}
	if !bytes.Equal(master.Bytes(), input) {
		t.Fatalf("forwarded bytes = %q, want %q", master.Bytes(), input)
	}
}
