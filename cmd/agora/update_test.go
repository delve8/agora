package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// captureStdout collects what a command prints, since that is part of its
// contract (`agora update --check` is read by scripts).
func captureStdout(t *testing.T, run func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	done := make(chan string, 1)
	go func() {
		var buffer bytes.Buffer
		_, _ = io.Copy(&buffer, reader)
		done <- buffer.String()
	}()
	run()
	_ = writer.Close()
	os.Stdout = original
	return <-done
}

// updateFixture serves what the Server serves for an update: the installer, the
// checksums of the artifact, and the artifact version.
type updateFixture struct {
	server     *httptest.Server
	installLog string
	script     string
	wrapper    string
	hash       string
}

// serveChecksum republishes the artifact checksum the Server reports.
func (f *updateFixture) serveChecksum(_ *testing.T, hash string) {
	f.hash = hash
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	fixture := &updateFixture{installLog: filepath.Join(t.TempDir(), "installer.log"), hash: strings.Repeat("a", 64)}
	fixture.wrapper = "#!/bin/sh\n# Agora PATH wrapper for tests\n"
	// The usage line is how this command recognises an installer that knows
	// --no-restart, so the fixture documents it like the real one does.
	fixture.script = "#!/bin/sh\n" +
		"# usage: install.sh [--no-restart]\n" +
		"printf 'args:%s\\n' \"$*\" > " + fixture.installLog + "\n" +
		"cat >> " + fixture.installLog + "\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/download/install.sh", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, fixture.script)
	})
	mux.HandleFunc("/download/version.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "2.0.0\n")
	})
	mux.HandleFunc("/download/agora-wrapper.sh", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, fixture.wrapper)
	})
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		artifact, err := artifactName()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "%s  %s\n", fixture.hash, artifact)
	})
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

// installFullInstallation writes what a complete installation looks like: the
// binary, the wrapper, and the pi/claude links. --check treats all three as one
// installation, so tests that only care about the binary verdict need this.
func installFullInstallation(t *testing.T, options updateOptions, fixture *updateFixture, binary []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(options.installDir, "agora"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.installDir, "agora-wrapper.sh"), []byte(fixture.wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pi", "claude"} {
		if err := os.Symlink("agora-wrapper.sh", filepath.Join(options.installDir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func testUpdateOptions(t *testing.T) updateOptions {
	t.Helper()
	options := defaultUpdateOptions()
	options.installDir = t.TempDir()
	// A service that is installed but never actually restarted in the test.
	options.unitExists = func(string) bool { return true }
	options.runVersion = func(string) (string, error) { return "1.0.0", nil }
	return options
}

// `agora update --check` has to answer without touching anything, and it has to
// tell apart "up to date" from "the Server publishes something else".
func TestUpdateCheckComparesInstalledAndPublishedBuilds(t *testing.T) {
	fixture := newUpdateFixture(t)
	options := testUpdateOptions(t)
	options.baseURL = fixture.server.URL
	options.check = true
	target := filepath.Join(options.installDir, "agora")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	output := captureStdout(t, func() {
		if err := applyUpdateOptions(options); err != nil {
			t.Fatalf("check: %v", err)
		}
	})
	if !strings.Contains(output, "update available: 1.0.0 -> 2.0.0") {
		t.Fatalf("check output = %q, want an update verdict", output)
	}
	if _, err := os.Stat(fixture.installLog); !os.IsNotExist(err) {
		t.Fatal("--check ran the installer")
	}

	// The published hash now matches what is installed, and the rest of the
	// installation is in place: nothing to do.
	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(installed)
	fixture.serveChecksum(t, hex.EncodeToString(sum[:]))
	options.runVersion = func(string) (string, error) { return "2.0.0", nil }
	installFullInstallation(t, options, fixture, installed)
	output = captureStdout(t, func() {
		if err := applyUpdateOptions(options); err != nil {
			t.Fatalf("check: %v", err)
		}
	})
	if !strings.Contains(output, "up to date (2.0.0)") {
		t.Fatalf("check output = %q, want an up-to-date verdict", output)
	}
}

// The update path delegates to the Server's installer (one implementation of
// download/verify/replace), tells it not to restart, and restarts the service
// itself when a service is installed.
func TestUpdateRunsTheServerInstallerAndRestartsTheService(t *testing.T) {
	fixture := newUpdateFixture(t)
	options := testUpdateOptions(t)
	options.baseURL = fixture.server.URL
	var restarted []string
	options.runRestart = func(name string, args ...string) error {
		restarted = append([]string{name}, args...)
		return nil
	}
	options.runInstaller = func(script []byte, args []string) error {
		if string(script) != fixture.script {
			t.Fatalf("installer script = %q, want the served one", script)
		}
		return runInstallerScript(script, args)
	}

	output := captureStdout(t, func() {
		if err := applyUpdateOptions(options); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	if !strings.Contains(output, "updated") || !strings.Contains(output, "2.0.0") {
		t.Fatalf("update output = %q, want the new version", output)
	}
	logged, err := os.ReadFile(fixture.installLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(logged)
	for _, want := range []string{"--server " + fixture.server.URL, "--install-dir " + options.installDir, "--no-restart"} {
		if !strings.Contains(text, want) {
			t.Fatalf("installer arguments = %q, want %q", text, want)
		}
	}
	wantName := "systemctl"
	if runtime.GOOS == "darwin" {
		wantName = "launchctl"
	}
	if len(restarted) == 0 || restarted[0] != wantName {
		t.Fatalf("service restart = %v, want a %s restart", restarted, wantName)
	}
	if !strings.Contains(output, "session-host") {
		t.Fatalf("update output = %q, want the note about sessions keeping their old session-host", output)
	}
}

// --no-restart leaves the daemon alone and says so.
func TestUpdateHonoursNoRestart(t *testing.T) {
	fixture := newUpdateFixture(t)
	options := testUpdateOptions(t)
	options.baseURL = fixture.server.URL
	options.noRestart = true
	restarted := false
	options.runRestart = func(string, ...string) error {
		restarted = true
		return nil
	}
	output := captureStdout(t, func() {
		if err := applyUpdateOptions(options); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	if restarted {
		t.Fatal("--no-restart restarted the service")
	}
	if !strings.Contains(output, "--no-restart") {
		t.Fatalf("update output = %q, want the no-restart notice", output)
	}
}

// A dev install is a symlink into a checkout; replacing it would leave the
// checkout inconsistent, so the update refuses unless forced.
func TestUpdateRefusesToClobberASourceCheckout(t *testing.T) {
	fixture := newUpdateFixture(t)
	options := testUpdateOptions(t)
	options.baseURL = fixture.server.URL
	checkout := t.TempDir()
	built := filepath.Join(checkout, "bin", "agora")
	if err := os.MkdirAll(filepath.Dir(built), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(built, []byte("checkout build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(built, filepath.Join(options.installDir, "agora")); err != nil {
		t.Fatal(err)
	}

	err := applyUpdateOptions(options)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("update of a symlinked install = %v, want a refusal", err)
	}
	if _, err := os.Stat(fixture.installLog); !os.IsNotExist(err) {
		t.Fatal("the refused update still ran the installer")
	}
}

// A concurrent update must not race the same install directory.
func TestUpdateLockIsHeldByOneProcessAtATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.lock")
	unlock, err := lockUpdate(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockUpdate(path); err == nil || !strings.Contains(err.Error(), "another") {
		t.Fatalf("second lock = %v, want a refusal", err)
	}
	unlock()
	if _, err := lockUpdate(path); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
}

// A lock left behind by a killed update must not wedge later ones.
func TestUpdateLockIgnoresAStaleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.lock")
	// pid 1 is init, which this test cannot have started but is always alive;
	// use an improbable pid instead to model a dead owner.
	if err := os.WriteFile(path, []byte("4194302\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(path)
	if err != nil {
		t.Fatalf("stale lock blocked the update: %v", err)
	}
	unlock()
}

func TestServiceRestartCommands(t *testing.T) {
	home := "/home/test"
	if got := serviceUnitPath("darwin", home); got != "/home/test/Library/LaunchAgents/com.delve8.agora.daemon.plist" {
		t.Fatalf("darwin unit = %q", got)
	}
	if got := serviceUnitPath("linux", home); got != "/home/test/.config/systemd/user/agora-daemon.service" {
		t.Fatalf("linux unit = %q", got)
	}
	if got := serviceUnitPath("windows", home); got != "" {
		t.Fatalf("unsupported unit = %q", got)
	}
	darwin := serviceRestartCommand("darwin", home, 501)
	if strings.Join(darwin, " ") != "launchctl kickstart -k gui/501/com.delve8.agora.daemon" {
		t.Fatalf("darwin restart = %v", darwin)
	}
	linux := serviceRestartCommand("linux", home, 501)
	if strings.Join(linux, " ") != "systemctl --user restart agora-daemon.service" {
		t.Fatalf("linux restart = %v", linux)
	}
	if command := serviceRestartCommand("windows", home, 501); command != nil {
		t.Fatalf("unsupported restart = %v", command)
	}
}

func TestChecksumForReadsSha256sumFormat(t *testing.T) {
	body := "aaaa  agora-linux-amd64\nbbbb  *agora-darwin-arm64\n"
	if got := checksumFor(body, "agora-darwin-arm64"); got != "bbbb" {
		t.Fatalf("checksum = %q, want the binary-mode entry", got)
	}
	if got := checksumFor(body, "agora-windows-amd64"); got != "" {
		t.Fatalf("checksum for an unpublished artifact = %q", got)
	}
}

// `agora version` is what `agora update --check` asks the installed binary for.
func TestVersionCommandPrintsBuildInfo(t *testing.T) {
	originalVersion, originalCommit, originalDate := version, commit, date
	t.Cleanup(func() { version, commit, date = originalVersion, originalCommit, originalDate })
	t.Setenv("AGORA_VERSION", "")
	version, commit, date = "1.2.3", "abc1234", "2026-01-01T00:00:00Z"

	if got := buildVersion(); got != "1.2.3" {
		t.Fatalf("buildVersion = %q", got)
	}
	output := captureStdout(t, func() {
		if err := runVersion([]string{"--porcelain"}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.TrimSpace(output) != "1.2.3" {
		t.Fatalf("version --porcelain = %q", output)
	}
	t.Setenv("AGORA_VERSION", "9.9.9")
	if got := buildVersion(); got != "9.9.9" {
		t.Fatalf("buildVersion with AGORA_VERSION = %q, want the override", got)
	}
}

func TestUpdateRejectsUnknownOptions(t *testing.T) {
	if err := runUpdate([]string{"--wat"}); err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("unknown option = %v", err)
	}
}

// `--help` must not reach the network or the service: it only answers.
func TestUpdateHelpChangesNothing(t *testing.T) {
	t.Setenv("AGORA_SERVER_URL", "http://127.0.0.1:1")
	output := captureStdout(t, func() {
		if err := runUpdate([]string{"--help"}); err != nil {
			t.Fatalf("--help: %v", err)
		}
	})
	if !strings.Contains(output, "usage: agora update") {
		t.Fatalf("--help output = %q, want the usage", output)
	}
}

// A Server older than this command serves an installer that does not know
// --no-restart. Passing it would make the installer refuse the whole update, so
// the flag is only passed when the script supports it.
func TestUpdateCopesWithAnInstallerThatPredatesNoRestart(t *testing.T) {
	fixture := newUpdateFixture(t)
	// Mimic the deployed installer's argument parser.
	// Deliberately does not spell the flag with its leading dashes: that is how
	// this command recognises a script which supports it.
	fixture.script = "#!/bin/sh\n" +
		"case \"$*\" in *no-restart*) echo 'install.sh: unknown argument' >&2; exit 2 ;; esac\n" +
		"printf 'args:%s\\n' \"$*\" > " + fixture.installLog + "\n"
	options := testUpdateOptions(t)
	options.baseURL = fixture.server.URL
	options.runRestart = func(string, ...string) error { return nil }
	output := captureStdout(t, func() {
		if err := applyUpdateOptions(options); err != nil {
			t.Fatalf("update against an older Server: %v", err)
		}
	})
	logged, err := os.ReadFile(fixture.installLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "--no-restart") {
		t.Fatalf("installer arguments = %q, want the flag omitted", logged)
	}
	if !strings.Contains(output, "predates --no-restart") {
		t.Fatalf("update output = %q, want the compatibility note", output)
	}
}

// This deployment resets curl's TLS ClientHello while wget works, and another
// network may reset Go's. Fetching therefore has to fall through the same
// downloaders the served installer tries, instead of pinning one HTTP stack.
func TestFetchFallsBackToAnotherDownloader(t *testing.T) {
	original := updateDownloaders
	t.Cleanup(func() { updateDownloaders = original })

	failed := errors.New("connection reset by peer")
	attempts := 0
	updateDownloaders = []func(string) ([]byte, error){
		func(string) ([]byte, error) { attempts++; return nil, failed },
		func(string) ([]byte, error) { attempts++; return []byte("payload"), nil },
	}
	body, err := fetchBytes("https://example.invalid/artifact")
	if err != nil || string(body) != "payload" {
		t.Fatalf("fetchBytes = %q, %v, want the second downloader's body", body, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the fallback to run", attempts)
	}

	updateDownloaders = []func(string) ([]byte, error){
		func(string) ([]byte, error) { return nil, failed },
		func(string) ([]byte, error) { return nil, errors.New("second failure") },
	}
	if _, err := fetchBytes("https://example.invalid/artifact"); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("all downloaders failing = %v, want the first error reported", err)
	}
}

// The external downloaders run the tools that exist and are quiet about their
// own failures.
// A stalled downloader must not hold the whole update: the arguments carry
// timeouts, because curl's default is to wait forever and wget's is to retry 20
// times with a 900s read timeout.
func TestDownloaderArgumentsAreBounded(t *testing.T) {
	for _, args := range [][]string{wgetArgs("u", "d"), curlArgs("u", "d")} {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "timeout") {
			t.Errorf("args %q have no timeout", joined)
		}
	}
	if joined := strings.Join(wgetArgs("u", "d"), " "); !strings.Contains(joined, "--tries=2") {
		t.Errorf("wget args %q keep the default retry count", joined)
	}
}

func TestFetchWithCommandUsesTheTool(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "fake-wget")
	payload := "artifact bytes"
	// Read the destination from -O rather than from a fixed position, so the
	// downloader's extra bounding flags do not have to be mirrored here.
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in -O) shift; dest=\"$1\" ;; esac; shift; done\n" +
		"printf '%s' '" + payload + "' > \"$dest\"\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fetch := fetchWithCommand(tool, wgetArgs)
	body, err := fetch("https://example.invalid/artifact")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != payload {
		t.Fatalf("body = %q, want what the tool wrote", body)
	}
	if _, err := fetchWithCommand(filepath.Join(dir, "missing-tool"), wgetArgs)("https://example.invalid/x"); err == nil {
		t.Fatal("a missing downloader did not fail")
	}
}

// The PATH wrapper is what makes `pi` and `claude` create managed sessions, so
// --check has to cover it: a fresh binary with a stale wrapper silently keeps
// old behaviour.
func TestUpdateCheckReportsAStaleWrapper(t *testing.T) {
	fixture := newUpdateFixture(t)
	options := testUpdateOptions(t)
	options.baseURL = fixture.server.URL
	options.check = true
	target := filepath.Join(options.installDir, "agora")
	installed := []byte("current binary")
	if err := os.WriteFile(target, []byte("current binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(installed)
	fixture.serveChecksum(t, hex.EncodeToString(sum[:]))
	options.runVersion = func(string) (string, error) { return "2.0.0", nil }

	// Wrapper missing, links missing: not up to date.
	output := captureStdout(t, func() {
		if err := applyUpdateOptions(options); err != nil {
			t.Fatalf("check: %v", err)
		}
	})
	if !strings.Contains(output, "PATH wrapper is not") {
		t.Fatalf("check output = %q, want the wrapper reported", output)
	}
	if strings.Contains(output, "up to date") {
		t.Fatalf("check output = %q, want no up-to-date verdict", output)
	}

	// Wrapper and links in place: up to date.
	installFullInstallation(t, options, fixture, installed)
	output = captureStdout(t, func() {
		if err := applyUpdateOptions(options); err != nil {
			t.Fatalf("check: %v", err)
		}
	})
	if !strings.Contains(output, "up to date (2.0.0)") {
		t.Fatalf("check output = %q, want an up-to-date verdict", output)
	}
	if !strings.Contains(output, "pi, claude -> agora-wrapper.sh") {
		t.Fatalf("check output = %q, want the links described", output)
	}
}

// A provider binary the user installed at those names is not the Agora wrapper;
// the installer leaves it alone and --check must not call it up to date.
func TestUpdateCheckDistinguishesAProviderBinary(t *testing.T) {
	for _, name := range []string{"pi", "claude"} {
		dir := t.TempDir()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho real provider\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		wrapper, description := inspectWrapper(dir)
		if wrapper.linked {
			t.Fatalf("%s: a provider binary counted as the Agora wrapper (%s)", name, description)
		}
		if !strings.Contains(description, "not the Agora wrapper") {
			t.Fatalf("%s: description = %q, want it named as a provider binary", name, description)
		}
	}
}
