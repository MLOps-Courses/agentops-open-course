package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// restorePhase is where a restore transaction had got to when it was last
// written down.
//
// The three values are the whole recovery decision: committed rolls forward and
// only cleans up, prepared and rolling_back roll back. Nothing else is a legal
// phase, and a journal that records anything else is refused rather than
// guessed at.
type restorePhase string

const (
	// phasePrepared means staging is complete and the journal is durable, but
	// the old generation is still the live one — or is being quarantined.
	phasePrepared restorePhase = "prepared"
	// phaseRollingBack means the transaction has given up and the old
	// generation is being reinstated.
	phaseRollingBack restorePhase = "rolling_back"
	// phaseCommitted means the new generation is live and complete; only the
	// transaction's own residue is left to remove.
	phaseCommitted restorePhase = "committed"
)

// inventoryRole labels which generation an inventory describes, so the
// rejection messages tell an operator which side of the transaction is wrong.
type inventoryRole string

const (
	oldRole inventoryRole = "old"
	newRole inventoryRole = "new"
)

// fileEvidence is the byte-level record of one state file: enough to prove a
// file on disk is the same file, and not enough to reconstruct it.
type fileEvidence struct {
	Filename  string
	SHA256    string
	SizeBytes int64
}

// payload renders the evidence as its JSON object.
func (e fileEvidence) payload() map[string]any {
	return map[string]any{
		"filename":   e.Filename,
		"sha256":     e.SHA256,
		"size_bytes": e.SizeBytes,
	}
}

// inventory is an ordered set of file evidence.
//
// Order is preserved rather than sorted because rollback replays the old
// generation in the order it was recorded, and Python's dict gave that ordering
// for free. A Go map would randomize it and make a partially completed rollback
// resume differently every time.
type inventory []fileEvidence

// find returns the evidence recorded for a filename.
func (inv inventory) find(name string) (fileEvidence, bool) {
	for _, evidence := range inv {
		if evidence.Filename == name {
			return evidence, true
		}
	}
	return fileEvidence{}, false
}

// names returns the recorded filenames, sorted, for a rejection message.
func (inv inventory) names() []string {
	names := make([]string, 0, len(inv))
	for _, evidence := range inv {
		names = append(names, evidence.Filename)
	}
	slices.Sort(names)
	return names
}

// payload renders the inventory as its JSON array. It is never nil, so an empty
// old generation encodes as [] rather than null.
func (inv inventory) payload() []any {
	encoded := make([]any, 0, len(inv))
	for _, evidence := range inv {
		encoded = append(encoded, evidence.payload())
	}
	return encoded
}

// restoreJournal is the durable record of one restore transaction.
type restoreJournal struct {
	TransactionID string
	Phase         restorePhase
	StagingDir    string
	QuarantineDir string
	Old           inventory
	New           inventory
}

// newRestoreJournal builds a prepared journal for a transaction.
func newRestoreJournal(transactionID string, old, published inventory) *restoreJournal {
	return &restoreJournal{
		TransactionID: transactionID,
		Phase:         phasePrepared,
		StagingDir:    stagingPrefix + transactionID,
		QuarantineDir: quarantinePrefix + transactionID,
		Old:           old,
		New:           published,
	}
}

// payload renders the journal as the JSON document written to disk.
func (j restoreJournal) payload() map[string]any {
	return map[string]any{
		"format_version": restoreJournalFormatVersion,
		"transaction_id": j.TransactionID,
		"phase":          string(j.Phase),
		"staging_dir":    j.StagingDir,
		"quarantine_dir": j.QuarantineDir,
		"old_inventory":  j.Old.payload(),
		"new_inventory":  j.New.payload(),
	}
}

// stagingPath is where the new generation waits to be published.
func (j restoreJournal) stagingPath(stateDir string) string {
	return filepath.Join(stateDir, j.StagingDir)
}

// quarantinePath is where the old generation is held during publication.
func (j restoreJournal) quarantinePath(stateDir string) string {
	return filepath.Join(stateDir, j.QuarantineDir)
}

// temporaryPath is the journal's own write-ahead name.
func (j restoreJournal) temporaryPath(stateDir string) string {
	return filepath.Join(stateDir, journalPrefix+j.TransactionID+journalTempSuffix)
}

// journalPath is the durable journal inside a state directory.
func journalPath(stateDir string) string {
	return filepath.Join(stateDir, restoreJournalName)
}

// writeRestoreJournal makes a journal durable and reports whether the durable
// name now exists.
//
// The sequence matters more than any other few lines in this package:
//
//  1. Create the temporary exclusively, write it, fsync the file.
//  2. fsync the state directory — *before* the rename. A file fsync does not
//     make a newly created name durable, and between here and the rename the
//     temporary is the only recoverable record that this transaction exists. A
//     crash in that window is recovered by adoptInitialRestoreJournal.
//  3. Rename over the durable name, which is atomic.
//  4. fsync the state directory again, so the durable name survives power loss.
//
// The published flag is what lets a caller distinguish "nothing was recorded"
// from "the record exists but the write reported a failure afterwards"; the
// first can be cleaned up silently, the second must be rolled back.
func writeRestoreJournal(stateDir string, journal restoreJournal) (published bool, err error) {
	encoded, err := encodeDocument(journal.payload())
	if err != nil {
		return false, err
	}
	temporary := journal.temporaryPath(stateDir)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privatePerm)
	if err != nil {
		return false, fmt.Errorf("%w: Could not stage the restore journal %s: %w", ErrSnapshot, temporary, err)
	}
	defer func() {
		if published {
			// The rename consumed the temporary name; nothing to discard.
			return
		}
		// A write that failed before the rename leaves a temporary that no
		// recovery should adopt, so it goes away here. A *crash* skips this
		// entirely, which is precisely how the adoption path gets its evidence.
		if removeErr := removeIfPresent(temporary); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}()

	if err := writeAndSync(file, encoded, temporary); err != nil {
		return false, err
	}
	if err := fsyncDir(stateDir); err != nil {
		return false, err
	}
	if err := renameFile(temporary, journalPath(stateDir)); err != nil {
		return false, fmt.Errorf("%w: Could not publish the restore journal in %s: %w", ErrSnapshot, stateDir, err)
	}
	published = true
	return true, fsyncDir(stateDir)
}

// writeAndSync writes a whole payload to an open file and flushes it.
func writeAndSync(file *os.File, payload []byte, path string) error {
	if _, err := file.Write(payload); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, err), closeQuietly(file, path))
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("fsync %s: %w", path, err), closeQuietly(file, path))
	}
	return closeQuietly(file, path)
}

// readRestoreJournal parses and validates a journal document.
//
// Every field is checked against what this binary would itself have written,
// including that the staging and quarantine directory names are derived from
// the transaction id. That is what stops a tampered journal from pointing
// recovery at a directory outside the state directory.
func readRestoreJournal(path string) (*restoreJournal, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, snapshotErrorf("Restore journal must be a regular file: %s", path)
	}
	document, err := loadJSONObject(path)
	if err != nil {
		return nil, err
	}
	expected := []string{
		"format_version", "transaction_id", "phase",
		"staging_dir", "quarantine_dir", "old_inventory", "new_inventory",
	}
	if !hasExactFields(document, expected) {
		return nil, snapshotErrorf("Restore journal is incomplete or has unsupported fields.")
	}
	if version, ok := jsonInt(document["format_version"]); !ok || version != restoreJournalFormatVersion {
		return nil, snapshotErrorf("Unsupported restore journal format %s; this application supports format %d.",
			renderJSONValue(document["format_version"]), restoreJournalFormatVersion)
	}
	transactionID, ok := document["transaction_id"].(string)
	if !ok || len(transactionID) != 32 || !isHex(transactionID) {
		return nil, snapshotErrorf("Restore journal has an invalid transaction id.")
	}
	phase, ok := document["phase"].(string)
	if !ok || !slices.Contains(
		[]string{string(phasePrepared), string(phaseRollingBack), string(phaseCommitted)}, phase,
	) {
		return nil, snapshotErrorf("Restore journal has an invalid phase: %s", renderJSONValue(document["phase"]))
	}
	if document["staging_dir"] != any(stagingPrefix+transactionID) {
		return nil, snapshotErrorf("Restore journal has an invalid staging directory.")
	}
	if document["quarantine_dir"] != any(quarantinePrefix+transactionID) {
		return nil, snapshotErrorf("Restore journal has an invalid quarantine directory.")
	}
	old, err := parseInventory(document["old_inventory"], oldRole)
	if err != nil {
		return nil, err
	}
	published, err := parseInventory(document["new_inventory"], newRole)
	if err != nil {
		return nil, err
	}
	return &restoreJournal{
		TransactionID: transactionID,
		Phase:         restorePhase(phase),
		StagingDir:    stagingPrefix + transactionID,
		QuarantineDir: quarantinePrefix + transactionID,
		Old:           old,
		New:           published,
	}, nil
}

// hasExactFields reports whether a document declares exactly the named fields.
// An unknown field is refused rather than ignored: it means the journal was
// written by something this binary does not understand.
func hasExactFields(document map[string]any, expected []string) bool {
	if len(document) != len(expected) {
		return false
	}
	for _, field := range expected {
		if _, ok := document[field]; !ok {
			return false
		}
	}
	return true
}

// parseInventory validates one generation's recorded file evidence.
func parseInventory(raw any, role inventoryRole) (inventory, error) {
	entries, ok := raw.([]any)
	// An empty old generation is legal — a restore into an empty state
	// directory records one — but an empty new generation would mean the
	// transaction published nothing, which no code path can produce.
	if !ok || (role == newRole && len(entries) == 0) {
		return nil, snapshotErrorf("Restore journal has an invalid %s inventory.", role)
	}
	parsed := make(inventory, 0, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok || !hasExactFields(entry, []string{"filename", "sha256", "size_bytes"}) {
			return nil, snapshotErrorf("Restore journal has incomplete %s file evidence.", role)
		}
		name, ok := entry["filename"].(string)
		if !ok || !isSafeStateFilename(name, role) {
			return nil, snapshotErrorf("Restore journal has an unsafe %s filename: %s",
				role, renderJSONValue(entry["filename"]))
		}
		if _, duplicate := parsed.find(name); duplicate {
			return nil, snapshotErrorf("Restore journal repeats %s filename: %s", role, name)
		}
		digest, ok := entry["sha256"].(string)
		if !ok || len(digest) != 64 || !isHex(digest) {
			return nil, snapshotErrorf("Restore journal has an invalid %s SHA-256 for %s.", role, name)
		}
		size, ok := jsonInt(entry["size_bytes"])
		// Zero is legal here, unlike in the snapshot manifest: an empty WAL
		// sidecar is a real file the old generation may contain.
		if !ok || size < 0 {
			return nil, snapshotErrorf("Restore journal has an invalid %s size for %s.", role, name)
		}
		parsed = append(parsed, fileEvidence{Filename: name, SHA256: digest, SizeBytes: size})
	}
	return parsed, nil
}

// isSafeStateFilename reports whether a journal filename is a bare, visible
// state filename.
//
// The new generation is restricted to `.db` because only databases are ever
// published; the old generation also admits the WAL, SHM and journal sidecars,
// because those are exactly what a running agent leaves beside its databases
// and what the restore has to account for.
func isSafeStateFilename(name string, role inventoryRole) bool {
	if name == "" || filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return false
	}
	if !isStateFilename(name) {
		return false
	}
	return role == oldRole || strings.HasSuffix(name, ".db")
}

// isStateFilename reports whether a name belongs to the state generation: a
// SQLite database or one of its sidecars.
func isStateFilename(name string) bool {
	for _, suffix := range []string{".db", ".db-wal", ".db-shm", ".db-journal"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// loadRestoreJournal reads the durable journal, or reports that there is none.
func loadRestoreJournal(stateDir string) (*restoreJournal, error) {
	path := journalPath(stateDir)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: Could not inspect the restore journal %s: %w", ErrSnapshot, path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		// Checked before the read: a symlinked journal would let anything that
		// can write the state directory choose what recovery believes.
		return nil, snapshotErrorf("Restore journal must be a regular file: %s", path)
	}
	return readRestoreJournal(path)
}

// stateFiles lists the live state generation, sorted by name.
//
// A symlink or any other non-regular file is a hard error rather than a skip:
// the restore takes byte evidence over these paths, and evidence that can be
// redirected is not evidence.
func stateFiles(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not list the state directory %s: %w", ErrSnapshot, stateDir, err)
	}
	var files []string
	for _, entry := range entries {
		if !isStateFilename(entry.Name()) {
			continue
		}
		path := filepath.Join(stateDir, entry.Name())
		if !entry.Type().IsRegular() {
			return nil, snapshotErrorf("State evidence must be a regular file: %s", path)
		}
		files = append(files, path)
	}
	return files, nil
}

// evidenceOf measures one file for the journal.
func evidenceOf(path string) (fileEvidence, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileEvidence{}, fmt.Errorf("%w: Could not measure %s: %w", ErrSnapshot, path, err)
	}
	digest, err := sha256File(path)
	if err != nil {
		return fileEvidence{}, fmt.Errorf("%w: Could not hash %s: %w", ErrSnapshot, path, err)
	}
	return fileEvidence{Filename: filepath.Base(path), SHA256: digest, SizeBytes: info.Size()}, nil
}

// matchesEvidence reports whether a path is byte-for-byte what was recorded.
func matchesEvidence(path string, evidence fileEvidence) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if info.Size() != evidence.SizeBytes {
		return false
	}
	digest, err := sha256File(path)
	return err == nil && digest == evidence.SHA256
}

// requireEvidence refuses to continue when a file is not what was recorded.
func requireEvidence(path string, evidence fileEvidence, location string) error {
	if !matchesEvidence(path, evidence) {
		return snapshotErrorf("Restore recovery cannot verify %s evidence for %s; refusing to guess.",
			location, evidence.Filename)
	}
	return nil
}

// restoreArtifacts lists every transaction residue in a state directory except
// the durable journal itself.
func restoreArtifacts(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not list the state directory %s: %w", ErrSnapshot, stateDir, err)
	}
	var artifacts []string
	for _, entry := range entries {
		name := entry.Name()
		if name == restoreJournalName {
			continue
		}
		if strings.HasPrefix(name, stagingPrefix) ||
			strings.HasPrefix(name, quarantinePrefix) ||
			strings.HasPrefix(name, journalPrefix) {
			artifacts = append(artifacts, filepath.Join(stateDir, name))
		}
	}
	return artifacts, nil
}

// baseNames renders paths for a rejection message.
func baseNames(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}
	slices.Sort(names)
	return names
}
