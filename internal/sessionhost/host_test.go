package sessionhost

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/terminal"
)

func TestHostLifecycleAndAuthenticatedControl(t *testing.T) {
	runtimeDir := t.TempDir()
	host, err := New(Config{
		HostID:     "host-test",
		SessionID:  "daemon/test/pi://native-test",
		DaemonID:   "test",
		Agent:      "pi",
		Workspace:  t.TempDir(),
		Command:    []string{"/bin/sh", "-c", "sleep 30"},
		RuntimeDir: runtimeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run() }()

	metadataPath := filepath.Join(runtimeDir, "metadata.json")
	var client *Client
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(metadataPath); statErr == nil {
			client, err = NewClient(metadataPath)
			if err == nil {
				if _, err = client.State(context.Background()); err == nil {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil || err != nil {
		select {
		case runErr := <-done:
			t.Fatalf("host did not become controllable: client=%v err=%v run=%v", client != nil, err, runErr)
		default:
			t.Fatalf("host did not become controllable: client=%v err=%v", client != nil, err)
		}
	}
	if got := client.Metadata().Agent; got != "pi" {
		t.Fatalf("agent = %q, want pi", got)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session-host did not exit after stopping its Agent")
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory still exists after cleanup: %v", err)
	}
}

func TestHostRejectsInvalidToken(t *testing.T) {
	runtimeDir := t.TempDir()
	host, err := New(Config{SessionID: "session-1", Agent: "pi", Workspace: t.TempDir(), Command: []string{"/bin/sh", "-c", "sleep 30"}, RuntimeDir: runtimeDir})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run() }()
	metadataPath := filepath.Join(runtimeDir, "metadata.json")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(metadataPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	metadata, err := LoadMetadata(metadataPath)
	if err != nil {
		select {
		case runErr := <-done:
			t.Fatalf("load metadata: %v (host exited: %v)", err, runErr)
		default:
			t.Fatal(err)
		}
	}
	conn, err := (&netDialer{}).dial(context.Background(), metadata.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request{Version: MetadataVersion, Token: "wrong", Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	var reply response
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.OK || reply.Error != "invalid host token" {
		t.Fatalf("unexpected invalid-token response: %+v", reply)
	}
	client, err := NewClient(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Stop(context.Background())
	<-done
}

// Attaching to a session that has already painted must show the screen. Without
// a replay the client sees an empty terminal until the Agent happens to write
// again, which is exactly what a wrapper that attaches after the Agent started
// used to experience.
func TestAttachReplaysTheCurrentScreen(t *testing.T) {
	runtimeDir := t.TempDir()
	// The Agent paints immediately and then stays quiet: only a replay can tell
	// the attaching client what is on the screen.
	host, err := New(Config{
		HostID:     "host-replay",
		SessionID:  "daemon/test/claude://native-replay",
		DaemonID:   "test",
		Agent:      "claude",
		Workspace:  t.TempDir(),
		Command:    []string{"/bin/sh", "-c", "printf 'PAINTED\r\\n'; sleep 30"},
		RuntimeDir: runtimeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run() }()
	var client *Client
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		if client != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.Stop(stopCtx)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("host did not exit after stopping its Agent")
		}
	})

	metadataPath := filepath.Join(runtimeDir, "metadata.json")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(metadataPath); statErr == nil {
			if client, err = NewClient(metadataPath); err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("host did not become controllable: %v", err)
	}

	// Wait for the Agent to have painted, so the attach cannot race it.
	painted := time.Now().Add(5 * time.Second)
	for time.Now().Before(painted) {
		conn := dialAttach(t, client.AttachSocket())
		data := readAttach(t, conn)
		if strings.Contains(data, "PAINTED") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("an attaching client never saw the screen the Agent painted")
}

func TestAttachResizesThePTY(t *testing.T) {
	runtimeDir := t.TempDir()
	host, err := New(Config{
		HostID:     "host-resize",
		SessionID:  "daemon/test/pi://native-resize",
		DaemonID:   "test",
		Agent:      "pi",
		Workspace:  t.TempDir(),
		Command:    []string{"/bin/sh", "-c", "sleep 30"},
		RuntimeDir: runtimeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run() }()
	var client *Client
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		if client != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.Stop(stopCtx)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("host did not exit after stopping its Agent")
		}
	})

	metadataPath := filepath.Join(runtimeDir, "metadata.json")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(metadataPath); statErr == nil {
			if client, err = NewClient(metadataPath); err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("host did not become controllable: %v", err)
	}

	conn := dialAttach(t, client.AttachSocket())
	if err := terminal.WriteResize(conn, 140, 50); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var snapshot terminal.Snapshot
		if err := client.Call(context.Background(), "snapshot", nil, &snapshot); err == nil && snapshot.Cols == 140 && snapshot.Rows == 50 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("attaching client never resized the PTY")
}

func dialAttach(t *testing.T, socket string) net.Conn {
	t.Helper()
	if socket == "" {
		t.Fatal("host reported no attach socket")
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Fatalf("dial attach socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readAttach(t *testing.T, conn net.Conn) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	data, err := io.ReadAll(conn)
	if err != nil && !os.IsTimeout(err) {
		t.Fatalf("read attach socket: %v", err)
	}
	return string(data)
}
