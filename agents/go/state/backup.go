package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
)

// BackupOptions carries the settings a snapshot records or is bounded by.
//
// They are values the caller passes in rather than environment reads, so a
// snapshot taken from a test, a host script, or a CronJob is reproducible from
// its arguments alone. [Main] is the one place that resolves them from the
// process environment, because that is where an operator sets them.
type BackupOptions struct {
	// Logger receives the snapshot's progress. Nil uses slog.Default.
	Logger *slog.Logger
	// Timestamp pins the snapshot directory name. Empty means "now", rendered
	// as YYYYMMDDTHHMMSSZ; a value that does not parse in that layout is
	// rejected before anything is published.
	Timestamp string
	// Build is the validated binary identity recorded in the manifest. Its zero
	// value resolves through buildinfo.Current, so direct callers and the CLI use
	// the same linker-owned authority.
	Build buildinfo.Info
	// Keep bounds retention: the newest Keep completed snapshots survive and the
	// rest are pruned. It must be positive.
	Keep int
}

// BackupState publishes one complete, online-safe snapshot and applies bounded
// retention.
//
// The snapshot is published by renaming a fully written staging directory into
// place, so a reader either sees a complete snapshot or does not see it at all.
// Crash recovery runs first, under the same lock, which is what guarantees a
// snapshot is never taken of a half-restored generation.
func BackupState(ctx context.Context, stateDir, backupRoot string, opts BackupOptions) (string, error) {
	logger := resolveLogger(opts.Logger)
	if opts.Keep <= 0 {
		return "", snapshotErrorf("Snapshot retention must be positive, got %d.", opts.Keep)
	}
	build, err := resolveBuildInfo(opts.Build)
	if err != nil {
		return "", err
	}
	opts.Build = build
	// This is intentionally before even the existence check below. A crashed
	// restore can leave a half-published generation that must not be inspected
	// as if it were ordinary live state.
	if recoveryErr := RecoverInterruptedRestore(stateDir, RecoverOptions{Logger: logger}); recoveryErr != nil {
		return "", recoveryErr
	}
	if info, statErr := os.Stat(stateDir); statErr != nil || !info.IsDir() {
		return "", snapshotErrorf("State directory not found: %s", stateDir)
	}
	// Both guards run before the backup root is created: a rejected invocation
	// must not leave a directory behind that suggests a snapshot was attempted.
	var snapshot string
	err = withRestoreLock(filepath.Join(stateDir, restoreLockName), func() error {
		if recoverErr := recoverRestoreLocked(stateDir, logger); recoverErr != nil {
			return recoverErr
		}
		published, backupErr := backupLocked(ctx, stateDir, backupRoot, opts, logger)
		snapshot = published
		return backupErr
	})
	if err != nil {
		return "", err
	}
	return snapshot, nil
}

// backupLocked creates a snapshot while restore publication is excluded.
func backupLocked(
	ctx context.Context,
	stateDir, backupRoot string,
	opts BackupOptions,
	logger *slog.Logger,
) (string, error) {
	// Only *.db is snapshotted. The WAL, SHM and journal sidecars are not
	// separate state: VACUUM INTO materializes their committed content into the
	// copy, and snapshotting them alongside would record a torn pair.
	databases, err := databaseFiles(stateDir)
	if err != nil {
		return "", err
	}
	if len(databases) == 0 {
		return "", snapshotErrorf("No SQLite databases found under %s", stateDir)
	}
	if mkdirErr := os.MkdirAll(backupRoot, dirPerm); mkdirErr != nil {
		return "", fmt.Errorf("%w: Could not create the backup root %s: %w", ErrSnapshot, backupRoot, mkdirErr)
	}
	stamp, err := snapshotStamp(opts.Timestamp)
	if err != nil {
		return "", err
	}
	snapshot := filepath.Join(backupRoot, stamp)
	if _, statErr := os.Lstat(snapshot); statErr == nil {
		return "", snapshotErrorf("Completed snapshot already exists: %s", snapshot)
	}
	// A directory mkdir is the atomic same-timestamp mutex: two backups pinned
	// to one stamp cannot both believe they own it.
	stampLock := filepath.Join(backupRoot, stampLockPrefix+stamp)
	if claimErr := os.Mkdir(stampLock, dirPerm); claimErr != nil {
		if errors.Is(claimErr, fs.ErrExist) {
			return "", snapshotErrorf("Another backup owns timestamp %s", stamp)
		}
		return "", fmt.Errorf("%w: Could not claim timestamp %s: %w", ErrSnapshot, stamp, claimErr)
	}

	published, err := publishSnapshot(ctx, publication{
		databases:  databases,
		backupRoot: backupRoot,
		snapshot:   snapshot,
		stamp:      stamp,
		build:      opts.Build,
		logger:     logger,
	})
	if removeErr := os.Remove(stampLock); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		err = errors.Join(err, fmt.Errorf("release the timestamp claim %s: %w", stampLock, removeErr))
	}
	if err != nil {
		return "", err
	}

	if pruneErr := pruneSnapshots(backupRoot, opts.Keep, logger); pruneErr != nil {
		return "", pruneErr
	}
	logger.Info("snapshot complete", "snapshot", published)
	return published, nil
}

// publication is the argument set of one snapshot publication.
type publication struct {
	logger     *slog.Logger
	backupRoot string
	snapshot   string
	stamp      string
	build      buildinfo.Info
	databases  []string
}

// publishSnapshot copies, verifies, manifests, and atomically publishes.
func publishSnapshot(ctx context.Context, args publication) (published string, err error) {
	staging, err := os.MkdirTemp(args.backupRoot, incompletePrefix+args.stamp+".")
	if err != nil {
		return "", fmt.Errorf("%w: Could not stage a snapshot under %s: %w", ErrSnapshot, args.backupRoot, err)
	}
	defer func() {
		if published != "" {
			// The staging directory became the snapshot; there is nothing left
			// under its old name to remove.
			return
		}
		if removeErr := os.RemoveAll(staging); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("discard the incomplete snapshot %s: %w", staging, removeErr))
		}
	}()

	inventory := make([]any, 0, len(args.databases))
	for _, source := range args.databases {
		name := filepath.Base(source)
		target := filepath.Join(staging, name)
		if copyErr := copyDatabase(ctx, source, target); copyErr != nil {
			return "", copyErr
		}
		// The copy is inspected, not the original: what the manifest signs has
		// to be the bytes a restore will publish.
		identity, inspectErr := inspectDatabase(ctx, target)
		if inspectErr != nil {
			return "", inspectErr
		}
		digest, hashErr := sha256File(target)
		if hashErr != nil {
			return "", fmt.Errorf("%w: Could not hash the snapshot copy %s: %w", ErrSnapshot, target, hashErr)
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			return "", fmt.Errorf("%w: Could not measure the snapshot copy %s: %w", ErrSnapshot, target, statErr)
		}
		inventory = append(inventory, map[string]any{
			"filename":   name,
			"sha256":     digest,
			"size_bytes": info.Size(),
			"sqlite":     identity.payload(),
		})
		args.logger.Info("backed up state file", "file", name, "target", target)
	}

	manifestPath := filepath.Join(staging, manifestName)
	manifest := map[string]any{
		"format_version": SnapshotFormatVersion,
		"created_at":     time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"source":         manifestSource(args.build),
		"databases":      inventory,
	}
	if writeErr := writeJSONDocument(manifestPath, manifest, documentPerm); writeErr != nil {
		return "", fmt.Errorf("%w: Could not write %s: %w", ErrSnapshot, manifestPath, writeErr)
	}
	manifestDigest, hashErr := sha256File(manifestPath)
	if hashErr != nil {
		return "", fmt.Errorf("%w: Could not hash %s: %w", ErrSnapshot, manifestPath, hashErr)
	}
	marker := map[string]any{
		"format_version":  SnapshotFormatVersion,
		"manifest_sha256": manifestDigest,
	}
	if writeErr := writeJSONDocument(filepath.Join(staging, completeName), marker, documentPerm); writeErr != nil {
		return "", fmt.Errorf("%w: Could not write the %s marker under %s: %w",
			ErrSnapshot, completeName, staging, writeErr)
	}

	// fsync the staging directory before the rename so its contents are durable
	// under the name that is about to become the published snapshot.
	if syncErr := fsyncDir(staging); syncErr != nil {
		return "", syncErr
	}
	if renameErr := os.Rename(staging, args.snapshot); renameErr != nil {
		return "", fmt.Errorf("%w: Could not publish the snapshot %s: %w", ErrSnapshot, args.snapshot, renameErr)
	}
	// The publication point. Until the backup root is fsynced, the snapshot's
	// name is not durable and a crash could lose a complete snapshot.
	if syncErr := fsyncDir(args.backupRoot); syncErr != nil {
		return "", syncErr
	}
	return args.snapshot, nil
}

// pruneSnapshots keeps the newest completed snapshots and removes the rest.
func pruneSnapshots(backupRoot string, keep int, logger *slog.Logger) error {
	completed, err := completedSnapshots(backupRoot)
	if err != nil {
		return err
	}
	if len(completed) <= keep {
		return nil
	}
	for _, expired := range completed[keep:] {
		if err := os.RemoveAll(expired); err != nil {
			return fmt.Errorf("%w: Could not prune the expired snapshot %s: %w", ErrSnapshot, expired, err)
		}
		logger.Info("pruned expired snapshot", "snapshot", expired)
	}
	return nil
}

// completedSnapshots lists published snapshots, newest name first.
//
// Only a directory that is visible and carries its `.complete` marker counts. A
// half-written `.incomplete-*` staging directory is therefore never pruned by
// retention and never mistaken for a generation to restore from.
func completedSnapshots(backupRoot string) ([]string, error) {
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not list the snapshots under %s: %w", ErrSnapshot, backupRoot, err)
	}
	var completed []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !isRegularFile(filepath.Join(backupRoot, name, completeName)) {
			continue
		}
		completed = append(completed, filepath.Join(backupRoot, name))
	}
	// Newest first: the stamp layout sorts lexicographically in time order, so
	// everything past the retention count is the oldest.
	slices.SortFunc(completed, func(a, b string) int { return strings.Compare(b, a) })
	return completed, nil
}

// snapshotStamp resolves and validates the snapshot directory name.
func snapshotStamp(configured string) (string, error) {
	stamp := configured
	if stamp == "" {
		return time.Now().UTC().Format(stampLayout), nil
	}
	if _, err := time.Parse(stampLayout, stamp); err != nil {
		// The message names the environment variable because that is where an
		// operator sets it; Main is the only caller that ever passes one.
		return "", snapshotErrorf("STATE_BACKUP_TIMESTAMP must use YYYYMMDDTHHMMSSZ, got %q", stamp)
	}
	return stamp, nil
}

func resolveBuildInfo(configured buildinfo.Info) (buildinfo.Info, error) {
	resolved := configured
	var err error
	if resolved == (buildinfo.Info{}) {
		resolved, err = buildinfo.Current()
		if err != nil {
			return buildinfo.Info{}, fmt.Errorf("%w: Could not read build identity: %w", ErrSnapshot, err)
		}
	}
	if err := buildinfo.Validate(resolved); err != nil {
		return buildinfo.Info{}, fmt.Errorf("%w: Invalid build identity: %w", ErrSnapshot, err)
	}
	return resolved, nil
}

func manifestSource(build buildinfo.Info) map[string]any {
	timestamp := ""
	if !build.Timestamp.IsZero() {
		timestamp = build.Timestamp.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"application":     applicationName,
		"build_timestamp": timestamp,
		// commit remains as a compatibility projection for pre-Go snapshots and
		// the frozen reference; it is never an independent input.
		"commit":          build.SourceIdentity,
		"dirty":           build.Dirty,
		"mode":            string(build.Mode),
		"revision":        build.Revision,
		"source_identity": build.SourceIdentity,
		"tree_digest":     build.TreeDigest,
		"version":         build.Version,
	}
}
