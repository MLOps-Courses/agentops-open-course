package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutePrintsDevelopmentDisplayAndReleaseFields(t *testing.T) {
	repository := testRepository(t)
	var output bytes.Buffer
	if err := execute(t.Context(), []string{"--root", repository, "--mode", "release", "--field", "revision"}, &output); err != nil {
		t.Fatal(err)
	}
	if revision := strings.TrimSpace(output.String()); len(revision) != 40 {
		t.Fatalf("revision = %q", revision)
	}

	if err := os.WriteFile(filepath.Join(repository, "new.go"), []byte("package dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := execute(t.Context(), []string{"--root", repository, "--mode", "development", "--field", "display"}, &output); err != nil {
		t.Fatal(err)
	}
	if display := strings.TrimSpace(output.String()); !strings.HasPrefix(display, "unknown+dirty.") {
		t.Fatalf("display = %q", display)
	}
	if err := execute(t.Context(), []string{"--root", repository, "--mode", "release"}, &output); err == nil {
		t.Fatal("release mode accepted dirty source")
	}
}

func testRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.name", "Source Identity CLI Test"},
		{"config", "user.email", "test@example.invalid"},
		{"add", "."},
		{"commit", "-m", "test: seed"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	return repository
}
