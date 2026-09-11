// Package state publishes and restores versioned, integrity-checked snapshots
// of the agent's SQLite state.
//
// The host scripts and the Kubernetes CronJob both drive this package, so the
// snapshot format, the compatibility checks, and the restore rollback have one
// implementation and one set of guarantees.
//
// # What the module actually promises
//
// Writers must be stopped before a restore. A restore is not a multi-file
// rename: it is a three-phase journaled transaction, serialized across
// processes by an advisory lock on the state directory, with an fsync at every
// point where the next step would otherwise be unrecoverable.
//
//   - Nothing mutates live state before the snapshot manifest fully validates.
//   - The journal is durable before the first live-state mutation.
//   - Every live-state mutation is followed by an fsync of both the source and
//     the destination directory.
//   - A crash at any point leaves either the complete old generation or the
//     complete new generation, never a mix, and the next
//     [RecoverInterruptedRestore] proves which.
//   - Recovery refuses to guess. Residue it cannot explain, or bytes it cannot
//     match to a recorded generation, is a hard error that leaves the evidence
//     in place for an operator.
//
// The committed seed database is never touched here: this package only ever
// reads and writes the disposable state directory and a backup root beside it.
//
// # Why the byte encodings are pinned
//
// The `.complete` marker records the SHA-256 of `manifest.json`, and the schema
// identity of every database is a SHA-256 over its serialized `sqlite_schema`
// rows. Both are byte contracts: re-encoding the same content a different way
// gives the same snapshot a different digest, so validation would reject a
// snapshot that is in fact intact. The encoding is therefore pinned to one
// spelling — sorted keys, two-space indent, no HTML escaping, and every
// non-ASCII rune escaped as \uXXXX — and the encoders below exist to hold those
// four properties.
package state

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/safefile"
)

// SnapshotFormatVersion is the on-disk snapshot format this binary writes and
// the only one it accepts.
const SnapshotFormatVersion = 1

// restoreJournalFormatVersion is the restore journal's own format version. It
// is deliberately separate from [SnapshotFormatVersion]: the journal describes
// a transaction in progress, the snapshot describes published bytes, and the
// two can evolve independently.
const restoreJournalFormatVersion = 1

// File and directory names the format owns.
const (
	manifestName       = "manifest.json"
	completeName       = ".complete"
	restoreJournalName = ".restore-journal.json"
	restoreLockName    = ".state-restore.lock"

	stagingPrefix     = ".restore-staged."
	quarantinePrefix  = ".restore-quarantine."
	journalPrefix     = ".restore-journal."
	journalTempSuffix = ".tmp"

	incompletePrefix = ".incomplete-"
	stampLockPrefix  = ".lock-"
)

// applicationName is the stable product identity stamped into every manifest.
// It is intentionally independent from the Go module path so repository
// ownership changes cannot silently invalidate a state snapshot.
const applicationName = "agentops-agent"

// stampLayout is the snapshot directory name format, YYYYMMDDTHHMMSSZ. The
// trailing Z is a literal here — Go only reads it as a zone offset when it is
// followed by an offset layout such as Z07:00.
const stampLayout = "20060102T150405Z"

// File modes.
const (
	// dirPerm is the mode of every directory this package creates.
	dirPerm = 0o750
	// documentPerm is readable beyond the owner on purpose: a manifest is the
	// evidence an operator reads to decide whether a restore is safe, and the
	// account doing that reading is often not the one that published it.
	documentPerm = 0o644
	// privatePerm is for the journal and the lock: transaction-local files that
	// only the process that owns the state directory ever reads.
	privatePerm = 0o600
)

// hashChunkSize is the streaming digest buffer. State databases are small, but
// the snapshot path must not be bounded by the size of a file it is asked to
// hash.
const hashChunkSize = 1 << 20

// ErrSnapshot marks every failure this package reports.
//
// Callers that need to tell "the snapshot boundary rejected this" apart from
// any other failure test it with errors.Is; the cause wrapped underneath keeps
// the original message.
var ErrSnapshot = errors.New("state snapshot failed")

// snapshotErrorf reports a snapshot failure with no underlying cause.
//
// The name ends in f and the arguments are forwarded verbatim, so vet treats it
// as a printf wrapper and checks the format string at every call site.
func snapshotErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSnapshot, fmt.Sprintf(format, args...))
}

// resolveLogger returns the caller's logger or the process default.
func resolveLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.Default()
}

// sha256File returns the hex SHA-256 of a file's contents.
func sha256File(path string) (string, error) {
	opened, err := safefile.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for hashing: %w", path, err)
	}
	defer func() { _ = opened.Close() }()
	digest := sha256.New()
	if _, err := io.CopyBuffer(digest, opened.File(), make([]byte, hashChunkSize)); err != nil {
		return "", fmt.Errorf("read %s for hashing: %w", path, err)
	}
	if err := opened.Verify(); err != nil {
		return "", fmt.Errorf("verify %s after hashing: %w", path, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// fsyncDir flushes a directory's own entries to stable storage.
//
// This is the operation the whole module turns on: renaming a file makes the
// new name visible, but only an fsync of the directory makes that name durable
// across a power loss. Every publication point in this package is followed by
// one, and the journal write is preceded by one as well.
func fsyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open the directory %s to fsync it: %w", path, err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("fsync the directory %s: %w", path, err),
			closeQuietly(directory, path),
		)
	}
	return closeQuietly(directory, path)
}

// fsyncFile flushes one file's contents to stable storage.
func fsyncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s to fsync it: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("fsync %s: %w", path, err), closeQuietly(file, path))
	}
	return closeQuietly(file, path)
}

// closeQuietly closes a handle and names the file when the close itself fails.
func closeQuietly(file *os.File, path string) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// databaseFiles lists the SQLite databases directly inside a directory, sorted
// by name.
//
// Dot-prefixed entries are skipped so that nothing hidden beside the databases
// — this package's own journal, lock and transaction directories included — can
// be captured as part of a snapshot. os.ReadDir already returns entries sorted
// by name, which is the order the manifest and every publication loop rely on.
func databaseFiles(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not list the databases under %s: %w", ErrSnapshot, directory, err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".db") {
			continue
		}
		files = append(files, filepath.Join(directory, name))
	}
	return files, nil
}

// removeIfPresent deletes a path, treating "already gone" as success.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// encodeDocument renders a JSON document in the pinned snapshot encoding:
// two-space indent, sorted keys, ASCII only, and a trailing newline.
//
// Sorting comes free from encoding/json, which always emits map keys in sorted
// order; that is why every payload in this package is built as a map rather
// than as a struct, where field order would decide the bytes.
func encodeDocument(payload map[string]any) ([]byte, error) {
	return encodeJSON(payload, "  ")
}

// encodeCompact renders a JSON value with no whitespace and no trailing
// newline, which is what the schema identity is hashed over.
func encodeCompact(payload any) ([]byte, error) {
	encoded, err := encodeJSON(payload, "")
	if err != nil {
		return nil, err
	}
	// Encoder.Encode always terminates its value with a newline, and the digest
	// is defined over the value without it.
	return bytes.TrimSuffix(encoded, []byte("\n")), nil
}

// encodeJSON is the shared encoder behind encodeDocument and encodeCompact.
func encodeJSON(payload any, indent string) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Go escapes <, > and & by default; the pinned encoding leaves them alone.
	// This is not hypothetical: the seed's audit schema contains a CHECK
	// constraint with a comparison operator, so leaving the default on would
	// change that database's schema identity.
	encoder.SetEscapeHTML(false)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err := encoder.Encode(payload); err != nil {
		return nil, fmt.Errorf("encode the JSON payload: %w", err)
	}
	return escapeNonASCII(buffer.Bytes()), nil
}

// escapeNonASCII rewrites every non-ASCII rune as a \uXXXX escape, the last of
// the four pinned encoding properties.
//
// Every structural character in JSON is ASCII, so a non-ASCII byte can only
// appear inside a string literal and rewriting it in place is safe.
func escapeNonASCII(encoded []byte) []byte {
	if isASCII(encoded) {
		return encoded
	}
	var out bytes.Buffer
	out.Grow(len(encoded))
	for _, character := range string(encoded) {
		switch {
		case character < utf8.RuneSelf:
			out.WriteByte(byte(character))
		case character > 0xFFFF:
			// A \u escape carries sixteen bits, so a rune outside the basic
			// plane has to be written as the surrogate pair JSON defines for it.
			high, low := utf16.EncodeRune(character)
			fmt.Fprintf(&out, `\u%04x\u%04x`, high, low)
		default:
			fmt.Fprintf(&out, `\u%04x`, character)
		}
	}
	return out.Bytes()
}

// isASCII reports whether every byte is below 0x80.
func isASCII(value []byte) bool {
	for _, character := range value {
		if character >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// writeJSONDocument writes an encoded document and flushes it to disk.
func writeJSONDocument(path string, payload map[string]any, perm fs.FileMode) error {
	encoded, err := encodeDocument(payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return fsyncFile(path)
}

// loadJSONObject reads a JSON object with numbers preserved as literals.
//
// json.Number rather than float64 is what lets the validators insist a field
// was written as an integer literal: "1" is an integer and "1.0" is not, and
// float64 cannot tell those apart.
func loadJSONObject(path string) (map[string]any, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not read snapshot metadata %s: %w", ErrSnapshot, path, err)
	}
	if !utf8.Valid(content) {
		// encoding/json replaces invalid UTF-8 inside a string with U+FFFD
		// rather than failing, so corrupted metadata would otherwise decode as
		// if it were clean. The check has to run before the decoder does.
		return nil, snapshotErrorf("Could not read snapshot metadata %s", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: Could not read snapshot metadata %s: %w", ErrSnapshot, path, err)
	}
	// json.Decoder reads whatever follows the first value as simply the next
	// value rather than as an error, so trailing content is rejected here.
	// More skips whitespace, which is why a trailing newline still parses.
	if decoder.More() {
		return nil, snapshotErrorf("Could not read snapshot metadata %s", path)
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil, snapshotErrorf("Snapshot metadata must be a JSON object: %s", path)
	}
	return object, nil
}

// jsonInt reads a decoded JSON value as an integer, rejecting anything that was
// not written as an integer literal.
func jsonInt(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// hasText reports whether a decoded JSON value is a non-empty string.
func hasText(value any) bool {
	text, ok := value.(string)
	return ok && text != ""
}

// renderJSONValue describes a decoded JSON value for a rejection message, so a
// reader can tell whether the offending field was a string, a number, or
// absent — an absent field renders as "None", not as "null".
func renderJSONValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return strconv.Quote(typed)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

// isHex reports whether every character is a lowercase hexadecimal digit.
func isHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// newTransactionID mints the identifier that names a restore transaction's
// journal, staging directory, and quarantine directory.
//
// It is 16 random bytes rendered as 32 lowercase hex characters, which is the
// shape the journal validator enforces. It is not a formatted UUID: nothing
// reads the version or variant bits, and the format's only requirement is that
// the identifier be unguessable and exactly 32 hex characters.
func newTransactionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint a restore transaction id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
