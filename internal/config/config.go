package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type File struct {
	DaemonID  string `json:"daemon_id"`
	ServerURL string `json:"server_url,omitempty"`
}

func ResolveDaemonID(override, path string) (string, error) {
	if value := strings.TrimSpace(override); value != "" {
		if err := validateDaemonID(value); err != nil {
			return "", err
		}
		return value, nil
	}
	if path == "" {
		path = defaultPath()
	}
	value, err := load(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if value.DaemonID != "" {
		return value.DaemonID, nil
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	value.DaemonID = id
	if err := save(path, value); err != nil {
		return "", err
	}
	return id, nil
}

// ResolveServerURL returns the Server URL remembered from pairing, so a daemon
// started later (for example by a service manager after a reboot) connects to
// the same Server without AGORA_SERVER_URL being exported in its environment.
func ResolveServerURL(path string) (string, error) {
	if path == "" {
		path = defaultPath()
	}
	value, err := load(path)
	if err != nil {
		return "", err
	}
	return value.ServerURL, nil
}

// SaveDaemonID atomically persists an explicit daemon identity (e.g. the
// device_id returned by pairing) so later runs resolve the same id without
// an override. It replaces any previously stored UUID identity while keeping
// the remembered Server URL.
func SaveDaemonID(path, id string) error {
	id = strings.TrimSpace(id)
	if err := validateDaemonID(id); err != nil {
		return err
	}
	if path == "" {
		path = defaultPath()
	}
	value, err := loadForUpdate(path)
	if err != nil {
		return err
	}
	value.DaemonID = id
	return save(path, value)
}

// SaveServerURL remembers the Server the daemon was paired with.
func SaveServerURL(path, serverURL string) error {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if serverURL == "" {
		return fmt.Errorf("server url is empty")
	}
	if path == "" {
		path = defaultPath()
	}
	value, err := loadForUpdate(path)
	if err != nil {
		return err
	}
	value.ServerURL = serverURL
	return save(path, value)
}

// loadForUpdate reads the config, tolerating a missing file so the caller can
// create it by saving the returned value.
func loadForUpdate(path string) (File, error) {
	if path == "" {
		path = defaultPath()
	}
	value, err := load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return File{}, nil
		}
		return File{}, err
	}
	return value, nil
}

func DeviceCredentialPath(override string) string {
	if value := strings.TrimSpace(override); value != "" {
		return value
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agora", "device.credential")
}

func LoadDeviceCredential(path string) (string, error) {
	path = DeviceCredentialPath(path)
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(body))
	if value == "" {
		return "", fmt.Errorf("device credential file %s is empty", path)
	}
	return value, nil
}

func SaveDeviceCredential(path, credential string) error {
	path = DeviceCredentialPath(path)
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return fmt.Errorf("device credential is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create device credential directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".device-credential-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(credential + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install device credential: %w", err)
	}
	return nil
}
func defaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agora", "config.json")
}

func load(path string) (File, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var value File
	if err := json.Unmarshal(body, &value); err != nil {
		return File{}, fmt.Errorf("decode Agora config %s: %w", path, err)
	}
	// A config that only remembers a Server URL (no daemon id yet) is valid;
	// any stored id still has to be well formed.
	if value.DaemonID != "" {
		if err := validateDaemonID(value.DaemonID); err != nil {
			return File{}, fmt.Errorf("invalid Agora config %s: %w", path, err)
		}
	}
	return value, nil
}

func save(path string, value File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Agora config directory: %w", err)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create Agora config temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install Agora config: %w", err)
	}
	return nil
}

func validateDaemonID(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\?#:\t\r\n") {
		return fmt.Errorf("invalid daemon id %q", value)
	}
	return nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate daemon id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
