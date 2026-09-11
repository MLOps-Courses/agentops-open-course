// Package data is the access layer over the committed AgentOps dataset: the
// SQLite incident database and the runbook and log corpus beside it.
//
// Two directories, two contracts, and the separation is the whole point:
//
//   - DataDir holds the committed dataset. It is immutable input. Nothing here
//     ever opens incidents.db read-write from that directory, because the
//     repository gate rebuilds it with a pinned SQLite and compares it byte for
//     byte — one stray write there breaks the build for everyone.
//   - StateDir holds disposable runtime state. The first *write* publishes a
//     copy of the seed into it, and every mutation lands there.
//
// Reads fall back to the seed when no state has been published, so the entire
// read surface works on a fresh checkout with an empty state directory. Only a
// write creates state.
package data

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/safefile"
)

// databaseName is the file name the committed seed and its runtime copy share.
const databaseName = "incidents.db"

// stateDirPerm is the mode of the runtime state directory. The runtime database
// itself inherits 0600 from the staging file whose inode it links to.
const stateDirPerm = 0o750

// ErrDataAccess marks every failure this package reports and is the Go
// counterpart of Python's DataAccessError. Callers that need to tell "the
// dataset boundary rejected this" apart from any other failure test it with
// errors.Is; the cause wrapped underneath keeps the driver's own message.
var ErrDataAccess = errors.New("data access failed")

// Config is the slice of agent configuration this package needs.
//
// It is a value the caller passes in, never a package-level singleton. The
// Python module read a mutable `settings` global that its tests monkeypatched;
// reproducing that in Go would make every test order-dependent and would make
// two [Store] values pointed at different directories impossible.
type Config struct {
	// DataDir holds the committed dataset — incidents.db, runbooks/, logs/.
	DataDir string
	// StateDir holds disposable runtime state. It is created on first write.
	StateDir string
}

// Store reads the committed dataset and mutates runtime state.
//
// The zero value is not usable; construct one with [New].
type Store struct {
	// The two seams come first only because the fieldalignment analyzer wants
	// the pointer-sized fields at the front of the struct.

	// linkFile publishes runtime state, and is os.Link everywhere but one test.
	//
	// It is a field because the two failures it guards against cannot be
	// provoked otherwise: the window between "the destination does not exist"
	// and "link it into place" is microseconds wide, and a link that fails for
	// filesystem reasons needs a filesystem that refuses to link. Python's
	// tests monkeypatched os.link for exactly these two cases.
	linkFile func(oldname, newname string) error

	// onRestartContext runs inside the restart transaction, immediately after
	// the decision context is read.
	//
	// It exists for one regression test: the guarantee that the context is read
	// *after* BEGIN IMMEDIATE, not before. A competing writer has no other
	// observation point inside another connection's transaction, and the
	// difference is invisible from the outside because prepareRuntimeDatabase's
	// own transaction serializes the two. Production leaves it nil.
	onRestartContext func()

	// beforeDatabaseConnect is a deterministic replacement-race seam. The
	// production value is nil; tests use it after the no-follow reference is
	// open but before SQLite resolves the pathname.
	beforeDatabaseConnect func(string)

	dataDir  string
	stateDir string
}

// New returns a Store bound to cfg's directories.
func New(cfg Config) *Store {
	return &Store{dataDir: cfg.DataDir, stateDir: cfg.StateDir, linkFile: os.Link}
}

// DataDir returns the directory holding the committed, immutable dataset.
func (s *Store) DataDir() string { return s.dataDir }

// StateDir returns the directory holding disposable runtime state.
func (s *Store) StateDir() string { return s.stateDir }

// SeedPath returns the committed seed database. It is read-only input; use
// [Store.DBPath] for anything that writes.
func (s *Store) SeedPath() string { return filepath.Join(s.dataDir, databaseName) }

// RuntimePath returns where published runtime state lives, whether or not it
// has been published yet.
func (s *Store) RuntimePath() string { return filepath.Join(s.stateDir, databaseName) }

// DBPath returns a writable runtime copy of the committed SQLite seed,
// publishing it on first use.
//
// This is the only function that creates state, which is why every read path
// deliberately avoids it.
func (s *Store) DBPath() (string, error) {
	if err := s.verifyStateDirectory(); err != nil {
		return "", err
	}
	destination := s.RuntimePath()
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%w: Runtime database must be a regular file: %s", ErrDataAccess, destination)
		}
		opened, openErr := safefile.Open(destination)
		if openErr != nil {
			return "", fmt.Errorf("%w: Runtime database must remain a regular file: %s: %w",
				ErrDataAccess, destination, openErr)
		}
		if closeErr := opened.Close(); closeErr != nil {
			return "", fmt.Errorf("%w: Could not close the runtime database inspection for %s: %w",
				ErrDataAccess, destination, closeErr)
		}
		return destination, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: Could not inspect the runtime database %s without following links: %w",
			ErrDataAccess, destination, err)
	}
	source := s.SeedPath()
	if info, err := os.Lstat(source); errors.Is(err, fs.ErrNotExist) {
		// The full path, not the base name: a missing seed is almost always a
		// misconfigured AGENT_DATA_DIR, and the operator needs to see where we
		// actually looked.
		return "", fmt.Errorf("%w: Seed database is missing: %s", ErrDataAccess, source)
	} else if err != nil {
		return "", fmt.Errorf("%w: Could not inspect the seed database %s without following links: %w",
			ErrDataAccess, source, err)
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: Seed database must be a regular file: %s", ErrDataAccess, source)
	}
	seed, err := safefile.Open(source)
	if err != nil {
		return "", fmt.Errorf("%w: Seed database must remain a regular file: %s: %w", ErrDataAccess, source, err)
	}
	defer func() { _ = seed.Close() }()
	if err := os.MkdirAll(s.stateDir, stateDirPerm); err != nil {
		return "", fmt.Errorf("%w: Could not create the runtime state directory %s: %w", ErrDataAccess, s.stateDir, err)
	}
	if info, err := os.Lstat(s.stateDir); err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: Runtime state directory must be a real directory: %s", ErrDataAccess, s.stateDir)
	}
	if err := s.publish(seed.File(), destination); err != nil {
		return "", fmt.Errorf("%w: Could not initialize runtime database: %s: %w", ErrDataAccess, destination, err)
	}
	return destination, nil
}

// verifyStateDirectory rejects a final-component link before any runtime path
// is resolved beneath it. Missing is valid for a fresh write boundary; DBPath
// creates the directory only after the immutable seed is proven usable.
func (s *Store) verifyStateDirectory() error {
	info, err := os.Lstat(s.stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: Could not inspect the runtime state directory %s: %w", ErrDataAccess, s.stateDir, err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: Runtime state directory must be a real directory: %s", ErrDataAccess, s.stateDir)
	}
	return nil
}

// publish copies the seed into a staging file inside the state directory and
// hard-links it into place.
//
// The hard link is what makes publication safe under a race, because it is an
// exclusive create: two workers starting at once each stage a complete copy,
// exactly one link succeeds, and the loser's EEXIST is a *success* — the
// winner's file is a byte-identical copy of the same seed. os.Rename would be
// wrong here; it would clobber live state the winner may already be writing to.
func (s *Store) publish(source *os.File, destination string) (err error) {
	staging, err := os.CreateTemp(s.stateDir, ".incidents-*.tmp")
	if err != nil {
		return fmt.Errorf("stage the seed copy: %w", err)
	}
	name := staging.Name()
	defer func() {
		// The staging file goes away on every path, including the successful
		// one where the state directory now holds a second name for the same
		// inode. A staging file left behind is the failure mode the Python
		// tests assert against explicitly.
		if removeErr := os.Remove(name); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove the staging file %s: %w", name, removeErr))
		}
	}()
	if copyErr := copyInto(staging, source); copyErr != nil {
		_ = staging.Close()
		return copyErr
	}
	// fsync before linking: the link makes the copy visible to every other
	// worker, so the bytes must be durable before the name exists.
	if syncErr := staging.Sync(); syncErr != nil {
		_ = staging.Close()
		return fmt.Errorf("flush the seed copy to disk: %w", syncErr)
	}
	if closeErr := staging.Close(); closeErr != nil {
		return fmt.Errorf("close the seed copy: %w", closeErr)
	}
	if linkErr := s.linkFile(name, destination); linkErr != nil && !errors.Is(linkErr, fs.ErrExist) {
		return fmt.Errorf("link the runtime database into place: %w", linkErr)
	}
	published, err := safefile.Open(destination)
	if err != nil {
		return fmt.Errorf("published runtime database is not a stable regular file: %w", err)
	}
	if closeErr := published.Close(); closeErr != nil {
		return fmt.Errorf("close the published runtime database inspection: %w", closeErr)
	}
	return nil
}

// copyInto streams the seed into an already-open staging file.
func copyInto(staging, seed *os.File) error {
	if _, err := io.Copy(staging, seed); err != nil {
		return fmt.Errorf("copy the seed database %s: %w", seed.Name(), err)
	}
	return nil
}

// verifyRegularPath applies the shared restore/data policy without retaining a
// handle. Callers that will use the file reopen it through the same policy at
// the actual operation boundary.
func verifyRegularPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	opened, err := safefile.Open(path)
	if err != nil {
		return err
	}
	return opened.Close()
}
