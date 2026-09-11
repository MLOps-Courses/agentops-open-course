package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type runtimeCommand func(context.Context, string, ...string) ([]byte, error)

// runtimeVersion is the subset of the agent's `version` output the harness
// compares. Pointers separate "absent" from "empty" so a truncated tuple is a
// decode failure instead of a silently matching zero value.
type runtimeVersion struct {
	Revision   *string `json:"revision"`
	TreeDigest *string `json:"tree_digest"`
	Dirty      *bool   `json:"dirty"`
}

// verifyRuntimeIdentity refuses a stale agent. The agent embeds the revision,
// content digest, and dirty flag of the checkout it was built from, so asking
// it for its own version and comparing the tuple is the whole check: a binary
// built before the last edit cannot pretend to be the code under test.
func verifyRuntimeIdentity(
	ctx context.Context, binary string, expected SourceEvidence, run runtimeCommand,
) error {
	if strings.TrimSpace(binary) == "" {
		return errors.New("runtime identity needs an agent binary")
	}
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("validate expected checkout: %w", err)
	}
	if !treeDigestPattern.MatchString(expected.TreeDigest) {
		return errors.New("runtime identity needs the checkout tree digest")
	}
	if run == nil {
		return errors.New("runtime identity command is unavailable")
	}
	output, err := run(ctx, binary, "version")
	if err != nil {
		return fmt.Errorf("query runtime identity: %w", err)
	}
	actual, err := decodeRuntimeVersion(output)
	if err != nil {
		return fmt.Errorf("decode runtime identity: %w", err)
	}
	if actual != expected {
		return fmt.Errorf(
			"agent source does not match the evaluated checkout: revision=%q dirty=%t tree_digest=%q, want revision=%q dirty=%t tree_digest=%q",
			actual.Revision, actual.Dirty, actual.TreeDigest,
			expected.Revision, expected.Dirty, expected.TreeDigest,
		)
	}
	return nil
}

// resolveContainerRuntimeIdentity applies the same check to an image, and
// returns the immutable image ID it verified. Resolving the mutable tag once
// and running that ID keeps the verified image and the evaluated image equal.
func resolveContainerRuntimeIdentity(
	ctx context.Context, engine, image string, expected SourceEvidence, run runtimeCommand,
) (string, error) {
	if strings.TrimSpace(engine) == "" || strings.TrimSpace(image) == "" {
		return "", errors.New("container runtime identity needs an engine and image")
	}
	if run == nil {
		return "", errors.New("container runtime identity command is unavailable")
	}
	inspectOutput, err := run(ctx, engine, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return "", fmt.Errorf("inspect container image identity: %w", err)
	}
	imageID := strings.TrimSpace(string(inspectOutput))
	if imageID == "" {
		return "", errors.New("container image inspection returned an empty image ID")
	}
	if err := verifyRuntimeIdentity(ctx, imageID, expected, func(
		ctx context.Context, id string, arguments ...string,
	) ([]byte, error) {
		return run(ctx, engine, append([]string{"run", "--rm", "--network", "none", "--read-only", id}, arguments...)...)
	}); err != nil {
		return "", err
	}
	return imageID, nil
}

func decodeRuntimeVersion(output []byte) (SourceEvidence, error) {
	var wire runtimeVersion
	if err := json.Unmarshal(output, &wire); err != nil {
		return SourceEvidence{}, fmt.Errorf("version output is not JSON: %w", err)
	}
	if wire.TreeDigest == nil || wire.Dirty == nil {
		return SourceEvidence{}, errors.New("version output has an incomplete source tuple")
	}
	revision := ""
	if wire.Revision != nil {
		revision = *wire.Revision
	}
	evidence := SourceEvidence{Revision: revision, TreeDigest: *wire.TreeDigest, Dirty: *wire.Dirty}
	if err := evidence.Validate(); err != nil {
		return SourceEvidence{}, fmt.Errorf("version output has an invalid source tuple: %w", err)
	}
	return evidence, nil
}

// busyRetryLimit and busyRetryDelay bound the ETXTBSY retry below. Five attempts
// 20ms apart cover the fork window without turning a genuinely locked binary into
// a slow failure; a real "text file busy" still surfaces after 100ms.
const (
	busyRetryLimit = 5
	busyRetryDelay = 20 * time.Millisecond
)

func executeRuntimeCommand(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	// Linux refuses to exec a file that any process still holds open for writing.
	// snapshotRuntimeBinary writes the binary this function then runs, and the
	// harness starts agents concurrently, so a sibling fork can inherit the write
	// descriptor for the few microseconds between fork and exec and make an
	// otherwise correct snapshot fail with ETXTBSY (golang/go#22315). The window
	// closes on its own, so retry it briefly rather than reporting "text file
	// busy" — an error naming a condition the learner did not cause and cannot fix.
	for attempt := 0; ; attempt++ {
		output, err := executeRuntimeCommandOnce(ctx, name, arguments...)
		if err == nil || !errors.Is(err, syscall.ETXTBSY) || attempt == busyRetryLimit-1 {
			return output, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(busyRetryDelay):
		}
	}
}

func executeRuntimeCommandOnce(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		stderr := strings.TrimSpace(string(exitError.Stderr))
		if stderr != "" {
			return nil, fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return nil, err
}

func snapshotRuntimeBinary(source, directory string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open runtime binary: %w", err)
	}
	defer func() { _ = input.Close() }()
	before, err := input.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return "", errors.New("runtime binary must be a regular file")
	}
	destination := filepath.Join(directory, "agent-evaluated")
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", fmt.Errorf("create runtime binary snapshot: %w", err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	after, statErr := input.Stat()
	if err := errors.Join(copyErr, closeErr, statErr); err != nil {
		_ = os.Remove(destination)
		return "", fmt.Errorf("copy runtime binary snapshot: %w", err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		_ = os.Remove(destination)
		return "", errors.New("runtime binary changed while it was snapshotted")
	}
	return destination, nil
}
