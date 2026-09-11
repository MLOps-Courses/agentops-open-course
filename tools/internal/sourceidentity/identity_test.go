package sourceidentity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/gitenv"
)

func TestReleaseIdentityRequiresCleanSource(t *testing.T) {
	repository := initializeRepository(t)
	identity, err := Resolve(t.Context(), repository, Release)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Mode != Release || identity.Dirty || len(identity.Revision) != 40 {
		t.Fatalf("identity = %#v", identity)
	}
	if !strings.HasPrefix(identity.TreeDigest, "sha256:") || identity.Display != identity.Revision {
		t.Fatalf("identity = %#v", identity)
	}

	for name, mutate := range map[string]func(*testing.T, string){
		"tracked edit": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "source.txt"), "changed")
		},
		"untracked source": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "new.go"), "package newfile\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository := initializeRepository(t)
			mutate(t, repository)
			if _, err := Resolve(t.Context(), repository, Release); err == nil || !strings.Contains(err.Error(), "dirty") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDevelopmentIdentityNeverClaimsHEADForDirtySource(t *testing.T) {
	repository := initializeRepository(t)
	head := runGit(t, repository, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repository, "source.txt"), "changed")

	first, err := Resolve(t.Context(), repository, Development)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(t.Context(), repository, Development)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Dirty || first.Revision != "" || first.Display == head || !strings.HasPrefix(first.Display, "unknown+dirty.") {
		t.Fatalf("identity = %#v, HEAD = %s", first, head)
	}
	if first != second {
		t.Fatalf("identity is not deterministic: %#v != %#v", first, second)
	}

	writeFile(t, filepath.Join(repository, "source.txt"), "another change")
	third, err := Resolve(t.Context(), repository, Development)
	if err != nil {
		t.Fatal(err)
	}
	if third.TreeDigest == first.TreeDigest || third.Display == first.Display {
		t.Fatalf("content change did not change identity: %#v == %#v", third, first)
	}
}

func TestIgnoredFilesDoNotDirtySourceIdentity(t *testing.T) {
	repository := initializeRepository(t)
	writeFile(t, filepath.Join(repository, "ignored.tmp"), "local output")

	identity, err := Resolve(t.Context(), repository, Release)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Dirty {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestCleanIdentityIgnoresPermissionBitsGitDoesNotTrack(t *testing.T) {
	repository := initializeRepository(t)
	first, err := Resolve(t.Context(), repository, Release)
	if err != nil {
		t.Fatal(err)
	}
	if chmodErr := os.Chmod(filepath.Join(repository, "source.txt"), 0o644); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	second, err := Resolve(t.Context(), repository, Release)
	if err != nil {
		t.Fatal(err)
	}
	if first.TreeDigest != second.TreeDigest {
		t.Fatalf("non-Git permission bits changed the tree digest: %s != %s", first.TreeDigest, second.TreeDigest)
	}
}

func TestDevelopmentIdentityTracksExecutableBehavior(t *testing.T) {
	repository := initializeRepository(t)
	clean, err := Resolve(t.Context(), repository, Release)
	if err != nil {
		t.Fatal(err)
	}
	if chmodErr := os.Chmod(filepath.Join(repository, "source.txt"), 0o755); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	executable, err := Resolve(t.Context(), repository, Development)
	if err != nil {
		t.Fatal(err)
	}
	if !executable.Dirty || executable.TreeDigest == clean.TreeDigest {
		t.Fatalf("executable-bit change did not affect development identity: clean=%#v executable=%#v", clean, executable)
	}
}

func TestResolveRejectsSourceChangedWhileIdentityIsComputed(t *testing.T) {
	repository := initializeRepository(t)
	_, err := resolve(t.Context(), repository, Release, func(root string) error {
		writeFile(t, filepath.Join(root, "source.txt"), "changed during identity")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("error = %v, want concurrent source-change rejection", err)
	}
}

func TestResolveRejectsHEADChangedWhileIdentityIsComputed(t *testing.T) {
	repository := initializeRepository(t)
	firstRevision := runGit(t, repository, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repository, "source.txt"), "second commit")
	runGit(t, repository, "add", "source.txt")
	runGit(t, repository, "commit", "-m", "test: second")
	secondRevision := runGit(t, repository, "rev-parse", "HEAD")
	runGit(t, repository, "checkout", "--quiet", firstRevision)

	_, err := resolve(t.Context(), repository, Release, func(root string) error {
		command := gitenv.Detach(exec.Command("git", "checkout", "--quiet", secondRevision))
		command.Dir = root
		return command.Run()
	})
	if err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("error = %v, want concurrent HEAD-change rejection", err)
	}
}

func TestResolveSupportsShallowRepositories(t *testing.T) {
	source := initializeRepository(t)
	clone := filepath.Join(t.TempDir(), "clone")
	command := gitenv.Detach(exec.Command("git", "clone", "--depth=1", "file://"+source, clone))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, output)
	}

	identity, err := Resolve(t.Context(), clone, Release)
	if err != nil {
		t.Fatal(err)
	}
	if !identity.Shallow || len(identity.Revision) != 40 {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestResolveRejectsMissingOrUnrelatedGitMetadata(t *testing.T) {
	for name, directory := range map[string]string{
		"outside repository": t.TempDir(),
		"missing metadata":   filepath.Join(t.TempDir(), "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Resolve(context.Background(), directory, Development); err == nil {
				t.Fatal("Resolve() accepted a source outside a Git repository")
			}
		})
	}
}

func initializeRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	writeFile(t, filepath.Join(repository, ".gitignore"), "*.tmp\n")
	writeFile(t, filepath.Join(repository, "source.txt"), "original")
	for _, arguments := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.name", "Source Identity Test"},
		{"config", "user.email", "test@example.invalid"},
		{"add", "."},
		{"commit", "-m", "test: seed"},
	} {
		runGit(t, repository, arguments...)
	}
	return repository
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := gitenv.Detach(exec.Command("git", arguments...))
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
