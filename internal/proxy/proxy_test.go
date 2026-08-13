package proxy

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestResolveBinaryEnvPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveBinary([]string{"AGORA_CLAUDE_BINARY=" + path}, func(string) (string, error) { t.Fatal("PATH lookup should not run"); return "", nil }, os.Stat)
	if err != nil || got != path {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestResolveBinaryPathFallback(t *testing.T) {
	got, err := ResolveBinary(nil, func(name string) (string, error) {
		if name != "claude" {
			t.Fatalf("unexpected lookup %q", name)
		}
		return "/tmp/claude", nil
	}, os.Stat)
	if err != nil || got != "/tmp/claude" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRelayStdoutPreservesBytesAndDropsOversizedObservation(t *testing.T) {
	input := append(bytes.Repeat([]byte{'a'}, maxObservationLine+1), '\n')
	input = append(input, []byte(`{"type":"result","result":"ok"}`)...)
	input = append(input, '\n')
	queue := make(chan []byte, 4)
	var out bytes.Buffer
	var dropped atomic.Int64
	if err := relayStdout(bytes.NewReader(input), &out, queue, &dropped); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), input) {
		t.Fatal("stdout bytes changed")
	}
	if got := len(queue); got != 1 {
		t.Fatalf("expected one parseable line, got %d", got)
	}
}
