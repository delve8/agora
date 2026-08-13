package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
)

const (
	maxObservationLine = 8 << 20
	observationQueue   = 256
	observationBatch   = 32
	observationFlush   = 500 * time.Millisecond
	httpTimeout        = 2 * time.Second
)

type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("claude exited with status %d", e.Code) }
func (e ExitError) ExitCode() int { return e.Code }

type Options struct {
	Args       []string
	Env        []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	AgoraBase  string
	HTTPClient *http.Client
}

func DefaultOptions(args []string) Options {
	return Options{Args: args, Env: os.Environ(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, AgoraBase: agoraBase(os.Getenv("AGORA_ADDR")), HTTPClient: &http.Client{Timeout: httpTimeout}}
}

func ResolveBinary(env []string, lookup func(string) (string, error), stat func(string) (os.FileInfo, error)) (string, error) {
	binary := envValue(env, "AGORA_CLAUDE_BINARY")
	if binary != "" {
		if strings.ContainsRune(binary, os.PathSeparator) {
			info, err := stat(binary)
			if err != nil {
				return "", fmt.Errorf("AGORA_CLAUDE_BINARY %q: %w", binary, err)
			}
			if info.IsDir() || info.Mode()&0o111 == 0 {
				return "", fmt.Errorf("AGORA_CLAUDE_BINARY %q is not executable", binary)
			}
			return binary, nil
		}
		resolved, err := lookup(binary)
		if err != nil {
			return "", fmt.Errorf("find Claude binary %q: %w", binary, err)
		}
		return resolved, nil
	}
	resolved, err := lookup("claude")
	if err != nil {
		return "", fmt.Errorf("find Claude binary in PATH: %w", err)
	}
	return resolved, nil
}

func Run(ctx context.Context, opts Options) (int, error) {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: httpTimeout}
	}
	if opts.AgoraBase == "" {
		opts.AgoraBase = agoraBase(envValue(opts.Env, "AGORA_ADDR"))
	}
	binary, err := ResolveBinary(opts.Env, exec.LookPath, os.Stat)
	if err != nil {
		logf(opts.Stderr, "%v", err)
		return 1, err
	}

	cmd := exec.Command(binary, opts.Args...)
	cmd.Env = opts.Env
	cmd.Stdin = opts.Stdin
	cmd.Stderr = opts.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, fmt.Errorf("create Claude stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		logf(opts.Stderr, "start Claude: %v", err)
		return 1, err
	}

	queue := make(chan []byte, observationQueue)
	var dropped atomic.Int64
	stdoutDone := make(chan error, 1)
	go func() {
		stdoutDone <- relayStdout(stdout, opts.Stdout, queue, &dropped)
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	parseDone := make(chan struct{})
	var sessionID atomic.Value
	sessionID.Store("")
	go observationLoop(ctx, opts, queue, &dropped, &sessionID, parseDone)

	registerCtx, registerCancel := context.WithTimeout(context.Background(), httpTimeout)
	registered, registerErr := register(registerCtx, opts, cmd.Process.Pid)
	registerCancel()
	if registerErr != nil {
		logf(opts.Stderr, "Agora unavailable; observation disabled: %v", registerErr)
	} else {
		sessionID.Store(registered)
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		for {
			select {
			case sig := <-signals:
				if sig != nil {
					_ = cmd.Process.Signal(sig)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	waitErr := cmd.Wait()
	<-stdoutDone
	close(queue)
	<-parseDone
	code := exitCode(waitErr)
	if registered != "" {
		reportCtx, reportCancel := context.WithTimeout(context.Background(), httpTimeout)
		if err := reportExit(reportCtx, opts, registered, code, waitErr, dropped.Load()); err != nil {
			logf(opts.Stderr, "report exit: %v", err)
		}
		reportCancel()
	}
	if waitErr != nil && code == 1 {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return code, waitErr
		}
	}
	return code, nil
}

func relayStdout(src io.Reader, dst io.Writer, queue chan<- []byte, dropped *atomic.Int64) error {
	buf := make([]byte, 32*1024)
	var line bytes.Buffer
	overlong := false
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			for _, b := range buf[:n] {
				if b == '\n' {
					if !overlong {
						item := append([]byte(nil), line.Bytes()...)
						select {
						case queue <- item:
						default:
							dropped.Add(1)
						}
					}
					line.Reset()
					overlong = false
					continue
				}
				if !overlong {
					if line.Len() >= maxObservationLine {
						overlong = true
					} else {
						line.WriteByte(b)
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func observationLoop(ctx context.Context, opts Options, queue <-chan []byte, dropped *atomic.Int64, sessionID *atomic.Value, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(observationFlush)
	defer ticker.Stop()
	batch := make([]event.Event, 0, observationBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		id, _ := sessionID.Load().(string)
		if id != "" {
			requestCtx, cancel := context.WithTimeout(context.Background(), httpTimeout)
			if err := sendEvents(requestCtx, opts, id, batch); err != nil {
				logf(opts.Stderr, "send observation events: %v", err)
			}
			cancel()
		}
		batch = batch[:0]
	}
	for {
		select {
		case line, ok := <-queue:
			if !ok {
				flush()
				return
			}
			id, _ := sessionID.Load().(string)
			if id == "" {
				continue
			}
			parsed, err := adapter.ParseStreamEvent(id, line)
			if err != nil {
				logf(opts.Stderr, "parse observation event (%d bytes): %v", len(line), err)
				continue
			}
			if parsed.Event.ID == "" {
				continue
			}
			batch = append(batch, parsed.Event)
			if len(batch) >= observationBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}

type registration struct {
	ID string `json:"id"`
}

func register(ctx context.Context, opts Options, realPID int) (string, error) {
	body := map[string]any{
		"workspace":    currentWorkspace(),
		"display_name": "Claude Code Proxy",
		"wrapper_pid":  os.Getpid(),
		"real_pid":     realPID,
		"resume_id":    resumeID(opts.Args),
		"args_summary": summarizeArgs(opts.Args),
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.AgoraBase+"/api/proxy/sessions", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	setProxyAuth(req)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("Agora returned %s", resp.Status)
	}
	var value registration
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&value); err != nil {
		return "", err
	}
	if value.ID == "" {
		return "", errors.New("Agora returned no proxy session id")
	}
	return value.ID, nil
}

func sendEvents(ctx context.Context, opts Options, id string, events []event.Event) error {
	data, err := json.Marshal(events)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.AgoraBase+"/api/proxy/sessions/"+id+"/events", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setProxyAuth(req)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Agora returned %s", resp.Status)
	}
	return nil
}

func reportExit(ctx context.Context, opts Options, id string, code int, waitErr error, dropped int64) error {
	input := map[string]any{"exit_code": code, "state": "stopped", "dropped_events": dropped}
	if code != 0 {
		input["state"] = "failed"
	}
	if status, ok := waitStatus(waitErr); ok && status.Signaled() {
		input["signal"] = status.Signal().String()
	}
	data, _ := json.Marshal(input)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.AgoraBase+"/api/proxy/sessions/"+id+"/exit", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setProxyAuth(req)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Agora returned %s", resp.Status)
	}
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if status, ok := waitStatus(err); ok {
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
		return status.ExitStatus()
	}
	return 1
}

func waitStatus(err error) (syscall.WaitStatus, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return status, ok
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(item, prefix))
		}
	}
	return ""
}

func currentWorkspace() string {
	value, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Clean(value)
}

func resumeID(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--resume=") {
			return strings.TrimPrefix(arg, "--resume=")
		}
	}
	return ""
}

func summarizeArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			if index := strings.IndexByte(arg, '='); index >= 0 {
				parts = append(parts, arg[:index]+"=<redacted>")
			} else {
				parts = append(parts, arg)
			}
		}
	}
	return strings.Join(parts, " ")
}

func agoraBase(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "http://127.0.0.1:8080"
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return "http://" + value
}

func setProxyAuth(req *http.Request) {
	if token := strings.TrimSpace(os.Getenv("AGORA_PROXY_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func logf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "[agora-proxy] "+format+"\n", args...)
}

var _ = syscall.Errno(0)
