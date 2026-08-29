package runtime

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiManagerInteractivePTY(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pi")
	// This fake behaves like the part of Pi that matters to the transport:
	// it requires a terminal, renders a screen, and accepts submitted input.
	script := `#!/bin/sh
printf 'PI TUI READY\r\n'
while IFS= read -r line; do
  printf 'received: %s\r\n' "$line"
done
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewPiManager(PiConfig{Binary: binary, Provider: "anthropic", Model: "claude-sonnet-5"})
	process, err := manager.Start("agora-1", dir, "native-1")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if process.PID == 0 || !manager.IsRunning("agora-1") {
		t.Fatalf("process not running: %+v", process)
	}

	conn, err := dialUnix(process.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "PI TUI READY") {
		t.Fatalf("PTY output = %q", string(buf[:n]))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Send(ctx, "agora-1", "hello"); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !strings.Contains(output.String(), "received: hello") {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err = conn.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil && !os.IsTimeout(err) {
			break
		}
	}
	if !strings.Contains(output.String(), "received: hello") {
		t.Fatalf("submitted PTY output = %q", output.String())
	}

	snapshot, err := manager.Snapshot("agora-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Sequence == 0 || len(snapshot.Lines) == 0 {
		t.Fatalf("PTY snapshot was not updated: %+v", snapshot)
	}
	joined := strings.Join(snapshot.Lines, "\n")
	if !strings.Contains(joined, "PI TUI READY") || !strings.Contains(joined, "received: hello") {
		t.Fatalf("PTY snapshot = %q", joined)
	}
}

func dialUnix(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}
