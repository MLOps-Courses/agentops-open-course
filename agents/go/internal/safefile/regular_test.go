package safefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// This package is the repository's no-follow boundary, so every case here builds
// the hostile condition on a real filesystem: real symlinks, a real directory
// where a file is expected, and a real racing rename. A fake that "returns a
// symlink" would prove only that the fake works — the whole value of os.Root and
// of the before/open/after identity checks is what the kernel does with them.
//
// One limit is stated by test rather than left to be discovered:
// TestOpenFollowsASymlinkedParent. safefile confines the final entry, not the
// path to it; the parent guard lives in the callers (data/store.go).

const (
	// payload is arbitrary; only the identity of the inode behind a pathname
	// matters to this package, never its contents.
	payload = "safefile fixture payload"
	// raceBudget bounds the replacement race. A hit lands within milliseconds on
	// a multi-core machine and within a few hundred on a single-core one, so this
	// is roughly two orders of magnitude of slack rather than a tuned value.
	raceBudget = 30 * time.Second
	// swapAttempts bounds the escaping-symlink race. Each attempt is a handful of
	// syscalls, and the refusal rate observed at this size is far above zero on
	// every core count.
	swapAttempts = 5000
)

// hostileParent returns a fresh anchor directory and the pathname under test
// inside it. The anchor is the pathname's grandparent, so a case that needs to
// escape the opened parent has somewhere real to escape to.
func hostileParent(t *testing.T) (anchor, path string) {
	t.Helper()

	anchor = t.TempDir()
	parent := filepath.Join(anchor, "parent")
	if err := os.Mkdir(parent, 0o750); err != nil {
		t.Fatalf("create the parent directory: %v", err)
	}
	return anchor, filepath.Join(parent, "target.db")
}

// writeRegular plants an ordinary regular file and returns its path.
func writeRegular(t *testing.T, path string) string {
	t.Helper()

	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write the regular file %s: %v", path, err)
	}
	return path
}

// mustSymlink creates a real symlink, skipping when the platform will not make
// one. This mirrors every other symlink case in the repository: a filesystem
// that cannot represent the attack cannot be used to test the defense.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("create a symlink on this platform: %v", err)
	}
}

// refuse asserts Open rejects path and returns the refusal. A refusal must hand
// back no handle at all: a non-nil *Regular here would be an open descriptor on
// the very entry the policy just rejected.
func refuse(t *testing.T, path string) error {
	t.Helper()

	opened, err := Open(path)
	if err == nil {
		_ = opened.Close()
		t.Fatalf("Open(%s) succeeded on a path it must refuse", path)
	}
	if opened != nil {
		_ = opened.Close()
		t.Fatalf("Open(%s) returned a handle alongside its refusal", path)
	}
	return err
}

// openRegular asserts Open accepts path and closes the handle with the test.
func openRegular(t *testing.T, path string) *Regular {
	t.Helper()

	opened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s) error = %v, want nil", path, err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return opened
}

func TestOpenAcceptsAndKeepsVerifyingAStableRegularFile(t *testing.T) {
	t.Parallel()

	_, path := hostileParent(t)
	writeRegular(t, path)

	opened := openRegular(t, path)

	content, err := os.ReadFile(opened.File().Name())
	if err != nil {
		t.Fatalf("read back the opened file: %v", err)
	}
	if string(content) != payload {
		t.Errorf("File() names %q, whose content is %q, want the opened fixture", opened.File().Name(), content)
	}
	// Verify is the re-check callers run before they act on the handle, so it has
	// to stay quiet when nothing moved; a boundary that cries wolf gets removed.
	if err := opened.Verify(); err != nil {
		t.Errorf("Verify() error = %v, want nil on an untouched file", err)
	}
	if err := opened.Verify(); err != nil {
		t.Errorf("second Verify() error = %v, want nil: verification must not consume the handle", err)
	}
}

func TestOpenAcceptsAPathWithNoDirectoryComponent(t *testing.T) {
	// Not parallel: t.Chdir changes process-wide state. Every other case in this
	// package addresses its fixtures absolutely, so they are unaffected.
	_, path := hostileParent(t)
	writeRegular(t, path)
	t.Chdir(filepath.Dir(path))

	// filepath.Dir returns "." here, which os.OpenRoot must accept as the parent
	// rather than treat as a missing directory.
	opened := openRegular(t, filepath.Base(path))

	if err := opened.Verify(); err != nil {
		t.Errorf("Verify() error = %v, want nil for a bare relative name", err)
	}
}

func TestOpenRefusesANonRegularFinalEntry(t *testing.T) {
	tests := []struct {
		plant func(t *testing.T, anchor, path string)
		name  string
	}{
		{
			name: "symlink to a regular file beside it",
			plant: func(t *testing.T, _, path string) {
				beside := writeRegular(t, filepath.Join(filepath.Dir(path), "beside.db"))
				mustSymlink(t, beside, path)
			},
		},
		{
			name: "symlink escaping the anchored parent",
			plant: func(t *testing.T, anchor, path string) {
				outside := writeRegular(t, filepath.Join(anchor, "outside.db"))
				mustSymlink(t, outside, path)
			},
		},
		{
			name: "symlink pointing nowhere",
			plant: func(t *testing.T, anchor, path string) {
				mustSymlink(t, filepath.Join(anchor, "absent.db"), path)
			},
		},
		{
			name: "relative symlink to a regular file beside it",
			plant: func(t *testing.T, _, path string) {
				writeRegular(t, filepath.Join(filepath.Dir(path), "beside.db"))
				mustSymlink(t, "beside.db", path)
			},
		},
		{
			name: "directory",
			plant: func(t *testing.T, _, path string) {
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatalf("create the directory: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			anchor, path := hostileParent(t)
			test.plant(t, anchor, path)

			// Every symlink case here resolves to something openable, so a refusal
			// proves the inspection was an Lstat that never followed the link.
			if err := refuse(t, path); !strings.Contains(err.Error(), "must be a regular file") {
				t.Fatalf("Open() error = %v, want the no-follow regular-file refusal", err)
			}
		})
	}
}

func TestOpenRefusesAParentItCannotAnchor(t *testing.T) {
	tests := []struct {
		plant func(t *testing.T, anchor string)
		name  string
	}{
		{
			name:  "missing parent directory",
			plant: func(*testing.T, string) {},
		},
		{
			name: "parent is a regular file",
			plant: func(t *testing.T, anchor string) {
				writeRegular(t, filepath.Join(anchor, "impostor"))
			},
		},
		{
			name: "parent is a symlink pointing nowhere",
			plant: func(t *testing.T, anchor string) {
				mustSymlink(t, filepath.Join(anchor, "absent"), filepath.Join(anchor, "impostor"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			anchor := t.TempDir()
			test.plant(t, anchor)

			err := refuse(t, filepath.Join(anchor, "impostor", "target.db"))

			if !strings.Contains(err.Error(), "open parent of") {
				t.Fatalf("Open() error = %v, want the anchoring failure", err)
			}
		})
	}
}

func TestOpenRefusesAMissingFinalEntry(t *testing.T) {
	t.Parallel()

	_, path := hostileParent(t)

	err := refuse(t, path)

	if !strings.Contains(err.Error(), "without following links") {
		t.Fatalf("Open() error = %v, want the no-follow inspection failure", err)
	}
	// The cause has to survive the wrapping: callers distinguish "not published
	// yet" from "refused" with errors.Is rather than by reading the message.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open() error = %v, want it to unwrap to fs.ErrNotExist", err)
	}
}

func TestOpenRefusesARegularFileItCannotOpen(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		// Root ignores the file mode, so the unreadable entry this case describes
		// cannot be simulated. Reported rather than silently passed.
		t.Skip("running as root: an unopenable regular file cannot be simulated")
	}
	_, path := hostileParent(t)
	writeRegular(t, path)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("make the target unreadable: %v", err)
	}

	// The inspection succeeds — an unreadable regular file is still regular — so
	// this is the one refusal that can only come from the open itself.
	err := refuse(t, path)

	if !strings.Contains(err.Error(), "from its anchored parent") {
		t.Fatalf("Open() error = %v, want the anchored-open failure", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Open() error = %v, want it to unwrap to fs.ErrPermission", err)
	}
}

func TestOpenFollowsASymlinkedParent(t *testing.T) {
	t.Parallel()

	// Stated as a test because it is the package's boundary, not an oversight:
	// Open anchors on filepath.Dir(path), which is resolved the ordinary way, so
	// safefile confines the final entry and nothing above it. Callers that need
	// the directory itself checked do it themselves (data/store.go). If this ever
	// starts refusing, the callers' own directory guards became redundant.
	anchor := t.TempDir()
	resolved := filepath.Join(anchor, "resolved")
	if err := os.Mkdir(resolved, 0o750); err != nil {
		t.Fatalf("create the resolved directory: %v", err)
	}
	writeRegular(t, filepath.Join(resolved, "target.db"))
	mustSymlink(t, resolved, filepath.Join(anchor, "linked"))

	opened := openRegular(t, filepath.Join(anchor, "linked", "target.db"))

	if err := opened.Verify(); err != nil {
		t.Errorf("Verify() error = %v, want nil through a symlinked parent", err)
	}
}

func TestVerifyRefusesAPathThatNoLongerNamesTheOpenedFile(t *testing.T) {
	tests := []struct {
		rebind func(t *testing.T, anchor, path string)
		name   string
		want   string
	}{
		{
			name: "entry unlinked after the open",
			rebind: func(t *testing.T, _, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatalf("unlink the opened target: %v", err)
				}
			},
			want: "without following links",
		},
		{
			name: "entry replaced by another regular file",
			rebind: func(t *testing.T, anchor, path string) {
				staging := writeRegular(t, filepath.Join(anchor, "staging.db"))
				if err := os.Rename(staging, path); err != nil {
					t.Fatalf("replace the opened target: %v", err)
				}
			},
			want: "changed while it was being opened as a regular file",
		},
		{
			name: "entry replaced by a symlink",
			rebind: func(t *testing.T, anchor, path string) {
				outside := writeRegular(t, filepath.Join(anchor, "outside.db"))
				if err := os.Remove(path); err != nil {
					t.Fatalf("unlink the opened target: %v", err)
				}
				mustSymlink(t, outside, path)
			},
			want: "changed while it was being opened as a regular file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			anchor, path := hostileParent(t)
			writeRegular(t, path)
			opened := openRegular(t, path)
			test.rebind(t, anchor, path)

			// The descriptor is still perfectly usable here. Verify's job is to say
			// that the *pathname* stopped meaning it, which is what a caller about
			// to publish over that pathname has to know.
			err := opened.Verify()

			if err == nil {
				t.Fatal("Verify() error = nil, want a rebinding refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Verify() error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestVerifyReportsAHandleWhoseDescriptorIsClosed(t *testing.T) {
	t.Parallel()

	_, path := hostileParent(t)
	writeRegular(t, path)
	opened := openRegular(t, path)
	inspected, err := opened.File().Stat()
	if err != nil {
		t.Fatalf("inspect the opened file: %v", err)
	}
	if err := opened.File().Close(); err != nil {
		t.Fatalf("close the descriptor under the handle: %v", err)
	}

	if err := opened.Verify(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Verify() error = %v, want a closed-descriptor failure", err)
	}
	// verifyAgainst is exercised directly because Verify stats the descriptor
	// first: through the exported API the second stat can never be the one that
	// fails, yet Open calls verifyAgainst with an inspection Verify never sees.
	if err := opened.verifyAgainst(inspected); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("verifyAgainst() error = %v, want a closed-descriptor failure", err)
	}
}

func TestCloseIsSafeOnANilRegular(t *testing.T) {
	t.Parallel()

	// Callers join Close into an error return on paths where the open itself may
	// have failed, so the nil receiver has to be quiet rather than a panic that
	// replaces the real failure.
	var absent *Regular

	if err := absent.Close(); err != nil {
		t.Errorf("Close() on a nil *Regular error = %v, want nil", err)
	}
}

func TestCloseReleasesBothHandlesAndReportsASecondAttempt(t *testing.T) {
	t.Parallel()

	_, path := hostileParent(t)
	writeRegular(t, path)
	opened, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}

	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	// Both halves are released, so the pathname's own file and the anchored
	// parent are gone together; a second Close reports it rather than pretending.
	if err := opened.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("second Close() error = %v, want a closed-handle failure", err)
	}
	if err := opened.Verify(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Verify() after Close() error = %v, want a closed-handle failure", err)
	}
}

// replacer renames a fresh entry over a pathname in a tight loop, which is what
// turns the time-of-check/time-of-use window from a description into an event
// the test can actually observe. Rename is atomic, so the pathname is occupied
// at every instant and nothing but the swap can explain a refusal.
type replacer struct {
	failure chan error
	halt    chan struct{}
}

// startReplacer begins swapping entries over path. stage plants the next
// replacement at the staging pathname it is handed, in the same directory, so
// the rename stays within one filesystem and therefore stays atomic.
func startReplacer(path string, stage func(staging string, round int) error) *replacer {
	swapper := &replacer{failure: make(chan error, 1), halt: make(chan struct{})}
	staging := filepath.Join(filepath.Dir(path), "staging.tmp")
	go func() {
		for round := 0; ; round++ {
			select {
			case <-swapper.halt:
				swapper.failure <- nil
				return
			default:
			}
			err := stage(staging, round)
			if err == nil {
				err = os.Rename(staging, path)
			}
			if err != nil {
				swapper.failure <- err
				return
			}
		}
	}()
	return swapper
}

// stop halts the loop and surfaces whatever it could not do, so a race that
// never happened is reported as a broken fixture rather than a passing test.
func (r *replacer) stop(t *testing.T) {
	t.Helper()

	close(r.halt)
	if err := <-r.failure; err != nil {
		t.Errorf("replacement loop failed: %v", err)
	}
}

func TestOpenRefusesAFileReplacedBetweenInspectionAndOpen(t *testing.T) {
	t.Parallel()

	_, path := hostileParent(t)
	writeRegular(t, path)
	// Every replacement is itself a regular file, so the inspection and the open
	// both always see one. The only thing that changes is which inode the name
	// resolves to, which isolates the identity checks from every other refusal.
	swapper := startReplacer(path, func(staging string, _ int) error {
		return os.WriteFile(staging, []byte(payload), 0o600)
	})
	defer swapper.stop(t)

	attempts, deadline := 0, time.Now().Add(raceBudget)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("no replacement was caught in %s across %d attempts", raceBudget, attempts)
		}
		attempts++
		opened, err := Open(path)
		if err == nil {
			if closeErr := opened.Close(); closeErr != nil {
				t.Fatalf("Close() error = %v, want nil", closeErr)
			}
			continue
		}
		// Strict on purpose: under a pure regular-file swap the identity refusal
		// is the only failure Open can legitimately produce. Anything else — a
		// descriptor exhaustion from a refusal path that forgot to close, say —
		// is a real defect and must not be absorbed as "the race did not hit".
		if !strings.Contains(err.Error(), "changed while it was being opened as a regular file") {
			t.Fatalf("Open() error = %v after %d attempts, want the identity refusal", err, attempts)
		}
		return
	}
}

func TestOpenNeverYieldsAnEscapingSymlinkUnderAReplacementRace(t *testing.T) {
	t.Parallel()

	anchor, path := hostileParent(t)
	outside := writeRegular(t, filepath.Join(anchor, "attacker.db"))
	outsideInfo, err := os.Lstat(outside)
	if err != nil {
		t.Fatalf("inspect the attacker file: %v", err)
	}
	writeRegular(t, path)
	probe := filepath.Join(anchor, "probe")
	mustSymlink(t, outside, probe)
	if err := os.Remove(probe); err != nil {
		t.Fatalf("remove the symlink probe: %v", err)
	}

	// Alternating a symlink out of the anchored parent with an ordinary regular
	// file is the attack this package exists for: whatever the timing, a caller
	// must never end up holding the file outside the parent it anchored on.
	swapper := startReplacer(path, func(staging string, round int) error {
		if round%2 == 0 {
			return os.Symlink(outside, staging)
		}
		return os.WriteFile(staging, []byte(payload), 0o600)
	})
	defer swapper.stop(t)

	// The three refusals the swap can produce, depending on where the rename
	// lands: before the inspection, between it and the open, or after the open.
	// Listing them exhaustively is what makes a leaked descriptor visible — an
	// exhausted table would surface as a fourth message rather than as a refusal
	// that happens to keep the count non-zero.
	expected := []string{"must be a regular file", "from its anchored parent", "changed while"}
	refusals := 0
	for attempt := range swapAttempts {
		opened, err := Open(path)
		if err != nil {
			refusals++
			if !slices.ContainsFunc(expected, func(want string) bool {
				return strings.Contains(err.Error(), want)
			}) {
				t.Fatalf("Open() error = %v on attempt %d, want one of %q", err, attempt, expected)
			}
			continue
		}
		held, statErr := opened.File().Stat()
		if statErr != nil {
			t.Fatalf("inspect the handle Open returned on attempt %d: %v", attempt, statErr)
		}
		if os.SameFile(held, outsideInfo) {
			t.Fatalf("Open() handed back the file outside the anchored parent on attempt %d", attempt)
		}
		if !held.Mode().IsRegular() {
			t.Fatalf("Open() handed back a %s on attempt %d", held.Mode().Type(), attempt)
		}
		if closeErr := opened.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v, want nil", closeErr)
		}
	}
	if refusals == 0 {
		t.Fatalf("no refusal across %d attempts: the swap never raced the open", swapAttempts)
	}
}
