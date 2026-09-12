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

	"github.com/delve8/agora/internal/terminal"
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

func TestPiManagerResizesPTYFromAttach(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pi")
	script := `#!/bin/sh
trap 'printf "SIZE %s\r\n" "$(stty size)"' WINCH
printf 'SIZE %s\r\n' "$(stty size)"
while IFS= read -r line; do
  printf 'received: %s\r\n' "$line"
done
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewPiManager(PiConfig{Binary: binary})
	process, err := manager.Start("agora-resize", dir, "native-resize")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	conn, err := dialUnix(process.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var output strings.Builder
	buf := make([]byte, 4096)
	readUntil := func(needle string, timeout time.Duration) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if strings.Contains(output.String(), needle) {
				return true
			}
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, readErr := conn.Read(buf)
			if n > 0 {
				output.Write(buf[:n])
			}
			if readErr != nil && !os.IsTimeout(readErr) {
				return strings.Contains(output.String(), needle)
			}
		}
		return strings.Contains(output.String(), needle)
	}
	// Wait until the child has printed its startup size and installed the
	// WINCH trap, otherwise a resize can arrive before the handler exists.
	if !readUntil("SIZE 40 120", 2*time.Second) {
		t.Fatalf("initial PTY size output = %q", output.String())
	}
	if err := terminal.WriteResize(conn, 140, 50); err != nil {
		t.Fatal(err)
	}
	if !readUntil("SIZE 50 140", 2*time.Second) {
		t.Fatalf("resized PTY size output = %q", output.String())
	}
}

func TestPiManagerForwardsAgentArgsUnchanged(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pi")
	argsPath := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nsleep 2\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	reporter := filepath.Join(dir, "agora-session-reporter.ts")
	manager := NewPiManager(PiConfig{Binary: binary, ReporterExtension: reporter})
	_, err := manager.StartWithArgs("agora-args", dir, "", []string{"--session", "native-id", "--model", "model-id", "initial prompt"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(argsPath); readErr == nil {
			got := strings.Split(strings.TrimSpace(string(data)), "\n")
			// The Agora reporter extension is Agora-owned instrumentation, so it
			// is injected in front of the user's arguments, which stay verbatim.
			want := []string{"-e", reporter, "--session", "native-id", "--model", "model-id", "initial prompt"}
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("Pi arguments = %#v, want %#v", got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Pi did not receive forwarded arguments")
}

func dialUnix(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}

// New sessions are synthesized from Agora's own flags, and the reporter
// extension must be present there too, otherwise a switch inside a fresh
// session could not be reported.
func TestPiManagerCommandInjectsReporterExtension(t *testing.T) {
	dir := t.TempDir()
	reporter := filepath.Join(dir, "agora-session-reporter.ts")
	manager := NewPiManager(PiConfig{Binary: "pi", Provider: "anthropic", ReporterExtension: reporter})
	command := manager.Command("native-id", "", nil)
	joined := strings.Join(command, " ")
	if !strings.Contains(joined, "-e "+reporter) {
		t.Fatalf("reporter extension missing from %q", joined)
	}
	if !strings.Contains(joined, "--session-id native-id") {
		t.Fatalf("session id missing from %q", joined)
	}
	// A resumed session passes the transcript path instead.
	command = manager.Command("native-id", "/tmp/session.jsonl", nil)
	if !strings.Contains(strings.Join(command, " "), "--session /tmp/session.jsonl") {
		t.Fatalf("history path missing from %q", strings.Join(command, " "))
	}
	// Without a reporter the command stays exactly as before.
	manager.SetReporterExtension("")
	if got := strings.Join(manager.Command("native-id", "", nil), " "); strings.Contains(got, "-e") {
		t.Fatalf("reporter extension unexpectedly present: %q", got)
	}
}
