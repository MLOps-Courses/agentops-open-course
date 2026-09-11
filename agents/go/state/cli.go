package state

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// Command is the subcommand name the agent binary dispatches to [Main].
//
// The host scripts and the Kubernetes CronJob call `agent state backup` and
// `agent state restore <snapshot>`, which is the Go track's replacement for the
// Python track's `python -m agent.state`.
const Command = "state"

// Subcommand names, so a caller can route without re-spelling them.
const (
	BackupSubcommand  = "backup"
	RestoreSubcommand = "restore"
)

// The environment variables an operator sets, named here so the process
// boundary and this command agree on the spelling.
//
// Nothing in this package reads them. They arrive through [CommandEnvironment],
// for the same reason config.Config is threaded in as a value rather than read
// from a package singleton: an operation on durable state must be a function of
// its arguments, and the restore path in particular must never be steerable by
// a variable that happened to be set in the process.
const (
	// EnvBackupKeep bounds snapshot retention when --keep is not given.
	EnvBackupKeep = "STATE_BACKUP_KEEP"
	// EnvBackupTimestamp pins the snapshot directory name, which is what makes
	// a backup drill reproducible.
	EnvBackupTimestamp = "STATE_BACKUP_TIMESTAMP"
)

// defaultBackupRoot is where snapshots land when --backup-root is not given.
const defaultBackupRoot = ".state-backups"

// defaultKeep is the retention applied when neither --keep nor STATE_BACKUP_KEEP
// says otherwise: a week of daily snapshots.
const defaultKeep = 7

// CommandEnvironment carries the process-level values [Main] does not read for
// itself. The caller resolves them once, at the process boundary.
type CommandEnvironment struct {
	// Keep is EnvBackupKeep exactly as the operator wrote it. Nil means the
	// variable is unset; a pointer to an empty or unparseable string is a
	// mistake worth reporting rather than rounding down to the default.
	Keep *string
	// Build overrides the link-time build identity for deterministic tests.
	// Nil makes BackupState resolve the running binary's identity.
	Build *buildinfo.Info
	// Timestamp is EnvBackupTimestamp. Empty means "stamp this snapshot now".
	Timestamp string
}

// Main runs the repository-owned backup and restore command.
//
// It returns an exit code rather than an error because it is the process
// boundary's decision, and it reuses the config package's codes so an operator
// reads one table for every subcommand the binary offers.
func Main(
	ctx context.Context,
	arguments []string,
	environment CommandEnvironment,
	out, errOut io.Writer,
) int {
	logger := commandLogger(out)
	err := dispatch(ctx, arguments, environment, out, logger)
	if err == nil {
		return config.ExitValid
	}
	if _, writeErr := fmt.Fprintf(errOut, "error: %v\n", err); writeErr != nil {
		return config.ExitUnreportable
	}
	return config.ExitInvalid
}

// commandLogger renders progress as bare lines, the way the Python track's
// logging.basicConfig(format="%(message)s") did, so the CronJob logs stay
// readable without a JSON viewer.
func commandLogger(out io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && (attr.Key == slog.TimeKey || attr.Key == slog.LevelKey) {
				return slog.Attr{}
			}
			return attr
		},
	}))
}

// dispatch routes one invocation to its subcommand.
func dispatch(
	ctx context.Context,
	arguments []string,
	environment CommandEnvironment,
	out io.Writer,
	logger *slog.Logger,
) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s: expected %s or %s", Command, BackupSubcommand, RestoreSubcommand)
	}
	switch arguments[0] {
	case BackupSubcommand:
		return runBackup(ctx, arguments[1:], environment, out, logger)
	case RestoreSubcommand:
		return runRestore(ctx, arguments[1:], out, logger)
	default:
		return fmt.Errorf("%s: unknown subcommand %q; expected %s or %s",
			Command, arguments[0], BackupSubcommand, RestoreSubcommand)
	}
}

// runBackup parses and executes `state backup`.
func runBackup(
	ctx context.Context,
	arguments []string,
	environment CommandEnvironment,
	out io.Writer,
	logger *slog.Logger,
) error {
	set := newFlagSet(BackupSubcommand, out)
	stateDir := set.String("state-dir", "", "state directory to snapshot (default: "+config.EnvStateDir+")")
	backupRoot := set.String("backup-root", defaultBackupRoot, "directory that holds published snapshots")
	keep := set.Int("keep", defaultKeep, "completed snapshots to retain (default: "+EnvBackupKeep+")")
	positional, err := parseFlags(set, arguments)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fmt.Errorf("%s %s: unexpected argument %q", Command, BackupSubcommand, positional[0])
	}
	resolvedKeep, err := resolveKeep(set, *keep, environment.Keep)
	if err != nil {
		return err
	}
	resolvedStateDir, err := resolveStateDir(set, *stateDir)
	if err != nil {
		return err
	}
	build := buildinfo.Info{}
	if environment.Build != nil {
		build = *environment.Build
	}
	_, err = BackupState(ctx, resolvedStateDir, filepath.Clean(*backupRoot), BackupOptions{
		Logger: logger, Keep: resolvedKeep, Timestamp: environment.Timestamp, Build: build,
	})
	return err
}

// runRestore parses and executes `state restore <snapshot>`.
func runRestore(ctx context.Context, arguments []string, out io.Writer, logger *slog.Logger) error {
	set := newFlagSet(RestoreSubcommand, out)
	stateDir := set.String("state-dir", "", "state directory to restore into (default: "+config.EnvStateDir+")")
	positional, err := parseFlags(set, arguments)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("%s %s: expected exactly one snapshot directory, got %d",
			Command, RestoreSubcommand, len(positional))
	}
	resolvedStateDir, err := resolveStateDir(set, *stateDir)
	if err != nil {
		return err
	}
	// No BeforePublish: the failure-injection seam exists for tests, and the
	// command deliberately offers no way to reach it.
	_, err = RestoreState(ctx, filepath.Clean(positional[0]), resolvedStateDir, RestoreOptions{Logger: logger})
	return err
}

// newFlagSet builds a subcommand's flag set with usage on the command's own
// output stream.
func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet(Command+" "+name, flag.ContinueOnError)
	set.SetOutput(out)
	return set
}

// parseFlags parses a subcommand's arguments and returns its positional ones.
//
// The loop is what lets flags appear after a positional argument, so
// `state restore <snapshot> --state-dir x` works the way the host scripts and
// the Python argparse-based command spell it. The standard flag package stops
// at the first non-flag argument on its own.
func parseFlags(set *flag.FlagSet, arguments []string) ([]string, error) {
	var positional []string
	remaining := arguments
	for {
		if err := set.Parse(remaining); err != nil {
			return nil, fmt.Errorf("%s: %w", set.Name(), err)
		}
		remaining = set.Args()
		if len(remaining) == 0 {
			return positional, nil
		}
		positional = append(positional, remaining[0])
		remaining = remaining[1:]
	}
}

// resolveKeep applies the environment fallback when --keep was not given.
func resolveKeep(set *flag.FlagSet, provided int, configured *string) (int, error) {
	if flagWasSet(set, "keep") {
		return provided, nil
	}
	if configured == nil {
		return defaultKeep, nil
	}
	keep, err := strconv.Atoi(*configured)
	if err != nil {
		// A typo in a CronJob's environment must not silently fall back to the
		// default retention and start deleting snapshots on a different schedule.
		return 0, fmt.Errorf("%s must be an integer, got %q", EnvBackupKeep, *configured)
	}
	return keep, nil
}

// resolveStateDir applies the configured default when --state-dir was not given.
//
// The configuration is only loaded when the flag is absent, so an operator can
// snapshot an arbitrary directory on a host whose agent environment is not set
// up — which is exactly the position somebody restoring from a backup is in.
func resolveStateDir(set *flag.FlagSet, provided string) (string, error) {
	if flagWasSet(set, "state-dir") {
		return filepath.Clean(provided), nil
	}
	resolved, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("resolving the default state directory from the environment: %w", err)
	}
	return filepath.Clean(resolved.StateDir), nil
}

// flagWasSet reports whether a flag was given on the command line, which is how
// "not given" is told apart from "given the default value".
func flagWasSet(set *flag.FlagSet, name string) bool {
	found := false
	set.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
