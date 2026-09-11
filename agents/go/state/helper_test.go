package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
)

// repositoryDataset is the committed dataset, relative to this package.
//
// `go test` runs with the working directory set to the package directory, so
// this resolves to agents/data. Every fixture copies incidents.db out of it: the
// committed seed is byte-reproducible and gated by a rebuild comparison, so a
// test that wrote to it would break the repository rather than only itself.
const repositoryDataset = "../../data"

// fixedStamp pins every fixture snapshot's directory name, which is what makes
// the "a second backup owns this timestamp" cases reachable at all.
const fixedStamp = "20990101T000000Z"

// fixedCommit is the provenance a fixture snapshot records: a plausible commit
// hash, so the manifest's source identity is exercised rather than defaulted.
var fixedCommit = strings.Repeat("a", 40)

var fixedTreeDigest = "sha256:" + strings.Repeat("b", 64)

func fixedBuild() buildinfo.Info {
	return buildinfo.Info{
		Timestamp:      time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC),
		Mode:           buildinfo.Development,
		Version:        buildinfo.DevelopmentVersion,
		SourceIdentity: fixedCommit,
		Revision:       fixedCommit,
		TreeDigest:     fixedTreeDigest,
		Dirty:          false,
	}
}

// quietLogger keeps a passing test's output empty. Nothing in the Python suite
// asserts on a log line, and a restore emits one per database.
func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// backupOptions is the fixture backup configuration: pinned stamp, pinned
// commit, and the default retention.
func backupOptions() BackupOptions {
	return BackupOptions{
		Logger: quietLogger(), Keep: defaultKeep, Timestamp: fixedStamp, Build: fixedBuild(),
	}
}

// restoreOptions is the fixture restore configuration.
func restoreOptions() RestoreOptions {
	return RestoreOptions{Logger: quietLogger()}
}

// recoverOptions is the fixture recovery configuration.
func recoverOptions() RecoverOptions {
	return RecoverOptions{Logger: quietLogger()}
}

// seedState builds a state directory holding the two databases a running agent
// keeps: a copy of the committed incident seed and an ADK-shaped session store.
func seedState(t *testing.T, stateDir string) string {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	copyFixture(t, filepath.Join(repositoryDataset, "incidents.db"), filepath.Join(stateDir, "incidents.db"))
	execSQL(t, filepath.Join(stateDir, "runtime.db"),
		"CREATE TABLE sessions (id TEXT PRIMARY KEY)",
		"INSERT INTO sessions VALUES ('session-1')",
	)
	return stateDir
}

// copyFixture copies a file, failing the test rather than the caller.
func copyFixture(t *testing.T, source, target string) {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the fixture %s: %v", source, err)
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatalf("write the fixture %s: %v", target, err)
	}
}

// execSQL runs setup statements against a fixture database.
//
// It deliberately does not go through this package's own connection helpers: a
// fixture built by the code under test could not contradict it.
func execSQL(t *testing.T, path string, statements ...string) {
	t.Helper()
	db := openFixture(t, path)
	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("fixture statement %q on %s: %v", statement, path, err)
		}
	}
}

// openFixture opens a database read-write for setup and assertions.
//
// ignore_check_constraints is on because two of the ported cases have to store
// an audit row this schema's CHECK constraint would otherwise reject — that is
// the point of those cases: a database written by a future or broken binary.
func openFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve the fixture path %s: %v", path, err)
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	db, err := sql.Open(sqliteDriver, uri.String()+"?_pragma=ignore_check_constraints(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open the fixture database %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the fixture database %s: %v", path, err)
		}
	})
	return db
}

// snapshotFixture seeds a state directory and publishes one snapshot from it.
func snapshotFixture(t *testing.T) (stateDir, backupRoot, snapshot string) {
	t.Helper()
	root := t.TempDir()
	stateDir = seedState(t, filepath.Join(root, "state"))
	backupRoot = filepath.Join(root, "backups")
	snapshot, err := BackupState(t.Context(), stateDir, backupRoot, backupOptions())
	if err != nil {
		t.Fatalf("publish the fixture snapshot: %v", err)
	}
	return stateDir, backupRoot, snapshot
}

// fingerprints reads the bytes of every database in a directory.
func fingerprints(t *testing.T, directory string) map[string]string {
	t.Helper()
	paths, err := databaseFiles(directory)
	if err != nil {
		t.Fatalf("list the databases under %s: %v", directory, err)
	}
	return readAll(t, paths)
}

// generationFingerprints reads the bytes of every state file in a directory,
// sidecars included. It is what proves a rollback restored the *generation* and
// not merely the databases.
func generationFingerprints(t *testing.T, directory string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("list %s: %v", directory, err)
	}
	var paths []string
	for _, entry := range entries {
		if isStateFilename(entry.Name()) {
			paths = append(paths, filepath.Join(directory, entry.Name()))
		}
	}
	return readAll(t, paths)
}

// readAll reads every named file, keyed by base name.
func readAll(t *testing.T, paths []string) map[string]string {
	t.Helper()
	content := make(map[string]string, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		content[filepath.Base(path)] = string(raw)
	}
	return content
}

// assertSameFingerprints fails when live state is not byte-identical to what it
// was before the operation under test.
func assertSameFingerprints(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("state holds %d files, want %d: got %v, want %v",
			len(got), len(want), sortedKeys(got), sortedKeys(want))
	}
	for name, expected := range want {
		actual, ok := got[name]
		if !ok {
			t.Errorf("state is missing %s", name)
			continue
		}
		if actual != expected {
			t.Errorf("%s changed on disk: %d bytes before, %d after", name, len(expected), len(actual))
		}
	}
}

// sortedKeys renders a fingerprint map's names for a failure message.
func sortedKeys(content map[string]string) []string {
	names := make([]string, 0, len(content))
	for name := range content {
		names = append(names, name)
	}
	return baseNames(names)
}

// assertNoRestoreResidue fails when a transaction left anything behind.
func assertNoRestoreResidue(t *testing.T, stateDir string) {
	t.Helper()
	artifacts, err := restoreArtifacts(stateDir)
	if err != nil {
		t.Fatalf("list the restore residue: %v", err)
	}
	if len(artifacts) > 0 {
		t.Errorf("restore residue survived: %v", baseNames(artifacts))
	}
	if _, err := os.Lstat(journalPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the durable restore journal survived: %v", err)
	}
}

// assertSnapshotError fails unless err is this package's error and its message
// contains want.
func assertSnapshotError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want one containing %q", want)
	}
	if !errors.Is(err, ErrSnapshot) {
		t.Errorf("error %v is not an ErrSnapshot", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// readJSONFixture decodes a JSON document written by the code under test.
func readJSONFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return document
}

// writeJSONFixture publishes a hand-built document, using the package's own
// encoder so the bytes are exactly what a real write would have produced.
func writeJSONFixture(t *testing.T, path string, document map[string]any) {
	t.Helper()
	if err := writeJSONDocument(path, document, 0o600); err != nil {
		t.Fatalf("write the fixture document %s: %v", path, err)
	}
}

// publishManifest re-signs a mutated manifest so the marker still matches it.
//
// This is what makes the manifest-corruption cases meaningful: a tampered
// manifest that fails the hash check would only ever prove the hash check
// works, never the field validation behind it.
func publishManifest(t *testing.T, snapshot string, manifest map[string]any) {
	t.Helper()
	manifestPath := filepath.Join(snapshot, manifestName)
	writeJSONFixture(t, manifestPath, manifest)
	digest, err := sha256File(manifestPath)
	if err != nil {
		t.Fatalf("hash the fixture manifest: %v", err)
	}
	writeJSONFixture(t, filepath.Join(snapshot, completeName), map[string]any{
		"format_version":  SnapshotFormatVersion,
		"manifest_sha256": digest,
	})
}

// mustList, mustObject, mustNumber and mustString read a decoded JSON value a
// fixture knows the shape of. A wrong shape is a broken fixture, not a finding,
// so they stop the test rather than returning a zero value that would be
// asserted against later.
func mustList(t *testing.T, value any) []any {
	t.Helper()
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("fixture value %v is not a JSON list", value)
	}
	return list
}

func mustObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("fixture value %v is not a JSON object", value)
	}
	return object
}

func mustNumber(t *testing.T, value any) float64 {
	t.Helper()
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("fixture value %v is not a JSON number", value)
	}
	return number
}

func mustString(t *testing.T, value any) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("fixture value %v is not a JSON string", value)
	}
	return text
}

// journalDocument builds a syntactically valid restore journal.
func journalDocument(transactionID string, phase restorePhase) map[string]any {
	return map[string]any{
		"format_version": restoreJournalFormatVersion,
		"transaction_id": transactionID,
		"phase":          string(phase),
		"staging_dir":    stagingPrefix + transactionID,
		"quarantine_dir": quarantinePrefix + transactionID,
		"old_inventory":  []any{},
		"new_inventory": []any{
			map[string]any{"filename": "new.db", "sha256": strings.Repeat("0", 64), "size_bytes": 0},
		},
	}
}

// fixedTransactionID is a valid 32-character lowercase hex transaction id for
// hand-built journals.
var fixedTransactionID = strings.Repeat("a", 32)

// waitForFile blocks until a subprocess marker appears.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the subprocess marker %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
