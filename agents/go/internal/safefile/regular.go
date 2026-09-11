// Package safefile owns the repository's no-follow regular-file policy.
package safefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Regular is a regular file opened relative to an anchored directory root.
// The root and file remain open together so callers can re-verify that the
// pathname still names the inode they inspected.
type Regular struct {
	root *os.Root
	file *os.File
	name string
}

// Open opens path only when its final entry is and remains a regular file.
// os.Root confines any raced symlink resolution to the opened parent, while
// the before/open/after identity checks reject both pre-existing links and
// replacements during the open itself.
func Open(path string) (*Regular, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open parent of %s: %w", path, err)
	}
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect %s without following links: %w", path, err), root.Close())
	}
	if !before.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("%s must be a regular file", path), root.Close())
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open %s from its anchored parent: %w", path, err), root.Close())
	}
	opened := &Regular{root: root, file: file, name: name}
	if err := opened.verifyAgainst(before); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	return opened, nil
}

// File returns the already-open no-follow-verified file handle.
func (r *Regular) File() *os.File { return r.file }

// Verify proves the anchored pathname still names the opened regular file.
func (r *Regular) Verify() error {
	opened, err := r.file.Stat()
	if err != nil {
		return fmt.Errorf("measure opened file %s: %w", r.file.Name(), err)
	}
	return r.verifyAgainst(opened)
}

func (r *Regular) verifyAgainst(expected fs.FileInfo) error {
	opened, err := r.file.Stat()
	if err != nil {
		return fmt.Errorf("measure opened file %s: %w", r.file.Name(), err)
	}
	current, err := r.root.Lstat(r.name)
	if err != nil {
		return fmt.Errorf("reinspect %s without following links: %w", r.file.Name(), err)
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) {
		return fmt.Errorf("%s changed while it was being opened as a regular file", r.file.Name())
	}
	return nil
}

// Close releases both the file and its anchored parent directory.
func (r *Regular) Close() error {
	if r == nil {
		return nil
	}
	return errors.Join(r.file.Close(), r.root.Close())
}
