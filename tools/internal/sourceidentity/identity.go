// Package sourceidentity resolves honest, content-bound identities for a Git checkout.
package sourceidentity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/gitenv"
)

type Mode string

const (
	Development Mode = "development"
	Release     Mode = "release"
)

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Identity struct {
	Mode       Mode   `json:"mode"`
	Display    string `json:"display"`
	Revision   string `json:"revision,omitempty"`
	TreeDigest string `json:"tree_digest"`
	Dirty      bool   `json:"dirty"`
	Shallow    bool   `json:"shallow"`
}

func Resolve(ctx context.Context, source string, mode Mode) (Identity, error) {
	return resolve(ctx, source, mode, nil)
}

func resolve(ctx context.Context, source string, mode Mode, beforeDigest func(string) error) (Identity, error) {
	if mode != Development && mode != Release {
		return Identity{}, fmt.Errorf("source identity mode %q is invalid", mode)
	}
	root, err := repositoryRoot(ctx, source)
	if err != nil {
		return Identity{}, err
	}
	status, err := sourceStatus(ctx, root)
	if err != nil {
		return Identity{}, fmt.Errorf("reading source status: %w", err)
	}
	dirty := len(status) != 0
	if dirty && mode == Release {
		return Identity{}, errors.New("release source is dirty; tracked and untracked inputs must match HEAD")
	}

	revision, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Identity{}, fmt.Errorf("resolving source revision: %w", err)
	}
	revision = []byte(strings.TrimSpace(string(revision)))
	if !revisionPattern.Match(revision) {
		return Identity{}, fmt.Errorf("source revision %q is not a full lowercase commit SHA", revision)
	}

	if beforeDigest != nil {
		if hookErr := beforeDigest(root); hookErr != nil {
			return Identity{}, fmt.Errorf("before source digest: %w", hookErr)
		}
	}
	digest, err := workingTreeDigest(ctx, root)
	if err != nil {
		return Identity{}, err
	}
	afterStatus, err := sourceStatus(ctx, root)
	if err != nil {
		return Identity{}, fmt.Errorf("rechecking source status: %w", err)
	}
	if !bytes.Equal(status, afterStatus) || (mode == Release && len(afterStatus) != 0) {
		return Identity{}, errors.New("source changed while its identity was computed")
	}
	afterRevision, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Identity{}, fmt.Errorf("rechecking source revision: %w", err)
	}
	if !bytes.Equal(revision, bytes.TrimSpace(afterRevision)) {
		return Identity{}, errors.New("source changed while its identity was computed")
	}
	shallowOutput, err := gitOutput(ctx, root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return Identity{}, fmt.Errorf("checking shallow source metadata: %w", err)
	}
	shallow := strings.TrimSpace(string(shallowOutput)) == "true"
	identity := Identity{
		Mode: mode, Revision: string(revision), TreeDigest: "sha256:" + digest,
		Display: string(revision), Dirty: dirty, Shallow: shallow,
	}
	if dirty {
		// A dirty build deliberately carries no revision: HEAD does not describe
		// the behavior being built, while the digest keeps development reproducible.
		identity.Revision = ""
		identity.Display = "unknown+dirty." + digest[:12]
	}
	return identity, nil
}

func sourceStatus(ctx context.Context, root string) ([]byte, error) {
	return gitOutput(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=no")
}

func repositoryRoot(ctx context.Context, source string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", errors.New("source directory is required")
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolving source directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspecting source directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("source path must be a directory")
	}
	output, err := gitOutput(ctx, absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("source is not inside a Git repository: %w", err)
	}
	root := filepath.Clean(strings.TrimSpace(string(output)))
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", errors.New("git returned an invalid repository root")
	}
	return root, nil
}

func workingTreeDigest(ctx context.Context, root string) (string, error) {
	output, err := gitOutput(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("listing source inputs: %w", err)
	}
	paths := splitNUL(output)
	sort.Strings(paths)
	modes, err := indexModes(ctx, root)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	for _, relative := range paths {
		if relative == "" {
			continue
		}
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") {
			return "", fmt.Errorf("git returned source path outside the repository: %q", relative)
		}
		if err := hashPath(digest, root, relative, modes[relative]); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func indexModes(ctx context.Context, root string) (map[string]uint32, error) {
	output, err := gitOutput(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, fmt.Errorf("reading tracked source modes: %w", err)
	}
	modes := make(map[string]uint32)
	for _, record := range splitNUL(output) {
		metadata, path, found := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !found || len(fields) != 3 || fields[2] != "0" {
			continue
		}
		mode, parseErr := strconv.ParseUint(fields[0], 8, 32)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing Git mode for %s: %w", path, parseErr)
		}
		modes[path] = uint32(mode)
	}
	return modes, nil
}

func hashPath(digest hash.Hash, root, relative string, gitMode uint32) error {
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		writeDigestRecord(digest, 'D', relative, nil, gitMode)
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting source input %s: %w", relative, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return fmt.Errorf("reading source symlink %s: %w", relative, readErr)
		}
		writeDigestRecord(digest, 'L', relative, []byte(target), normalizedMode(info, gitMode))
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source input %s is not a regular file or symlink", relative)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening source input %s: %w", relative, err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("source input %s changed while its identity was computed", relative)
	}
	writeDigestHeader(digest, 'F', relative, uint64(openedInfo.Size()), normalizedMode(info, gitMode))
	_, copyErr := io.Copy(digest, bufio.NewReader(file))
	finalInfo, finalStatErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(copyErr, finalStatErr, closeErr); err != nil {
		return fmt.Errorf("hashing source input %s: %w", relative, err)
	}
	if !os.SameFile(openedInfo, finalInfo) || openedInfo.Size() != finalInfo.Size() ||
		!openedInfo.ModTime().Equal(finalInfo.ModTime()) {
		return fmt.Errorf("source input %s changed while its identity was computed", relative)
	}
	return nil
}

func normalizedMode(info os.FileInfo, _ uint32) uint32 {
	if info.Mode()&os.ModeSymlink != 0 {
		return 0o120000
	}
	// Git records only the regular-file executable bit. Normalize away local
	// read/write permissions while retaining behavior-changing 0644/0755 drift.
	if info.Mode().Perm()&0o111 != 0 {
		return 0o100755
	}
	return 0o100644
}

func writeDigestRecord(digest hash.Hash, kind byte, path string, content []byte, mode uint32) {
	writeDigestHeader(digest, kind, path, uint64(len(content)), mode)
	_, _ = digest.Write(content)
}

func writeDigestHeader(digest hash.Hash, kind byte, path string, size uint64, mode uint32) {
	_, _ = digest.Write([]byte{kind})
	_ = binary.Write(digest, binary.BigEndian, uint64(len(path)))
	_, _ = digest.Write([]byte(path))
	_ = binary.Write(digest, binary.BigEndian, mode)
	_ = binary.Write(digest, binary.BigEndian, size)
}

func splitNUL(content []byte) []string {
	parts := strings.Split(string(content), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func gitOutput(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	// Detached, because `-C` alone does not win. An inherited GIT_DIR — which every
	// git hook exports, and `mise run test` is a pre-push command — would make this
	// resolver answer for the caller's repository while reporting the directory it
	// was given, so a release could be stamped with a tree nobody asked about.
	command := gitenv.Detach(exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...))
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			message := strings.TrimSpace(string(exitError.Stderr))
			if message != "" {
				return nil, fmt.Errorf("git %s: %s", strings.Join(arguments, " "), message)
			}
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
	}
	return output, nil
}
