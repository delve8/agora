package runtime

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/delve8/agora/internal/terminal"
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
		}, nil)
		close(done)
	}()
	input := []byte("/resume\r")
	if err := terminal.WriteData(right, input); err != nil {
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

func TestCopyTerminalInputAppliesResizeWithoutForwardingIt(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var master bytes.Buffer
	resized := make(chan [2]int, 1)
	got := make(chan string, 1)
	go func() {
		copyTerminalInput(&master, left, func() string { return "sess-1" }, func(_, text string) {
			got <- text
		}, func(cols, rows int) {
			resized <- [2]int{cols, rows}
		})
	}()
	if err := terminal.WriteResize(right, 160, 48); err != nil {
		t.Fatal(err)
	}
	if err := terminal.WriteData(right, []byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-resized:
		if size != [2]int{160, 48} {
			t.Fatalf("resize = %v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("resize was not applied")
	}
	select {
	case value := <-got:
		if value != "hello" {
			t.Fatalf("reported input = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("framed input was not reported")
	}
	if !bytes.Equal(master.Bytes(), []byte("hello\r")) {
		t.Fatalf("forwarded bytes = %q", master.Bytes())
	}
}
