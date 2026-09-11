package platformdrill

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/state"
)

// seedDataDir is the immutable seed data the agent ships beside the module. The
// drill reads incidents and services from it exactly as the running agent does,
// so the CLI tests point at the same directory the platform workflow passes.
func seedDataDir() string {
	return filepath.Clean(filepath.Join("..", "..", "data"))
}

// writeEvidenceFile stores an evidence document the way `seed` publishes it and
// `restore-drill` consumes it: one JSON file handed between two workflow steps.
func writeEvidenceFile(t *testing.T, directory string, evidence Evidence) string {
	t.Helper()
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("encode evidence fixture: %v", err)
	}
	path := filepath.Join(directory, "evidence.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write evidence fixture: %v", err)
	}
	return path
}

// run invokes the subcommand the way cmd/agent does and returns both streams.
func run(t *testing.T, arguments ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(t.Context(), arguments, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestCLISeedPublishesTheEvidenceItActuallyPersisted is the contract between the
// two workflow steps: the JSON on stdout is the only thing the restore step
// receives, so every row it names has to exist in the state directory. A
// document that named a row the seeder never wrote would make the drill verify
// nothing while still reporting success.
func TestCLISeedPublishesTheEvidenceItActuallyPersisted(t *testing.T) {
	skipDrillInShortMode(t)
	stateDir := t.TempDir()
	// Distinct identifiers, so the assertion below proves the command read them
	// out of the persisted A2A state rather than assuming the fixture's usual.
	seedA2AState(t, stateDir, "session-cli", "task-cli")

	code, out, errOut := run(t, "seed",
		"-state-dir", stateDir, "-data-dir", seedDataDir(), "-marker", "cli-fixture")
	if code != 0 {
		t.Fatalf("Main() = %d, stderr = %q, want 0", code, errOut)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing on success", errOut)
	}

	var evidence Evidence
	if err := json.Unmarshal([]byte(out), &evidence); err != nil {
		t.Fatalf("stdout = %q, want one JSON evidence document: %v", out, err)
	}
	if evidence.AuditInvocation != "cli-fixture" {
		t.Errorf("audit invocation = %q, want the marker the operator passed", evidence.AuditInvocation)
	}
	if evidence.SessionID != "session-cli" || evidence.TaskID != "task-cli" {
		t.Errorf("evidence = %+v, want the newest persisted A2A session and task", evidence)
	}
	for _, check := range evidenceChecks(evidence) {
		count, err := queryCount(t.Context(), filepath.Join(stateDir, check.database), check.query, check.args, false)
		if err != nil {
			t.Fatalf("query published %s evidence: %v", check.database, err)
		}
		if count != 1 {
			t.Errorf("published %s evidence rows = %d, want 1", check.database, count)
		}
	}
}

// TestCLIRestoreDrillVerifiesARealSnapshot runs the destructive half end to end
// through the same entry point the platform workflow calls. It also pins the
// progress output: the drill's log is a CI artifact, so timestamps and levels
// are stripped to keep two runs of the same drill byte-comparable.
func TestCLIRestoreDrillVerifiesARealSnapshot(t *testing.T) {
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

	code, out, errOut := run(t, "restore-drill",
		"-snapshot", snapshot, "-state-dir", stateDir,
		"-evidence", writeEvidenceFile(t, root, evidence),
		"-expected-source-identity", testSourceIdentity)
	if code != 0 {
		t.Fatalf("Main() = %d, stderr = %q, want 0", code, errOut)
	}
	if !strings.Contains(out, "platform backup restore drill verified") {
		t.Errorf("stdout = %q, want the verification line", out)
	}
	if !strings.Contains(out, "restore complete") {
		t.Errorf("stdout = %q, want the restore progress log", out)
	}
	for _, noisy := range []string{"time=", "level="} {
		if strings.Contains(out, noisy) {
			t.Errorf("stdout = %q, want %q stripped so two runs stay comparable", out, noisy)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "obsolete.db")); !os.IsNotExist(err) {
		t.Errorf("obsolete.db survived the drill, stat error = %v", err)
	}
}

// TestCLIReportsFailuresOnStderrWithANonZeroStatus keeps the workflow honest.
// Every one of these is a way the platform job can be invoked wrongly, and each
// has to stop the drill with an exit status a CI step fails on — a drill that
// exits zero after doing nothing is worse than no drill.
func TestCLIReportsFailuresOnStderrWithANonZeroStatus(t *testing.T) {
	root := t.TempDir()
	stateDir := t.TempDir()
	seedA2AState(t, stateDir, "session-1", "task-1")

	incomplete := fixtureEvidence()
	incomplete.TaskID = "   "
	incompletePath := writeEvidenceFile(t, t.TempDir(), incomplete)

	malformedPath := filepath.Join(root, "malformed.json")
	if err := os.WriteFile(malformedPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed evidence: %v", err)
	}

	for name, testCase := range map[string]struct {
		want      string
		arguments []string
	}{
		"no subcommand":              {arguments: nil, want: "expected seed or restore-drill"},
		"unknown subcommand":         {arguments: []string{"verify"}, want: `unknown subcommand "verify"`},
		"undefined seed flag":        {arguments: []string{"seed", "-nope"}, want: "not defined"},
		"positional after seed":      {arguments: []string{"seed", "-marker", "m", "extra"}, want: `unexpected argument "extra"`},
		"undefined restore flag":     {arguments: []string{"restore-drill", "-nope"}, want: "not defined"},
		"positional after restore":   {arguments: []string{"restore-drill", "extra"}, want: `unexpected argument "extra"`},
		"seed without a marker":      {arguments: []string{"seed", "-state-dir", stateDir, "-data-dir", seedDataDir()}, want: "marker must not be empty"},
		"restore without evidence":   {arguments: []string{"restore-drill", "-evidence", filepath.Join(root, "absent.json")}, want: "open backup evidence"},
		"malformed evidence file":    {arguments: []string{"restore-drill", "-evidence", malformedPath}, want: "decode backup evidence"},
		"evidence missing a field":   {arguments: []string{"restore-drill", "-evidence", incompletePath}, want: "task_id must not be empty"},
		"restore without a snapshot": {arguments: []string{"restore-drill", "-evidence", writeEvidenceFile(t, root, fixtureEvidence()), "-expected-source-identity", testSourceIdentity}, want: "open snapshot manifest"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := run(t, testCase.arguments...)
			if code != 1 {
				t.Errorf("Main() = %d, stderr = %q, want 1", code, errOut)
			}
			if !strings.Contains(errOut, testCase.want) {
				t.Errorf("stderr = %q, want it to contain %q", errOut, testCase.want)
			}
			if !strings.HasPrefix(errOut, "error: ") {
				t.Errorf("stderr = %q, want the failure prefixed for a CI log", errOut)
			}
		})
	}
}
