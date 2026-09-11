package evals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessClientFactoryAttestsExactLocalBinaryBeforeTrials(t *testing.T) {
	source := testSourceEvidence()
	binary := filepath.Join(t.TempDir(), "agent")
	writeVersionScript := func(version []byte) {
		t.Helper()
		script := "#!/bin/sh\nprintf '%s\\n' '" + string(version) + "'\n"
		if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := ProcessClientFactoryConfig{
		Source: source, Binary: binary, DataDir: t.TempDir(), Transport: "rest", Entrypoint: "agent",
	}

	stale := source
	stale.TreeDigest = "sha256:" + strings.Repeat("c", 64)
	writeVersionScript(runtimeVersionJSON(t, stale))
	if _, err := NewProcessClientFactory(t.Context(), config); err == nil || !strings.Contains(err.Error(), "tree_digest") {
		t.Fatalf("stale local binary error = %v", err)
	}

	writeVersionScript(runtimeVersionJSON(t, source))
	if _, err := NewProcessClientFactory(t.Context(), config); err != nil {
		t.Fatalf("matching local binary: %v", err)
	}
}

func TestProcessClientFactoryRejectsBinaryReplacementBetweenTrials(t *testing.T) {
	source := testSourceEvidence()
	binary := filepath.Join(t.TempDir(), "agent")
	writeVersionScript := func(version []byte) {
		t.Helper()
		script := "#!/bin/sh\nprintf '%s\\n' '" + string(version) + "'\n"
		if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeVersionScript(runtimeVersionJSON(t, source))
	factory, err := NewProcessClientFactory(t.Context(), ProcessClientFactoryConfig{
		Source: source, Binary: binary, DataDir: t.TempDir(), Transport: "rest", Entrypoint: "agent",
	})
	if err != nil {
		t.Fatalf("NewProcessClientFactory() error = %v", err)
	}

	stale := source
	stale.TreeDigest = "sha256:" + strings.Repeat("c", 64)
	writeVersionScript(runtimeVersionJSON(t, stale))
	if _, cleanup, err := factory(t.Context(), EvalCase{}, 0); err == nil ||
		!strings.Contains(err.Error(), "tree_digest") {
		if cleanup != nil {
			_ = cleanup()
		}
		t.Fatalf("replaced local binary error = %v", err)
	}
}
