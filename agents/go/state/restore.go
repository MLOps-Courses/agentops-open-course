package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// renameFile is every rename that participates in the restore transaction: the
// journal's publication, the quarantine of the old generation, the publication
// of the new one, and the reinstatement during a rollback.
//
// It is a variable for exactly one reason. The three-phase journal only earns
// its keep if a real process dying between two of those renames still recovers,
// and the only honest way to prove that is to kill a real process at a real
// seam. The crash tests re-exec this package's own test binary and replace this
// function with one that calls os.Exit at the seam under test; the Python track
// monkeypatched Path.replace to do the same. It is unexported, so no production
// caller — in this module or any other — can reach it.
var renameFile = os.Rename

// RestoreOptions carries the settings a restore is driven by.
type RestoreOptions struct {
	// Logger receives the restore's progress. Nil uses slog.Default.
	Logger *slog.Logger

	// BeforePublish runs immediately before each database is published, with
	// the count of databases already installed.
	//
	// It is a test-only failure-injection seam and it is in the production
	// signature on purpose: publication failures are the case the rollback
	// exists for, and a caller that can provoke one can prove the rollback
	// works. It is a parameter rather than an environment lookup because
	// production behavior during a restore must never depend on an ambient
	// variable somebody could set on the way in.
	BeforePublish func(installed int) error

	// mkdir and syncDir are deterministic failure seams for the three
	// pre-journal durability boundaries. They stay unexported so production
	// callers cannot replace filesystem semantics.
	mkdir   func(string, os.FileMode) error
	syncDir func(string) error
}

func (o RestoreOptions) filesystem() (func(string, os.FileMode) error, func(string) error) {
	mkdir := o.mkdir
	if mkdir == nil {
		mkdir = os.Mkdir
	}
	syncDir := o.syncDir
	if syncDir == nil {
		syncDir = fsyncDir
	}
	return mkdir, syncDir
}

// RestoreState restores one complete generation with durable crash recovery
// across every file.
//
// Writers must be stopped first. The sequence is: validate the whole snapshot,
// take the generation lock, finish any interrupted transaction, stage and
// verify the new generation, record a durable journal, quarantine the old
// generation, publish the new one, then commit and clean up. A failure at any
// point after the journal exists rolls back to the byte-exact old generation; a
// crash at any point leaves a journal the next recovery can act on.
func RestoreState(ctx context.Context, snapshot, stateDir string, opts RestoreOptions) ([]string, error) {
	logger := resolveLogger(opts.Logger)
	// Nothing below this line runs until the snapshot has fully validated, which
	// is what makes every rejection leave live state untouched.
	entries, err := validateInventory(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	if err := RecoverInterruptedRestore(stateDir, RecoverOptions{Logger: logger}); err != nil {
		return nil, err
	}
	if mkdirErr := os.MkdirAll(stateDir, dirPerm); mkdirErr != nil {
		return nil, fmt.Errorf("%w: Could not create the state directory %s: %w", ErrSnapshot, stateDir, mkdirErr)
	}
	if err := withRestoreLock(filepath.Join(stateDir, restoreLockName), func() error {
		if recoverErr := recoverRestoreLocked(stateDir, logger); recoverErr != nil {
			return recoverErr
		}
		return publishRestore(snapshot, stateDir, entries, opts)
	}); err != nil {
		return nil, err
	}

	installed := make([]string, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(stateDir, entry.Filename)
		installed = append(installed, path)
		logger.Info("restored state file", "file", entry.Filename, "target", path)
	}
	logger.Info("restore complete", "state_dir", stateDir)
	return installed, nil
}

// publishRestore runs one restore transaction under the generation lock.
func publishRestore(snapshot, stateDir string, entries []manifestEntry, opts RestoreOptions) (err error) {
	mkdir, syncDir := opts.filesystem()
	transactionID, err := newTransactionID()
	if err != nil {
		return err
	}
	staging := filepath.Join(stateDir, stagingPrefix+transactionID)
	quarantine := filepath.Join(stateDir, quarantinePrefix+transactionID)
	// Mkdir, not MkdirAll: the transaction id is fresh, so an existing directory
	// under either name means something is very wrong and must not be reused.
	if mkdirErr := mkdir(staging, dirPerm); mkdirErr != nil {
		return fmt.Errorf("%w: Could not create the restore staging directory %s: %w", ErrSnapshot, staging, mkdirErr)
	}
	// journal is nil until the durable journal actually exists on disk, which is
	// what tells the unwind below whether a rollback is possible or whether the
	// transaction directories are the only owned evidence to discard. Install
	// this immediately: a failed quarantine mkdir or state-dir fsync must not
	// strand unexplained residue that the next startup correctly refuses.
	var journal *restoreJournal
	defer func() {
		recovered := recover()
		if err == nil && recovered == nil {
			return
		}
		unwound := unwindRestore(stateDir, journal, staging, quarantine)
		if recovered == nil {
			err = errors.Join(err, unwound)
			return
		}
		// The Python track catches BaseException here so that an interrupt
		// rolls back rather than leaving a half-published state. Go's closest
		// equivalent is a panic: recover, undo, and re-panic with the original
		// value so the caller still sees the failure it would have seen. A
		// signal is not recoverable in either language, which is exactly why
		// the on-disk journal exists.
		if unwound != nil {
			panic(fmt.Errorf("restore rollback after panic %v: %w", recovered, unwound))
		}
		panic(recovered)
	}()
	if mkdirErr := mkdir(quarantine, dirPerm); mkdirErr != nil {
		return fmt.Errorf("%w: Could not create the restore quarantine directory %s: %w",
			ErrSnapshot, quarantine, mkdirErr)
	}
	if syncErr := syncDir(stateDir); syncErr != nil {
		return syncErr
	}

	published, err := stageGeneration(snapshot, staging, entries)
	if err != nil {
		return err
	}
	current, err := stateFiles(stateDir)
	if err != nil {
		return err
	}
	// The old generation records the sidecars too — WAL, SHM and journal files —
	// because those are live state a rollback has to put back, and because the
	// publish step below only moves databases, which is what removes a previous
	// generation's stale sidecars for good.
	old := make(inventory, 0, len(current))
	for _, path := range current {
		evidence, evidenceErr := evidenceOf(path)
		if evidenceErr != nil {
			return evidenceErr
		}
		old = append(old, evidence)
	}

	prepared := newRestoreJournal(transactionID, old, published)
	durable, err := writeRestoreJournal(stateDir, *prepared)
	if durable {
		// From here on a failure is recoverable from disk, so the unwind rolls
		// back instead of discarding.
		journal = prepared
	}
	if err != nil {
		return err
	}

	if err := quarantineCurrentGeneration(stateDir, quarantine, current); err != nil {
		return err
	}
	if err := publishStagedDatabases(stateDir, staging, opts); err != nil {
		return err
	}
	if err := assertExactLiveInventory(stateDir, published, "new"); err != nil {
		return err
	}
	if err := setRestorePhase(stateDir, journal, phaseCommitted); err != nil {
		return err
	}
	return cleanupRestoreTransaction(stateDir, *journal)
}

// stageGeneration copies the snapshot into the staging directory and proves
// every copy is byte-identical to what the manifest signed.
func stageGeneration(snapshot, staging string, entries []manifestEntry) (inventory, error) {
	staged := make(inventory, 0, len(entries))
	for _, entry := range entries {
		source := filepath.Join(snapshot, entry.Filename)
		target := filepath.Join(staging, entry.Filename)
		if err := copyFileContents(source, target); err != nil {
			return nil, err
		}
		evidence, err := evidenceOf(target)
		if err != nil {
			return nil, err
		}
		if evidence.SizeBytes != entry.SizeBytes || evidence.SHA256 != entry.SHA256 {
			// The manifest was verified moments ago, so a mismatch means the
			// snapshot is being written while it is being restored from.
			return nil, snapshotErrorf("Staged snapshot database changed while copying: %s", entry.Filename)
		}
		staged = append(staged, evidence)
	}
	// The staged bytes must be durable before the journal names them, or a crash
	// would leave a journal pointing at content that was never written.
	if err := fsyncDir(staging); err != nil {
		return nil, err
	}
	return staged, nil
}

// copyFileContents copies a snapshot database into staging and flushes it.
func copyFileContents(source, target string) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("%w: Could not read the snapshot database %s: %w", ErrSnapshot, source, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, documentPerm)
	if err != nil {
		return fmt.Errorf("%w: Could not stage the database %s: %w", ErrSnapshot, target, err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("%w: Could not close the staged database %s: %w", ErrSnapshot, target, closeErr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("%w: Could not stage the database %s: %w", ErrSnapshot, target, err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("%w: Could not flush the staged database %s: %w", ErrSnapshot, target, err)
	}
	return nil
}

// quarantineCurrentGeneration moves the live generation aside, one file at a
// time, fsyncing both directories after each move.
//
// Per file rather than in a batch: the fsyncs are what let recovery tell "this
// file has moved" from "this file might have moved", and batching them would
// leave a window where neither directory's entry is durable.
func quarantineCurrentGeneration(stateDir, quarantine string, current []string) error {
	for _, path := range current {
		if err := renameFile(path, filepath.Join(quarantine, filepath.Base(path))); err != nil {
			return fmt.Errorf("%w: Could not quarantine %s: %w", ErrSnapshot, path, err)
		}
		if err := fsyncDir(quarantine); err != nil {
			return err
		}
		if err := fsyncDir(stateDir); err != nil {
			return err
		}
	}
	return nil
}

// publishStagedDatabases moves the new generation into place.
//
// Only *.db is published, which is how a previous generation's stale WAL, SHM
// and journal sidecars disappear: they were quarantined and nothing puts them
// back. A restored database beside another generation's WAL would be a silent
// corruption.
func publishStagedDatabases(stateDir, staging string, opts RestoreOptions) error {
	staged, err := databaseFiles(staging)
	if err != nil {
		return err
	}
	installed := 0
	for _, path := range staged {
		if opts.BeforePublish != nil {
			if err := opts.BeforePublish(installed); err != nil {
				return err
			}
		}
		if err := renameFile(path, filepath.Join(stateDir, filepath.Base(path))); err != nil {
			return fmt.Errorf("%w: Could not publish %s: %w", ErrSnapshot, path, err)
		}
		installed++
		if err := fsyncDir(staging); err != nil {
			return err
		}
		if err := fsyncDir(stateDir); err != nil {
			return err
		}
	}
	return nil
}

// unwindRestore undoes as much of a failed transaction as the durable record
// allows.
//
// With no journal on disk nothing was published and nothing can be recovered
// from disk either, so the two directories this transaction created are simply
// removed and live state is left exactly as it was found. With a journal, the
// full rollback runs — the same one crash recovery would run on the next start.
func unwindRestore(stateDir string, journal *restoreJournal, staging, quarantine string) error {
	if journal == nil {
		return errors.Join(
			removeDirectory(staging),
			removeDirectory(quarantine),
			fsyncDir(stateDir),
		)
	}
	if journal.Phase == phaseCommitted {
		// The generation is published and the journal says so; the failure was
		// in the cleanup, and the next recovery rolls it forward.
		return nil
	}
	if journal.Phase == phasePrepared {
		if err := setRestorePhase(stateDir, journal, phaseRollingBack); err != nil {
			return err
		}
	}
	if err := validateRollbackEvidence(stateDir, *journal); err != nil {
		return err
	}
	return rollbackRestore(stateDir, *journal)
}

// removeDirectory deletes a transaction directory this call created.
func removeDirectory(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("%w: Could not discard the restore directory %s: %w", ErrSnapshot, path, err)
	}
	return nil
}
