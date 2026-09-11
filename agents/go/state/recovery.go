package state

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RecoverOptions carries the settings crash recovery needs.
type RecoverOptions struct {
	// Logger receives the recovery decision. Nil uses slog.Default.
	Logger *slog.Logger
}

// RecoverInterruptedRestore finishes or reverses a restore that was interrupted.
//
// Every process that reads or publishes state calls this first. It is the only
// thing that makes a crashed restore safe: until it runs, the state directory
// may hold a half-published generation, and nothing else in the system can tell
// that from a healthy one.
//
// It takes the generation lock even when the directory is clean, and that is
// deliberate. Taking the lock is what creates the one shared lock inode that a
// read-only Kubernetes backup mount later reuses; skipping it here would leave
// the backup job to create its own lock on a filesystem it cannot write, and it
// would fail closed instead of serializing.
func RecoverInterruptedRestore(stateDir string, opts RecoverOptions) error {
	info, err := os.Lstat(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing has ever been published here, so there is nothing to recover.
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: Could not inspect the state directory %s: %w", ErrSnapshot, stateDir, err)
	}
	if !info.IsDir() {
		return snapshotErrorf("State directory not found: %s", stateDir)
	}
	logger := resolveLogger(opts.Logger)
	return withRestoreLock(filepath.Join(stateDir, restoreLockName), func() error {
		return recoverRestoreLocked(stateDir, logger)
	})
}

// recoverRestoreLocked is the recovery state machine, run under the lock.
//
// The decision rule is short and total: a committed transaction rolls forward
// and only its residue is removed; a prepared or rolling-back transaction rolls
// back to the old generation. Anything the journal cannot account for stops
// recovery with the evidence left in place, because an operator inspecting a
// state directory is a better outcome than a process guessing at it.
func recoverRestoreLocked(stateDir string, logger *slog.Logger) error {
	journal, err := loadRestoreJournal(stateDir)
	if err != nil {
		return err
	}
	artifacts, err := restoreArtifacts(stateDir)
	if err != nil {
		return err
	}
	if journal == nil {
		if len(artifacts) == 0 {
			return nil
		}
		if journal, err = adoptInitialRestoreJournal(stateDir, artifacts); err != nil {
			return err
		}
	}

	if err := requireOwnedArtifacts(stateDir, *journal, artifacts); err != nil {
		return err
	}
	if err := discardOwnedJournalTemporary(stateDir, *journal); err != nil {
		return err
	}

	if journal.Phase == phaseCommitted {
		return recoverCommitted(stateDir, *journal, logger)
	}
	if journal.Phase == phasePrepared {
		if err := validateForwardEvidence(stateDir, *journal); err != nil {
			return err
		}
		// The phase is advanced before anything is undone, so a crash inside the
		// rollback resumes as a rollback rather than being re-evaluated as a
		// prepared transaction whose evidence no longer adds up.
		if err := setRestorePhase(stateDir, journal, phaseRollingBack); err != nil {
			return err
		}
	} else if err := validateRollbackEvidence(stateDir, *journal); err != nil {
		return err
	}
	if err := rollbackRestore(stateDir, *journal); err != nil {
		return err
	}
	logger.Info("rolled back interrupted restore", "state_dir", stateDir)
	return nil
}

// recoverCommitted rolls a committed transaction forward.
func recoverCommitted(stateDir string, journal restoreJournal, logger *slog.Logger) error {
	if _, err := validateResidue(journal.stagingPath(stateDir), journal.New, "staging", false); err != nil {
		return err
	}
	if _, err := validateResidue(journal.quarantinePath(stateDir), journal.Old, "quarantine", false); err != nil {
		return err
	}
	// The new generation must be live and complete before the journal that
	// proves it is removed. If it is not, the transaction is not actually
	// committed and recovery says so rather than deleting the evidence.
	if err := assertExactLiveInventory(stateDir, journal.New, "committed"); err != nil {
		return err
	}
	if err := cleanupRestoreTransaction(stateDir, journal); err != nil {
		return err
	}
	logger.Info("recovered committed restore", "state_dir", stateDir)
	return nil
}

// requireOwnedArtifacts refuses residue the journal does not account for.
func requireOwnedArtifacts(stateDir string, journal restoreJournal, artifacts []string) error {
	allowed := map[string]struct{}{
		filepath.Base(journal.stagingPath(stateDir)):    {},
		filepath.Base(journal.quarantinePath(stateDir)): {},
		filepath.Base(journal.temporaryPath(stateDir)):  {},
	}
	var unexplained []string
	for _, artifact := range artifacts {
		if _, ok := allowed[filepath.Base(artifact)]; !ok {
			unexplained = append(unexplained, filepath.Base(artifact))
		}
	}
	if len(unexplained) > 0 {
		return snapshotErrorf("Restore journal does not own residue: %s", strings.Join(unexplained, ", "))
	}
	return nil
}

// adoptInitialRestoreJournal recovers the window between "the journal's
// temporary is durable" and "the temporary has been renamed to the durable
// name".
//
// That window exists because writeRestoreJournal fsyncs the state directory
// before the rename, which is what makes the temporary name survive a power
// loss. Adoption is only safe when the residue proves publication had not
// started: staging complete, quarantine empty, live state exactly the old
// generation. Anything else and the transaction is past the point where a
// temporary journal describes it.
func adoptInitialRestoreJournal(stateDir string, artifacts []string) (*restoreJournal, error) {
	var candidates []string
	for _, artifact := range artifacts {
		name := filepath.Base(artifact)
		if strings.HasPrefix(name, journalPrefix) && strings.HasSuffix(name, journalTempSuffix) {
			candidates = append(candidates, artifact)
		}
	}
	if len(candidates) != 1 {
		return nil, snapshotErrorf(
			"Restore residue exists without a durable journal; expected one durable initial journal: %s",
			strings.Join(baseNames(artifacts), ", "),
		)
	}
	temporary := candidates[0]
	journal, err := readRestoreJournal(temporary)
	if err != nil {
		return nil, err
	}
	if journal.Phase != phasePrepared {
		return nil, snapshotErrorf("An initial restore journal must record the prepared phase.")
	}

	expectedTemporary := journal.temporaryPath(stateDir)
	allowed := []string{
		filepath.Base(journal.stagingPath(stateDir)),
		filepath.Base(journal.quarantinePath(stateDir)),
		filepath.Base(expectedTemporary),
	}
	if temporary != expectedTemporary || !sameNameSet(baseNames(artifacts), allowed) {
		return nil, snapshotErrorf("Initial restore journal does not own its exact residue: expected=%v, actual=%v",
			baseNames(allowed), baseNames(artifacts))
	}

	staged, err := validateResidue(journal.stagingPath(stateDir), journal.New, "staging", true)
	if err != nil {
		return nil, err
	}
	quarantined, err := validateResidue(journal.quarantinePath(stateDir), journal.Old, "quarantine", true)
	if err != nil {
		return nil, err
	}
	if len(staged) != len(journal.New) || len(quarantined) != 0 {
		return nil, snapshotErrorf("Initial restore residue shows that state publication already began.")
	}
	if err := assertExactLiveInventory(stateDir, journal.Old, "pre-restore"); err != nil {
		return nil, err
	}

	if err := renameFile(temporary, journalPath(stateDir)); err != nil {
		return nil, fmt.Errorf("%w: Could not adopt the initial restore journal in %s: %w", ErrSnapshot, stateDir, err)
	}
	if err := fsyncDir(stateDir); err != nil {
		return nil, err
	}
	return journal, nil
}

// sameNameSet reports whether two sorted name lists hold the same names.
func sameNameSet(actual, allowed []string) bool {
	sorted := baseNames(allowed)
	if len(actual) != len(sorted) {
		return false
	}
	for index, name := range actual {
		if name != sorted[index] {
			return false
		}
	}
	return true
}

// discardOwnedJournalTemporary removes a phase update that was made durable but
// never renamed.
//
// The durable journal stays authoritative: an unrenamed temporary records an
// intention the transaction never published, and honoring it would let a phase
// advance that no reader ever observed.
func discardOwnedJournalTemporary(stateDir string, journal restoreJournal) error {
	temporary := journal.temporaryPath(stateDir)
	info, err := os.Lstat(temporary)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: Could not inspect the restore journal temporary %s: %w", ErrSnapshot, temporary, err)
	}
	if !info.Mode().IsRegular() {
		return snapshotErrorf("Restore journal temporary evidence must be a regular file: %s", temporary)
	}
	if err := os.Remove(temporary); err != nil {
		return fmt.Errorf("%w: Could not discard the restore journal temporary %s: %w", ErrSnapshot, temporary, err)
	}
	return fsyncDir(stateDir)
}

// validateResidue proves a transaction directory holds only files the journal
// recorded, each byte-for-byte as recorded.
func validateResidue(
	directory string,
	expected inventory,
	role string,
	required bool,
) (map[string]string, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		if required {
			return nil, snapshotErrorf("Restore recovery is missing its %s directory: %s",
				role, filepath.Base(directory))
		}
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: Could not inspect the restore %s directory %s: %w",
			ErrSnapshot, role, directory, err)
	}
	// Lstat does not follow, so a symlink — dangling or not — is caught here
	// rather than being treated as the directory it points at.
	if !info.IsDir() {
		return nil, snapshotErrorf("Restore %s evidence must be a directory: %s", role, directory)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not list the restore %s directory %s: %w",
			ErrSnapshot, role, directory, err)
	}
	present := make(map[string]string, len(entries))
	for _, entry := range entries {
		evidence, ok := expected.find(entry.Name())
		if !ok {
			return nil, snapshotErrorf("Restore %s contains unexplained evidence: %s", role, entry.Name())
		}
		path := filepath.Join(directory, entry.Name())
		if err := requireEvidence(path, evidence, role); err != nil {
			return nil, err
		}
		present[entry.Name()] = path
	}
	return present, nil
}

// validateLiveSubset proves every live state file belongs to one of the two
// recorded generations, byte-for-byte.
func validateLiveSubset(stateDir string, inventories ...inventory) (map[string]string, error) {
	files, err := stateFiles(stateDir)
	if err != nil {
		return nil, err
	}
	live := make(map[string]string, len(files))
	var unexplained []string
	for _, path := range files {
		name := filepath.Base(path)
		live[name] = path
		named := false
		for _, recorded := range inventories {
			if _, ok := recorded.find(name); ok {
				named = true
				break
			}
		}
		if !named {
			unexplained = append(unexplained, name)
		}
	}
	// Reported before any byte comparison, and for a reason: a file nobody
	// recorded is a different problem from a file that changed, and an operator
	// needs to be told which one they actually have.
	if len(unexplained) > 0 {
		return nil, snapshotErrorf("Restore recovery found live state outside both recorded generations: %s",
			strings.Join(unexplained, ", "))
	}
	// Iterated over the sorted file list rather than the map so two mismatching
	// files always produce the same message.
	for _, path := range files {
		name := filepath.Base(path)
		matched := false
		for _, recorded := range inventories {
			evidence, ok := recorded.find(name)
			if ok && matchesEvidence(path, evidence) {
				matched = true
				break
			}
		}
		if !matched {
			// A live file may legitimately match either generation — that is how
			// a partially published transaction looks — but it must match one.
			return nil, snapshotErrorf(
				"Restore recovery cannot match live evidence for %s to a recorded generation.", name,
			)
		}
	}
	return live, nil
}

// validateForwardEvidence checks a prepared transaction before rolling it back.
func validateForwardEvidence(stateDir string, journal restoreJournal) error {
	staged, err := validateResidue(journal.stagingPath(stateDir), journal.New, "staging", true)
	if err != nil {
		return err
	}
	quarantined, err := validateResidue(journal.quarantinePath(stateDir), journal.Old, "quarantine", true)
	if err != nil {
		return err
	}
	live, err := validateLiveSubset(stateDir, journal.Old, journal.New)
	if err != nil {
		return err
	}
	if err := requireRegularJournalTemporary(stateDir, journal); err != nil {
		return err
	}
	// Every old file is either quarantined or still live and unchanged: that is
	// what proves the old generation can be reconstructed in full.
	if err := requireCompleteGeneration(journal.Old, quarantined, live, missingOldEvidence); err != nil {
		return err
	}
	// And every new file is either staged or already published, which is what
	// proves the transaction has not lost part of the generation it prepared.
	return requireCompleteGeneration(journal.New, staged, live, missingNewEvidence)
}

// missingOldEvidence and missingNewEvidence name the two ways a generation can
// come up short. They are functions rather than format strings so vet can still
// check the formatting at the point it is written.
func missingOldEvidence(name string) error {
	return snapshotErrorf("Restore recovery is missing byte-identical old evidence for %s.", name)
}

func missingNewEvidence(name string) error {
	return snapshotErrorf("Restore recovery is missing prepared new evidence for %s.", name)
}

// validateRollbackEvidence checks a transaction that is already rolling back.
//
// The staging and quarantine directories are optional here because rollback
// removes them as it goes, and only the old generation's completeness is
// enforced: the new generation is what is being destroyed.
func validateRollbackEvidence(stateDir string, journal restoreJournal) error {
	if _, err := validateResidue(journal.stagingPath(stateDir), journal.New, "staging", false); err != nil {
		return err
	}
	quarantined, err := validateResidue(journal.quarantinePath(stateDir), journal.Old, "quarantine", false)
	if err != nil {
		return err
	}
	live, err := validateLiveSubset(stateDir, journal.Old, journal.New)
	if err != nil {
		return err
	}
	if err := requireRegularJournalTemporary(stateDir, journal); err != nil {
		return err
	}
	return requireCompleteGeneration(journal.Old, quarantined, live, missingOldEvidence)
}

// requireCompleteGeneration proves every recorded file is either held in the
// transaction's own directory or live and byte-identical.
func requireCompleteGeneration(
	recorded inventory,
	held map[string]string,
	live map[string]string,
	missing func(name string) error,
) error {
	for _, evidence := range recorded {
		if _, ok := held[evidence.Filename]; ok {
			continue
		}
		path, ok := live[evidence.Filename]
		if !ok || !matchesEvidence(path, evidence) {
			return missing(evidence.Filename)
		}
	}
	return nil
}

// requireRegularJournalTemporary refuses a temporary that is not a plain file.
func requireRegularJournalTemporary(stateDir string, journal restoreJournal) error {
	temporary := journal.temporaryPath(stateDir)
	info, err := os.Lstat(temporary)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return snapshotErrorf("Restore journal temporary evidence is invalid: %s", temporary)
	}
	return nil
}

// assertExactLiveInventory proves the live generation is exactly the recorded
// one — no extra file, no missing file, and every byte accounted for.
func assertExactLiveInventory(stateDir string, expected inventory, generation string) error {
	files, err := stateFiles(stateDir)
	if err != nil {
		return err
	}
	live := make(map[string]string, len(files))
	for _, path := range files {
		live[filepath.Base(path)] = path
	}
	if len(live) != len(expected) {
		return snapshotErrorf("Restore %s generation inventory mismatch: expected=%v, actual=%v",
			generation, expected.names(), baseNames(files))
	}
	for _, evidence := range expected {
		path, ok := live[evidence.Filename]
		if !ok {
			return snapshotErrorf("Restore %s generation inventory mismatch: expected=%v, actual=%v",
				generation, expected.names(), baseNames(files))
		}
		if err := requireEvidence(path, evidence, generation+" live"); err != nil {
			return err
		}
	}
	return nil
}

// setRestorePhase advances a transaction and makes the advance durable.
func setRestorePhase(stateDir string, journal *restoreJournal, phase restorePhase) error {
	if err := discardOwnedJournalTemporary(stateDir, *journal); err != nil {
		return err
	}
	journal.Phase = phase
	_, err := writeRestoreJournal(stateDir, *journal)
	return err
}

// cleanupRestoreTransaction removes a finished transaction's residue.
//
// The durable journal goes last and is required to exist: it is the record that
// says which generation is live, so it must outlive everything it describes.
func cleanupRestoreTransaction(stateDir string, journal restoreJournal) error {
	for _, directory := range []string{journal.stagingPath(stateDir), journal.quarantinePath(stateDir)} {
		info, err := os.Lstat(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: Could not inspect the restore residue %s: %w", ErrSnapshot, directory, err)
		}
		if !info.IsDir() {
			return snapshotErrorf("Restore cleanup evidence must be a directory: %s", directory)
		}
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("%w: Could not remove the restore residue %s: %w", ErrSnapshot, directory, err)
		}
	}
	if err := removeIfPresent(journal.temporaryPath(stateDir)); err != nil {
		return err
	}
	if err := fsyncDir(stateDir); err != nil {
		return err
	}
	if err := os.Remove(journalPath(stateDir)); err != nil {
		return fmt.Errorf("%w: Could not remove the restore journal in %s: %w", ErrSnapshot, stateDir, err)
	}
	return fsyncDir(stateDir)
}

// rollbackRestore reinstates the old generation, byte for byte.
//
// The six steps are ordered so the whole procedure is idempotent and
// re-entrant: every step consults the quarantine first, so a crash anywhere
// inside a rollback resumes correctly on the next recovery rather than
// compounding.
func rollbackRestore(stateDir string, journal restoreJournal) error {
	quarantine := journal.quarantinePath(stateDir)
	if err := os.Mkdir(quarantine, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: Could not create the restore quarantine %s: %w", ErrSnapshot, quarantine, err)
	}
	if err := fsyncDir(stateDir); err != nil {
		return err
	}
	if err := quarantineOldGeneration(stateDir, journal, quarantine); err != nil {
		return err
	}
	if err := removePublishedGeneration(stateDir, journal); err != nil {
		return err
	}
	if err := reinstateOldGeneration(stateDir, journal, quarantine); err != nil {
		return err
	}
	if err := assertExactLiveInventory(stateDir, journal.Old, "rolled-back"); err != nil {
		return err
	}
	return cleanupRestoreTransaction(stateDir, journal)
}

// quarantineOldGeneration moves every old file that is still live out of the way.
func quarantineOldGeneration(stateDir string, journal restoreJournal, quarantine string) error {
	files, err := stateFiles(stateDir)
	if err != nil {
		return err
	}
	live := make(map[string]string, len(files))
	for _, path := range files {
		live[filepath.Base(path)] = path
	}
	for _, evidence := range journal.Old {
		quarantined := filepath.Join(quarantine, evidence.Filename)
		if _, err := os.Lstat(quarantined); err == nil {
			// Already moved by an earlier attempt; re-verify rather than redo.
			if err := requireEvidence(quarantined, evidence, "quarantine"); err != nil {
				return err
			}
			continue
		}
		current, ok := live[evidence.Filename]
		if !ok {
			return snapshotErrorf("Restore rollback lost old evidence for %s.", evidence.Filename)
		}
		if err := requireEvidence(current, evidence, "old live"); err != nil {
			return err
		}
		if err := renameFile(current, quarantined); err != nil {
			return fmt.Errorf("%w: Could not quarantine %s: %w", ErrSnapshot, current, err)
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

// removePublishedGeneration deletes what the interrupted transaction managed to
// publish — and only that. A live file whose bytes match no recorded new
// generation is somebody else's data, and rollback refuses to remove it.
func removePublishedGeneration(stateDir string, journal restoreJournal) error {
	files, err := stateFiles(stateDir)
	if err != nil {
		return err
	}
	for _, path := range files {
		name := filepath.Base(path)
		evidence, ok := journal.New.find(name)
		if !ok || !matchesEvidence(path, evidence) {
			return snapshotErrorf(
				"Restore rollback found unexplained replacement evidence for %s; refusing to remove it.", name,
			)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%w: Could not remove the partially published %s: %w", ErrSnapshot, path, err)
		}
		if err := fsyncDir(stateDir); err != nil {
			return err
		}
	}
	return nil
}

// reinstateOldGeneration moves the quarantined old generation back into place.
func reinstateOldGeneration(stateDir string, journal restoreJournal, quarantine string) error {
	for _, evidence := range journal.Old {
		quarantined := filepath.Join(quarantine, evidence.Filename)
		if err := requireEvidence(quarantined, evidence, "quarantine"); err != nil {
			return err
		}
		if err := renameFile(quarantined, filepath.Join(stateDir, evidence.Filename)); err != nil {
			return fmt.Errorf("%w: Could not reinstate %s: %w", ErrSnapshot, evidence.Filename, err)
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
