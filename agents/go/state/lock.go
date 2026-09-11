package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// lockOpenFlags are the flags used to open an already-initialized generation
// lock.
//
// The access mode is deliberately O_RDONLY. A Kubernetes backup CronJob mounts
// the state volume read-only, and flock is an advisory lock on an open file
// description — it needs a descriptor, not write permission. Opening the lock
// read-write would make that mount fail on a file the job is not trying to
// change. O_NOFOLLOW is what stops a symlink planted at the lock path from
// redirecting the whole serialization boundary somewhere else.
const lockOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW

// withRestoreLock runs body while holding the exclusive generation lock.
//
// This is the boundary that makes backup, restore, and crash recovery safe to
// run concurrently from different processes: the host script, the CronJob, and
// the agent's own startup recovery all serialize on one inode. The lock is
// advisory and whole-file, and it is released on every exit path including a
// panic.
func withRestoreLock(lockPath string, body func() error) (err error) {
	if mkdirErr := os.MkdirAll(filepath.Dir(lockPath), dirPerm); mkdirErr != nil {
		return fmt.Errorf("create the directory holding %s: %w", lockPath, mkdirErr)
	}
	descriptor, err := openLockFile(lockPath)
	if err != nil {
		return err
	}
	defer func() {
		// Unlock before close on every path, including the ones that never took
		// the lock: LOCK_UN on an unlocked descriptor succeeds, and relying on
		// close alone would leave the lock held for as long as any duplicated
		// descriptor survived.
		if unlockErr := syscall.Flock(int(descriptor.Fd()), syscall.LOCK_UN); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release the state generation lock %s: %w", lockPath, unlockErr))
		}
		if closeErr := descriptor.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the state generation lock %s: %w", lockPath, closeErr))
		}
	}()

	info, err := descriptor.Stat()
	if err != nil {
		return fmt.Errorf("inspect the state generation lock %s: %w", lockPath, err)
	}
	if !info.Mode().IsRegular() {
		// A directory or a device at the lock path would take a lock that no
		// other participant can find. Refusing is the only safe answer.
		return snapshotErrorf("State generation lock must be a regular file: %s", lockPath)
	}
	// Blocking, exclusive, whole-file. A second process waits here rather than
	// racing the first one's publication.
	if err := syscall.Flock(int(descriptor.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("take the state generation lock %s: %w", lockPath, err)
	}
	return body()
}

// openLockFile opens the generation lock, creating it exactly once.
//
// The create is O_EXCL rather than O_CREAT alone so two processes starting
// together cannot each believe they made the file; the loser reopens the
// winner's inode. A creation failure that is not "it already exists" — a
// read-only filesystem, a full disk, a permission wall — propagates, so a
// caller fails closed instead of silently serializing on a different lock than
// everybody else.
func openLockFile(path string) (*os.File, error) {
	descriptor, err := openExistingLock(path)
	if err == nil {
		return descriptor, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	descriptor, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, privatePerm)
	if createErr == nil {
		return descriptor, nil
	}
	if !errors.Is(createErr, fs.ErrExist) {
		return nil, fmt.Errorf("create the state generation lock %s: %w", path, createErr)
	}
	return openExistingLock(path)
}

// openExistingLock opens an already-initialized lock inode.
func openExistingLock(path string) (*os.File, error) {
	descriptor, err := os.OpenFile(path, lockOpenFlags, 0)
	if err == nil {
		return descriptor, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		// O_NOFOLLOW turns "this is a symlink" into ELOOP. Say so plainly: the
		// operator needs to know somebody redirected the lock, not that a path
		// loop exists.
		return nil, snapshotErrorf("State generation lock must not be a symlink: %s", path)
	}
	return nil, err
}
