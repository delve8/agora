package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/protocol"
	"github.com/delve8/agora/internal/terminal"
)

func listingEntries() []protocol.SessionListEntry {
	now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	return []protocol.SessionListEntry{
		{SessionID: "daemon-1/pi://history-1", AgentSessionID: "pi://history-1", Agent: "pi", DisplayName: "old work", State: "stale", Source: "history", UpdatedAt: now.Add(-3 * time.Hour)},
		{SessionID: "daemon-1/pi://running-1", AgentSessionID: "pi://running-1", Agent: "pi", DisplayName: "live work", State: "running", Attachable: true, UpdatedAt: now.Add(-30 * time.Second)},
	}
}

// The numbering has to be stable and match what the user just saw, so attachable
// sessions come first.
func TestSortSessionListingsPutsAttachableFirst(t *testing.T) {
	entries := listingEntries()
	sortSessionListings(entries)
	if entries[0].SessionID != "daemon-1/pi://running-1" || entries[1].SessionID != "daemon-1/pi://history-1" {
		t.Fatalf("sorted order = %+v", entries)
	}
}

func TestFormatSessionListMarksHistoryAndShortensIds(t *testing.T) {
	entries := listingEntries()
	sortSessionListings(entries)
	now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	table := formatSessionList(entries, now, false)
	for _, want := range []string{"NAME", "live work", "running", "just now", "history", "3h ago", "pi://running-1"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table = %q, want it to contain %q", table, want)
		}
	}
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table has %d lines, want a header and two rows:\n%s", len(lines), table)
	}
	if !strings.HasPrefix(lines[0], "#") || !strings.Contains(lines[0], "SESSION") {
		t.Fatalf("header = %q", lines[0])
	}
	// No workspace column unless the list covers several workspaces.
	if strings.Contains(table, "WORKSPACE") {
		t.Fatalf("workspace column shown for a single workspace:\n%s", table)
	}
	if withWorkspace := formatSessionList(entries, now, true); !strings.Contains(withWorkspace, "WORKSPACE") {
		t.Fatalf("workspace column missing for an all-workspaces listing:\n%s", withWorkspace)
	}
}

// A session id is not memorable, so every prefix of the canonical id and of the
// provider id has to work, and an ambiguous prefix must be refused rather than
// guessed.
func TestResolveSessionArgumentMatchesIdsAndPrefixes(t *testing.T) {
	entries := listingEntries()
	checks := []struct {
		argument string
		want     string
	}{
		{"daemon-1/pi://running-1", "daemon-1/pi://running-1"},
		{"pi://running-1", "daemon-1/pi://running-1"},
		{"daemon-1/pi://run", "daemon-1/pi://running-1"},
		{"pi://hist", "daemon-1/pi://history-1"},
	}
	for _, check := range checks {
		entry, matches := matchSession(entries, check.argument)
		if matches != 1 || entry.SessionID != check.want {
			t.Fatalf("matchSession(%q) = %+v, %d matches, want %s", check.argument, entry, matches, check.want)
		}
	}
	if _, matches := matchSession(entries, "daemon-1/pi://"); matches != 2 {
		t.Fatalf("shared prefix matched %d sessions, want 2 (an ambiguous prefix must not be guessed)", matches)
	}
	if _, matches := matchSession(entries, "pi://nope"); matches != 0 {
		t.Fatalf("unknown prefix matched %d sessions", matches)
	}
	if err := ambiguousSessionError("daemon-1/pi://", entries); !strings.Contains(err.Error(), "matches 2 sessions") {
		t.Fatalf("ambiguous error = %v", err)
	}
}

func TestHumanAgeBuckets(t *testing.T) {
	now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	cases := map[time.Time]string{
		now.Add(-10 * time.Second): "just now",
		now.Add(-5 * time.Minute):  "5m ago",
		now.Add(-3 * time.Hour):    "3h ago",
		now.Add(-50 * time.Hour):   "2d ago",
		now.Add(5 * time.Minute):   "just now",
		time.Time{}:                "-",
	}
	for then, want := range cases {
		if got := humanAge(now, then); got != want {
			t.Fatalf("humanAge(%v) = %q, want %q", then, got, want)
		}
	}
}

// standInDaemon serves one canned reply per connection and reports the request
// it received, so the CLI's side of the protocol is pinned without a real Daemon.
func standInDaemon(t *testing.T, reply protocol.SessionListResponse) func() protocol.SessionListRequest {
	t.Helper()
	// A short path: unix sockets are limited to ~100 bytes and macOS temporary
	// directories are long.
	socket := filepath.Join("/tmp", fmt.Sprintf("agora-cli-list-%d.sock", time.Now().UnixNano()))
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	t.Setenv("AGORA_DAEMON_SOCKET", socket)
	requests := make(chan protocol.SessionListRequest, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request protocol.SessionListRequest
			decodeErr := json.NewDecoder(conn).Decode(&request)
			_ = json.NewEncoder(conn).Encode(reply)
			_ = conn.Close()
			if decodeErr == nil {
				requests <- request
			}
		}
	}()
	return func() protocol.SessionListRequest {
		t.Helper()
		select {
		case request := <-requests:
			return request
		case <-time.After(2 * time.Second):
			t.Fatal("the daemon stand-in received no request")
			return protocol.SessionListRequest{}
		}
	}
}

// Listing must ask the right question: the working directory, and a provider no
// Server accepts, so a Daemon that predates listing cannot turn the request into
// a new session.
func TestSessionListingsAsksForTheWorkingDirectory(t *testing.T) {
	lastRequest := standInDaemon(t, protocol.SessionListResponse{Sessions: listingEntries()})
	entries, err := sessionListings("/tmp/some/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	request := lastRequest()
	if request.Type != protocol.SessionList {
		t.Fatalf("request type = %q", request.Type)
	}
	if request.Workspace != "/tmp/some/project" {
		t.Fatalf("request workspace = %q", request.Workspace)
	}
	if request.Agent != unsupportedListAgent {
		t.Fatalf("request agent = %q, want the guard that stops an old Daemon from creating a session", request.Agent)
	}
}

// A Daemon that predates listing answers with a Server error about the guard
// provider; that has to read as an upgrade instruction.
func TestSessionListingsExplainsAnOldDaemon(t *testing.T) {
	standInDaemon(t, protocol.SessionListResponse{Error: `unsupported agent "agora-session-list"`})
	_, err := sessionListings("/tmp")
	if err == nil {
		t.Fatal("expected an error from an old daemon")
	}
	if _, ok := err.(sessionListingUnsupportedError); !ok {
		t.Fatalf("error = %v (%T), want a sessionListingUnsupportedError", err, err)
	}
	if !strings.Contains(err.Error(), "restart it") {
		t.Fatalf("error = %v, want it to tell the user to restart the daemon", err)
	}
}

// Attaching must paint the screen the Daemon reports, so the client does not
// start with an empty terminal.
func TestSessionScreenRendersWhatTheDaemonReports(t *testing.T) {
	snapshot := terminal.Snapshot{
		Sequence: 7, Cols: 40, Rows: 3,
		Lines:         []string{"PAINTED BY THE AGENT", "", ""},
		CursorCol:     0,
		CursorRow:     1,
		CursorVisible: true,
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	standInSnapshotDaemon(t, protocol.SessionSnapshotResponse{Snapshot: encoded})
	screen := sessionScreen("daemon-1/pi://running-1")
	if !strings.Contains(screen, "PAINTED BY THE AGENT") {
		t.Fatalf("screen = %q, want the Daemon's screen", screen)
	}
	if !strings.Contains(screen, "\x1b[2J") || !strings.Contains(screen, "\x1b[?25h") {
		t.Fatalf("screen = %q, want a full repaint ending with a visible cursor", screen)
	}
}

// Anything unexpected leaves the screen empty: attaching must work even when the
// Daemon cannot answer, for instance because it predates the request.
func TestSessionScreenStaysEmptyWhenTheDaemonCannotAnswer(t *testing.T) {
	standInSnapshotDaemon(t, protocol.SessionSnapshotResponse{Error: "session is not running"})
	if screen := sessionScreen("daemon-1/pi://gone"); screen != "" {
		t.Fatalf("screen = %q, want nothing", screen)
	}
}

func standInSnapshotDaemon(t *testing.T, reply protocol.SessionSnapshotResponse) {
	t.Helper()
	socket := filepath.Join("/tmp", fmt.Sprintf("agora-cli-screen-%d.sock", time.Now().UnixNano()))
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	t.Setenv("AGORA_DAEMON_SOCKET", socket)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request protocol.SessionSnapshotRequest
			_ = json.NewDecoder(conn).Decode(&request)
			if request.Type != protocol.SessionSnapshot {
				_ = json.NewEncoder(conn).Encode(protocol.SessionSnapshotResponse{Error: "unexpected request"})
			} else {
				_ = json.NewEncoder(conn).Encode(reply)
			}
			_ = conn.Close()
		}
	}()
}
