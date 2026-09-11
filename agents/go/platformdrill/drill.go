// Package platformdrill exercises the Kubernetes state backup and restore
// contract with evidence created through the same Go boundaries as production.
//
// The drill is intentionally destructive only after every writer has been
// stopped by the platform script. It removes four exact evidence rows, marks
// every live database, restores the selected snapshot through package state,
// and then proves provenance, byte identity, SQLite integrity, inventory, and
// row recovery.
package platformdrill

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/memory"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/state"
)

const (
	applicationName = "agentops-agent"
	operatorUser    = "platform-ci"
	sentinelTable   = "ci_restore_sentinel"
	sqliteDriver    = "sqlite"
)

var requiredDatabases = []string{"incidents.db", "memory.db", "runtime.db", "tasks.db"}

// Evidence identifies the four durable rows followed across a restore.
type Evidence struct {
	AuditAction      string `json:"audit_action"`
	AuditInvocation  string `json:"audit_invocation_id"`
	AuditTarget      string `json:"audit_target"`
	MemoryIncidentID string `json:"memory_incident_id"`
	MemoryNote       string `json:"memory_note"`
	MemoryUserID     string `json:"memory_user_id"`
	SessionID        string `json:"session_id"`
	TaskID           string `json:"task_id"`
}

// SeedOptions names the runtime and immutable data boundaries used by Seed.
type SeedOptions struct {
	StateDir string
	DataDir  string
	Marker   string
}

// Seed persists one note and one audited mock action linked to the newest A2A
// session and task. Repeating the same marker is idempotent.
func Seed(ctx context.Context, opts SeedOptions) (Evidence, error) {
	reference := domain.Reference()
	incidentID := reference.Incidents.InventoryDown
	serviceName := reference.Services.Inventory
	marker := strings.TrimSpace(opts.Marker)
	if marker == "" {
		return Evidence{}, errors.New("backup evidence marker must not be empty")
	}
	if opts.StateDir == "" || opts.DataDir == "" {
		return Evidence{}, errors.New("state and data directories are required")
	}
	if err := state.RecoverInterruptedRestore(opts.StateDir, state.RecoverOptions{}); err != nil {
		return Evidence{}, fmt.Errorf("recover runtime state before seeding backup evidence: %w", err)
	}

	sessionID, taskID, err := latestA2AEvidence(ctx, opts.StateDir)
	if err != nil {
		return Evidence{}, err
	}

	store := data.New(data.Config{DataDir: opts.DataDir, StateDir: opts.StateDir})
	governance, err := policy.New(policy.Config{})
	if err != nil {
		return Evidence{}, fmt.Errorf("build the persisted-data redactor: %w", err)
	}
	memories, err := memory.New(memory.Config{
		Store: store,
		Guard: func(callCtx context.Context, _ string, call func(context.Context) error) error {
			return call(callCtx)
		},
		Redact:           governance.PersistedRedactor(ctx),
		StateDir:         opts.StateDir,
		EmbeddingTimeout: time.Second,
	})
	if err != nil {
		return Evidence{}, fmt.Errorf("build the memory boundary: %w", err)
	}

	note := marker + "-long-term-note"
	present, err := noteCount(ctx, filepath.Join(opts.StateDir, "memory.db"), operatorUser, incidentID, note, true)
	if err != nil {
		return Evidence{}, err
	}
	if present == 0 {
		result, saveErr := memories.SaveOperatorIncidentNote(ctx, applicationName, operatorUser,
			memory.SaveIncidentNoteArgs{IncidentID: incidentID, Note: note})
		if saveErr != nil {
			return Evidence{}, fmt.Errorf("seed long-term evidence: %w", saveErr)
		}
		if result.Saved == nil {
			return Evidence{}, fmt.Errorf("seed long-term evidence: %s", result.Error)
		}
	} else if present != 1 {
		return Evidence{}, fmt.Errorf("long-term evidence is ambiguous: found %d matching rows", present)
	}

	slug, err := domain.ParseSlug(serviceName)
	if err != nil {
		return Evidence{}, fmt.Errorf("parse the backup fixture service: %w", err)
	}
	audit, err := store.RestartServiceWithAudit(ctx, slug, data.AuditIdentity{
		Actor:        applicationName,
		ApprovedBy:   operatorUser,
		Rationale:    "Automated backup fixture for the disposable local platform; the target is a mock service.",
		SessionID:    sessionID,
		InvocationID: marker,
	})
	if err != nil {
		return Evidence{}, fmt.Errorf("seed audited action evidence: %w", err)
	}
	if audit == nil {
		return Evidence{}, errors.New("seed audited action evidence: fixture service was not found")
	}

	evidence := Evidence{
		AuditAction:      audit.Action(),
		AuditInvocation:  marker,
		AuditTarget:      audit.Target(),
		MemoryIncidentID: incidentID,
		MemoryNote:       note,
		MemoryUserID:     operatorUser,
		SessionID:        sessionID,
		TaskID:           taskID,
	}
	if err := validateEvidence(evidence); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func latestA2AEvidence(ctx context.Context, stateDir string) (string, string, error) {
	runtimeDB, err := openDatabase(filepath.Join(stateDir, "runtime.db"), true)
	if err != nil {
		return "", "", fmt.Errorf("open persisted ADK sessions: %w", err)
	}
	defer func() { _ = runtimeDB.Close() }()
	var sessionID string
	if queryErr := runtimeDB.QueryRowContext(ctx,
		`SELECT id FROM sessions ORDER BY update_time DESC, id DESC LIMIT 1`).Scan(&sessionID); queryErr != nil {
		return "", "", fmt.Errorf("deterministic A2A traffic did not persist a session: %w", queryErr)
	}

	tasksDB, err := openDatabase(filepath.Join(stateDir, "tasks.db"), true)
	if err != nil {
		return "", "", fmt.Errorf("open persisted A2A tasks: %w", err)
	}
	defer func() { _ = tasksDB.Close() }()
	var taskID string
	if err := tasksDB.QueryRowContext(ctx,
		`SELECT id FROM tasks WHERE context_id = ? ORDER BY last_updated_ns DESC, id DESC LIMIT 1`,
		sessionID,
	).Scan(&taskID); err != nil {
		return "", "", fmt.Errorf("deterministic A2A traffic did not persist a task for session %s: %w", sessionID, err)
	}
	return sessionID, taskID, nil
}

// RestoreOptions names one complete snapshot and the stopped live generation.
type RestoreOptions struct {
	Logger                 *slog.Logger
	Snapshot               string
	StateDir               string
	ExpectedSourceIdentity string
	Evidence               Evidence
}

// RestoreDrill corrupts only the named live generation, restores the snapshot,
// and verifies the recovered bytes and evidence.
func RestoreDrill(ctx context.Context, opts RestoreOptions) error {
	manifest, err := readManifest(opts.Snapshot)
	if err != nil {
		return err
	}
	if err := validateManifest(manifest, opts.ExpectedSourceIdentity); err != nil {
		return err
	}
	if err := validateEvidence(opts.Evidence); err != nil {
		return err
	}
	if err := state.RecoverInterruptedRestore(opts.StateDir, state.RecoverOptions{Logger: opts.Logger}); err != nil {
		return fmt.Errorf("recover runtime state before the destructive drill: %w", err)
	}
	if err := Mutate(ctx, opts.StateDir, opts.Evidence); err != nil {
		return err
	}
	if _, err := state.RestoreState(ctx, opts.Snapshot, opts.StateDir, state.RestoreOptions{Logger: opts.Logger}); err != nil {
		return fmt.Errorf("restore the selected snapshot: %w", err)
	}
	return Verify(ctx, opts.Snapshot, opts.StateDir, opts.ExpectedSourceIdentity, opts.Evidence)
}

// Mutate removes the evidence and adds one sentinel to every live database.
// Callers must stop every writer before reaching this function.
func Mutate(ctx context.Context, stateDir string, evidence Evidence) error {
	if err := validateEvidence(evidence); err != nil {
		return err
	}
	mutations := map[string][]statement{
		"runtime.db": {{query: "DELETE FROM sessions WHERE id = ?", args: []any{evidence.SessionID}}},
		"tasks.db":   {{query: "DELETE FROM tasks WHERE id = ?", args: []any{evidence.TaskID}}},
		"memory.db": {{
			query: "DELETE FROM incident_notes WHERE user_id = ? AND incident_id = ? AND note = ?",
			args:  []any{evidence.MemoryUserID, evidence.MemoryIncidentID, evidence.MemoryNote},
		}},
		"incidents.db": {
			{query: "DROP TRIGGER audit_log_no_delete"},
			{
				query: "DELETE FROM audit_log WHERE invocation_id = ? AND action = ? AND target = ?",
				args:  []any{evidence.AuditInvocation, evidence.AuditAction, evidence.AuditTarget},
			},
		},
	}
	for _, name := range requiredDatabases {
		path := filepath.Join(stateDir, name)
		if err := mutateDatabase(ctx, path, mutations[name]); err != nil {
			return fmt.Errorf("mutate %s: %w", name, err)
		}
	}
	obsolete := filepath.Join(stateDir, "obsolete.db")
	if err := mutateDatabase(ctx, obsolete, nil); err != nil {
		return fmt.Errorf("create the obsolete restore sentinel: %w", err)
	}
	for _, check := range evidenceChecks(evidence) {
		count, err := queryCount(ctx, filepath.Join(stateDir, check.database), check.query, check.args, true)
		if err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("destructive fixture did not remove %s evidence", check.database)
		}
	}
	return nil
}

type statement struct {
	query string
	args  []any
}

func mutateDatabase(ctx context.Context, path string, statements []statement) (err error) {
	db, err := openDatabase(path, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mutation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	for _, item := range statements {
		if _, err := tx.ExecContext(ctx, item.query, item.args...); err != nil {
			return fmt.Errorf("execute destructive fixture statement: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "CREATE TABLE "+sentinelTable+" (value TEXT)"); err != nil {
		return fmt.Errorf("create restore sentinel: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+sentinelTable+" VALUES ('destroy me')"); err != nil {
		return fmt.Errorf("populate restore sentinel: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit destructive fixture: %w", err)
	}
	committed = true
	return nil
}

type manifestDocument struct {
	Source struct {
		Application    string `json:"application"`
		BuildTimestamp string `json:"build_timestamp"`
		Commit         string `json:"commit"`
		Mode           string `json:"mode"`
		Revision       string `json:"revision"`
		SourceIdentity string `json:"source_identity"`
		TreeDigest     string `json:"tree_digest"`
		Version        string `json:"version"`
		Dirty          bool   `json:"dirty"`
	} `json:"source"`
	Databases []manifestDatabase `json:"databases"`
}

type manifestDatabase struct {
	SQLite    any    `json:"sqlite"`
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

func readManifest(snapshot string) (manifestDocument, error) {
	path := filepath.Join(snapshot, "manifest.json")
	file, err := os.Open(path)
	if err != nil {
		return manifestDocument{}, fmt.Errorf("open snapshot manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	var manifest manifestDocument
	if err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&manifest); err != nil {
		return manifestDocument{}, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	return manifest, nil
}

func validateManifest(manifest manifestDocument, expectedIdentity string) error {
	if strings.TrimSpace(expectedIdentity) == "" {
		return errors.New("expected source identity must not be empty")
	}
	if manifest.Source.SourceIdentity != expectedIdentity {
		return fmt.Errorf("restored snapshot source identity %q does not match workflow identity %q",
			manifest.Source.SourceIdentity, expectedIdentity)
	}
	if manifest.Source.Application != applicationName || manifest.Source.Commit != manifest.Source.SourceIdentity {
		return errors.New("snapshot build identity is inconsistent with its compatibility projection")
	}
	if _, err := buildinfo.Parse(buildinfo.Raw{
		Mode:           manifest.Source.Mode,
		Version:        manifest.Source.Version,
		SourceIdentity: manifest.Source.SourceIdentity,
		Revision:       manifest.Source.Revision,
		TreeDigest:     manifest.Source.TreeDigest,
		Timestamp:      manifest.Source.BuildTimestamp,
		Dirty:          strconv.FormatBool(manifest.Source.Dirty),
	}); err != nil {
		return fmt.Errorf("snapshot build identity is invalid: %w", err)
	}
	present := make([]string, 0, len(manifest.Databases))
	for _, item := range manifest.Databases {
		present = append(present, item.Filename)
	}
	for _, required := range requiredDatabases {
		if !slices.Contains(present, required) {
			return fmt.Errorf("snapshot is missing required persistent database %s", required)
		}
	}
	return nil
}

// Verify proves provenance, inventory, hashes, SQLite integrity, sentinel
// cleanup, and the four exact evidence rows.
func Verify(ctx context.Context, snapshot, stateDir, expectedIdentity string, evidence Evidence) error {
	manifest, err := readManifest(snapshot)
	if err != nil {
		return err
	}
	if manifestErr := validateManifest(manifest, expectedIdentity); manifestErr != nil {
		return manifestErr
	}
	expected := make(map[string]manifestDatabase, len(manifest.Databases))
	for _, item := range manifest.Databases {
		expected[item.Filename] = item
	}
	actual, err := databaseInventory(stateDir)
	if err != nil {
		return err
	}
	want := make([]string, 0, len(expected))
	for name := range expected {
		want = append(want, name)
	}
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		return fmt.Errorf("restored state inventory differs from manifest: got %v, want %v", actual, want)
	}
	for _, name := range want {
		path := filepath.Join(stateDir, name)
		digest, err := hashFile(path)
		if err != nil {
			return err
		}
		if digest != expected[name].SHA256 {
			return fmt.Errorf("restored %s hash does not match its manifest", name)
		}
		db, err := openDatabase(path, true)
		if err != nil {
			return err
		}
		var integrity string
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
			_ = db.Close()
			return fmt.Errorf("check restored %s integrity: %w", name, err)
		}
		var sentinels int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", sentinelTable,
		).Scan(&sentinels); err != nil {
			_ = db.Close()
			return fmt.Errorf("inspect restored %s tables: %w", name, err)
		}
		if err := db.Close(); err != nil {
			return fmt.Errorf("close restored %s: %w", name, err)
		}
		if integrity != "ok" {
			return fmt.Errorf("restored %s failed SQLite integrity_check: %s", name, integrity)
		}
		if sentinels != 0 {
			return fmt.Errorf("restore retained the destructive sentinel in %s", name)
		}
	}
	for _, check := range evidenceChecks(evidence) {
		count, err := queryCount(ctx, filepath.Join(stateDir, check.database), check.query, check.args, true)
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("restore recovered %d matching %s evidence rows, want 1", count, check.database)
		}
	}
	return nil
}

type evidenceCheck struct {
	database string
	query    string
	args     []any
}

func evidenceChecks(evidence Evidence) []evidenceCheck {
	return []evidenceCheck{
		{database: "runtime.db", query: "SELECT COUNT(*) FROM sessions WHERE id = ?", args: []any{evidence.SessionID}},
		{database: "tasks.db", query: "SELECT COUNT(*) FROM tasks WHERE id = ?", args: []any{evidence.TaskID}},
		{
			database: "memory.db",
			query:    "SELECT COUNT(*) FROM incident_notes WHERE user_id = ? AND incident_id = ? AND note = ?",
			args:     []any{evidence.MemoryUserID, evidence.MemoryIncidentID, evidence.MemoryNote},
		},
		{
			database: "incidents.db",
			query:    "SELECT COUNT(*) FROM audit_log WHERE invocation_id = ? AND action = ? AND target = ?",
			args:     []any{evidence.AuditInvocation, evidence.AuditAction, evidence.AuditTarget},
		},
	}
}

func validateEvidence(evidence Evidence) error {
	values := map[string]string{
		"audit_action": evidence.AuditAction, "audit_invocation_id": evidence.AuditInvocation,
		"audit_target": evidence.AuditTarget, "memory_incident_id": evidence.MemoryIncidentID,
		"memory_note": evidence.MemoryNote, "memory_user_id": evidence.MemoryUserID,
		"session_id": evidence.SessionID, "task_id": evidence.TaskID,
	}
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("backup evidence field %s must not be empty", name)
		}
	}
	return nil
}

func noteCount(
	ctx context.Context, path, userID, incident, note string, missingIsZero bool,
) (int, error) {
	return queryCount(ctx, path,
		"SELECT COUNT(*) FROM incident_notes WHERE user_id = ? AND incident_id = ? AND note = ?",
		[]any{userID, incident, note}, missingIsZero)
}

func queryCount(ctx context.Context, path, query string, args []any, missingIsZero bool) (int, error) {
	if missingIsZero {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
	}
	db, err := openDatabase(path, true)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("query evidence in %s: %w", filepath.Base(path), err)
	}
	return count, nil
}

func openDatabase(path string, readOnly bool) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	parameters := "_pragma=busy_timeout(5000)&_txlock=immediate"
	if readOnly {
		parameters = "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open(sqliteDriver, uri.String()+"?"+parameters)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to %s: %w", filepath.Base(path), err), db.Close())
	}
	return db, nil
}

func databaseInventory(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("list restored state databases: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".db") {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

func hashFile(path string) (digest string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for hashing: %w", path, err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
