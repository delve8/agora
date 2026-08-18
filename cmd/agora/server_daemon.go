package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/config"
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
	authConfig, err := config.ResolveServerAuthConfig(os.Getenv("AGORA_AUTH_MODE"), os.Getenv("AGORA_LOGTO_ISSUER"), os.Getenv("AGORA_LOGTO_AUDIENCE"), os.Getenv("AGORA_LOGTO_PROVISIONING"))
	if err != nil {
		return err
	}
	if err := config.ValidateServerAddress(addr, authConfig.Mode); err != nil {
		return err
	}
	srv := server.NewWithWebDirAndAuth(addr, database, manager, os.Getenv("AGORA_WEB_DIR"), authConfig)
	return runHTTPServer(srv, manager.Close)
}

func runDaemon(pairCode string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	daemonID, err := config.ResolveDaemonID(os.Getenv("AGORA_DAEMON_ID"), os.Getenv("AGORA_CONFIG_PATH"))
	if err != nil {
		return err
	}
	credential := strings.TrimSpace(os.Getenv("AGORA_DEVICE_CREDENTIAL"))
	credentialPath := os.Getenv("AGORA_DEVICE_CREDENTIAL_PATH")
	if credential == "" {
		credential, _ = config.LoadDeviceCredential(credentialPath)
	}
	if strings.TrimSpace(pairCode) != "" {
		var err error
		credential, err = pairDevice(firstEnv("AGORA_SERVER_URL", "http://127.0.0.1:8080"), pairCode, os.Getenv("AGORA_DEVICE_NAME"))
		if err != nil {
			return err
		}
		if err := config.SaveDeviceCredential(credentialPath, credential); err != nil {
			return err
		}
	}
	d, err := daemon.New(daemon.Config{
		ID:             daemonID,
		Version:        firstEnv("AGORA_VERSION", "dev"),
		ServerURL:      daemonWSURL(firstEnv("AGORA_SERVER_URL", "http://127.0.0.1:8080")),
		Credential:     credential,
		CredentialPath: credentialPath,
		ClaudeBinary:   os.Getenv("AGORA_CLAUDE_BINARY"),
		HomeDir:        os.Getenv("HOME"),
	})
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Run(ctx)
}

func pairDevice(serverURL, code, name string) (string, error) {
	endpoint := strings.TrimRight(serverURL, "/") + "/api/daemon/pair"
	body := strings.NewReader(fmt.Sprintf(`{"code":%q,"name":%q}`, code, name))
	request, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result struct {
		Credential string `json:"credential"`
		Error      string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if result.Error != "" {
			return "", errors.New(result.Error)
		}
		return "", fmt.Errorf("pairing failed with status %s", response.Status)
	}
	if result.Credential == "" {
		return "", errors.New("pairing response did not include credential")
	}
	return result.Credential, nil
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
	if strings.HasPrefix(value, "ws://") || strings.HasPrefix(value, "wss://") {
		return value + "/api/daemon/ws"
	}
	if strings.HasPrefix(value, "https://") {
		return "wss://" + strings.TrimPrefix(value, "https://") + "/api/daemon/ws"
	}
	if strings.HasPrefix(value, "http://") {
		return "ws://" + strings.TrimPrefix(value, "http://") + "/api/daemon/ws"
	}
	return "ws://" + value + "/api/daemon/ws"
}
