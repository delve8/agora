package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAgentBinarySkipsAgoraWrapper(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "pi")
	real := filepath.Join(t.TempDir(), "pi")
	const wrapperBody = "#!/bin/sh\n# Generic PATH wrapper for Agora-managed native Agent sessions\n"
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(wrapper)+string(os.PathListSeparator)+filepath.Dir(real))

	got, err := resolveAgentBinary("pi", "pi")
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("resolved binary = %q, want %q", got, real)
	}
}

func TestResolveAgentBinaryRejectsOnlyAgoraWrapper(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "claude")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n# Generic PATH wrapper for Agora-managed native Agent sessions\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if _, err := resolveAgentBinary("claude", "claude"); err == nil {
		t.Fatal("expected wrapper-only PATH to be rejected")
	}
}
