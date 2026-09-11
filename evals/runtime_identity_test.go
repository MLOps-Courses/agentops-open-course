package evals

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeIdentityRejectsStaleBinary(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	stale := source
	stale.TreeDigest = "sha256:" + strings.Repeat("c", 64)
	runner := func(_ context.Context, name string, arguments ...string) ([]byte, error) {
		if name != "/tmp/agent" || len(arguments) != 1 || arguments[0] != "version" {
			t.Fatalf("command = %q %q, want exact process binary version", name, arguments)
		}
		return runtimeVersionJSON(t, stale), nil
	}

	err := verifyRuntimeIdentity(t.Context(), "/tmp/agent", source, runner)
	if err == nil || !strings.Contains(err.Error(), "tree_digest") {
		t.Fatalf("error = %v, want stale tree digest rejection", err)
	}
}

func TestRuntimeIdentityRejectsBinaryFromAnotherRevision(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	other := source
	other.Revision = strings.Repeat("c", 40)
	runner := func(context.Context, string, ...string) ([]byte, error) {
		return runtimeVersionJSON(t, other), nil
	}

	err := verifyRuntimeIdentity(t.Context(), "/tmp/agent", source, runner)
	if err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("error = %v, want stale revision rejection", err)
	}
}

func TestRuntimeIdentityAcceptsMatchingBinary(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	runner := func(context.Context, string, ...string) ([]byte, error) {
		return runtimeVersionJSON(t, source), nil
	}
	if err := verifyRuntimeIdentity(t.Context(), "/tmp/agent", source, runner); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeIdentityRejectsMalformedVersion(t *testing.T) {
	t.Parallel()

	runner := func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"dirty":false}`), nil
	}
	err := verifyRuntimeIdentity(t.Context(), "/tmp/agent", testSourceEvidence(), runner)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v, want malformed version rejection", err)
	}
}

func TestRuntimeIdentityRequiresTheCheckoutTreeDigest(t *testing.T) {
	t.Parallel()

	// Two empty digests must never compare equal by accident.
	source := SourceEvidence{Revision: strings.Repeat("a", 40)}
	runner := func(context.Context, string, ...string) ([]byte, error) {
		return runtimeVersionJSON(t, source), nil
	}
	err := verifyRuntimeIdentity(t.Context(), "/tmp/agent", source, runner)
	if err == nil || !strings.Contains(err.Error(), "tree digest") {
		t.Fatalf("error = %v, want missing checkout digest rejection", err)
	}
}

func TestContainerRuntimeIdentityPinsMutableTagToInspectedImage(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	imageID, err := resolveContainerRuntimeIdentity(
		t.Context(), "docker", "agent:candidate", source, fakeContainerIdentityRunner(t, source),
	)
	if err != nil {
		t.Fatal(err)
	}
	if imageID != "sha256:image-id" {
		t.Fatalf("resolved image = %q", imageID)
	}
}

func TestContainerRuntimeIdentityRejectsStaleImage(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	stale := source
	stale.TreeDigest = "sha256:" + strings.Repeat("c", 64)
	_, err := resolveContainerRuntimeIdentity(
		t.Context(), "docker", "agent:candidate", source, fakeContainerIdentityRunner(t, stale),
	)
	if err == nil || !strings.Contains(err.Error(), "tree_digest") {
		t.Fatalf("error = %v, want stale image rejection", err)
	}
}

func TestRuntimeVersionAllowsEmptyDirtyRevision(t *testing.T) {
	t.Parallel()
	source := SourceEvidence{TreeDigest: "sha256:" + strings.Repeat("b", 64), Dirty: true}
	decoded, err := decodeRuntimeVersion(runtimeVersionJSON(t, source))
	if err != nil || decoded != source {
		t.Fatalf("decoded/error = %#v/%v", decoded, err)
	}
}

func TestRuntimeIdentityRefusesAnUnanswerableRequest(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	answering := func(context.Context, string, ...string) ([]byte, error) {
		return runtimeVersionJSON(t, source), nil
	}
	for name, test := range map[string]struct {
		binary   string
		want     string
		run      runtimeCommand
		expected SourceEvidence
	}{
		"no binary": {binary: "  ", expected: source, run: answering, want: "needs an agent binary"},
		"unusable expectation": {
			binary:   "/tmp/agent",
			expected: SourceEvidence{Revision: strings.Repeat("a", 40), Dirty: true},
			run:      answering,
			want:     "validate expected checkout",
		},
		"no command": {binary: "/tmp/agent", expected: source, want: "command is unavailable"},
		"command failed": {
			binary: "/tmp/agent", expected: source,
			run:  func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("exec failed") },
			want: "query runtime identity",
		},
		"not json": {
			binary: "/tmp/agent", expected: source,
			run:  func(context.Context, string, ...string) ([]byte, error) { return []byte("agent v1"), nil },
			want: "version output is not JSON",
		},
		"impossible tuple": {
			// A build that claims both a revision and a dirty tree is describing two
			// different checkouts, so it can never be the one under evaluation.
			binary: "/tmp/agent", expected: source,
			run: func(context.Context, string, ...string) ([]byte, error) {
				return []byte(`{"revision":"` + strings.Repeat("a", 40) +
					`","tree_digest":"sha256:` + strings.Repeat("b", 64) + `","dirty":true}`), nil
			},
			want: "invalid source tuple",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := verifyRuntimeIdentity(t.Context(), test.binary, test.expected, test.run)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyRuntimeIdentity() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestContainerRuntimeIdentityRefusesAnUnresolvableImage(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	for name, test := range map[string]struct {
		engine string
		image  string
		run    runtimeCommand
		want   string
	}{
		"no engine": {image: "agent:candidate", run: fakeContainerIdentityRunner(t, source), want: "needs an engine and image"},
		"no image":  {engine: "docker", run: fakeContainerIdentityRunner(t, source), want: "needs an engine and image"},
		"no command": {
			engine: "docker", image: "agent:candidate", want: "command is unavailable",
		},
		"inspect failed": {
			engine: "docker", image: "agent:candidate",
			run:  func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no such image") },
			want: "inspect container image identity",
		},
		"no image id": {
			// Without an immutable image ID the harness would have to run the mutable
			// tag, which can point somewhere else by the time the trials start.
			engine: "docker", image: "agent:candidate",
			run:  func(context.Context, string, ...string) ([]byte, error) { return []byte("  \n"), nil },
			want: "empty image ID",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveContainerRuntimeIdentity(t.Context(), test.engine, test.image, source, test.run)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolveContainerRuntimeIdentity() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestExecuteRuntimeCommandKeepsTheChildDiagnosis(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	answering := filepath.Join(directory, "answering")
	writeExecutableScript(t, answering, "#!/bin/sh\nprintf 'agent-output'\n")
	output, err := executeRuntimeCommand(t.Context(), answering)
	if err != nil || string(output) != "agent-output" {
		t.Fatalf("executeRuntimeCommand() = %q, %v, want the child's stdout", output, err)
	}

	// A candidate that refuses to report its identity is the failure a learner has
	// to debug, so its own stderr is the one provider text worth keeping: it comes
	// from the binary under evaluation, not from a remote provider.
	failing := filepath.Join(directory, "failing")
	writeExecutableScript(t, failing, "#!/bin/sh\necho 'unknown command: version' >&2\nexit 3\n")
	if _, err := executeRuntimeCommand(t.Context(), failing, "version"); err == nil ||
		!strings.Contains(err.Error(), "unknown command: version") {
		t.Fatalf("executeRuntimeCommand(failing) error = %v, want the child's stderr", err)
	}

	silent := filepath.Join(directory, "silent")
	writeExecutableScript(t, silent, "#!/bin/sh\nexit 4\n")
	if _, err := executeRuntimeCommand(t.Context(), silent); err == nil ||
		!strings.Contains(err.Error(), "exit status 4") {
		t.Fatalf("executeRuntimeCommand(silent) error = %v, want the exit status", err)
	}
	if _, err := executeRuntimeCommand(t.Context(), filepath.Join(directory, "absent")); err == nil {
		t.Fatal("executeRuntimeCommand(absent) error = nil, want a start failure")
	}
}

// Linux refuses to exec a file another descriptor still holds open for writing,
// and the harness execs the binary it has just snapshotted. This reproduces that
// window deterministically — the writer is released well inside the retry budget —
// so the retry in executeRuntimeCommand is proven rather than assumed.
func TestExecuteRuntimeCommandWaitsOutABusyBinary(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), "busy")
	writer, err := os.OpenFile(binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatalf("OpenFile(busy) error = %v", err)
	}
	if _, writeErr := writer.WriteString("#!/bin/sh\nprintf 'released'\n"); writeErr != nil {
		t.Fatalf("WriteString(busy) error = %v", writeErr)
	}

	// Held open on purpose: every exec against it fails with ETXTBSY until Close.
	if _, busyErr := executeRuntimeCommandOnce(t.Context(), binary); !errors.Is(busyErr, syscall.ETXTBSY) {
		_ = writer.Close()
		t.Fatalf("single attempt error = %v, want ETXTBSY while the writer is open", busyErr)
	}

	released := make(chan error, 1)
	go func() {
		time.Sleep(busyRetryDelay)
		released <- writer.Close()
	}()

	output, err := executeRuntimeCommand(t.Context(), binary)
	if err != nil || string(output) != "released" {
		t.Fatalf("executeRuntimeCommand() = %q, %v, want the retry to outlast the writer", output, err)
	}
	if err := <-released; err != nil {
		t.Fatalf("Close(busy) error = %v", err)
	}
}

// A binary nothing ever releases must still fail, and fail inside the budget
// rather than retrying forever.
func TestExecuteRuntimeCommandGivesUpOnAPermanentlyBusyBinary(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), "held")
	writer, err := os.OpenFile(binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatalf("OpenFile(held) error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, writeErr := writer.WriteString("#!/bin/sh\nexit 0\n"); writeErr != nil {
		t.Fatalf("WriteString(held) error = %v", writeErr)
	}

	started := time.Now()
	if _, err := executeRuntimeCommand(t.Context(), binary); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("executeRuntimeCommand(held) error = %v, want the original ETXTBSY", err)
	}
	if elapsed := time.Since(started); elapsed > busyRetryLimit*busyRetryDelay*10 {
		t.Fatalf("gave up after %v, want a bounded retry budget", elapsed)
	}
}

func TestSnapshotRuntimeBinaryCopiesTheExactCandidate(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "agent")
	const body = "#!/bin/sh\nexit 0\n"
	writeExecutableScript(t, source, body)

	// The snapshot is what the trials actually execute, so it lives in the run's
	// own isolated state and is named independently of whatever the caller passed.
	state := t.TempDir()
	snapshot, err := snapshotRuntimeBinary(source, state)
	if err != nil {
		t.Fatalf("snapshotRuntimeBinary() error = %v", err)
	}
	if snapshot != filepath.Join(state, "agent-evaluated") {
		t.Fatalf("snapshot = %q, want the isolated agent-evaluated path", snapshot)
	}
	copied, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("ReadFile(snapshot) error = %v", err)
	}
	if string(copied) != body {
		t.Fatalf("snapshot body = %q, want the exact candidate %q", copied, body)
	}
	info, err := os.Stat(snapshot)
	if err != nil {
		t.Fatalf("Stat(snapshot) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("snapshot mode = %v, want 0700", got)
	}
	// Re-snapshotting into the same state would silently evaluate a stale copy.
	if _, err := snapshotRuntimeBinary(source, state); err == nil ||
		!strings.Contains(err.Error(), "create runtime binary snapshot") {
		t.Fatalf("snapshotRuntimeBinary(existing) error = %v, want an exclusive-create failure", err)
	}

	if _, err := snapshotRuntimeBinary(filepath.Join(state, "absent"), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "open runtime binary") {
		t.Fatalf("snapshotRuntimeBinary(absent) error = %v, want an open failure", err)
	}
	if _, err := snapshotRuntimeBinary(state, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("snapshotRuntimeBinary(directory) error = %v, want a regular-file refusal", err)
	}
}

func writeExecutableScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func runtimeVersionJSON(t *testing.T, source SourceEvidence) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"build_timestamp": "2026-08-09T08:00:00Z",
		"mode":            "development",
		"version":         "development",
		"revision":        source.Revision,
		"tree_digest":     source.TreeDigest,
		"dirty":           source.Dirty,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func fakeContainerIdentityRunner(t *testing.T, source SourceEvidence) runtimeCommand {
	t.Helper()
	return func(_ context.Context, name string, arguments ...string) ([]byte, error) {
		if name != "docker" {
			t.Fatalf("engine = %q, want docker", name)
		}
		switch {
		case len(arguments) == 5 && arguments[0] == "image" && arguments[1] == "inspect" &&
			arguments[2] == "--format" && arguments[3] == "{{.Id}}" && arguments[4] == "agent:candidate":
			return []byte("sha256:image-id\n"), nil
		case len(arguments) == 7 && arguments[0] == "run" && arguments[1] == "--rm" &&
			arguments[2] == "--network" && arguments[3] == "none" && arguments[4] == "--read-only" &&
			arguments[5] == "sha256:image-id" && arguments[6] == "version":
			return runtimeVersionJSON(t, source), nil
		default:
			t.Fatalf("unexpected container identity command: %q", arguments)
			return nil, nil
		}
	}
}
