package runtime

import (
	"path/filepath"
	"strings"
	"testing"
)

// New sessions are synthesized from Agora's own flags, and the reporter
// extension must be present there too, otherwise a switch inside a fresh
// session could not be reported.
func TestPiProviderCommandInjectsReporterExtension(t *testing.T) {
	dir := t.TempDir()
	reporter := filepath.Join(dir, "agora-session-reporter.ts")
	manager := NewPiProvider(PiConfig{Binary: "pi", Provider: "anthropic", ReporterExtension: reporter})
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

// The caller's own arguments are the whole command once they are present: Agora
// adds its reporter extension and nothing else, so `pi update` reaches Pi as
// `pi update`.
func TestPiProviderForwardsAgentArgsUnchanged(t *testing.T) {
	dir := t.TempDir()
	reporter := filepath.Join(dir, "agora-session-reporter.ts")
	manager := NewPiProvider(PiConfig{Binary: "pi", Provider: "anthropic", Model: "model-id", ReporterExtension: reporter})
	got := manager.Command("native-id", "", []string{"--session", "native-id", "--model", "model-id", "initial prompt"})
	// The executable itself is resolved (the PATH `pi` may be the Agora
	// wrapper); everything after it is the caller's arguments, untouched.
	want := []string{"-e", reporter, "--session", "native-id", "--model", "model-id", "initial prompt"}
	if strings.Join(got[1:], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Pi arguments = %#v, want %#v", got[1:], want)
	}
}
