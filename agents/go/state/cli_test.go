package state

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// runCommand drives Main with an empty process environment.
func runCommand(t *testing.T, arguments ...string) (code int, out, errOut string) {
	t.Helper()
	return runCommandWith(t, CommandEnvironment{}, arguments...)
}

// runCommandWith drives Main and returns its exit code with both streams.
func runCommandWith(t *testing.T, environment CommandEnvironment, arguments ...string) (code int, out, errOut string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code = Main(t.Context(), arguments, environment, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestStateCommandRoutesBackupRestoreAndErrors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stateDir := seedState(t, filepath.Join(root, "state"))
	backupRoot := filepath.Join(root, "backups")
	build := fixedBuild()
	pinned := CommandEnvironment{Timestamp: fixedStamp, Build: &build}

	code, out, errOut := runCommandWith(t, pinned,
		BackupSubcommand, "--state-dir", stateDir, "--backup-root", backupRoot, "--keep", "1")
	if code != config.ExitValid {
		t.Fatalf("backup exit = %d, want %d (stderr: %s)", code, config.ExitValid, errOut)
	}
	// Progress goes to the command's own output, one bare line per file, the way
	// the CronJob logs it.
	if !strings.Contains(out, "snapshot complete") {
		t.Errorf("backup output = %q, want it to report the published snapshot", out)
	}

	snapshot := filepath.Join(backupRoot, fixedStamp)
	expected := fingerprints(t, snapshot)
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("clear the state directory: %v", err)
	}

	// The snapshot comes before the flag, which is how the host scripts and the
	// Kubernetes manifests spell it.
	code, _, errOut = runCommandWith(t, pinned, RestoreSubcommand, snapshot, "--state-dir", stateDir)
	if code != config.ExitValid {
		t.Fatalf("restore exit = %d, want %d (stderr: %s)", code, config.ExitValid, errOut)
	}
	assertSameFingerprints(t, expected, fingerprints(t, stateDir))

	code, _, errOut = runCommandWith(t, pinned,
		BackupSubcommand, "--state-dir", stateDir, "--backup-root", backupRoot, "--keep", "0")
	if code != config.ExitInvalid {
		t.Fatalf("invalid retention exit = %d, want %d", code, config.ExitInvalid)
	}
	if !strings.HasPrefix(errOut, "error: ") || !strings.Contains(errOut, "Snapshot retention must be positive") {
		t.Errorf("invalid retention stderr = %q, want an error naming the retention", errOut)
	}
}

func TestStateCommandReportsAnInvalidBackupRetentionEnvironment(t *testing.T) {
	t.Parallel()

	invalid := "many"
	code, _, errOut := runCommandWith(t, CommandEnvironment{Keep: &invalid},
		BackupSubcommand, "--state-dir", t.TempDir())

	if code != config.ExitInvalid {
		t.Fatalf("exit = %d, want %d", code, config.ExitInvalid)
	}
	// A typo in a CronJob's environment must be reported, never rounded down to
	// the default retention.
	if !strings.Contains(errOut, EnvBackupKeep+" must be an integer") {
		t.Errorf("stderr = %q, want it to name %s", errOut, EnvBackupKeep)
	}
}

func TestStateCommandResolvesTheConfiguredStateDirectory(t *testing.T) {
	root := t.TempDir()
	stateDir := seedState(t, filepath.Join(root, "state"))
	backupRoot := filepath.Join(root, "backups")
	t.Setenv(config.EnvStateDir, stateDir)

	code, _, errOut := runCommandWith(t, CommandEnvironment{Timestamp: fixedStamp},
		BackupSubcommand, "--backup-root", backupRoot)

	if code != config.ExitValid {
		t.Fatalf("backup exit = %d, want %d (stderr: %s)", code, config.ExitValid, errOut)
	}
	if !isRegularFile(filepath.Join(backupRoot, fixedStamp, completeName)) {
		t.Error("the backup did not publish into the configured state directory's snapshot")
	}
}

func TestStateCommandRejectsMalformedInvocations(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		error     string
		arguments []string
	}{
		{"no subcommand", "expected backup or restore", nil},
		{"unknown subcommand", "unknown subcommand", []string{"prune"}},
		{"restore without a snapshot", "expected exactly one snapshot", []string{RestoreSubcommand}},
		{"restore with two snapshots", "expected exactly one snapshot", []string{RestoreSubcommand, "a", "b"}},
		{"backup with a positional", "unexpected argument", []string{BackupSubcommand, "somewhere"}},
		{"unknown flag", "flag provided but not defined", []string{BackupSubcommand, "--nope"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			code, _, errOut := runCommand(t, testCase.arguments...)

			if code != config.ExitInvalid {
				t.Fatalf("exit = %d, want %d", code, config.ExitInvalid)
			}
			if !strings.Contains(errOut, testCase.error) {
				t.Errorf("stderr = %q, want it to contain %q", errOut, testCase.error)
			}
		})
	}
}
