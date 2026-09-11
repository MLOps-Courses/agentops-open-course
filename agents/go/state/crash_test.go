package state

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The crash suite kills real processes at real seams.
//
// A journal that is only ever exercised in-process proves nothing about power
// loss: the whole point of fsyncing a directory before a rename is that the
// process may not survive to the next line. Each scenario below re-executes
// this package's own test binary, replaces the rename seam with one that calls
// os.Exit at a named point, and then asserts what the *parent* can recover from
// what the dead child left on disk. The Python suite does the same with
// os._exit and a monkeypatched Path.replace.
//
// The child takes its parameters as ordinary test-binary flags rather than
// environment variables, which keeps every input to a scenario visible in the
// command line the parent built.
var (
	crashScenario = flag.String("crash.scenario", "", "internal: the crash scenario a re-executed child runs")
	crashSnapshot = flag.String("crash.snapshot", "", "internal: the snapshot a crash child restores from")
	crashStateDir = flag.String("crash.state-dir", "", "internal: the state directory a crash child restores into")
	crashMarker   = flag.String("crash.marker", "", "internal: the file a crash child touches once it holds the lock")
	crashRelease  = flag.String("crash.release", "", "internal: the file whose arrival releases a crash child")
)

// Scenario names and the exit code each one leaves behind. Distinct codes make
// a failed assertion say which seam actually fired.
const (
	scenarioAfterQuarantine   = "after-quarantine"
	scenarioBeforeCleanup     = "before-cleanup"
	scenarioAtInitialJournal  = "at-initial-journal"
	scenarioAtCommittedPhase  = "at-committed-phase"
	scenarioAtRollingBack     = "at-rolling-back-phase"
	scenarioHoldLock          = "hold-lock"
	scenarioRestore           = "restore"
	exitAfterQuarantine       = 71
	exitBeforeCleanup         = 72
	exitAtCommittedPhase      = 74
	exitAtRollingBack         = 75
	exitAtInitialJournal      = 76
	exitChildReportedFailure  = 1
	childCompletionTimeout    = 30 * time.Second
	childBlockedObservation   = 300 * time.Millisecond
	injectedPublicationFailed = "injected restore publication failure after 1 database"
)

// TestMain turns this test binary into its own crash fixture. With the scenario
// variable set it never runs a test: it performs the restore under test and
// dies at the requested seam.
func TestMain(m *testing.M) {
	flag.Parse()
	if *crashScenario != "" {
		runCrashChild(*crashScenario)
	}
	os.Exit(m.Run())
}

// runCrashChild performs one scenario and never returns.
func runCrashChild(scenario string) {
	snapshot, stateDir := *crashSnapshot, *crashStateDir
	original := renameFile
	options := RestoreOptions{Logger: quietLogger()}

	switch scenario {
	case scenarioAfterQuarantine:
		// The old generation's first file has moved and both directories are
		// fsynced; the journal is durable and says "prepared".
		renameFile = func(oldpath, newpath string) error {
			err := original(oldpath, newpath)
			if err == nil && strings.HasPrefix(filepath.Base(filepath.Dir(newpath)), quarantinePrefix) {
				os.Exit(exitAfterQuarantine)
			}
			return err
		}
	case scenarioBeforeCleanup:
		// The committed journal is durable and the new generation is live, but
		// the transaction's residue has not been removed yet.
		renameFile = func(oldpath, newpath string) error {
			phase := journalPhaseBeingPublished(oldpath, newpath)
			err := original(oldpath, newpath)
			if err == nil && phase == phaseCommitted {
				os.Exit(exitBeforeCleanup)
			}
			return err
		}
	case scenarioAtInitialJournal:
		// The initial journal's temporary is fsynced and the state directory is
		// fsynced, but the rename to the durable name never happened.
		renameFile = crashBeforeJournalPhase(original, phasePrepared, exitAtInitialJournal)
	case scenarioAtCommittedPhase:
		renameFile = crashBeforeJournalPhase(original, phaseCommitted, exitAtCommittedPhase)
	case scenarioAtRollingBack:
		renameFile = crashBeforeJournalPhase(original, phaseRollingBack, exitAtRollingBack)
		options.BeforePublish = func(installed int) error {
			if installed >= 1 {
				return errors.New(injectedPublicationFailed)
			}
			return nil
		}
	case scenarioHoldLock:
		holdGenerationLock(stateDir)
		os.Exit(0)
	case scenarioRestore:
		// No injection: this child proves a second process blocks on the lock.
	default:
		fmt.Fprintf(os.Stderr, "unknown crash scenario %q\n", scenario)
		os.Exit(exitChildReportedFailure)
	}

	if _, err := RestoreState(context.Background(), snapshot, stateDir, options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitChildReportedFailure)
	}
	os.Exit(0)
}

// crashBeforeJournalPhase dies just before a journal recording the given phase
// becomes durable under its permanent name.
func crashBeforeJournalPhase(original func(string, string) error, phase restorePhase, code int) func(string, string) error {
	return func(oldpath, newpath string) error {
		if journalPhaseBeingPublished(oldpath, newpath) == phase {
			os.Exit(code)
		}
		return original(oldpath, newpath)
	}
}

// journalPhaseBeingPublished reports the phase a rename is about to publish, or
// the empty phase when the rename is not a journal publication.
func journalPhaseBeingPublished(oldpath, newpath string) restorePhase {
	if filepath.Base(newpath) != restoreJournalName || !strings.HasPrefix(filepath.Base(oldpath), journalPrefix) {
		return ""
	}
	raw, err := os.ReadFile(oldpath)
	if err != nil {
		return ""
	}
	var document struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return ""
	}
	return restorePhase(document.Phase)
}

// holdGenerationLock takes the lock, announces it, and holds it until released.
func holdGenerationLock(stateDir string) {
	marker, release := *crashMarker, *crashRelease
	err := withRestoreLock(filepath.Join(stateDir, restoreLockName), func() error {
		if err := os.WriteFile(marker, []byte("locked"), 0o600); err != nil {
			return err
		}
		for {
			if _, err := os.Stat(release); err == nil {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitChildReportedFailure)
	}
}

// childCommand builds a re-execution of this test binary in a crash scenario.
func childCommand(t *testing.T, scenario, snapshot, stateDir string, extra ...string) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	arguments := append([]string{
		"-crash.scenario=" + scenario,
		"-crash.snapshot=" + snapshot,
		"-crash.state-dir=" + stateDir,
	}, extra...)
	return exec.Command(binary, arguments...)
}

// skipCrashSuiteInShortMode drops one scenario from `go test -short`.
//
// Every scenario here forks, reaps, and waits on a real process, which is the
// slowest thing this package does and is worth paying for only when the restore
// path changed. The gate is opt-in and deliberately one-way: agents/go/mise.toml's
// test task passes no -short, so the repository's own gate and CI still run all
// six. It exists for the learner's inner loop, not for the suite's meaning.
func skipCrashSuiteInShortMode(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("this scenario forks a real process; run without -short to exercise it")
	}
}

// runChild runs a crash scenario to completion and returns its exit code.
func runChild(t *testing.T, scenario, snapshot, stateDir string) int {
	t.Helper()
	command := childCommand(t, scenario, snapshot, stateDir)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run the %s child: %v\n%s", scenario, err, output)
	}
	if command.ProcessState == nil {
		t.Fatalf("the %s child never ran\n%s", scenario, output)
	}
	if code := command.ProcessState.ExitCode(); code != 0 {
		t.Logf("%s child exited %d: %s", scenario, code, output)
	}
	return command.ProcessState.ExitCode()
}

func TestRestoreRecoversByteIdenticalOldGenerationAfterProcessExitOnFirstReplacement(t *testing.T) {
	skipCrashSuiteInShortMode(t)

	stateDir, _, snapshot := snapshotFixture(t)
	// Divergence after the snapshot is what makes the assertion meaningful: the
	// old generation must come back exactly, not be replaced by the snapshot.
	execSQL(t, filepath.Join(stateDir, "runtime.db"), "INSERT INTO sessions VALUES ('old-after-snapshot')")
	execSQL(t, filepath.Join(stateDir, "obsolete.db"),
		"CREATE TABLE obsolete (value TEXT)",
		"INSERT INTO obsolete VALUES ('old generation')",
	)
	if err := os.WriteFile(filepath.Join(stateDir, "incidents.db-journal"), []byte("old sidecar evidence"), 0o600); err != nil {
		t.Fatalf("write the sidecar fixture: %v", err)
	}
	before := generationFingerprints(t, stateDir)

	if code := runChild(t, scenarioAfterQuarantine, snapshot, stateDir); code != exitAfterQuarantine {
		t.Fatalf("child exit code = %d, want %d", code, exitAfterQuarantine)
	}
	if !isRegularFile(journalPath(stateDir)) {
		t.Fatal("the durable restore journal did not survive the crash")
	}

	if err := RecoverInterruptedRestore(stateDir, recoverOptions()); err != nil {
		t.Fatalf("RecoverInterruptedRestore() error = %v, want nil", err)
	}

	assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestorePreservesCommittedNewGenerationAfterProcessExitBeforeCleanup(t *testing.T) {
	skipCrashSuiteInShortMode(t)

	stateDir, _, snapshot := snapshotFixture(t)
	expected := fingerprints(t, snapshot)
	execSQL(t, filepath.Join(stateDir, "runtime.db"), "INSERT INTO sessions VALUES ('old-after-snapshot')")

	if code := runChild(t, scenarioBeforeCleanup, snapshot, stateDir); code != exitBeforeCleanup {
		t.Fatalf("child exit code = %d, want %d", code, exitBeforeCleanup)
	}
	journal := readJSONFixture(t, journalPath(stateDir))
	if journal["phase"] != string(phaseCommitted) {
		t.Fatalf("journal phase = %v, want %q", journal["phase"], phaseCommitted)
	}

	// Recovery through the *backup* path, exactly as the Python suite does it:
	// a snapshot taken after a crashed restore must roll the commit forward and
	// then snapshot the generation that actually won.
	recoveryBackups := filepath.Join(t.TempDir(), "recovery-backups")
	options := backupOptions()
	options.Timestamp = "20990101T000001Z"
	published, err := BackupState(t.Context(), stateDir, recoveryBackups, options)
	if err != nil {
		t.Fatalf("BackupState() after a committed crash: %v", err)
	}

	assertSameFingerprints(t, expected, fingerprints(t, stateDir))
	// The re-snapshot is compared by content rather than by bytes. VACUUM INTO
	// bumps the schema cookie in the header on every copy, so a copy of a copy
	// is logically identical and one byte different — the one place the Go port
	// cannot reproduce Python's backup API byte for byte. What the assertion
	// still has to prove is which generation won, and that is a row question:
	// the divergence written after the snapshot must be gone.
	if _, err := validateInventory(t.Context(), published); err != nil {
		t.Errorf("the recovery snapshot does not validate: %v", err)
	}
	if got, want := sortedKeys(fingerprints(t, published)), sortedKeys(expected); !equalNames(got, want) {
		t.Errorf("recovery snapshot holds %v, want %v", got, want)
	}
	assertSessions(t, filepath.Join(published, "runtime.db"), "session-1")
	assertSessions(t, filepath.Join(stateDir, "runtime.db"), "session-1")
	assertNoRestoreResidue(t, stateDir)
}

// assertSessions fails unless a session store holds exactly these rows. It is
// how a test says "the snapshot's generation won" rather than "these bytes
// match", which is the claim that actually matters after a rolled-forward
// commit.
func assertSessions(t *testing.T, path string, want ...string) {
	t.Helper()
	db := openFixture(t, path)
	rows, err := db.QueryContext(t.Context(), "SELECT id FROM sessions ORDER BY id")
	if err != nil {
		t.Fatalf("read the sessions in %s: %v", path, err)
	}
	defer func() { _ = rows.Close() }()
	var found []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan the sessions in %s: %v", path, err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the sessions in %s: %v", path, err)
	}
	if !equalNames(found, want) {
		t.Errorf("%s holds sessions %v, want %v", filepath.Base(path), found, want)
	}
}

// equalNames compares two name lists element by element.
func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestRestoreAdoptsADurableInitialJournalAndRecoversAfterProcessExit(t *testing.T) {
	skipCrashSuiteInShortMode(t)

	stateDir, _, snapshot := snapshotFixture(t)
	execSQL(t, filepath.Join(stateDir, "runtime.db"), "INSERT INTO sessions VALUES ('old-after-snapshot')")
	before := generationFingerprints(t, stateDir)

	if code := runChild(t, scenarioAtInitialJournal, snapshot, stateDir); code != exitAtInitialJournal {
		t.Fatalf("child exit code = %d, want %d", code, exitAtInitialJournal)
	}
	if _, err := os.Lstat(journalPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a durable journal exists after a crash at the initial rename: %v", err)
	}
	// The pre-rename directory fsync is what makes this the only recoverable
	// record of the transaction. Without it, this file might not exist at all.
	if temporaries := journalTemporaries(t, stateDir); len(temporaries) != 1 {
		t.Fatalf("found %d journal temporaries, want exactly 1: %v", len(temporaries), temporaries)
	}

	if err := RecoverInterruptedRestore(stateDir, recoverOptions()); err != nil {
		t.Fatalf("RecoverInterruptedRestore() error = %v, want nil", err)
	}

	assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

func TestRestoreDiscardsAnUnrenamedPhaseUpdateAndRecoversTheOldGeneration(t *testing.T) {
	skipCrashSuiteInShortMode(t)
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		scenario string
		exitCode int
	}{
		{"committed", scenarioAtCommittedPhase, exitAtCommittedPhase},
		{"rolling back", scenarioAtRollingBack, exitAtRollingBack},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			stateDir, _, snapshot := snapshotFixture(t)
			execSQL(t, filepath.Join(stateDir, "runtime.db"), "INSERT INTO sessions VALUES ('old-after-snapshot')")
			before := generationFingerprints(t, stateDir)

			if code := runChild(t, testCase.scenario, snapshot, stateDir); code != testCase.exitCode {
				t.Fatalf("child exit code = %d, want %d", code, testCase.exitCode)
			}
			// The durable journal still records the phase that was actually
			// published; the unrenamed update is a stale temporary.
			journal := readJSONFixture(t, journalPath(stateDir))
			if journal["phase"] != string(phasePrepared) {
				t.Fatalf("durable journal phase = %v, want %q", journal["phase"], phasePrepared)
			}
			if temporaries := journalTemporaries(t, stateDir); len(temporaries) != 1 {
				t.Fatalf("found %d journal temporaries, want exactly 1: %v", len(temporaries), temporaries)
			}

			if err := RecoverInterruptedRestore(stateDir, recoverOptions()); err != nil {
				t.Fatalf("RecoverInterruptedRestore() error = %v, want nil", err)
			}

			assertSameFingerprints(t, before, generationFingerprints(t, stateDir))
			assertNoRestoreResidue(t, stateDir)
		})
	}
}

func TestRestoreRecoveryFailsClosedWhenOldGenerationEvidenceIsMissing(t *testing.T) {
	skipCrashSuiteInShortMode(t)

	stateDir, _, snapshot := snapshotFixture(t)

	if code := runChild(t, scenarioAfterQuarantine, snapshot, stateDir); code != exitAfterQuarantine {
		t.Fatalf("child exit code = %d, want %d", code, exitAfterQuarantine)
	}
	journal := readJSONFixture(t, journalPath(stateDir))
	quarantine, ok := journal["quarantine_dir"].(string)
	if !ok {
		t.Fatalf("journal quarantine_dir = %v, want a string", journal["quarantine_dir"])
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, quarantine))
	if err != nil || len(entries) == 0 {
		t.Fatalf("read the quarantine: %v (%d entries)", err, len(entries))
	}
	// Destroy one piece of the old generation. Recovery must refuse rather than
	// publish a generation it cannot prove.
	if removeErr := os.Remove(filepath.Join(stateDir, quarantine, entries[0].Name())); removeErr != nil {
		t.Fatalf("remove the quarantined evidence: %v", removeErr)
	}

	err = RecoverInterruptedRestore(stateDir, recoverOptions())

	assertSnapshotError(t, err, "missing byte-identical old evidence")
	if !isRegularFile(journalPath(stateDir)) {
		t.Error("recovery removed the journal it could not act on; an operator needs that evidence")
	}
}

func TestConcurrentRestoresAreSerializedAcrossProcesses(t *testing.T) {
	skipCrashSuiteInShortMode(t)

	stateDir, _, snapshot := snapshotFixture(t)
	markers := t.TempDir()
	firstLocked := filepath.Join(markers, "first-locked")
	release := filepath.Join(markers, "release-first")

	holder := childCommand(t, scenarioHoldLock, snapshot, stateDir,
		"-crash.marker="+firstLocked, "-crash.release="+release)
	if err := holder.Start(); err != nil {
		t.Fatalf("start the lock holder: %v", err)
	}
	holderDone := waitInBackground(holder)
	defer func() { _ = holder.Process.Kill() }()
	waitForFile(t, firstLocked)

	restorer := childCommand(t, scenarioRestore, snapshot, stateDir)
	if err := restorer.Start(); err != nil {
		t.Fatalf("start the blocked restore: %v", err)
	}
	restorerDone := waitInBackground(restorer)
	defer func() { _ = restorer.Process.Kill() }()

	// The second process must not get past the lock: no staging directory, no
	// journal, no exit.
	select {
	case err := <-restorerDone:
		t.Fatalf("the second restore ran while the lock was held: %v", err)
	case <-time.After(childBlockedObservation):
	}
	artifacts, err := restoreArtifacts(stateDir)
	if err != nil {
		t.Fatalf("list the restore residue: %v", err)
	}
	if len(artifacts) > 0 {
		t.Errorf("the blocked restore created residue while waiting: %v", baseNames(artifacts))
	}

	if err := os.WriteFile(release, []byte("continue"), 0o600); err != nil {
		t.Fatalf("release the lock holder: %v", err)
	}
	assertChildSucceeded(t, "lock holder", holderDone)
	assertChildSucceeded(t, "blocked restore", restorerDone)

	assertSameFingerprints(t, fingerprints(t, snapshot), fingerprints(t, stateDir))
	assertNoRestoreResidue(t, stateDir)
}

// waitInBackground reaps a child so a test can observe whether it has exited.
func waitInBackground(command *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return done
}

// assertChildSucceeded waits for a child and fails unless it exited zero.
func assertChildSucceeded(t *testing.T, name string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the %s child failed: %v", name, err)
		}
	case <-time.After(childCompletionTimeout):
		t.Fatalf("the %s child did not finish", name)
	}
}

// journalTemporaries lists the unrenamed journal temporaries in a state
// directory.
func journalTemporaries(t *testing.T, stateDir string) []string {
	t.Helper()
	artifacts, err := restoreArtifacts(stateDir)
	if err != nil {
		t.Fatalf("list the restore residue: %v", err)
	}
	var temporaries []string
	for _, artifact := range artifacts {
		name := filepath.Base(artifact)
		if strings.HasPrefix(name, journalPrefix) && strings.HasSuffix(name, journalTempSuffix) {
			temporaries = append(temporaries, name)
		}
	}
	return temporaries
}
