package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/daemon"
	"github.com/delve8/agora/internal/runtime"
	"github.com/delve8/agora/internal/server"
	"github.com/delve8/agora/internal/store"
)

func runServer() error {
	database, err := store.Open(firstEnv("AGORA_SERVER_DB", "AGORA_DB"))
	if err != nil {
		return err
	}
	defer database.Close()
	manager := runtime.NewManager(database, adapter.NewClaudeCodeAdapter(os.Getenv("AGORA_CLAUDE_BINARY")), nil)
	addr := firstEnv("AGORA_SERVER_ADDR", "AGORA_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	srv := server.New(addr, database, manager)
	return runHTTPServer(srv, manager.Close)
}

func runDaemon() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d, err := daemon.New(daemon.Config{
		ID:             os.Getenv("AGORA_DAEMON_ID"),
		Version:        firstEnv("AGORA_VERSION", "dev"),
		ServerURL:      daemonWSURL(firstEnv("AGORA_SERVER_URL", "http://127.0.0.1:8080")),
		Credential:     os.Getenv("AGORA_DEVICE_CREDENTIAL"),
		CredentialPath: os.Getenv("AGORA_DEVICE_CREDENTIAL_PATH"),
		DatabasePath:   firstEnv("AGORA_DAEMON_DB", "AGORA_DB"),
		ClaudeBinary:   os.Getenv("AGORA_CLAUDE_BINARY"),
		HomeDir:        os.Getenv("HOME"),
	})
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Run(ctx)
}

func runHTTPServer(srv *server.Server, cleanup func()) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("agora: listening on %s", srv.Addr())
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		cleanup()
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

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func daemonWSURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	if strings.HasPrefix(value, "ws://") || strings.HasPrefix(value, "wss://") {
		return value + "/api/daemon/ws"
	}
	return "ws://" + value + "/api/daemon/ws"
}
