package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/delve8/agora/internal/config"
)

const (
	// These names are shared with the served installer: it writes the same unit
	// files, and `agora update` restarts what the installer enabled.
	updateServiceLabel = "com.delve8.agora.daemon"
	updateServiceUnit  = "agora-daemon.service"
	updateLockName     = "update.lock"
)

const updateUsage = `usage: agora update [--check] [--no-restart] [--force] [--server <url>] [--install-dir <dir>]

Updates this machine's agora binary and PATH wrapper from the Agora Server it is
paired with, then restarts the background service.

  --check              report the installed and published versions, change nothing
  --no-restart         install the new binary but leave the service alone
  --force              replace a symlinked install (for example a source checkout)
  --server <url>       Agora Server to update from (defaults to the paired one)
  --install-dir <dir>  where agora lives (defaults to $AGORA_INSTALL_DIR or ~/.local/bin)

Only the daemon on this machine is updated. The Agora Server itself is a
container image: redeploying it is a separate, deliberate step.`

type updateOptions struct {
	check      bool
	noRestart  bool
	force      bool
	serverURL  string
	installDir string

	// Seams for tests: none of these touch a real service or shell.
	baseURL      string
	unitExists   func(string) bool
	runInstaller func(script []byte, args []string) error
	runRestart   func(name string, args ...string) error
	runVersion   func(binary string) (string, error)
}

func defaultUpdateOptions() updateOptions {
	return updateOptions{
		unitExists:   fileExists,
		runInstaller: runInstallerScript,
		runRestart:   runCommand,
		runVersion:   installedBinaryVersion,
	}
}

func runUpdate(args []string) error {
	options := defaultUpdateOptions()
	help, err := parseUpdateArgs(args, &options)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	return applyUpdateOptions(options)
}

// parseUpdateArgs fills options from the command line. It is separate from
// running the update so tests can exercise both halves directly. help reports
// that the usage was printed and nothing else should happen.
func parseUpdateArgs(args []string, options *updateOptions) (bool, error) {
	for index := 0; index < len(args); index++ {
		value := args[index]
		switch {
		case value == "--check":
			options.check = true
		case value == "--no-restart":
			options.noRestart = true
		case value == "--force":
			options.force = true
		case value == "--server":
			if index+1 >= len(args) {
				return false, fmt.Errorf("--server requires a value\n\n%s", updateUsage)
			}
			index++
			options.serverURL = args[index]
		case strings.HasPrefix(value, "--server="):
			options.serverURL = strings.TrimPrefix(value, "--server=")
		case value == "--install-dir":
			if index+1 >= len(args) {
				return false, fmt.Errorf("--install-dir requires a value\n\n%s", updateUsage)
			}
			index++
			options.installDir = args[index]
		case strings.HasPrefix(value, "--install-dir="):
			options.installDir = strings.TrimPrefix(value, "--install-dir=")
		case value == "-h" || value == "--help":
			fmt.Fprintln(os.Stdout, updateUsage)
			return true, nil
		default:
			return false, fmt.Errorf("unknown option %q for `agora update`\n\n%s", value, updateUsage)
		}
	}

	return false, nil
}

// applyUpdateOptions is the update itself, with options already resolved.
func applyUpdateOptions(options updateOptions) error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("agora update supports linux and darwin, not %s", runtime.GOOS)
	}
	base, err := resolveUpdateServer(options)
	if err != nil {
		return err
	}
	installDir, err := resolveUpdateInstallDir(options)
	if err != nil {
		return err
	}
	if options.check {
		return checkForUpdate(options, base, installDir)
	}
	return applyUpdate(options, base, installDir)
}

func resolveUpdateServer(options updateOptions) (string, error) {
	if options.baseURL != "" {
		return strings.TrimRight(options.baseURL, "/"), nil
	}
	if value := strings.TrimSpace(options.serverURL); value != "" {
		return strings.TrimRight(value, "/"), nil
	}
	if value := strings.TrimSpace(os.Getenv("AGORA_SERVER_URL")); value != "" {
		return strings.TrimRight(value, "/"), nil
	}
	if value, err := config.ResolveServerURL(os.Getenv("AGORA_CONFIG_PATH")); err == nil && strings.TrimSpace(value) != "" {
		return strings.TrimRight(strings.TrimSpace(value), "/"), nil
	}
	return "", fmt.Errorf("no Agora Server to update from: pass --server <url>, or pair this machine first (`agora pair <code>`)")
}

func resolveUpdateInstallDir(options updateOptions) (string, error) {
	dir := strings.TrimSpace(options.installDir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("AGORA_INSTALL_DIR"))
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		dir = filepath.Join(home, ".local", "bin")
	}
	return filepath.Abs(dir)
}

// artifactName is the published file for this platform, matching the names the
// installer asks for.
func artifactName() (string, error) {
	arch := runtime.GOARCH
	switch arch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported architecture: %s", arch)
	}
	return fmt.Sprintf("agora-%s-%s", runtime.GOOS, arch), nil
}

type publishedBuild struct {
	version string
	hash    string
}

// fetchPublished reads what the Server currently offers: the artifact hash from
// checksums.txt and, when a newer Server publishes one, the artifact version.
func fetchPublished(base, artifact string) (publishedBuild, error) {
	checksums, err := fetchText(base + "/download/checksums.txt")
	if err != nil {
		return publishedBuild{}, fmt.Errorf("read %s/download/checksums.txt: %w", base, err)
	}
	build := publishedBuild{hash: checksumFor(checksums, artifact)}
	if build.hash == "" {
		return publishedBuild{}, fmt.Errorf("%s/download/checksums.txt has no entry for %s", base, artifact)
	}
	// version.txt is newer than checksums.txt, so an older Server simply cannot
	// answer this question yet.
	if version, err := fetchText(base + "/download/version.txt"); err == nil {
		build.version = strings.TrimSpace(version)
	}
	return build, nil
}

func checkForUpdate(options updateOptions, base, installDir string) error {
	artifact, err := artifactName()
	if err != nil {
		return err
	}
	target := filepath.Join(installDir, "agora")
	published, err := fetchPublished(base, artifact)
	if err != nil {
		return err
	}
	installed, installedErr := fileSHA256(target)
	if installedErr != nil {
		return fmt.Errorf("%s is not installed here: %w\nRun the installer from the Server, or pass --install-dir", target, installedErr)
	}
	installedVersion := "unknown"
	if options.runVersion != nil {
		if value, err := options.runVersion(target); err == nil {
			installedVersion = value
		}
	}

	fmt.Fprintf(os.Stdout, "installed  %s  %s  sha256 %s\n", installedVersion, target, shortHash(installed))
	publishedName := published.version
	if publishedName == "" {
		publishedName = "unknown"
	}
	fmt.Fprintf(os.Stdout, "published  %s  %s/download/%s  sha256 %s\n", publishedName, base, artifact, shortHash(published.hash))

	switch {
	case installed == published.hash:
		fmt.Fprintf(os.Stdout, "up to date (%s)\n", publishedName)
	case published.version != "" && installedVersion != "unknown" && installedVersion != published.version:
		fmt.Fprintf(os.Stdout, "update available: %s -> %s (run `agora update`)\n", installedVersion, publishedName)
	default:
		fmt.Fprintf(os.Stdout, "the installed binary differs from the published %s build (local or older build); run `agora update` to replace it\n", publishedName)
	}
	return nil
}

func applyUpdate(options updateOptions, base, installDir string) error {
	artifact, err := artifactName()
	if err != nil {
		return err
	}
	target := filepath.Join(installDir, "agora")
	if err := ensureUpdateTarget(target, installDir, options.force); err != nil {
		return err
	}
	// Read what is being installed before replacing anything, so the summary can
	// name the new version even if the network dies later.
	published, err := fetchPublished(base, artifact)
	if err != nil {
		return err
	}

	unlock, err := lockUpdate(filepath.Join(installDir, updateLockName))
	if err != nil {
		return err
	}
	defer unlock()

	script, err := fetchBytes(base + "/download/install.sh")
	if err != nil {
		return fmt.Errorf("download %s/download/install.sh: %w", base, err)
	}
	// The served installer is the single implementation of "download, verify,
	// replace, refresh the wrapper, refresh the service definition". Telling it
	// not to restart keeps the restart decision here, where it can report what
	// happened.
	installArgs := []string{"-s", "--", "--server", base, "--install-dir", installDir}
	if bytes.Contains(script, []byte("--no-restart")) {
		// The served installer owns download/verify/replace and the service
		// definition; asking it not to restart keeps the restart decision here.
		installArgs = append(installArgs, "--no-restart")
	} else {
		// A Server older than this command serves an installer without that
		// flag; it stops the service and starts it again itself.
		fmt.Fprintln(os.Stdout, "note: this Server's installer predates --no-restart; it restarts the service itself")
	}
	if err := options.runInstaller(script, installArgs); err != nil {
		return fmt.Errorf("run the Server installer: %w", err)
	}

	current := buildVersion()
	next := published.version
	if next == "" {
		next = "published build"
	}
	fmt.Fprintf(os.Stdout, "updated %s -> %s (%s)\n", current, next, shortHash(published.hash))

	if options.noRestart {
		fmt.Fprintln(os.Stdout, "--no-restart: the running daemon still uses the old binary; restart it to pick the update up.")
		return nil
	}
	restartUpdatedService(options)
	fmt.Fprintln(os.Stdout, "note: sessions that are already running keep their existing session-host process (old binary). Stop and resume one to move it to the new build.")
	fmt.Fprintln(os.Stdout, "note: this updated this machine's daemon only. The Agora Server is a container image: redeploy it to update the Server.")
	return nil
}

// ensureUpdateTarget refuses to silently clobber a source checkout. `make
// install-wrapper` links ~/.local/bin/agora at the repository build, and
// replacing that file would leave the checkout inconsistent.
func ensureUpdateTarget(target, installDir string, force bool) error {
	info, err := os.Lstat(target)
	if err != nil {
		return nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	resolved, resolveErr := filepath.EvalSymlinks(target)
	if resolveErr != nil {
		resolved = ""
	}
	if force {
		return nil
	}
	inside := resolved != "" && strings.HasPrefix(resolved, strings.TrimRight(installDir, "/")+string(os.PathSeparator))
	if inside {
		return nil
	}
	return fmt.Errorf("%s is a symlink to %s, which looks like a source checkout.\nUpdate it there (`make build-go && make install-wrapper`), or pass --force to replace the symlink", target, resolved)
}

func restartUpdatedService(options updateOptions) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not resolve the home directory to restart the service: %v\n", err)
		return
	}
	unit := serviceUnitPath(runtime.GOOS, home)
	if unit == "" {
		fmt.Fprintln(os.Stdout, "no known background service on this platform; restart the daemon yourself (`agora daemon`).")
		return
	}
	if options.unitExists != nil && !options.unitExists(unit) {
		fmt.Fprintf(os.Stdout, "no background service installed (%s); restart the daemon yourself (`agora daemon`).\n", unit)
		return
	}
	command := serviceRestartCommand(runtime.GOOS, home, os.Getuid())
	if len(command) == 0 {
		return
	}
	if err := options.runRestart(command[0], command[1:]...); err != nil {
		fmt.Fprintf(os.Stderr, "could not restart the service with %s: %v\n", strings.Join(command, " "), err)
		fmt.Fprintf(os.Stdout, "restart it yourself: %s\n", strings.Join(command, " "))
		return
	}
	fmt.Fprintf(os.Stdout, "restarted the background service (%s)\n", strings.Join(command, " "))
}

// serviceUnitPath is where the installer records the user-level service.
func serviceUnitPath(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", updateServiceLabel+".plist")
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", updateServiceUnit)
	default:
		return ""
	}
}

// serviceRestartCommand restarts a service that is already installed. Linux is
// explicit about restarting because the installer only enables the unit.
func serviceRestartCommand(goos, home string, uid int) []string {
	switch goos {
	case "darwin":
		return []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, updateServiceLabel)}
	case "linux":
		return []string{"systemctl", "--user", "restart", updateServiceUnit}
	default:
		return nil
	}
}

// lockUpdate serializes concurrent updates. The lock records its owner's pid so
// an update that was killed cannot wedge every later one.
func lockUpdate(path string) (func(), error) {
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		body, readErr := os.ReadFile(path)
		owner, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
		if readErr == nil && parseErr == nil && processAlive(owner) {
			return nil, fmt.Errorf("another `agora update` is running (pid %d, %s)", owner, path)
		}
		_ = os.Remove(path)
	}
	return nil, fmt.Errorf("could not take the update lock at %s", path)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 only checks for existence; it is never delivered.
	return syscall.Kill(pid, 0) == nil
}

func checksumFor(checksums, artifact string) string {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == artifact {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func shortHash(value string) string {
	if len(value) > 12 {
		return value[:12] + "…"
	}
	return value
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// installedBinaryVersion asks the installed binary what it is. A binary that is
// not Agora (or a broken download) simply answers "unknown".
func installedBinaryVersion(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "version", "--porcelain").Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", errors.New("empty version output")
	}
	return value, nil
}

func fetchText(target string) (string, error) {
	body, err := fetchBytes(target)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func fetchBytes(target string) ([]byte, error) {
	response, err := updateHTTPClient.Get(target)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", target, response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, 64<<20))
}

var updateHTTPClient = &http.Client{Timeout: 60 * time.Second}

func runCommand(name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	return command.Run()
}

// runInstallerScript runs the Server's installer with `sh -s`, the same way the
// documented `curl | sh -s -- ...` one-liner does.
func runInstallerScript(script []byte, args []string) error {
	command := exec.Command("sh", args...)
	command.Stdin = bytes.NewReader(script)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}
