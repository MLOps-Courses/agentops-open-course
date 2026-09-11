package platformdrill

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Command is the repository-owned agent subcommand for the platform drill.
const Command = "platform-backup"

const (
	seedSubcommand    = "seed"
	restoreSubcommand = "restore-drill"
)

// Main parses and runs the platform backup drill command.
func Main(ctx context.Context, arguments []string, out, errOut io.Writer) int {
	if err := dispatch(ctx, arguments, out); err != nil {
		_, _ = fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, arguments []string, out io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%s: expected %s or %s", Command, seedSubcommand, restoreSubcommand)
	}
	switch arguments[0] {
	case seedSubcommand:
		return runSeed(ctx, arguments[1:], out)
	case restoreSubcommand:
		return runRestore(ctx, arguments[1:], out)
	default:
		return fmt.Errorf("%s: unknown subcommand %q", Command, arguments[0])
	}
}

func runSeed(ctx context.Context, arguments []string, out io.Writer) error {
	set := flag.NewFlagSet(Command+" "+seedSubcommand, flag.ContinueOnError)
	set.SetOutput(out)
	stateDir := set.String("state-dir", "", "directory holding runtime SQLite state")
	dataDir := set.String("data-dir", "", "directory holding immutable seed data")
	marker := set.String("marker", "", "unique evidence marker")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("%s %s: unexpected argument %q", Command, seedSubcommand, set.Arg(0))
	}
	evidence, err := Seed(ctx, SeedOptions{StateDir: filepath.Clean(*stateDir), DataDir: filepath.Clean(*dataDir), Marker: *marker})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(evidence)
}

func runRestore(ctx context.Context, arguments []string, out io.Writer) error {
	set := flag.NewFlagSet(Command+" "+restoreSubcommand, flag.ContinueOnError)
	set.SetOutput(out)
	snapshot := set.String("snapshot", "", "complete snapshot directory")
	stateDir := set.String("state-dir", "", "stopped live state directory")
	evidencePath := set.String("evidence", "", "seed evidence JSON file")
	expectedIdentity := set.String("expected-source-identity", "", "workflow source identity recorded in the snapshot")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("%s %s: unexpected argument %q", Command, restoreSubcommand, set.Arg(0))
	}
	evidence, err := readEvidence(filepath.Clean(*evidencePath))
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && (attr.Key == slog.TimeKey || attr.Key == slog.LevelKey) {
				return slog.Attr{}
			}
			return attr
		},
	}))
	if restoreErr := RestoreDrill(ctx, RestoreOptions{
		Logger: logger, Snapshot: filepath.Clean(*snapshot), StateDir: filepath.Clean(*stateDir),
		ExpectedSourceIdentity: *expectedIdentity, Evidence: evidence,
	}); restoreErr != nil {
		return restoreErr
	}
	_, err = fmt.Fprintln(out, "platform backup restore drill verified")
	return err
}

func readEvidence(path string) (Evidence, error) {
	file, err := os.Open(path)
	if err != nil {
		return Evidence{}, fmt.Errorf("open backup evidence: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var evidence Evidence
	if err := decoder.Decode(&evidence); err != nil {
		return Evidence{}, fmt.Errorf("decode backup evidence: %w", err)
	}
	if err := validateEvidence(evidence); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}
