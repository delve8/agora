package runtime

import (
	"os"
	"strings"
	"sync"
)

const (
	defaultPiBinary   = "pi"
	defaultPiProvider = "anthropic"
)

// PiConfig is the provider configuration a managed Pi session is launched with.
type PiConfig struct {
	Binary     string
	Provider   string
	Model      string
	SessionDir string
	// ReporterExtension is the path of the Agora session reporter extension.
	// It is injected with `pi -e <path>` so the provider reports session
	// switches to the Daemon instead of Agora inferring them from keystrokes.
	ReporterExtension string
}

// PiProvider is the Pi provider as the Daemon sees it: which executable to run
// and with which arguments. The Agent process itself belongs to a Session Host,
// never to this process, so there is nothing here to start, stop or attach to.
type PiProvider struct {
	config    PiConfig
	binaryErr error
	mu        sync.Mutex
}

func NewPiProvider(config PiConfig) *PiProvider {
	// Keep direct/local construction consistent with the daemon command. The
	// explicit PiConfig value wins, then the conventional environment names,
	// and finally the safe built-in defaults. If `pi` resolves to the generic
	// PATH wrapper, skip it and select the real Pi executable later in PATH.
	if config.Binary == "" {
		config.Binary = firstEnvValue("AGORA_PI_BINARY", "PI_BINARY")
	}
	var binaryErr error
	if resolved, err := resolveAgentBinary(config.Binary, defaultPiBinary); err == nil {
		config.Binary = resolved
	} else {
		binaryErr = err
		if config.Binary == "" {
			config.Binary = defaultPiBinary
		}
	}
	if config.Provider == "" {
		config.Provider = firstEnvValue("AGORA_PI_PROVIDER", "PI_PROVIDER")
	}
	if config.Provider == "" {
		config.Provider = defaultPiProvider
	}
	if config.Model == "" {
		config.Model = firstEnvValue("AGORA_PI_MODEL", "PI_MODEL")
	}
	if config.SessionDir == "" {
		config.SessionDir = firstEnvValue("AGORA_PI_SESSION_DIR", "PI_SESSION_DIR")
	}
	return &PiProvider{config: config, binaryErr: binaryErr}
}

func firstEnvValue(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// Err reports why the configured Pi executable could not be resolved. Session
// creation refuses to start rather than spawn a binary that would fail as an
// unrelated "host exited" error.
func (m *PiProvider) Err() error {
	if m == nil {
		return nil
	}
	return m.binaryErr
}

func (m *PiProvider) SessionDir() string { return m.config.SessionDir }

// SetReporterExtension installs the Agora reporter extension path. It is
// instrumentation owned by Agora, so it is added to every Pi invocation,
// including the ones that forward the user's own arguments verbatim.
func (m *PiProvider) SetReporterExtension(path string) {
	m.mu.Lock()
	m.config.ReporterExtension = strings.TrimSpace(path)
	m.mu.Unlock()
}

func (m *PiProvider) reporterExtension() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.TrimSpace(m.config.ReporterExtension)
}

// Command returns the exact provider command used for a managed Pi session.
// Session Host uses it so the Agent process is created outside the Daemon.
//
// The command is the complete contract with Pi: Agora adds its reporter
// extension and its own provider defaults, and otherwise forwards the caller's
// arguments unchanged.
func (m *PiProvider) Command(nativeID, historyPath string, agentArgs []string) []string {
	args := []string{m.config.Binary}
	if extension := m.reporterExtension(); extension != "" {
		args = append(args, "-e", extension)
	}
	if len(agentArgs) > 0 {
		return append(args, agentArgs...)
	}
	if m.config.Provider != "" {
		args = append(args, "--provider", m.config.Provider)
	}
	if m.config.Model != "" {
		args = append(args, "--model", m.config.Model)
	}
	if m.config.SessionDir != "" {
		args = append(args, "--session-dir", m.config.SessionDir)
	}
	if historyPath != "" {
		args = append(args, "--session", historyPath)
	} else if nativeID != "" {
		args = append(args, "--session-id", nativeID)
	}
	return args
}

func cleanPiEnv() []string {
	blocked := map[string]bool{
		"PI_SESSION_ID":      true,
		"PI_SESSION_FILE":    true,
		"PI_CODING_AGENT":    true,
		"PI_PROVIDER":        true,
		"PI_MODEL":           true,
		"PI_REASONING_LEVEL": true,
		"AI_AGENT":           true,
	}
	values := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if !ok || !blocked[key] {
			values = append(values, item)
		}
	}
	return withDefaultTerminal(values)
}
