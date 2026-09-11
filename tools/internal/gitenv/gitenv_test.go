package gitenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The regression this package exists for: `git -C <dir>` loses to an inherited
// GIT_DIR, so a detached command must resolve the directory it was given.
func TestDetachResolvesTheNamedDirectoryUnderAnInheritedRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	outer, inner := t.TempDir(), t.TempDir()
	for _, directory := range []string{outer, inner} {
		if output, err := Detach(exec.Command("git", "-C", directory, "init", "--quiet")).CombinedOutput(); err != nil {
			t.Fatalf("git init in %s: %v: %s", directory, err, output)
		}
	}

	command := exec.Command("git", "-C", inner, "rev-parse", "--absolute-git-dir")
	command.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(outer, ".git"))
	leaked, err := command.Output()
	if err != nil {
		t.Fatalf("attached run: %v", err)
	}
	if got := strings.TrimSpace(string(leaked)); !strings.HasPrefix(got, outer) {
		t.Fatalf("attached run resolved %q; the fixture no longer reproduces the leak", got)
	}

	detached := Detach(exec.Command("git", "-C", inner, "rev-parse", "--absolute-git-dir"))
	detached.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(outer, ".git"))
	resolved, err := Detach(detached).Output()
	if err != nil {
		t.Fatalf("detached run: %v", err)
	}
	if got := strings.TrimSpace(string(resolved)); !strings.HasPrefix(got, inner) {
		t.Fatalf("detached run resolved %q, want a git dir under %s", got, inner)
	}
}

// Detach must remove only what redirects git; a command still needs its PATH.
func TestDetachKeepsEverythingElse(t *testing.T) {
	command := exec.Command("git", "status")
	command.Env = []string{"GIT_DIR=/elsewhere/.git", "PATH=/usr/bin", "HOME=/home/test", "GIT_AUTHOR_NAME=test"}
	Detach(command)
	if slices.Contains(command.Env, "GIT_DIR=/elsewhere/.git") {
		t.Fatalf("env = %v, want GIT_DIR removed", command.Env)
	}
	for _, wanted := range []string{"PATH=/usr/bin", "HOME=/home/test", "GIT_AUTHOR_NAME=test"} {
		if !slices.Contains(command.Env, wanted) {
			t.Fatalf("env = %v, want %s kept", command.Env, wanted)
		}
	}
}
