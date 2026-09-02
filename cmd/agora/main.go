package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/server"
	"github.com/delve8/agora/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agora serve | agora server | agora daemon | agora session-host --config <path> | agora pty <args...> | agora attach <session-id> | agora wrap <pi|claude> [session-id|prompt...] | agora wrapper [session-id] | agora pi-wrapper [session-id] | agora claude-proxy [args...]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "server":
		err = runServer()
	case "session-host":
		err = runSessionHost(os.Args[2:])
	case "daemon":
		pairCode := ""
		if len(os.Args) == 4 && os.Args[2] == "--pair" {
			pairCode = os.Args[3]
		} else if len(os.Args) > 2 {
			err = fmt.Errorf("daemon supports only --pair <code>")
		}
		if err == nil {
			err = runDaemon(pairCode)
		}
	case "pty":
		err = runPTY(os.Args[2:])
	case "attach":
		if len(os.Args) < 3 {
			err = fmt.Errorf("attach requires a session id")
		} else {
			err = runAttach(os.Args[2])
		}
	case "wrap":
		if len(os.Args) < 3 {
			err = fmt.Errorf("wrap requires an agent: pi or claude")
		} else {
			switch os.Args[2] {
			case "pi":
				err = runPiWrapper(os.Args[3:])
			case "claude", "claude-code":
				err = runWrapper(os.Args[3:])
			default:
				err = fmt.Errorf("unsupported wrapper agent %q: expected pi or claude", os.Args[2])
			}
		}
	case "wrapper":
		// Backward-compatible alias for `agora wrap claude`.
		err = runWrapper(os.Args[2:])
	case "pi-wrapper":
		// Backward-compatible alias for `agora wrap pi`.
		err = runPiWrapper(os.Args[2:])
	case "claude-proxy":
		err = runClaudeProxy(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: agora serve | agora server | agora daemon | agora session-host --config <path> | agora pty <args...> | agora attach <session-id> | agora wrap <pi|claude> [session-id|prompt...] | agora wrapper [session-id] | agora pi-wrapper [session-id] | agora claude-proxy [args...]")
		os.Exit(2)
	}
	if err != nil {
		log.Printf("agora: %v", err)
		if value, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(value.ExitCode())
		}
		os.Exit(1)
	}
}

func serve() error {
	database, err := store.Open(os.Getenv("AGORA_DB"))
	if err != nil {
		return err
	}
	defer database.Close()

	agentAdapter := adapter.NewClaudeCodeAdapter(os.Getenv("AGORA_CLAUDE_BINARY"))
	ptyManager := runtime.NewPTYManager(os.Getenv("AGORA_CLAUDE_BINARY"), os.Getenv("HOME"))
	manager := runtime.NewManager(database, agentAdapter, ptyManager)
	manager.AttachPi(runtime.NewPiManager(runtime.PiConfig{
		Binary: os.Getenv("AGORA_PI_BINARY"), Provider: os.Getenv("AGORA_PI_PROVIDER"),
		Model: os.Getenv("AGORA_PI_MODEL"), SessionDir: os.Getenv("AGORA_PI_SESSION_DIR"),
	}))
	if err := manager.ReconcileObservers(context.Background()); err != nil {
		log.Printf("agora: reconcile observers: %v", err)
	}
	if err := manager.ResumeManagedSessions(context.Background()); err != nil {
		log.Printf("agora: resume managed sessions: %v", err)
	}
	addr := os.Getenv("AGORA_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	srv := server.NewWithWebDir(addr, database, manager, os.Getenv("AGORA_WEB_DIR"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("agora: listening on %s", srv.Addr())
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		manager.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
