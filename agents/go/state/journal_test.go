package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// decodeInventoryFixture runs a hand-built inventory through the parser the way
// a journal read would, so the adversarial cases test the real code path rather
// than a Go-typed shortcut.
func decodeInventoryFixture(t *testing.T, raw string, role inventoryRole) error {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode the inventory fixture %s: %v", raw, err)
	}
	_, err := parseInventory(payload, role)
	return err
}

func TestRestoreJournalInventoryParserRejectsIncompleteEvidence(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("0", 64)
	for _, testCase := range []struct {
		name  string
		raw   string
		role  inventoryRole
		error string
	}{
		{"not a list", `null`, oldRole, "invalid old inventory"},
		{"empty new generation", `[]`, newRole, "invalid new inventory"},
		{"entry is not an object", `["not evidence"]`, oldRole, "incomplete old file evidence"},
		{
			"entry has an extra field",
			`[{"filename":"old.db","sha256":"` + digest + `","size_bytes":1,"extra":true}]`,
			oldRole, "incomplete old file evidence",
		},
		{
			"traversing filename",
			`[{"filename":"../old.db","sha256":"` + digest + `","size_bytes":1}]`,
			oldRole, "unsafe old filename",
		},
		{
			"sidecar in the new generation",
			`[{"filename":"old.db-wal","sha256":"` + digest + `","size_bytes":1}]`,
			newRole, "unsafe new filename",
		},
		{
			"duplicate filename",
			`[{"filename":"old.db","sha256":"` + digest + `","size_bytes":1},` +
				`{"filename":"old.db","sha256":"` + digest + `","size_bytes":1}]`,
			oldRole, "repeats old filename",
		},
		{
			"digest is not hexadecimal",
			`[{"filename":"old.db","sha256":"not-a-digest","size_bytes":1}]`,
			oldRole, "invalid old SHA-256",
		},
		{
			"digest is upper case",
			`[{"filename":"old.db","sha256":"` + strings.Repeat("A", 64) + `","size_bytes":1}]`,
			oldRole, "invalid old SHA-256",
		},
		{
			"size is a boolean",
			`[{"filename":"old.db","sha256":"` + digest + `","size_bytes":true}]`,
			oldRole, "invalid old size",
		},
		{
			"size is fractional",
			`[{"filename":"old.db","sha256":"` + digest + `","size_bytes":1.5}]`,
			oldRole, "invalid old size",
		},
		{
			"size is negative",
			`[{"filename":"old.db","sha256":"` + digest + `","size_bytes":-1}]`,
			oldRole, "invalid old size",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			assertSnapshotError(t, decodeInventoryFixture(t, testCase.raw, testCase.role), testCase.error)
		})
	}
}

// An empty old generation is legal — restoring into a fresh directory records
// one — and a zero-byte sidecar is a real file, so neither may be rejected.
func TestRestoreJournalInventoryParserAcceptsTheLegalEdges(t *testing.T) {
	t.Parallel()

	if err := decodeInventoryFixture(t, `[]`, oldRole); err != nil {
		t.Errorf("an empty old inventory was rejected: %v", err)
	}
	zeroSidecar := `[{"filename":"a.db-wal","sha256":"` + strings.Repeat("0", 64) + `","size_bytes":0}]`
	if err := decodeInventoryFixture(t, zeroSidecar, oldRole); err != nil {
		t.Errorf("a zero-byte sidecar was rejected: %v", err)
	}
}

func TestRestoreJournalRejectsInvalidProtocolFields(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		mutate func(document map[string]any)
		error  string
	}{
		{"missing field", func(d map[string]any) { delete(d, "phase") }, "incomplete or has unsupported fields"},
		{"unknown field", func(d map[string]any) { d["extra"] = true }, "incomplete or has unsupported fields"},
		{"future format", func(d map[string]any) { d["format_version"] = 2 }, "Unsupported restore journal format"},
		{"invalid transaction id", func(d map[string]any) { d["transaction_id"] = "invalid" }, "invalid transaction id"},
		{"unknown phase", func(d map[string]any) { d["phase"] = "unknown" }, "invalid phase"},
		{"traversing staging", func(d map[string]any) { d["staging_dir"] = "../staging" }, "invalid staging directory"},
		{
			"traversing quarantine",
			func(d map[string]any) { d["quarantine_dir"] = "../quarantine" },
			"invalid quarantine directory",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			stateDir := t.TempDir()
			document := journalDocument(fixedTransactionID, phasePrepared)
			testCase.mutate(document)
			writeJSONFixture(t, journalPath(stateDir), document)

			assertSnapshotError(t, RecoverInterruptedRestore(stateDir, recoverOptions()), testCase.error)
		})
	}
}

func TestRestoreJournalAndStateEvidenceRejectSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("evidence"), 0o600); err != nil {
		t.Fatalf("write the symlink target: %v", err)
	}

	for _, testCase := range []struct{ name, target string }{
		{"resolving", target},
		{"dangling", filepath.Join(root, "missing-journal")},
	} {
		if err := os.Symlink(testCase.target, journalPath(stateDir)); err != nil {
			t.Fatalf("plant the %s journal symlink: %v", testCase.name, err)
		}
		assertSnapshotError(t, RecoverInterruptedRestore(stateDir, recoverOptions()), "journal must be a regular file")
		if err := os.Remove(journalPath(stateDir)); err != nil {
			t.Fatalf("remove the %s journal symlink: %v", testCase.name, err)
		}
	}

	// A symlinked state file is refused three ways over: it is not listable as
	// evidence, it never matches evidence, and requiring it fails.
	linked := filepath.Join(stateDir, "linked.db")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatalf("plant the state symlink: %v", err)
	}
	_, err := stateFiles(stateDir)
	assertSnapshotError(t, err, "State evidence must be a regular file")

	evidence, err := evidenceOf(target)
	if err != nil {
		t.Fatalf("measure the symlink target: %v", err)
	}
	evidence.Filename = "linked.db"
	if matchesEvidence(linked, evidence) {
		t.Error("a symlink matched the evidence of the file it points at")
	}
	assertSnapshotError(t, requireEvidence(linked, evidence, "test"), "cannot verify test evidence")
}

func TestRestoreRecoveryRejectsOrphanAndUnownedResidue(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	orphan := filepath.Join(root, "orphan")
	if err := os.MkdirAll(filepath.Join(orphan, stagingPrefix+"orphan"), 0o750); err != nil {
		t.Fatalf("create the orphan residue: %v", err)
	}
	assertSnapshotError(t, RecoverInterruptedRestore(orphan, recoverOptions()),
		"residue exists without a durable journal")

	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, 0o750); err != nil {
		t.Fatalf("create the owned state directory: %v", err)
	}
	document := journalDocument(fixedTransactionID, phaseCommitted)
	newDatabase := filepath.Join(owned, "new.db")
	if err := os.WriteFile(newDatabase, nil, 0o600); err != nil {
		t.Fatalf("write the new generation: %v", err)
	}
	evidence, err := evidenceOf(newDatabase)
	if err != nil {
		t.Fatalf("measure the new generation: %v", err)
	}
	document["new_inventory"] = []any{evidence.payload()}
	for _, name := range []string{stagingPrefix + fixedTransactionID, quarantinePrefix + fixedTransactionID} {
		if err := os.Mkdir(filepath.Join(owned, name), 0o750); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	writeJSONFixture(t, journalPath(owned), document)
	if err := os.Mkdir(filepath.Join(owned, stagingPrefix+"unowned"), 0o750); err != nil {
		t.Fatalf("create the unowned residue: %v", err)
	}

	assertSnapshotError(t, RecoverInterruptedRestore(owned, recoverOptions()), "journal does not own residue")
}

func TestCommittedRestoreRecoveryRejectsDanglingResidueSymlinks(t *testing.T) {
	t.Parallel()

	for _, symlinked := range []string{"staging_dir", "quarantine_dir"} {
		t.Run(symlinked, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			if err := os.Mkdir(stateDir, 0o750); err != nil {
				t.Fatalf("create the state directory: %v", err)
			}
			document := journalDocument(fixedTransactionID, phaseCommitted)
			newDatabase := filepath.Join(stateDir, "new.db")
			if err := os.WriteFile(newDatabase, []byte("new generation"), 0o600); err != nil {
				t.Fatalf("write the new generation: %v", err)
			}
			evidence, err := evidenceOf(newDatabase)
			if err != nil {
				t.Fatalf("measure the new generation: %v", err)
			}
			document["new_inventory"] = []any{evidence.payload()}
			for _, role := range []string{"staging_dir", "quarantine_dir"} {
				residue := filepath.Join(stateDir, mustString(t, document[role]))
				if role == symlinked {
					if err := os.Symlink(filepath.Join(root, "missing-"+role), residue); err != nil {
						t.Fatalf("plant the %s symlink: %v", role, err)
					}
					continue
				}
				if err := os.Mkdir(residue, 0o750); err != nil {
					t.Fatalf("create %s: %v", role, err)
				}
			}
			writeJSONFixture(t, journalPath(stateDir), document)

			// Twice: a refusal must be idempotent and must leave the journal for
			// an operator, not consume it on the first attempt.
			for attempt := range 2 {
				err := RecoverInterruptedRestore(stateDir, recoverOptions())
				assertSnapshotError(t, err, "evidence must be a directory")
				if !isRegularFile(journalPath(stateDir)) {
					t.Fatalf("attempt %d removed the journal it refused to act on", attempt+1)
				}
			}
		})
	}
}

func TestRestoreEvidenceValidatorsFailClosedOnMissingOrUnexplainedFiles(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	known := filepath.Join(stateDir, "known.db")
	if err := os.WriteFile(known, []byte("known"), 0o600); err != nil {
		t.Fatalf("write the known state file: %v", err)
	}
	evidence, err := evidenceOf(known)
	if err != nil {
		t.Fatalf("measure the known state file: %v", err)
	}
	recorded := inventory{evidence}

	_, err = validateResidue(filepath.Join(stateDir, "missing"), recorded, "required", true)
	assertSnapshotError(t, err, "missing its required directory")

	optional, err := validateResidue(filepath.Join(stateDir, "missing"), recorded, "optional", false)
	if err != nil || len(optional) != 0 {
		t.Errorf("an optional missing directory returned (%v, %v), want (empty, nil)", optional, err)
	}

	residue := filepath.Join(stateDir, "residue")
	if mkdirErr := os.Mkdir(residue, 0o750); mkdirErr != nil {
		t.Fatalf("create the residue directory: %v", mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(residue, "unexpected.db"), []byte("unexpected"), 0o600); writeErr != nil {
		t.Fatalf("write the unexplained residue: %v", writeErr)
	}
	_, err = validateResidue(residue, recorded, "test", true)
	assertSnapshotError(t, err, "contains unexplained evidence")

	_, err = validateLiveSubset(stateDir)
	assertSnapshotError(t, err, "outside both recorded generations")

	mismatched := inventory{{Filename: evidence.Filename, SHA256: strings.Repeat("0", 64), SizeBytes: evidence.SizeBytes}}
	_, err = validateLiveSubset(stateDir, mismatched)
	assertSnapshotError(t, err, "cannot match live evidence")

	assertSnapshotError(t, assertExactLiveInventory(stateDir, nil, "test"), "generation inventory mismatch")
}

func TestRestoreRecoveryInitializesCleanStateLockAndRejectsNonDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// A state directory that was never created is not an error: nothing has
	// been published there, so there is nothing to recover.
	if err := RecoverInterruptedRestore(filepath.Join(root, "missing"), recoverOptions()); err != nil {
		t.Errorf("RecoverInterruptedRestore() on a missing directory = %v, want nil", err)
	}

	clean := filepath.Join(root, "clean")
	if err := os.Mkdir(clean, 0o750); err != nil {
		t.Fatalf("create the clean state directory: %v", err)
	}
	if err := RecoverInterruptedRestore(clean, recoverOptions()); err != nil {
		t.Fatalf("RecoverInterruptedRestore() on a clean directory = %v, want nil", err)
	}
	// The lock inode is the deliverable here: a read-only backup mount can only
	// reuse a lock somebody already created.
	if !isRegularFile(filepath.Join(clean, restoreLockName)) {
		t.Error("recovering a clean directory did not initialize the generation lock")
	}

	notADirectory := filepath.Join(root, "file")
	if err := os.WriteFile(notADirectory, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write the non-directory fixture: %v", err)
	}
	assertSnapshotError(t, RecoverInterruptedRestore(notADirectory, recoverOptions()), "State directory not found")
}

func TestStateLockReusesAReadOnlyRegularInodeAndRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	// The access mode is the contract a read-only Kubernetes state mount
	// depends on, and it is decided by this constant alone.
	if lockOpenFlags&syscall.O_ACCMODE != os.O_RDONLY {
		t.Errorf("lockOpenFlags access mode = %d, want O_RDONLY", lockOpenFlags&syscall.O_ACCMODE)
	}

	root := t.TempDir()
	lock := filepath.Join(root, restoreLockName)
	// Mode 0400 makes the property observable rather than merely declared: a
	// read-write open of this inode fails, so a lock that took one would too.
	if err := os.WriteFile(lock, nil, 0o400); err != nil {
		t.Fatalf("create the read-only lock inode: %v", err)
	}
	taken := false
	if err := withRestoreLock(lock, func() error { taken = true; return nil }); err != nil {
		t.Errorf("withRestoreLock() on a read-only inode = %v, want nil", err)
	}
	if !taken {
		t.Error("withRestoreLock() never ran its body")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove the lock inode: %v", err)
	}

	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write the symlink target: %v", err)
	}
	if err := os.Symlink(target, lock); err != nil {
		t.Fatalf("plant the lock symlink: %v", err)
	}
	assertSnapshotError(t, withRestoreLock(lock, func() error { return nil }), "must not be a symlink")
	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove the lock symlink: %v", err)
	}

	if err := os.Mkdir(lock, 0o750); err != nil {
		t.Fatalf("create the lock directory: %v", err)
	}
	assertSnapshotError(t, withRestoreLock(lock, func() error { return nil }), "must be a regular file")
}

func TestBackupFailsClosedWhenAReadOnlyStateMountHasNoInitializedLock(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		// Root ignores the directory mode, so the read-only mount this case
		// describes cannot be simulated. Reported rather than silently passed.
		t.Skip("running as root: a read-only state directory cannot be simulated")
	}
	root := t.TempDir()
	stateDir := seedState(t, filepath.Join(root, "state"))
	backupRoot := filepath.Join(root, "backups")
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("make the state directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o750) })

	_, err := BackupState(t.Context(), stateDir, backupRoot, backupOptions())

	if err == nil {
		t.Fatal("BackupState() succeeded on a state directory whose lock cannot be created")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("BackupState() error = %v, want a permission failure", err)
	}
	// Failing closed means no lock and no backup root: a backup that silently
	// serialized on a different lock than everybody else would be worse than
	// no backup at all.
	if _, err := os.Lstat(filepath.Join(stateDir, restoreLockName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a lock was created on a read-only mount: %v", err)
	}
	if _, err := os.Lstat(backupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a backup root was created despite the lock failure: %v", err)
	}
}
