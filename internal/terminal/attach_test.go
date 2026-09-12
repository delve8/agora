package terminal

import (
	"io"
	"testing"
	"time"
)

func TestAttachStreamDeliversDataAndResize(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	resized := make(chan [2]int, 1)
	stream := NewAttachStream(pr, func(cols, rows int) {
		resized <- [2]int{cols, rows}
	})
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := io.ReadFull(stream, buf[:8])
		if err != nil {
			done <- nil
			return
		}
		done <- append([]byte(nil), buf[:n]...)
	}()
	if err := WriteResize(pw, 140, 50); err != nil {
		t.Fatal(err)
	}
	if err := WriteData(pw, []byte("/resume\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-resized:
		if size != [2]int{140, 50} {
			t.Fatalf("resize = %v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("resize was not delivered")
	}
	select {
	case value := <-done:
		if string(value) != "/resume\r" {
			t.Fatalf("data = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("framed data was not delivered")
	}
}
