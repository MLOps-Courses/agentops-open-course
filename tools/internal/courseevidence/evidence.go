package courseevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/sourceidentity"
)

const (
	formatVersion = 2
	courseID      = "MLOps-Courses/agentops-open-course"
	claim         = "deterministic offline course gates at the recorded source revision"
)

// artifactListPath holds the inventory a manifest attests to. It is a committed
// file rather than a Go literal so that changing what a completion certificate
// covers is a reviewable diff instead of a recompile.
const artifactListPath = "tools/internal/courseevidence/artifacts.txt"

var DefaultGates = [][]string{{"mise", "run", "check:core"}, {"mise", "run", "test"}}

// DefaultArtifacts reads the committed inventory. A missing or empty list is an
// error at use, not a silent empty manifest.
func DefaultArtifacts(root string) ([]string, error) {
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(artifactListPath)))
	if err != nil {
		return nil, fmt.Errorf("reading the course-evidence artifact inventory: %w", err)
	}
	var paths []string
	for line := range strings.Lines(string(content)) {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			paths = append(paths, trimmed)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s lists no evidence artifacts", artifactListPath)
	}
	return paths, nil
}

// ErrMissingArtifact names the one failure a learner can fix without reading
// the code: an attested file is not on disk.
var ErrMissingArtifact = errors.New("evidence artifact is missing")

type GateResult struct {
	Command string `json:"command"`
	Result  string `json:"result"`
}

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Course      string       `json:"course"`
	Revision    string       `json:"revision"`
	TreeDigest  string       `json:"tree_digest"`
	GeneratedAt string       `json:"generated_at"`
	Claim       string       `json:"claim"`
	Gates       []GateResult `json:"gates"`
	Artifacts   []Artifact   `json:"artifacts"`
	Format      int          `json:"format"`
}

type Config struct {
	Now       func() time.Time
	Run       func(context.Context, string, []string, io.Writer, io.Writer) error
	Root      string
	Gates     [][]string
	Artifacts []string
}

func DefaultConfig(root string) Config {
	// A read failure surfaces at the first artifact instead of here, which keeps
	// DefaultConfig total and leaves the actionable error to artifactRecords.
	artifacts, _ := DefaultArtifacts(root)
	return Config{
		Root: root, Gates: cloneCommands(DefaultGates), Artifacts: artifacts,
		Now: time.Now, Run: runCommand,
	}
}

func Create(ctx context.Context, config Config, output string) error {
	identity, err := sourceidentity.Resolve(ctx, config.Root, sourceidentity.Release)
	if err != nil {
		return err
	}
	// Hash the artifacts before running the gates. A missing artifact is cheap to
	// detect and expensive to detect late: the other order cost a learner a whole
	// `check:core` plus `test` run before telling them a file was not there.
	artifacts, err := artifactRecords(config.Root, config.Artifacts)
	if err != nil {
		return err
	}
	gates, err := runGates(ctx, config)
	if err != nil {
		return err
	}
	manifest := Manifest{
		Format: formatVersion, Course: courseID, Revision: identity.Revision, TreeDigest: identity.TreeDigest,
		GeneratedAt: config.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Claim:       claim, Gates: gates, Artifacts: artifacts,
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding course evidence: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
		return fmt.Errorf("creating the course-evidence directory: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(output, encoded, 0o600); err != nil {
		return fmt.Errorf("writing course evidence %s: %w", output, err)
	}
	// The JSON is the machine artifact; the Markdown is the thing a learner
	// pastes into a pull request or a message. Both carry the same facts.
	summary := SummaryPath(output)
	if err := os.WriteFile(summary, []byte(renderSummary(manifest)), 0o600); err != nil {
		return fmt.Errorf("writing course evidence summary %s: %w", summary, err)
	}
	return nil
}

// SummaryPath is the human-readable twin of a manifest path.
func SummaryPath(manifestPath string) string {
	return strings.TrimSuffix(manifestPath, filepath.Ext(manifestPath)) + ".md"
}

func renderSummary(manifest Manifest) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Course completion — %s\n\n", manifest.Course)
	fmt.Fprintf(&out, "- **Revision**: `%s`\n", manifest.Revision)
	fmt.Fprintf(&out, "- **Tree digest**: `%s`\n", manifest.TreeDigest)
	fmt.Fprintf(&out, "- **Generated**: %s\n", manifest.GeneratedAt)
	fmt.Fprintf(&out, "- **Claim**: %s\n\n", manifest.Claim)
	out.WriteString("## Gates\n\n| Command | Result |\n| --- | --- |\n")
	for _, gate := range manifest.Gates {
		fmt.Fprintf(&out, "| `%s` | %s |\n", gate.Command, gate.Result)
	}
	out.WriteString("\n## Artifacts\n\n| Path | SHA-256 |\n| --- | --- |\n")
	for _, artifact := range manifest.Artifacts {
		fmt.Fprintf(&out, "| `%s` | `%s` |\n", artifact.Path, artifact.SHA256)
	}
	out.WriteString("\nReproduce with `mise run course:evidence:verify` on a checkout at that revision.\n")
	return out.String()
}

// RepositoryRoot walks up from the working directory to the checkout that owns
// this course. Both completion commands need it before they can resolve their
// default manifest path, so it lives here rather than being copied into each
// `package main`: the two are a pair, and a root one of them disagreed about
// would silently point them at different manifests.
func RepositoryRoot() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("reading working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "mise.toml")); err == nil {
			if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err == nil {
				return root, nil
			}
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", errors.New("could not find the repository root")
		}
		root = parent
	}
}

// DefaultManifestPath is gitignored on purpose: the manifest attests to a
// checkout, not to a commit, so committing one would be attesting to the wrong
// thing.
func DefaultManifestPath(root string) string {
	return filepath.Join(root, ".agents/tmp/course-completion.json")
}

// DisplayPath shortens a path to the working directory when it sits inside it.
// The default output paths are absolute because the commands resolve them from
// the repository root rather than from wherever they were invoked, but an
// absolute path is the wrong thing to print: it is noise a learner has to read
// past, and pasting the line into an issue or a course page leaks a home
// directory. Anything outside the working directory is printed unchanged, since
// shortening that would hide where the file actually went.
func DisplayPath(path string) string {
	working, err := os.Getwd()
	if err != nil {
		return path
	}
	relative, err := filepath.Rel(working, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return path
	}
	return relative
}

// Decode parses a manifest and rejects one this repository did not produce. It
// is strict about unknown fields because a manifest is evidence: a field this
// build does not understand means the file was written by a different contract,
// and silently ignoring it would let a certificate quote figures nothing here
// checked.
func Decode(content []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decoding course evidence: %w", err)
	}
	if manifest.Format != formatVersion || manifest.Course != courseID || manifest.Claim != claim {
		return Manifest{}, errors.New("manifest format, course, or claim does not match this repository")
	}
	return manifest, nil
}

func Verify(ctx context.Context, config Config, manifestPath string) (string, error) {
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("reading course evidence %s: %w", manifestPath, err)
	}
	manifest, err := Decode(content)
	if err != nil {
		return "", err
	}
	identity, err := sourceidentity.Resolve(ctx, config.Root, sourceidentity.Release)
	if err != nil {
		return "", err
	}
	if manifest.Revision != identity.Revision || manifest.TreeDigest != identity.TreeDigest {
		return "", fmt.Errorf(
			"manifest source identity %q (%s) does not match this checkout %s (%s)",
			manifest.Revision, manifest.TreeDigest, identity.Revision, identity.TreeDigest,
		)
	}
	expectedGates := gateInventory(config.Gates)
	if !equalGates(manifest.Gates, expectedGates) {
		return "", errors.New("manifest gate inventory does not match this revision's course contract")
	}
	expectedArtifacts, err := artifactRecords(config.Root, config.Artifacts)
	if err != nil {
		return "", err
	}
	if !equalArtifacts(manifest.Artifacts, expectedArtifacts) {
		return "", errors.New("manifest artifact hashes do not match this checkout")
	}
	if _, err := runGates(ctx, config); err != nil {
		return "", err
	}
	return identity.Revision, nil
}

func runGates(ctx context.Context, config Config) ([]GateResult, error) {
	results := make([]GateResult, 0, len(config.Gates))
	for _, command := range config.Gates {
		if len(command) == 0 {
			return nil, errors.New("course evidence contains an empty gate command")
		}
		if err := config.Run(ctx, config.Root, command, os.Stdout, os.Stderr); err != nil {
			return nil, fmt.Errorf("%s failed: %w", strings.Join(command, " "), err)
		}
		results = append(results, GateResult{Command: strings.Join(command, " "), Result: "passed"})
	}
	return results, nil
}

func artifactRecords(root string, paths []string) ([]Artifact, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no evidence artifacts to hash; check %s", artifactListPath)
	}
	records := make([]Artifact, 0, len(paths))
	for _, relative := range paths {
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return nil, fmt.Errorf(
				"%w: %s is missing; restore it with `git restore -- %s` or correct %s",
				ErrMissingArtifact, relative, relative, artifactListPath,
			)
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return nil, fmt.Errorf("hashing evidence artifact %s: %w", relative, err)
		}
		records = append(records, Artifact{Path: relative, SHA256: hex.EncodeToString(digest.Sum(nil))})
	}
	return records, nil
}

func runCommand(ctx context.Context, root string, command []string, stdout, stderr io.Writer) error {
	process := exec.CommandContext(ctx, command[0], command[1:]...)
	process.Dir, process.Stdout, process.Stderr = root, stdout, stderr
	return process.Run()
}

func gateInventory(commands [][]string) []GateResult {
	results := make([]GateResult, len(commands))
	for index, command := range commands {
		results[index] = GateResult{Command: strings.Join(command, " "), Result: "passed"}
	}
	return results
}

func equalGates(left, right []GateResult) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalArtifacts(left, right []Artifact) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneCommands(commands [][]string) [][]string {
	cloned := make([][]string, len(commands))
	for index, command := range commands {
		cloned[index] = append([]string(nil), command...)
	}
	return cloned
}
