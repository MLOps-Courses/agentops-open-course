package platformdrill

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/state"
)

var testSourceIdentity = strings.Repeat("a", 40)

func testBuild() buildinfo.Info {
	return buildinfo.Info{
		Timestamp:      time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		Mode:           buildinfo.Development,
		Version:        buildinfo.DevelopmentVersion,
		SourceIdentity: testSourceIdentity,
		Revision:       testSourceIdentity,
		TreeDigest:     "sha256:" + strings.Repeat("b", 64),
	}
}

// skipDrillInShortMode drops one drill from `go test -short`.
//
// A drill is not a unit test: it seeds a real state directory, snapshots it
// through SQLite's VACUUM INTO, and restores it, so each one costs whole
// database copies. The gate is opt-in and deliberately one-way:
// agents/go/mise.toml's test task passes no -short, so the repository's own gate
// and CI still run every drill. It exists for the learner's inner loop, not for
// the suite's meaning.
func skipDrillInShortMode(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("this drill copies real databases; run without -short to exercise it")
	}
}

func TestSeedUsesProductionWriteBoundariesAndIsIdempotent(t *testing.T) {
	skipDrillInShortMode(t)

	stateDir := t.TempDir()
	seedA2AState(t, stateDir, "session-1", "task-1")
	options := SeedOptions{
		StateDir: stateDir,
		DataDir:  filepath.Clean(filepath.Join("..", "..", "data")),
		Marker:   "fixture-seed",
	}
	first, err := Seed(t.Context(), options)
	if err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	second, err := Seed(t.Context(), options)
	if err != nil {
		t.Fatalf("Seed() replay error = %v", err)
	}
	if first != second {
		t.Fatalf("Seed() replay = %+v, want %+v", second, first)
	}
	for _, check := range evidenceChecks(first) {
		count, queryErr := queryCount(t.Context(), filepath.Join(stateDir, check.database), check.query, check.args, false)
		if queryErr != nil {
			t.Fatalf("query seeded %s evidence: %v", check.database, queryErr)
		}
		if count != 1 {
			t.Errorf("seeded %s evidence rows = %d, want 1", check.database, count)
		}
	}
}

func TestSeedRecoversBeforeReadingOrPublishingDrillState(t *testing.T) {
	skipDrillInShortMode(t)

	stateDir := t.TempDir()
	seedA2AState(t, stateDir, "session-1", "task-1")
	if mkdirErr := os.Mkdir(filepath.Join(stateDir, ".restore-staged.unexplained"), 0o750); mkdirErr != nil {
		t.Fatalf("plant restore residue: %v", mkdirErr)
	}
	_, err := Seed(t.Context(), SeedOptions{
		StateDir: stateDir,
		DataDir:  filepath.Clean(filepath.Join("..", "..", "data")),
		Marker:   "fixture-seed",
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "residue") {
		t.Fatalf("Seed() error = %v, want crash-recovery refusal", err)
	}
	for _, name := range []string{"memory.db", "incidents.db"} {
		if _, statErr := os.Stat(filepath.Join(stateDir, name)); !os.IsNotExist(statErr) {
			t.Errorf("%s was published before crash recovery: %v", name, statErr)
		}
	}
}

func TestRestoreDrillDestroysThenRecoversExactGeneration(t *testing.T) {
	skipDrillInShortMode(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	backupRoot := filepath.Join(root, "backups")
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("create state fixture: %v", err)
	}
	evidence := fixtureEvidence()
	seedRestoreState(t, stateDir, evidence)
	snapshot, err := state.BackupState(t.Context(), stateDir, backupRoot, state.BackupOptions{
		Keep: 1, Timestamp: "20260808T120000Z", Build: testBuild(),
	})
	if err != nil {
		t.Fatalf("BackupState() error = %v", err)
	}
	if err := RestoreDrill(t.Context(), RestoreOptions{
		Snapshot: snapshot, StateDir: stateDir, ExpectedSourceIdentity: testSourceIdentity, Evidence: evidence,
	}); err != nil {
		t.Fatalf("RestoreDrill() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "obsolete.db")); !os.IsNotExist(err) {
		t.Fatalf("obsolete.db survived restore, stat error = %v", err)
	}
}

func TestRestoreDrillRefusesProvenanceMismatchBeforeMutation(t *testing.T) {
	skipDrillInShortMode(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("create state fixture: %v", err)
	}
	evidence := fixtureEvidence()
	seedRestoreState(t, stateDir, evidence)
	snapshot, err := state.BackupState(t.Context(), stateDir, filepath.Join(root, "backups"), state.BackupOptions{
		Keep: 1, Timestamp: "20260808T120000Z", Build: testBuild(),
	})
	if err != nil {
		t.Fatalf("BackupState() error = %v", err)
	}
	err = RestoreDrill(t.Context(), RestoreOptions{
		Snapshot: snapshot, StateDir: stateDir, ExpectedSourceIdentity: strings.Repeat("c", 40), Evidence: evidence,
	})
	if err == nil {
		t.Fatal("RestoreDrill() accepted a snapshot from another revision")
	}
	count, queryErr := queryCount(t.Context(), filepath.Join(stateDir, "runtime.db"),
		"SELECT COUNT(*) FROM sessions WHERE id = ?", []any{evidence.SessionID}, false)
	if queryErr != nil {
		t.Fatalf("query untouched state: %v", queryErr)
	}
	if count != 1 {
		t.Errorf("session evidence rows after refusal = %d, want 1", count)
	}
}

func TestRestoreDrillRecoversBeforeDestructiveMutation(t *testing.T) {
	skipDrillInShortMode(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("create state fixture: %v", err)
	}
	evidence := fixtureEvidence()
	seedRestoreState(t, stateDir, evidence)
	snapshot, err := state.BackupState(t.Context(), stateDir, filepath.Join(root, "backups"), state.BackupOptions{
		Keep: 1, Timestamp: "20260808T120000Z", Build: testBuild(),
	})
	if err != nil {
		t.Fatalf("BackupState() error = %v", err)
	}
	if mkdirErr := os.Mkdir(filepath.Join(stateDir, ".restore-staged.unexplained"), 0o750); mkdirErr != nil {
		t.Fatalf("plant restore residue: %v", mkdirErr)
	}
	err = RestoreDrill(t.Context(), RestoreOptions{
		Snapshot: snapshot, StateDir: stateDir, ExpectedSourceIdentity: testSourceIdentity, Evidence: evidence,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "residue") {
		t.Fatalf("RestoreDrill() error = %v, want crash-recovery refusal", err)
	}
	count, queryErr := queryCount(t.Context(), filepath.Join(stateDir, "runtime.db"),
		"SELECT COUNT(*) FROM sessions WHERE id = ?", []any{evidence.SessionID}, false)
	if queryErr != nil {
		t.Fatalf("query untouched state: %v", queryErr)
	}
	if count != 1 {
		t.Errorf("session evidence rows after recovery refusal = %d, want 1", count)
	}
}

// TestSeedRefusesAnIncompleteInvocation keeps the marker and both boundaries
// mandatory. The marker is what makes a replay idempotent and what ties the
// audited action to one workflow run, so seeding without one would write a row
// no later step can find again.
func TestSeedRefusesAnIncompleteInvocation(t *testing.T) {
	for name, options := range map[string]SeedOptions{
		"no marker":         {StateDir: t.TempDir(), DataDir: seedDataDir()},
		"blank marker":      {StateDir: t.TempDir(), DataDir: seedDataDir(), Marker: "   "},
		"no state boundary": {DataDir: seedDataDir(), Marker: "fixture-seed"},
		"no data boundary":  {StateDir: t.TempDir(), Marker: "fixture-seed"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Seed(t.Context(), options); err == nil {
				t.Fatal("Seed() error = nil, want an incomplete-invocation refusal")
			}
		})
	}
}

// TestSeedRefusesStateWithoutDeterministicA2ATraffic is the drill's precondition.
// The evidence follows one session and one task created by real A2A traffic; if
// the platform script never produced them there is nothing whose recovery a
// restore could prove, so seeding has to fail loudly rather than publish
// identifiers it invented.
func TestSeedRefusesStateWithoutDeterministicA2ATraffic(t *testing.T) {
	skipDrillInShortMode(t)
	for name, testCase := range map[string]struct {
		prepare func(*testing.T, string)
		want    string
	}{
		"no persisted sessions at all": {
			prepare: func(*testing.T, string) {},
			want:    "open persisted ADK sessions",
		},
		"an empty session table": {
			prepare: func(t *testing.T, stateDir string) {
				execFixture(t, filepath.Join(stateDir, "runtime.db"),
					"CREATE TABLE sessions (id TEXT PRIMARY KEY, update_time TEXT NOT NULL)", nil)
			},
			want: "did not persist a session",
		},
		"no persisted tasks at all": {
			prepare: func(t *testing.T, stateDir string) {
				execFixture(t, filepath.Join(stateDir, "runtime.db"),
					"CREATE TABLE sessions (id TEXT PRIMARY KEY, update_time TEXT NOT NULL)", nil)
				execFixture(t, filepath.Join(stateDir, "runtime.db"),
					"INSERT INTO sessions VALUES (?, ?)", []any{"session-1", "2026-08-08T12:00:00Z"})
			},
			want: "open persisted A2A tasks",
		},
		"a task belonging to another session": {
			prepare: func(t *testing.T, stateDir string) {
				seedA2AState(t, stateDir, "session-1", "task-1")
				execFixture(t, filepath.Join(stateDir, "tasks.db"),
					"UPDATE tasks SET context_id = ?", []any{"another-session"})
			},
			want: "did not persist a task for session session-1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			testCase.prepare(t, stateDir)
			_, err := Seed(t.Context(), SeedOptions{
				StateDir: stateDir, DataDir: seedDataDir(), Marker: "fixture-seed",
			})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Seed() error = %v, want it to contain %q", err, testCase.want)
			}
		})
	}
}

// TestSeedRefusesAmbiguousLongTermEvidence protects the replay rule. Seed is
// idempotent because it finds the note it wrote last time; two identical notes
// mean it can no longer tell which row the restore is supposed to recover, and
// guessing would make the drill pass while proving nothing.
func TestSeedRefusesAmbiguousLongTermEvidence(t *testing.T) {
	skipDrillInShortMode(t)
	stateDir := t.TempDir()
	seedA2AState(t, stateDir, "session-1", "task-1")
	options := SeedOptions{StateDir: stateDir, DataDir: seedDataDir(), Marker: "fixture-seed"}
	evidence, err := Seed(t.Context(), options)
	if err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	// A second identical row is what a partially replayed workflow leaves behind.
	// Copying the seeded row keeps every column the production schema requires.
	execFixture(t, filepath.Join(stateDir, "memory.db"),
		"INSERT INTO incident_notes (ts, app_name, user_id, incident_id, note) "+
			"SELECT ts, app_name, user_id, incident_id, note FROM incident_notes WHERE note = ?",
		[]any{evidence.MemoryNote})

	if _, err := Seed(t.Context(), options); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Seed() error = %v, want an ambiguity refusal", err)
	}
}

// TestRestoreDrillRefusesAnUnusableSnapshotBeforeMutating is the safety property
// the whole drill depends on: every provenance check runs before the destructive
// fixture, so a snapshot that cannot be trusted leaves the live generation
// exactly as it was rather than deleted with nothing to restore from.
func TestRestoreDrillRefusesAnUnusableSnapshotBeforeMutating(t *testing.T) {
	skipDrillInShortMode(t)
	// One live generation and one real snapshot serve the whole table: every
	// case here is refused before the destructive fixture runs, so no subtest can
	// change what the next one sees — which is itself the property under test.
	stateDir, snapshot, evidence := restorableFixture(t)
	original := func(t *testing.T) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(snapshot, "manifest.json"))
		if err != nil {
			t.Fatalf("read snapshot manifest: %v", err)
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("decode snapshot manifest: %v", err)
		}
		return document
	}

	for name, testCase := range map[string]struct {
		// rewrite returns the manifest bytes to publish in place of the real
		// snapshot, or nil to publish no manifest at all. A directory holding
		// only a manifest is enough, because none of these reach a snapshot file.
		rewrite  func(*testing.T) []byte
		evidence func() Evidence
		identity string
		want     string
	}{
		"no manifest at all": {
			rewrite: func(*testing.T) []byte { return nil },
			want:    "open snapshot manifest",
		},
		"a manifest that is not JSON": {
			rewrite: func(*testing.T) []byte { return []byte("{not json") },
			want:    "decode snapshot manifest",
		},
		"no expected source identity": {
			identity: "   ",
			want:     "expected source identity must not be empty",
		},
		"a snapshot from another application": {
			rewrite: func(t *testing.T) []byte {
				document := original(t)
				source, _ := document["source"].(map[string]any)
				source["application"] = "some-other-agent"
				return mustEncode(t, document)
			},
			want: "build identity is inconsistent",
		},
		"a build identity the parser rejects": {
			rewrite: func(t *testing.T) []byte {
				document := original(t)
				source, _ := document["source"].(map[string]any)
				source["mode"] = "chaos"
				return mustEncode(t, document)
			},
			want: "build identity is invalid",
		},
		"a snapshot missing a required database": {
			rewrite: func(t *testing.T) []byte {
				document := original(t)
				databases, _ := document["databases"].([]any)
				kept := make([]any, 0, len(databases))
				for _, entry := range databases {
					item, _ := entry.(map[string]any)
					if item["filename"] != "memory.db" {
						kept = append(kept, entry)
					}
				}
				document["databases"] = kept
				return mustEncode(t, document)
			},
			want: "missing required persistent database memory.db",
		},
		"evidence with an empty field": {
			evidence: func() Evidence { return Evidence{} },
			want:     "must not be empty",
		},
	} {
		t.Run(name, func(t *testing.T) {
			selected := snapshot
			if testCase.rewrite != nil {
				rewritten := testCase.rewrite(t)
				selected = t.TempDir()
				if rewritten != nil {
					if err := os.WriteFile(filepath.Join(selected, "manifest.json"), rewritten, 0o600); err != nil {
						t.Fatalf("publish rewritten manifest: %v", err)
					}
				}
			}
			identity := testSourceIdentity
			if testCase.identity != "" {
				identity = testCase.identity
			}
			selectedEvidence := evidence
			if testCase.evidence != nil {
				selectedEvidence = testCase.evidence()
			}

			err := RestoreDrill(t.Context(), RestoreOptions{
				Logger: quietLogger(), Snapshot: selected, StateDir: stateDir,
				ExpectedSourceIdentity: identity, Evidence: selectedEvidence,
			})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("RestoreDrill() error = %v, want it to contain %q", err, testCase.want)
			}
			// Nothing was destroyed: the live generation still holds the evidence
			// the destructive fixture would have deleted first.
			count, queryErr := queryCount(t.Context(), filepath.Join(stateDir, "runtime.db"),
				"SELECT COUNT(*) FROM sessions WHERE id = ?", []any{evidence.SessionID}, false)
			if queryErr != nil {
				t.Fatalf("query untouched state: %v", queryErr)
			}
			if count != 1 {
				t.Errorf("session evidence rows after refusal = %d, want 1", count)
			}
		})
	}
}

// TestMutateRefusesEvidenceItCannotFollowOrStateItDoesNotRecognize keeps the one
// destructive function in the package from running on the wrong input. Empty
// evidence would delete nothing and report success; a state directory without
// the production audit-delete guard is not the generation the agent writes, and
// mutating it would destroy something the drill never backed up.
func TestMutateRefusesEvidenceItCannotFollowOrStateItDoesNotRecognize(t *testing.T) {
	skipDrillInShortMode(t)
	if err := Mutate(t.Context(), t.TempDir(), Evidence{}); err == nil ||
		!strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("Mutate() error = %v, want an evidence refusal", err)
	}
	// An empty directory has no audit_log_no_delete trigger, which is exactly
	// what a state directory the agent never wrote looks like.
	err := Mutate(t.Context(), t.TempDir(), fixtureEvidence())
	if err == nil || !strings.Contains(err.Error(), "mutate incidents.db") {
		t.Errorf("Mutate() error = %v, want a refusal naming the unguarded database", err)
	}
}

// TestVerifyRefusesAnIncompleteRestore covers the four independent claims Verify
// makes. Each one alone can pass while the restore is still wrong, which is why
// the drill asserts inventory, bytes, sentinel cleanup and rows separately.
func TestVerifyRefusesAnIncompleteRestore(t *testing.T) {
	skipDrillInShortMode(t)
	for name, testCase := range map[string]struct {
		corrupt  func(*testing.T, string)
		evidence func(Evidence) Evidence
		want     string
	}{
		"a database the manifest never listed": {
			corrupt: func(t *testing.T, stateDir string) {
				if err := os.WriteFile(filepath.Join(stateDir, "stray.db"), []byte("x"), 0o600); err != nil {
					t.Fatalf("plant a stray database: %v", err)
				}
			},
			want: "inventory differs from manifest",
		},
		"restored bytes that do not match the manifest": {
			corrupt: func(t *testing.T, stateDir string) {
				appendByte(t, filepath.Join(stateDir, "runtime.db"))
			},
			want: "hash does not match its manifest",
		},
		"a state directory that is gone": {
			corrupt: func(t *testing.T, stateDir string) {
				if err := os.RemoveAll(stateDir); err != nil {
					t.Fatalf("remove the state directory: %v", err)
				}
			},
			want: "list restored state databases",
		},
		"an evidence row the restore did not recover": {
			evidence: func(evidence Evidence) Evidence {
				evidence.SessionID = "a-session-this-snapshot-never-held"
				return evidence
			},
			want: "restore recovered 0 matching runtime.db evidence rows",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir, snapshot, evidence := restoredFixture(t)
			if testCase.corrupt != nil {
				testCase.corrupt(t, stateDir)
			}
			if testCase.evidence != nil {
				evidence = testCase.evidence(evidence)
			}
			err := Verify(t.Context(), snapshot, stateDir, testSourceIdentity, evidence)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Verify() error = %v, want it to contain %q", err, testCase.want)
			}
		})
	}
}

// TestVerifyRefusesASnapshotTakenFromMutatedState closes the loop the sentinel
// exists for. The sentinel table is planted by the destructive fixture, so its
// presence in a byte-perfect restore means the snapshot itself was taken after
// the mutation — the one failure mode a hash comparison cannot see, because the
// hashes agree.
func TestVerifyRefusesASnapshotTakenFromMutatedState(t *testing.T) {
	skipDrillInShortMode(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("create state fixture: %v", err)
	}
	evidence := fixtureEvidence()
	seedRestoreState(t, stateDir, evidence)
	execFixture(t, filepath.Join(stateDir, "runtime.db"),
		"CREATE TABLE "+sentinelTable+" (value TEXT)", nil)

	snapshot, err := state.BackupState(t.Context(), stateDir, filepath.Join(root, "backups"), state.BackupOptions{
		Logger: quietLogger(), Keep: 1, Timestamp: "20260808T120000Z", Build: testBuild(),
	})
	if err != nil {
		t.Fatalf("BackupState() error = %v", err)
	}
	if _, restoreErr := state.RestoreState(t.Context(), snapshot, stateDir,
		state.RestoreOptions{Logger: quietLogger()}); restoreErr != nil {
		t.Fatalf("RestoreState() error = %v", restoreErr)
	}

	err = Verify(t.Context(), snapshot, stateDir, testSourceIdentity, evidence)
	if err == nil || !strings.Contains(err.Error(), "retained the destructive sentinel") {
		t.Fatalf("Verify() error = %v, want a sentinel refusal", err)
	}
}

// TestEvidenceQueriesSeparateAMissingDatabaseFromAMissingTable pins the one
// place the drill is allowed to read zero instead of failing. Before the
// mutation a database may legitimately not exist yet; a database that exists but
// has lost its table is a broken restore, and reporting zero rows for it would
// let the drill verify emptiness as success.
func TestEvidenceQueriesSeparateAMissingDatabaseFromAMissingTable(t *testing.T) {
	stateDir := t.TempDir()
	absent := filepath.Join(stateDir, "absent.db")
	const query = "SELECT COUNT(*) FROM sessions"

	count, err := queryCount(t.Context(), absent, query, nil, true)
	if err != nil || count != 0 {
		t.Errorf("queryCount(missingIsZero) = (%d, %v), want (0, nil)", count, err)
	}
	if _, err := queryCount(t.Context(), absent, query, nil, false); err == nil {
		t.Error("queryCount() on a missing database returned no error")
	}

	present := filepath.Join(stateDir, "present.db")
	execFixture(t, present, "CREATE TABLE unrelated (value TEXT)", nil)
	if _, err := queryCount(t.Context(), present, query, nil, true); err == nil {
		t.Error("queryCount() on a database without the table returned no error")
	}
}

func TestCLIRejectsUnknownEvidenceFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	reference := domain.Reference()
	payload := map[string]any{
		"audit_action": "restart_service", "audit_invocation_id": "fixture", "audit_target": reference.Services.Inventory,
		"memory_incident_id": reference.Incidents.InventoryDown, "memory_note": "fixture-note", "memory_user_id": "platform-ci",
		"session_id": "session-1", "task_id": "task-1", "prompt": "must not be accepted",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := readEvidence(path); err == nil {
		t.Fatal("readEvidence() accepted an unknown field")
	}
}

// quietLogger keeps `go test` output readable. Backup and restore narrate every
// file they move, and without a logger of their own they would write all of it
// to slog.Default, drowning the assertion that actually failed.
func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// restorableFixture builds a live state directory and one complete snapshot of
// it, which is the situation every restore drill starts from.
func restorableFixture(t *testing.T) (string, string, Evidence) {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("create state fixture: %v", err)
	}
	evidence := fixtureEvidence()
	seedRestoreState(t, stateDir, evidence)
	snapshot, err := state.BackupState(t.Context(), stateDir, filepath.Join(root, "backups"), state.BackupOptions{
		Logger: quietLogger(), Keep: 1, Timestamp: "20260808T120000Z", Build: testBuild(),
	})
	if err != nil {
		t.Fatalf("BackupState() error = %v", err)
	}
	return stateDir, snapshot, evidence
}

// restoredFixture additionally performs the restore, because the manifest
// records the hashes of the snapshot's own vacuumed files: only a state
// directory that has actually been restored is byte-identical to them.
func restoredFixture(t *testing.T) (string, string, Evidence) {
	t.Helper()
	stateDir, snapshot, evidence := restorableFixture(t)
	if _, err := state.RestoreState(t.Context(), snapshot, stateDir,
		state.RestoreOptions{Logger: quietLogger()}); err != nil {
		t.Fatalf("RestoreState() error = %v", err)
	}
	return stateDir, snapshot, evidence
}

func mustEncode(t *testing.T, document map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode rewritten manifest: %v", err)
	}
	return encoded
}

// appendByte changes a restored file's bytes without changing its name, which is
// the shape of a corrupted or partially written restore.
func appendByte(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s for corruption: %v", filepath.Base(path), err)
	}
	if _, err := file.Write([]byte{0}); err != nil {
		_ = file.Close()
		t.Fatalf("corrupt %s: %v", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", filepath.Base(path), err)
	}
}

func fixtureEvidence() Evidence {
	reference := domain.Reference()
	return Evidence{
		AuditAction:      "restart_service",
		AuditInvocation:  "fixture",
		AuditTarget:      reference.Services.Inventory,
		MemoryIncidentID: reference.Incidents.InventoryDown,
		MemoryNote:       "fixture-note",
		MemoryUserID:     "platform-ci",
		SessionID:        "session-1",
		TaskID:           "task-1",
	}
}

func seedA2AState(t *testing.T, stateDir, sessionID, taskID string) {
	t.Helper()
	execFixture(t, filepath.Join(stateDir, "runtime.db"),
		"CREATE TABLE sessions (id TEXT PRIMARY KEY, update_time TEXT NOT NULL)", nil)
	execFixture(t, filepath.Join(stateDir, "runtime.db"),
		"INSERT INTO sessions VALUES (?, ?)", []any{sessionID, "2026-08-08T12:00:00Z"})
	execFixture(t, filepath.Join(stateDir, "tasks.db"),
		"CREATE TABLE tasks (id TEXT PRIMARY KEY, context_id TEXT NOT NULL, last_updated_ns INTEGER NOT NULL)", nil)
	execFixture(t, filepath.Join(stateDir, "tasks.db"),
		"INSERT INTO tasks VALUES (?, ?, ?)", []any{taskID, sessionID, 1})
}

func seedRestoreState(t *testing.T, stateDir string, evidence Evidence) {
	t.Helper()
	execFixture(t, filepath.Join(stateDir, "runtime.db"), "CREATE TABLE sessions (id TEXT PRIMARY KEY)", nil)
	execFixture(t, filepath.Join(stateDir, "runtime.db"), "INSERT INTO sessions VALUES (?)", []any{evidence.SessionID})
	execFixture(t, filepath.Join(stateDir, "tasks.db"), "CREATE TABLE tasks (id TEXT PRIMARY KEY)", nil)
	execFixture(t, filepath.Join(stateDir, "tasks.db"), "INSERT INTO tasks VALUES (?)", []any{evidence.TaskID})
	execFixture(t, filepath.Join(stateDir, "memory.db"),
		"CREATE TABLE incident_notes (user_id TEXT, incident_id TEXT, note TEXT)", nil)
	execFixture(t, filepath.Join(stateDir, "memory.db"), "INSERT INTO incident_notes VALUES (?, ?, ?)",
		[]any{evidence.MemoryUserID, evidence.MemoryIncidentID, evidence.MemoryNote})
	execFixture(t, filepath.Join(stateDir, "incidents.db"),
		"CREATE TABLE audit_log (invocation_id TEXT, action TEXT, target TEXT)", nil)
	execFixture(t, filepath.Join(stateDir, "incidents.db"),
		"CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log BEGIN SELECT RAISE(ABORT, 'no'); END", nil)
	execFixture(t, filepath.Join(stateDir, "incidents.db"), "INSERT INTO audit_log VALUES (?, ?, ?)",
		[]any{evidence.AuditInvocation, evidence.AuditAction, evidence.AuditTarget})
}

func execFixture(t *testing.T, path, query string, args []any) {
	t.Helper()
	db, err := sql.Open(sqliteDriver, "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		_ = db.Close()
		t.Fatalf("execute %s fixture: %v", filepath.Base(path), err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", filepath.Base(path), err)
	}
}
