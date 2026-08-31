package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveAgentBinary resolves a configured Agent command before starting a
// child process. The generic Agora wrapper is commonly installed as `pi` or
// `claude` earlier in PATH than the real binary. Bare command names therefore
// need one extra PATH lookup that skips that wrapper; otherwise the Daemon
// would recursively launch the wrapper.
func resolveAgentBinary(configured, defaultName string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		configured = defaultName
	}
	if strings.ContainsRune(configured, os.PathSeparator) {
		if isAgoraWrapper(configured) {
			return "", fmt.Errorf("configured %s binary %q is the Agora wrapper; set AGORA_%s_BINARY to the real %s executable", defaultName, configured, strings.ToUpper(defaultName), defaultName)
		}
		return configured, nil
	}

	first, err := exec.LookPath(configured)
	if err != nil {
		return "", err
	}
	if !isAgoraWrapper(first) {
		return first, nil
	}

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, configured)
		info, statErr := os.Stat(candidate)
		if statErr != nil || info.IsDir() || info.Mode()&0111 == 0 || isAgoraWrapper(candidate) {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("only the Agora wrapper %q was found for %q; put the real %s binary later in PATH or set an explicit AGORA_%s_BINARY", first, configured, defaultName, strings.ToUpper(defaultName))
}

func isAgoraWrapper(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	if filepath.Base(path) == "agora-wrapper.sh" {
		return true
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(body), "Generic PATH wrapper for Agora-managed native Agent sessions")
}
