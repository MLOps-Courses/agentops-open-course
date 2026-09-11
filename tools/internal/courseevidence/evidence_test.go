package courseevidence

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/gitenv"
)

func TestCreateAndVerifyBindEvidenceToCleanRevisionAndArtifacts(t *testing.T) {
	root := initializeRepository(t)
	artifact := filepath.Join(root, "input.txt")
	output := filepath.Join(root, ".evidence", "manifest.json")
	config := Config{
		Root:      root,
		Gates:     [][]string{{"gate", "one"}},
		Artifacts: []string{"input.txt"},
		Now:       func() time.Time { return time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC) },
		Run: func(_ context.Context, _ string, command []string, _, _ io.Writer) error {
			if strings.Join(command, " ") != "gate one" {
				t.Fatalf("command = %v", command)
			}
			return nil
		},
	}
	if err := Create(t.Context(), config, output); err != nil {
		t.Fatal(err)
	}
	if revision, err := Verify(t.Context(), config, output); err != nil || len(revision) != 40 {
		t.Fatalf("revision = %q, error = %v", revision, err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(manifest.TreeDigest, "sha256:") {
		t.Fatalf("tree digest = %q", manifest.TreeDigest)
	}
	if err := os.WriteFile(artifact, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), config, output); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateRejectsDirtyCheckoutBeforeRunningGates(t *testing.T) {
	root := initializeRepository(t)
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	config := Config{
		Root: root, Gates: [][]string{{"gate"}}, Artifacts: []string{"input.txt"}, Now: time.Now,
		Run: func(context.Context, string, []string, io.Writer, io.Writer) error { called = true; return nil },
	}
	if err := Create(t.Context(), config, filepath.Join(root, "manifest.json")); err == nil ||
		!strings.Contains(err.Error(), "dirty") || called {
		t.Fatalf("error = %v, called = %v", err, called)
	}
}

func initializeRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".evidence/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.name", "Course Evidence Test"},
		{"config", "user.email", "test@example.invalid"},
		{"add", "."},
		{"commit", "-m", "test: seed"},
	} {
		command := gitenv.Detach(exec.Command("git", arguments...))
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	return root
}

// TestCreateChecksArtifactsBeforeSpendingTheGates keeps a missing file from
// costing a learner a full check:core plus test run before it is reported.
func TestCreateChecksArtifactsBeforeSpendingTheGates(t *testing.T) {
	root := initializeRepository(t)
	runs := 0
	config := Config{
		Root:      root,
		Gates:     [][]string{{"mise", "run", "check:core"}},
		Artifacts: []string{"never-written.txt"},
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
		Run: func(context.Context, string, []string, io.Writer, io.Writer) error {
			runs++
			return nil
		},
	}
	err := Create(t.Context(), config, filepath.Join(root, "manifest.json"))
	if !errors.Is(err, ErrMissingArtifact) {
		t.Fatalf("Create() error = %v, want %v", err, ErrMissingArtifact)
	}
	if !strings.Contains(err.Error(), "never-written.txt") {
		t.Errorf("Create() error = %v, want it to name the missing artifact", err)
	}
	if runs != 0 {
		t.Errorf("gates ran %d times before the missing artifact was reported, want 0", runs)
	}
}

func TestDefaultArtifactsReadsTheCommittedInventory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(artifactListPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# a comment\n\nmise.lock\n  tools/go.sum  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := DefaultArtifacts(root)
	if err != nil {
		t.Fatalf("DefaultArtifacts() error = %v", err)
	}
	if !slices.Equal(paths, []string{"mise.lock", "tools/go.sum"}) {
		t.Fatalf("DefaultArtifacts() = %v, want the two listed paths", paths)
	}
	if err := os.WriteFile(path, []byte("# only comments\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultArtifacts(root); err == nil {
		t.Error("DefaultArtifacts() accepted an inventory with no artifacts")
	}
}

// TestDisplayPathPrintsRepositoryRelativePaths pins what the commands put on
// stdout: the course pages quote those lines verbatim, and an absolute path would
// both fail the machine-path contract and leak a home directory into a capture.
func TestDisplayPathPrintsRepositoryRelativePaths(t *testing.T) {
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(working, ".agents", "tmp", "course-completion.json")
	if got := DisplayPath(inside); got != filepath.Join(".agents", "tmp", "course-completion.json") {
		t.Errorf("DisplayPath(%q) = %q, want it relative to the working directory", inside, got)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if got := DisplayPath(outside); got != outside {
		t.Errorf("DisplayPath(%q) = %q, want a path outside the working directory left alone", outside, got)
	}
}

func TestCreateWritesAReadableSummaryBesideTheManifest(t *testing.T) {
	root := initializeRepository(t)
	output := filepath.Join(root, ".evidence", "course-completion.json")
	config := Config{
		Root:      root,
		Gates:     [][]string{{"mise", "run", "test"}},
		Artifacts: []string{"input.txt"},
		Now:       func() time.Time { return time.Unix(0, 0).UTC() },
		Run:       func(context.Context, string, []string, io.Writer, io.Writer) error { return nil },
	}
	if err := Create(t.Context(), config, output); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	summary, err := os.ReadFile(SummaryPath(output))
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	for _, wanted := range []string{"# Course completion", "`mise run test`", "`input.txt`", "Tree digest"} {
		if !strings.Contains(string(summary), wanted) {
			t.Errorf("summary is missing %q:\n%s", wanted, summary)
		}
	}
}
