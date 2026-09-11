package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

func TestRestorePublishesExactInventoryAndRemovesAbsentLiveDatabase(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	expected := fingerprints(t, snapshot)
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("clear the state directory: %v", err)
	}
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("recreate the state directory: %v", err)
	}
	// A database the snapshot does not name is not part of the restored
	// generation and must not survive it.
	execSQL(t, filepath.Join(stateDir, "obsolete.db"), "CREATE TABLE obsolete (value TEXT)")

	restored, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions())
	if err != nil {
		t.Fatalf("RestoreState() error = %v, want nil", err)
	}

	if !equalNames(baseNames(restored), []string{"incidents.db", "runtime.db"}) {
		t.Errorf("restored %v, want [incidents.db runtime.db]", baseNames(restored))
	}
	assertSameFingerprints(t, expected, fingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreRemovesWALSHMAndJournalSidecarsFromThePreviousGeneration(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	suffixes := []string{"-wal", "-shm", "-journal"}
	sidecars := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		sidecar := filepath.Join(stateDir, "incidents.db"+suffix)
		if err := os.WriteFile(sidecar, []byte("stale sidecar"), 0o600); err != nil {
			t.Fatalf("write %s: %v", sidecar, err)
		}
		sidecars = append(sidecars, sidecar)
	}

	if _, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions()); err != nil {
		t.Fatalf("RestoreState() error = %v, want nil", err)
	}

	// A restored database beside the previous generation's write-ahead log is a
	// silent corruption, which is why the publish step only ever moves *.db and
	// the sidecars stay in the quarantine that is thrown away.
	for _, sidecar := range sidecars {
		if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the restore: %v", filepath.Base(sidecar), err)
		}
	}
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreRejectsBadHashBeforeMutatingLiveState(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	before := fingerprints(t, stateDir)
	appendBytes(t, filepath.Join(snapshot, "runtime.db"), []byte("tampered"))

	_, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions())

	assertSnapshotError(t, err, "hash or size mismatch")
	assertSameFingerprints(t, before, fingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreRejectsMissingHiddenAndIncompleteSnapshotsBeforeMutation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stateDir := seedState(t, filepath.Join(root, "state"))
	before := fingerprints(t, stateDir)

	for _, testCase := range []struct {
		name    string
		prepare func(t *testing.T, root string) string
		error   string
	}{
		{
			"missing",
			func(_ *testing.T, root string) string { return filepath.Join(root, "missing") },
			"Snapshot directory not found",
		},
		{
			"hidden",
			func(t *testing.T, root string) string { return makeDir(t, filepath.Join(root, ".hidden")) },
			"hidden and unpublished",
		},
		{
			"incomplete",
			func(t *testing.T, root string) string { return makeDir(t, filepath.Join(root, "incomplete")) },
			"Snapshot is incomplete",
		},
	} {
		snapshot := testCase.prepare(t, root)
		_, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions())
		assertSnapshotError(t, err, testCase.error)
	}

	assertSameFingerprints(t, before, fingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreRejectsUnreadableOrNonObjectMetadata(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		content string
		error   string
	}{
		{"truncated", "{", "Could not read snapshot metadata"},
		{"not an object", "[]", "Snapshot metadata must be a JSON object"},
		{"trailing document", "{}\n{}", "Could not read snapshot metadata"},
		{"invalid encoding", "{\"a\": \"\xff\"}", "Could not read snapshot metadata"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			stateDir, _, snapshot := snapshotFixture(t)
			before := fingerprints(t, stateDir)
			if err := os.WriteFile(filepath.Join(snapshot, manifestName), []byte(testCase.content), 0o600); err != nil {
				t.Fatalf("corrupt the manifest: %v", err)
			}

			_, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions())

			assertSnapshotError(t, err, testCase.error)
			assertSameFingerprints(t, before, fingerprints(t, stateDir))
			assertNoRestoreResidue(t, stateDir)
		})
	}
}

func TestRestoreRejectsMalformedSignedManifestFieldsBeforeMutation(t *testing.T) {
	t.Parallel()

	// Every case re-signs the manifest through publishManifest, so none of them
	// is caught by the marker hash. What is under test is the field validation
	// behind that hash — the checks that stop a *correctly signed* but
	// nonsensical manifest from reaching live state.
	for _, testCase := range []struct {
		name    string
		corrupt func(t *testing.T, manifest, entry map[string]any)
		error   string
	}{
		{
			"future manifest format",
			func(_ *testing.T, m, _ map[string]any) { m["format_version"] = SnapshotFormatVersion + 1 },
			"Unsupported snapshot manifest format",
		},
		{
			"empty creation timestamp",
			func(_ *testing.T, m, _ map[string]any) { m["created_at"] = "" },
			"manifest has no creation timestamp",
		},
		{
			"incomplete source identity",
			func(_ *testing.T, m, _ map[string]any) {
				m["source"] = map[string]any{"application": applicationName, "version": "0.5.0"}
			},
			"manifest has incomplete source identity",
		},
		{
			"inconsistent extended source identity",
			func(t *testing.T, m, _ map[string]any) {
				mustObject(t, m["source"])["tree_digest"] = "sha256:short"
			},
			"manifest has incomplete source identity",
		},
		{
			"empty inventory",
			func(_ *testing.T, m, _ map[string]any) { m["databases"] = []any{} },
			"manifest has no database inventory",
		},
		{
			"entry is not an object",
			func(t *testing.T, m, _ map[string]any) {
				m["databases"] = append([]any{"incidents.db"}, mustList(t, m["databases"])[1:]...)
			},
			"database entry must be an object",
		},
		{
			"traversing filename",
			func(_ *testing.T, _, entry map[string]any) { entry["filename"] = "../incidents.db" },
			"Unsafe snapshot database filename",
		},
		{
			"duplicate filename",
			func(t *testing.T, m, entry map[string]any) {
				m["databases"] = append(mustList(t, m["databases"]), cloneEntry(entry))
			},
			"Duplicate snapshot database filename",
		},
		{
			"invalid digest",
			func(_ *testing.T, _, entry map[string]any) { entry["sha256"] = "not-a-sha256" },
			"Invalid SHA-256",
		},
		{
			"zero size",
			func(_ *testing.T, _, entry map[string]any) { entry["size_bytes"] = 0 },
			"Invalid size",
		},
		{
			"missing schema identity",
			func(_ *testing.T, _, entry map[string]any) { entry["sqlite"] = nil },
			"Missing SQLite schema identity",
		},
		{
			"inventory shorter than the directory",
			func(t *testing.T, m, _ map[string]any) {
				records := mustList(t, m["databases"])
				m["databases"] = records[:len(records)-1]
			},
			"differs from its files",
		},
		{
			"schema identity mismatch",
			func(t *testing.T, _, entry map[string]any) {
				identity := mustObject(t, entry["sqlite"])
				identity["user_version"] = mustNumber(t, identity["user_version"]) + 1
			},
			"schema identity mismatch",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			stateDir, _, snapshot := snapshotFixture(t)
			manifest := readJSONFixture(t, filepath.Join(snapshot, manifestName))
			entry := mustObject(t, mustList(t, manifest["databases"])[0])
			testCase.corrupt(t, manifest, entry)
			publishManifest(t, snapshot, manifest)
			before := fingerprints(t, stateDir)

			_, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions())

			assertSnapshotError(t, err, testCase.error)
			assertSameFingerprints(t, before, fingerprints(t, stateDir))
			assertNoRestoreResidue(t, stateDir)
		})
	}
}

func TestRestoreAcceptsTheLegacyThreeFieldSourceProjection(t *testing.T) {
	t.Parallel()

	_, _, snapshot := snapshotFixture(t)
	manifest := readJSONFixture(t, filepath.Join(snapshot, manifestName))
	manifest["source"] = map[string]any{
		"application": applicationName,
		"version":     "0.5.0",
		"commit":      fixedCommit,
	}
	publishManifest(t, snapshot, manifest)

	if _, err := validateInventory(t.Context(), snapshot); err != nil {
		t.Fatalf("validateInventory() rejected a legacy source projection: %v", err)
	}
}

// cloneEntry copies a manifest record so a duplicate is a separate object.
func cloneEntry(entry map[string]any) map[string]any {
	clone := make(map[string]any, len(entry))
	for key, value := range entry {
		clone[key] = value
	}
	return clone
}

// makeDir creates a directory for a fixture and returns it.
func makeDir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	return path
}

// appendBytes tampers with a file the way a partial overwrite would.
func appendBytes(t *testing.T, path string, extra []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s for tampering: %v", path, err)
	}
	if _, err := file.Write(extra); err != nil {
		t.Fatalf("tamper with %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func TestRestoreRejectsManifestHashMismatchBeforeMutation(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	before := fingerprints(t, stateDir)
	// One trailing space: still valid JSON, still every field intact, and the
	// marker is what catches it.
	appendBytes(t, filepath.Join(snapshot, manifestName), []byte(" "))

	_, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions())

	assertSnapshotError(t, err, "manifest hash does not match")
	assertSameFingerprints(t, before, fingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreRejectsUnsupportedSnapshotFormatBeforeMutation(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	before := fingerprints(t, stateDir)
	marker := readJSONFixture(t, filepath.Join(snapshot, completeName))
	marker["format_version"] = SnapshotFormatVersion + 1
	writeJSONFixture(t, filepath.Join(snapshot, completeName), marker)

	_, err := RestoreState(t.Context(), snapshot, stateDir, restoreOptions())

	assertSnapshotError(t, err, "Unsupported snapshot marker format")
	assertSameFingerprints(t, before, fingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestEarlyRestoreFailuresLeaveNoUnjournaledResidue(t *testing.T) {
	tests := []struct {
		name      string
		failMkdir int
		failSync  bool
	}{
		{name: "staging directory creation", failMkdir: 1},
		{name: "quarantine directory creation", failMkdir: 2},
		{name: "state directory fsync", failSync: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stateDir, _, snapshot := snapshotFixture(t)
			before := generationFingerprints(t, stateDir)
			mkdirCalls := 0
			opts := restoreOptions()
			opts.mkdir = func(path string, mode os.FileMode) error {
				mkdirCalls++
				if mkdirCalls == test.failMkdir {
					return errors.New("injected restore mkdir failure")
				}
				return os.Mkdir(path, mode)
			}
			if test.failSync {
				opts.syncDir = func(string) error {
					return errors.New("injected restore fsync failure")
				}
			}

			_, err := RestoreState(t.Context(), snapshot, stateDir, opts)
			if err == nil || !strings.Contains(err.Error(), "injected restore") {
				t.Fatalf("RestoreState() error = %v, want injected early failure", err)
			}
			assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
			assertNoRestoreResidue(t, stateDir)
			if err := RecoverInterruptedRestore(stateDir, recoverOptions()); err != nil {
				t.Fatalf("next startup recovery after clean early failure: %v", err)
			}
			assertNoRestoreResidue(t, stateDir)
		})
	}
}

func TestRestoreRollsBackTheCompleteGenerationAfterAPublicationFailure(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	execSQL(t, filepath.Join(stateDir, "obsolete.db"), "CREATE TABLE obsolete (value TEXT)")
	before := generationFingerprints(t, stateDir)

	_, err := RestoreState(t.Context(), snapshot, stateDir, RestoreOptions{
		Logger: quietLogger(),
		BeforePublish: func(installed int) error {
			if installed >= 1 {
				return errors.New("injected restore publication failure after 1 database")
			}
			return nil
		},
	})

	if err == nil {
		t.Fatal("RestoreState() error = nil, want the injected publication failure")
	}
	if !strings.Contains(err.Error(), "injected restore publication failure") {
		t.Errorf("RestoreState() error = %v, want the injected publication failure", err)
	}
	// The whole generation comes back, sidecars and unmanifested databases
	// included — a rollback that restored only the manifest's files would have
	// silently deleted obsolete.db.
	assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreRollsBackTheCompleteGenerationWhenPublicationPanics(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	before := generationFingerprints(t, stateDir)

	// The Python track proves this with a KeyboardInterrupt, which it catches
	// through `except BaseException`. Go's equivalent of an unwinding
	// non-error failure is a panic, and it must roll back before it propagates.
	recovered := func() (recovered any) {
		defer func() { recovered = recover() }()
		_, _ = RestoreState(t.Context(), snapshot, stateDir, RestoreOptions{
			Logger: quietLogger(),
			BeforePublish: func(installed int) error {
				if installed >= 1 {
					panic("interrupted during publication")
				}
				return nil
			},
		})
		return nil
	}()

	if recovered == nil {
		t.Fatal("RestoreState() swallowed the panic; the caller must still see the failure")
	}
	assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreDetectsSnapshotChangeDuringStagingAndPreservesLiveState(t *testing.T) {
	t.Parallel()

	stateDir, _, snapshot := snapshotFixture(t)
	before := generationFingerprints(t, stateDir)
	entries, err := validateInventory(t.Context(), snapshot)
	if err != nil {
		t.Fatalf("validate the snapshot: %v", err)
	}
	// A snapshot that changes after it validated is exactly what the staged
	// re-verification exists for: the manifest was signed over bytes that are
	// no longer the bytes being copied.
	entries[0].SHA256 = strings.Repeat("0", 64)

	err = withRestoreLock(filepath.Join(stateDir, restoreLockName), func() error {
		return publishRestore(snapshot, stateDir, entries, restoreOptions())
	})

	assertSnapshotError(t, err, "changed while copying")
	assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreRejectsAManifestedUnsupportedSchemaBeforeMutatingLiveState(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		version any
		error   string
	}{
		{"future version", domain.CurrentAuditSchemaVersion + 1, "Upgrade the application or select a compatible snapshot"},
		{"non-integer version", "future", "unsupported audit schema version"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			stateDir, _, snapshot := snapshotFixture(t)
			// The row goes into the *snapshot*, and the manifest is re-signed
			// over it, so the only thing standing between it and live state is
			// the schema guard.
			db := openFixture(t, filepath.Join(snapshot, "incidents.db"))
			_, err := db.ExecContext(t.Context(), `
				INSERT INTO audit_log
					(schema_version, ts, actor, approved_by, rationale, context_summary,
					 session_id, invocation_id, action, target, detail)
				VALUES (?, '2099-01-01T00:00:00Z', 'future-agent', 'engineer',
						'future approval', 'future context', 'future-session',
						'future-invocation', 'restart_service', ?, 'future detail')`,
				testCase.version, domain.Reference().Services.Checkout,
			)
			if err != nil {
				t.Fatalf("insert the unsupported audit row: %v", err)
			}
			resignSnapshotDatabase(t, snapshot, "incidents.db")
			before := fingerprints(t, stateDir)

			_, err = RestoreState(t.Context(), snapshot, stateDir, restoreOptions())

			assertSnapshotError(t, err, testCase.error)
			assertSameFingerprints(t, before, fingerprints(t, stateDir))
			assertNoRestoreResidue(t, stateDir)
		})
	}
}

// resignSnapshotDatabase rewrites a manifest record over a database that was
// modified in place, so the snapshot stays internally consistent.
func resignSnapshotDatabase(t *testing.T, snapshot, filename string) {
	t.Helper()
	path := filepath.Join(snapshot, filename)
	digest, err := sha256File(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("measure %s: %v", path, err)
	}
	pool, err := openSQLite(t.Context(), path, readOnlyParameters)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	identity, err := readSchemaIdentity(t.Context(), pool)
	if err != nil {
		t.Fatalf("fingerprint %s: %v", path, err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}

	manifest := readJSONFixture(t, filepath.Join(snapshot, manifestName))
	for _, raw := range mustList(t, manifest["databases"]) {
		record := mustObject(t, raw)
		if record["filename"] != filename {
			continue
		}
		record["sha256"] = digest
		record["size_bytes"] = info.Size()
		record["sqlite"] = identity.payload()
	}
	publishManifest(t, snapshot, manifest)
}
