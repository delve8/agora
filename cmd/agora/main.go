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
		fmt.Fprintln(os.Stderr, "usage: agora serve | agora server | agora daemon | agora pty <args...> | agora attach <session-id> | agora wrapper [session-id] | agora claude-proxy [args...]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "server":
		err = runServer()
	case "daemon":
		err = runDaemon()
	case "pty":
		err = runPTY(os.Args[2:])
	case "attach":
		if len(os.Args) < 3 {
			err = fmt.Errorf("attach requires a session id")
		} else {
			err = runAttach(os.Args[2])
		}
	case "wrapper":
		err = runWrapper(os.Args[2:])
	case "claude-proxy":
		err = runClaudeProxy(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: agora serve | agora pty <args...> | agora attach <session-id> | agora wrapper [session-id] | agora claude-proxy [args...]")
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
	srv := server.New(addr, database, manager)
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
