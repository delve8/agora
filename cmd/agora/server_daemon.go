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
	// The remote server does not own Agent processes. Claude and Pi run in
	// connected daemons, so keep the manager nil-capable here and let the
	// daemon hub route history, input, resume, and live events to the owner.
	manager := (*runtime.Manager)(nil)
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
	// The split server deliberately has no local runtime manager. Keep the
	// shutdown callback nil-safe; a SIGTERM must not turn into a panic while
	// the server is already shutting down.
	cleanup := func() {}
	if manager != nil {
		cleanup = manager.Close
	}
	return runHTTPServer(srv, cleanup)
}

func runDaemon(pairCode string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	daemonID, credential, err := resolveDaemonIdentity(pairCode, os.Getenv("AGORA_CONFIG_PATH"), os.Getenv("AGORA_DEVICE_CREDENTIAL_PATH"))
	if err != nil {
		return err
	}
	d, err := daemon.New(daemon.Config{
		ID:                 daemonID,
		Version:            firstEnv("AGORA_VERSION", "dev"),
		ServerURL:          daemonWSURL(firstEnv("AGORA_SERVER_URL", "http://127.0.0.1:8080")),
		Credential:         credential,
		CredentialPath:     os.Getenv("AGORA_DEVICE_CREDENTIAL_PATH"),
		ClaudeBinary:       os.Getenv("AGORA_CLAUDE_BINARY"),
		PiBinary:           firstEnv("AGORA_PI_BINARY", "PI_BINARY"),
		PiProvider:         firstEnv("AGORA_PI_PROVIDER", "PI_PROVIDER"),
		PiModel:            firstEnv("AGORA_PI_MODEL", "PI_MODEL"),
		PiSessionDir:       firstEnv("AGORA_PI_SESSION_DIR", "PI_SESSION_DIR"),
		LocalSocketPath:    os.Getenv("AGORA_DAEMON_SOCKET"),
		HomeDir:            os.Getenv("HOME"),
		SessionHostEnabled: true,
	})
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Run(ctx)
}

// resolveDaemonIdentity decides the daemon's identity and credential.
//
// A pairing code binds the daemon to the device the server creates: the
// returned device_id becomes the daemon id (persisted to the config file so
// later runs without --pair reuse it) and the returned credential
// authenticates it. That keeps daemon.register's daemon_id equal to the
// credential-owned device_id, which the server enforces in logto mode.
func resolveDaemonIdentity(pairCode, configPath, credentialPath string) (string, string, error) {
	if strings.TrimSpace(pairCode) != "" {
		if override := strings.TrimSpace(os.Getenv("AGORA_DAEMON_ID")); override != "" {
			return "", "", fmt.Errorf("AGORA_DAEMON_ID cannot be combined with --pair: pairing binds the daemon to a server-issued device_id")
		}
		// Default the device alias to the hostname so paired devices are
		// distinguishable in the web device list without requiring the user to
		// set AGORA_DEVICE_NAME.
		deviceName := strings.TrimSpace(os.Getenv("AGORA_DEVICE_NAME"))
		if deviceName == "" {
			if hostname, hostErr := os.Hostname(); hostErr == nil {
				deviceName = strings.TrimSpace(hostname)
			}
		}
		deviceID, credential, err := pairDevice(firstEnv("AGORA_SERVER_URL", "http://127.0.0.1:8080"), pairCode, deviceName)
		if err != nil {
			return "", "", err
		}
		if err := config.SaveDeviceCredential(credentialPath, credential); err != nil {
			return "", "", err
		}
		if err := config.SaveDaemonID(configPath, deviceID); err != nil {
			return "", "", err
		}
		return deviceID, credential, nil
	}
	daemonID, err := config.ResolveDaemonID(os.Getenv("AGORA_DAEMON_ID"), configPath)
	if err != nil {
		return "", "", err
	}
	credential := strings.TrimSpace(os.Getenv("AGORA_DEVICE_CREDENTIAL"))
	if credential == "" {
		credential, _ = config.LoadDeviceCredential(credentialPath)
	}
	return daemonID, credential, nil
}

func pairDevice(serverURL, code, name string) (deviceID, credential string, err error) {
	endpoint := strings.TrimRight(serverURL, "/") + "/api/daemon/pair"
	body := strings.NewReader(fmt.Sprintf(`{"code":%q,"name":%q}`, code, name))
	request, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	var result struct {
		DeviceID   string `json:"device_id"`
		Credential string `json:"credential"`
		Error      string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if result.Error != "" {
			return "", "", errors.New(result.Error)
		}
		return "", "", fmt.Errorf("pairing failed with status %s", response.Status)
	}
	if result.Credential == "" {
		return "", "", errors.New("pairing response did not include credential")
	}
	if result.DeviceID == "" {
		return "", "", errors.New("pairing response did not include device_id")
	}
	return result.DeviceID, result.Credential, nil
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
	const suffix = "/api/daemon/ws"
	if strings.HasSuffix(value, suffix) {
		if strings.HasPrefix(value, "https://") {
			return "wss://" + strings.TrimPrefix(value, "https://")
		}
		if strings.HasPrefix(value, "http://") {
			return "ws://" + strings.TrimPrefix(value, "http://")
		}
		return value
	}
	if strings.HasPrefix(value, "ws://") || strings.HasPrefix(value, "wss://") {
		return value + suffix
	}
	if strings.HasPrefix(value, "https://") {
		return "wss://" + strings.TrimPrefix(value, "https://") + suffix
	}
	if strings.HasPrefix(value, "http://") {
		return "ws://" + strings.TrimPrefix(value, "http://") + suffix
	}
	return "ws://" + value + suffix
}
