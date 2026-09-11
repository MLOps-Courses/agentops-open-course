package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
)

// manifestEntry is one validated database record from a snapshot manifest.
type manifestEntry struct {
	// Identity is the manifest's recorded `sqlite` object, kept as the decoded
	// value so it can be compared against a freshly computed identity without
	// re-reading the file.
	Identity  any
	Filename  string
	SHA256    string
	SizeBytes int64
}

// validateInventory proves a snapshot directory is complete, self-consistent,
// and restorable — before a restore touches a single byte of live state.
//
// Every check here runs against the snapshot only. That ordering is the whole
// safety argument for the restore path: a snapshot that fails any of these
// leaves the running generation exactly as it was.
func validateInventory(ctx context.Context, snapshot string) ([]manifestEntry, error) {
	info, err := os.Stat(snapshot)
	if err != nil || !info.IsDir() {
		return nil, snapshotErrorf("Snapshot directory not found: %s", snapshot)
	}
	if strings.HasPrefix(filepath.Base(snapshot), ".") {
		// A dot-prefixed directory is a staging directory that never reached its
		// publication rename. Its contents may be a partial copy.
		return nil, snapshotErrorf("Snapshot is hidden and unpublished: %s", snapshot)
	}
	markerPath := filepath.Join(snapshot, completeName)
	manifestPath := filepath.Join(snapshot, manifestName)
	if !isRegularFile(markerPath) || !isRegularFile(manifestPath) {
		return nil, snapshotErrorf("Snapshot is incomplete: expected %s and %s under %s",
			completeName, manifestName, snapshot)
	}
	marker, err := loadJSONObject(markerPath)
	if err != nil {
		return nil, err
	}
	manifest, err := loadJSONObject(manifestPath)
	if err != nil {
		return nil, err
	}
	if version, ok := jsonInt(marker["format_version"]); !ok || version != SnapshotFormatVersion {
		return nil, snapshotErrorf("Unsupported snapshot marker format %s; this application supports format %d.",
			renderJSONValue(marker["format_version"]), SnapshotFormatVersion)
	}
	if version, ok := jsonInt(manifest["format_version"]); !ok || version != SnapshotFormatVersion {
		return nil, snapshotErrorf("Unsupported snapshot manifest format %s; this application supports format %d.",
			renderJSONValue(manifest["format_version"]), SnapshotFormatVersion)
	}
	if !hasText(manifest["created_at"]) {
		return nil, snapshotErrorf("Snapshot manifest has no creation timestamp: %s", manifestPath)
	}
	if !hasSourceIdentity(manifest["source"]) {
		return nil, snapshotErrorf("Snapshot manifest has incomplete source identity: %s", manifestPath)
	}
	digest, err := sha256File(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not read snapshot metadata %s: %w", ErrSnapshot, manifestPath, err)
	}
	if recorded, ok := marker["manifest_sha256"].(string); !ok || recorded != digest {
		// The marker is a self-signature, not a cryptographic one: it proves the
		// manifest has not been edited since the snapshot was published, which
		// is what catches a hand-tuned inventory or a truncated copy.
		return nil, snapshotErrorf("Snapshot manifest hash does not match %s: %s", completeName, snapshot)
	}

	inventory, err := parseManifestInventory(manifest["databases"], manifestPath)
	if err != nil {
		return nil, err
	}
	if err := matchInventoryToFiles(snapshot, inventory); err != nil {
		return nil, err
	}
	for _, entry := range inventory {
		if err := verifySnapshotDatabase(ctx, snapshot, entry); err != nil {
			return nil, err
		}
	}
	return inventory, nil
}

// hasSourceIdentity reports whether the manifest names the build that produced
// it. Legacy three-field manifests remain restorable while every newly written
// manifest carries and validates the complete buildinfo tuple.
func hasSourceIdentity(raw any) bool {
	source, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	for _, field := range []string{"application", "version", "commit"} {
		if !hasText(source[field]) {
			return false
		}
	}

	extended := []string{"build_timestamp", "dirty", "mode", "revision", "source_identity", "tree_digest"}
	hasExtended := false
	for _, field := range extended {
		if _, present := source[field]; present {
			hasExtended = true
		}
	}
	if !hasExtended {
		return true
	}
	for _, field := range extended {
		if _, present := source[field]; !present {
			return false
		}
	}

	text := func(field string) (string, bool) {
		value, valid := source[field].(string)
		return value, valid
	}
	mode, modeOK := text("mode")
	version, versionOK := text("version")
	identity, identityOK := text("source_identity")
	revision, revisionOK := text("revision")
	treeDigest, treeOK := text("tree_digest")
	timestamp, timestampOK := text("build_timestamp")
	dirty, dirtyOK := source["dirty"].(bool)
	commit, commitOK := text("commit")
	application, applicationOK := text("application")
	if !modeOK || !versionOK || !identityOK || !revisionOK || !treeOK || !timestampOK ||
		!dirtyOK || !commitOK || !applicationOK || application != applicationName || commit != identity {
		return false
	}

	if mode == string(buildinfo.Development) && version == buildinfo.DevelopmentVersion &&
		identity == buildinfo.DevelopmentIdentity && revision == "" && treeDigest == "" &&
		timestamp == "" && dirty {
		return true
	}
	_, err := buildinfo.Parse(buildinfo.Raw{
		Mode: mode, Version: version, SourceIdentity: identity, Revision: revision,
		TreeDigest: treeDigest, Timestamp: timestamp, Dirty: strconv.FormatBool(dirty),
	})
	return err == nil
}

// parseManifestInventory validates the manifest's database list.
func parseManifestInventory(raw any, manifestPath string) ([]manifestEntry, error) {
	rawEntries, ok := raw.([]any)
	if !ok || len(rawEntries) == 0 {
		return nil, snapshotErrorf("Snapshot manifest has no database inventory: %s", manifestPath)
	}
	inventory := make([]manifestEntry, 0, len(rawEntries))
	seen := make(map[string]struct{}, len(rawEntries))
	for _, rawEntry := range rawEntries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return nil, snapshotErrorf("Snapshot database entry must be an object: %s", manifestPath)
		}
		name, ok := entry["filename"].(string)
		if !ok || !isSafeSnapshotFilename(name) {
			// A name that is not a bare, visible, .db-suffixed base name is a
			// path traversal waiting to be executed against the state directory.
			return nil, snapshotErrorf("Unsafe snapshot database filename: %s", renderJSONValue(entry["filename"]))
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, snapshotErrorf("Duplicate snapshot database filename: %s", name)
		}
		digest, ok := entry["sha256"].(string)
		if !ok || len(digest) != 64 {
			return nil, snapshotErrorf("Invalid SHA-256 for snapshot database %s", name)
		}
		size, ok := jsonInt(entry["size_bytes"])
		if !ok || size <= 0 {
			// A zero-byte database is never a valid SQLite file, so accepting one
			// here would only defer the failure to the restore's publish step.
			return nil, snapshotErrorf("Invalid size for snapshot database %s", name)
		}
		identity, ok := entry["sqlite"].(map[string]any)
		if !ok {
			return nil, snapshotErrorf("Missing SQLite schema identity for snapshot database %s", name)
		}
		seen[name] = struct{}{}
		inventory = append(inventory, manifestEntry{
			Filename:  name,
			SHA256:    digest,
			SizeBytes: size,
			Identity:  identity,
		})
	}
	return inventory, nil
}

// matchInventoryToFiles refuses a snapshot whose directory holds a database the
// manifest never signed, or omits one it did.
func matchInventoryToFiles(snapshot string, inventory []manifestEntry) error {
	present, err := databaseFiles(snapshot)
	if err != nil {
		return err
	}
	actual := make([]string, 0, len(present))
	for _, path := range present {
		actual = append(actual, filepath.Base(path))
	}
	expected := make([]string, 0, len(inventory))
	for _, entry := range inventory {
		expected = append(expected, entry.Filename)
	}
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		return snapshotErrorf("Snapshot database inventory differs from its files: manifest=%v, files=%v",
			expected, actual)
	}
	return nil
}

// verifySnapshotDatabase proves one snapshot database still matches the bytes
// and the schema the manifest recorded for it.
func verifySnapshotDatabase(ctx context.Context, snapshot string, entry manifestEntry) error {
	path := filepath.Join(snapshot, entry.Filename)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: Snapshot database hash or size mismatch: %s: %w", ErrSnapshot, entry.Filename, err)
	}
	digest, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("%w: Snapshot database hash or size mismatch: %s: %w", ErrSnapshot, entry.Filename, err)
	}
	if info.Size() != entry.SizeBytes || digest != entry.SHA256 {
		return snapshotErrorf("Snapshot database hash or size mismatch: %s", entry.Filename)
	}
	identity, err := inspectDatabase(ctx, path)
	if err != nil {
		return err
	}
	same, err := identity.equals(entry.Identity)
	if err != nil {
		return err
	}
	if !same {
		// Matching bytes with a different schema identity means the manifest was
		// signed over a different database and the hashes were back-filled.
		return snapshotErrorf("Snapshot database schema identity mismatch: %s", entry.Filename)
	}
	return nil
}

// isSafeSnapshotFilename reports whether a manifest filename is a bare, visible
// base name ending in .db.
func isSafeSnapshotFilename(name string) bool {
	return name != "" &&
		filepath.Base(name) == name &&
		strings.HasSuffix(name, ".db") &&
		!strings.HasPrefix(name, ".")
}

// isRegularFile reports whether a path is a regular file, following symlinks
// exactly as Python's Path.is_file does.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
