package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

func TestBackupRecordsVersionedHashedInventoryAndSource(t *testing.T) {
	t.Parallel()

	_, _, snapshot := snapshotFixture(t)
	manifest := readJSONFixture(t, filepath.Join(snapshot, manifestName))
	marker := readJSONFixture(t, filepath.Join(snapshot, completeName))

	if version, ok := manifest["format_version"].(float64); !ok || int(version) != SnapshotFormatVersion {
		t.Errorf("manifest format_version = %v, want %d", manifest["format_version"], SnapshotFormatVersion)
	}
	source, ok := manifest["source"].(map[string]any)
	if !ok {
		t.Fatalf("manifest source = %v, want an object", manifest["source"])
	}
	want := map[string]any{
		"application":     applicationName,
		"build_timestamp": "2026-08-09T10:11:12Z",
		"commit":          fixedCommit,
		"dirty":           false,
		"mode":            "development",
		"revision":        fixedCommit,
		"source_identity": fixedCommit,
		"tree_digest":     fixedTreeDigest,
		"version":         "development",
	}
	for field, expected := range want {
		if source[field] != expected {
			t.Errorf("manifest source[%q] = %v, want %v", field, source[field], expected)
		}
	}
	if len(source) != len(want) {
		t.Errorf("manifest source has %d fields, want the complete %d-field identity", len(source), len(want))
	}

	records, ok := manifest["databases"].([]any)
	if !ok {
		t.Fatalf("manifest databases = %v, want a list", manifest["databases"])
	}
	names := make([]string, 0, len(records))
	for _, raw := range records {
		record, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("manifest entry = %v, want an object", raw)
		}
		name, _ := record["filename"].(string)
		names = append(names, name)
		if digest, _ := record["sha256"].(string); len(digest) != 64 {
			t.Errorf("%s sha256 = %q, want 64 characters", name, digest)
		}
		identity, ok := record["sqlite"].(map[string]any)
		if !ok {
			t.Fatalf("%s sqlite = %v, want an object", name, record["sqlite"])
		}
		if schema, _ := identity["schema_sha256"].(string); schema == "" {
			t.Errorf("%s has no schema identity", name)
		}
	}
	// Filename order, not set membership: the manifest is a signed sequence and
	// a restore publishes it in the order it is written.
	if !equalNames(names, []string{"incidents.db", "runtime.db"}) {
		t.Errorf("manifest names = %v, want [incidents.db runtime.db]", names)
	}

	manifestDigest, err := sha256File(filepath.Join(snapshot, manifestName))
	if err != nil {
		t.Fatalf("hash the manifest: %v", err)
	}
	if version, ok := marker["format_version"].(float64); !ok || int(version) != SnapshotFormatVersion {
		t.Errorf("marker format_version = %v, want %d", marker["format_version"], SnapshotFormatVersion)
	}
	if marker["manifest_sha256"] != manifestDigest {
		t.Errorf("marker manifest_sha256 = %v, want %q", marker["manifest_sha256"], manifestDigest)
	}
	if len(marker) != 2 {
		t.Errorf("marker has %d fields, want exactly format_version and manifest_sha256", len(marker))
	}
}

func TestBackupRejectsInvalidInputsBeforePublication(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	backupRoot := filepath.Join(root, "backups")
	missing := filepath.Join(root, "missing")

	options := backupOptions()
	options.Keep = 0
	_, err := BackupState(t.Context(), missing, backupRoot, options)
	assertSnapshotError(t, err, "retention must be positive")

	_, err = BackupState(t.Context(), missing, backupRoot, backupOptions())
	assertSnapshotError(t, err, "State directory not found")

	empty := filepath.Join(root, "empty")
	if mkdirErr := os.Mkdir(empty, 0o750); mkdirErr != nil {
		t.Fatalf("create the empty state directory: %v", mkdirErr)
	}
	_, err = BackupState(t.Context(), empty, backupRoot, backupOptions())
	assertSnapshotError(t, err, "No SQLite databases found")

	// None of the three reached publication, so the backup root must not exist:
	// an empty directory would read as "a snapshot was attempted here".
	if _, err := os.Lstat(backupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the backup root was created by a rejected backup: %v", err)
	}
}

func TestBackupRejectsInvalidTimestampWithoutPublicationArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stateDir := seedState(t, filepath.Join(root, "state"))
	backupRoot := filepath.Join(root, "backups")
	options := backupOptions()
	options.Timestamp = "31-07-2026"

	_, err := BackupState(t.Context(), stateDir, backupRoot, options)

	assertSnapshotError(t, err, "must use YYYYMMDDTHHMMSSZ")
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		t.Fatalf("read the backup root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the backup root holds %d entries after a rejected timestamp, want 0", len(entries))
	}
}

func TestBackupRejectsDuplicateSnapshotAndConcurrentOwner(t *testing.T) {
	t.Parallel()

	stateDir, backupRoot, snapshot := snapshotFixture(t)

	_, err := BackupState(t.Context(), stateDir, backupRoot, backupOptions())
	assertSnapshotError(t, err, "Completed snapshot already exists")

	if removeErr := os.RemoveAll(snapshot); removeErr != nil {
		t.Fatalf("remove the published snapshot: %v", removeErr)
	}
	// A leftover claim directory means another backup owns this timestamp and
	// may still be writing it.
	if mkdirErr := os.Mkdir(filepath.Join(backupRoot, stampLockPrefix+fixedStamp), 0o750); mkdirErr != nil {
		t.Fatalf("create the timestamp claim: %v", mkdirErr)
	}
	_, err = BackupState(t.Context(), stateDir, backupRoot, backupOptions())
	assertSnapshotError(t, err, "Another backup owns timestamp")
}

func TestBackupPrunesOnlyExpiredCompleteSnapshots(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stateDir := seedState(t, filepath.Join(root, "state"))
	backupRoot := filepath.Join(root, "backups")

	options := backupOptions()
	options.Keep = 1
	first, err := BackupState(t.Context(), stateDir, backupRoot, options)
	if err != nil {
		t.Fatalf("publish the first snapshot: %v", err)
	}
	incomplete := filepath.Join(backupRoot, incompletePrefix+"manual")
	if mkdirErr := os.Mkdir(incomplete, 0o750); mkdirErr != nil {
		t.Fatalf("create the incomplete directory: %v", mkdirErr)
	}
	options.Timestamp = "20990101T000001Z"
	second, err := BackupState(t.Context(), stateDir, backupRoot, options)
	if err != nil {
		t.Fatalf("publish the second snapshot: %v", err)
	}

	if _, err := os.Lstat(first); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the expired snapshot survived retention: %v", err)
	}
	if !isRegularFile(filepath.Join(second, completeName)) {
		t.Error("the newest snapshot was pruned")
	}
	// Retention only ever removes published snapshots. A half-written staging
	// directory is somebody's evidence, not this job's garbage.
	if info, err := os.Lstat(incomplete); err != nil || !info.IsDir() {
		t.Errorf("retention removed an unpublished directory: %v", err)
	}
}

func TestBackupWrapsSQLiteCopyFailures(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	err := copyDatabase(t.Context(), filepath.Join(root, "missing.db"), filepath.Join(root, "copy.db"))

	assertSnapshotError(t, err, "Could not back up SQLite database")
}

func TestDatabaseInspectionDistinguishesIntegrityFailureFromOpenFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	corrupt := filepath.Join(root, "corrupt.db")
	copyFixture(t, filepath.Join(repositoryDataset, "incidents.db"), corrupt)
	corruptPage(t, corrupt)

	_, err := inspectDatabase(t.Context(), corrupt)
	assertSnapshotError(t, err, "integrity check failed")

	// A file that is not a database at all fails at the door instead, and the
	// two messages have to stay distinct: one means "restore something else",
	// the other means "this path is wrong".
	notADatabase := filepath.Join(root, "not-sqlite.db")
	if writeErr := os.WriteFile(notADatabase, []byte("not a SQLite database"), 0o600); writeErr != nil {
		t.Fatalf("write the non-database fixture: %v", writeErr)
	}
	_, err = inspectDatabase(t.Context(), notADatabase)
	assertSnapshotError(t, err, "Could not inspect SQLite database")
}

// corruptPage damages a b-tree page header while leaving the file header — and
// therefore the ability to open the file — intact.
func corruptPage(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const pageSize = 4096
	if len(raw) < 2*pageSize {
		t.Fatalf("%s is %d bytes, too small to corrupt a second page", path, len(raw))
	}
	// Offset 3 into page 2 is the b-tree page header's cell-content pointer.
	for offset := pageSize + 3; offset < pageSize+11; offset++ {
		raw[offset] ^= 0xFF
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBackupRejectsAnInconsistentBuildIdentityBeforePublication(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	options := backupOptions()
	options.Build.SourceIdentity = strings.Repeat("c", 40)

	_, err := BackupState(t.Context(), filepath.Join(root, "state"), filepath.Join(root, "backups"), options)

	assertSnapshotError(t, err, "Invalid build identity")
	if _, statErr := os.Lstat(filepath.Join(root, "backups")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("invalid identity created a backup root: %v", statErr)
	}
}

func TestBackupRejectsUnsupportedSchemasWithoutMutatingInput(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		file     string
		mutate   func(t *testing.T, stateDir string)
		contains string
	}{
		{
			name: "future audit schema",
			file: "incidents.db",
			mutate: func(t *testing.T, stateDir string) {
				insertAuditRow(t, stateDir, domain.CurrentAuditSchemaVersion+1)
			},
			contains: "Upgrade the application or select a compatible snapshot",
		},
		{
			name: "non-integer audit schema",
			file: "incidents.db",
			mutate: func(t *testing.T, stateDir string) {
				insertAuditRow(t, stateDir, "future")
			},
			contains: "unsupported audit schema version",
		},
		{
			name: "future runtime schema",
			file: "runtime.db",
			mutate: func(t *testing.T, stateDir string) {
				execSQL(t, filepath.Join(stateDir, "runtime.db"),
					`CREATE TABLE adk_internal_metadata ("key" TEXT PRIMARY KEY, value TEXT NOT NULL)`,
					"INSERT INTO adk_internal_metadata VALUES ('schema_version', '99')",
				)
			},
			contains: "runtime schema version \"99\"",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			stateDir := seedState(t, filepath.Join(root, "state"))
			backupRoot := filepath.Join(root, "backups")
			testCase.mutate(t, stateDir)
			before := generationFingerprints(t, stateDir)

			_, err := BackupState(t.Context(), stateDir, backupRoot, backupOptions())

			assertSnapshotError(t, err, testCase.contains)
			// The refusal must be read-only: an operator whose binary is too old
			// still has an intact database to snapshot with a newer one.
			assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
			published, err := completedSnapshots(backupRoot)
			if err != nil {
				t.Fatalf("list the published snapshots: %v", err)
			}
			if len(published) != 0 {
				t.Errorf("a rejected backup published %v", baseNames(published))
			}
		})
	}
}

// insertAuditRow writes one audit row with an arbitrary schema_version, which
// is what a newer — or broken — binary would have left behind.
func insertAuditRow(t *testing.T, stateDir string, version any) {
	t.Helper()
	db := openFixture(t, filepath.Join(stateDir, "incidents.db"))
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO audit_log
			(schema_version, ts, actor, approved_by, rationale, context_summary,
			 session_id, invocation_id, action, target, detail)
		VALUES (?, '2026-07-31T08:00:00Z', 'future-agent', 'engineer',
				'future approval', 'future context', 'future-session',
				'future-invocation', 'restart_service', ?, 'future detail')`,
		version, domain.Reference().Services.Inventory,
	)
	if err != nil {
		t.Fatalf("insert the future audit row: %v", err)
	}
}

func TestDeterministicEncodingMatchesTheSharedSnapshotFormat(t *testing.T) {
	t.Parallel()

	// The snapshot format is shared with the Python track, so the encoder has to
	// reproduce json.dumps: sorted keys, two-space indent, no HTML escaping, and
	// every non-ASCII rune escaped.
	encoded, err := encodeDocument(map[string]any{
		"b":       1,
		"a":       []any{},
		"escaped": "a < b & c > d",
		"unicode": "héllo → \U0001F600",
	})
	if err != nil {
		t.Fatalf("encodeDocument() error = %v", err)
	}
	want := "{\n" +
		"  \"a\": [],\n" +
		"  \"b\": 1,\n" +
		"  \"escaped\": \"a < b & c > d\",\n" +
		"  \"unicode\": \"h\\u00e9llo \\u2192 \\ud83d\\ude00\"\n" +
		"}\n"
	if string(encoded) != want {
		t.Errorf("encodeDocument() =\n%s\nwant\n%s", encoded, want)
	}

	compact, err := encodeCompact([][]string{{"table", "audit_log", "audit_log", "CHECK (v >= 1)"}})
	if err != nil {
		t.Fatalf("encodeCompact() error = %v", err)
	}
	if got := string(compact); got != `[["table","audit_log","audit_log","CHECK (v >= 1)"]]` {
		t.Errorf("encodeCompact() = %s", got)
	}
	if strings.Contains(string(compact), "\n") {
		t.Error("encodeCompact() emitted a trailing newline; the schema digest is taken over the bare value")
	}
}
