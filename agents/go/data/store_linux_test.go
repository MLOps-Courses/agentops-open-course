//go:build linux

package data

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDBPathRejectsNamedPipesWithoutOpeningThem(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"seed", "runtime"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			path := store.SeedPath()
			if target == "runtime" {
				if err := os.MkdirAll(store.StateDir(), stateDirPerm); err != nil {
					t.Fatalf("create state directory: %v", err)
				}
				path = store.RuntimePath()
			} else if err := os.Remove(path); err != nil {
				t.Fatalf("remove copied seed: %v", err)
			}
			if err := syscall.Mkfifo(filepath.Clean(path), 0o600); err != nil {
				t.Fatalf("create named pipe: %v", err)
			}

			_, err := store.DBPath()
			if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("DBPath() error = %v, want non-regular-file refusal", err)
			}
		})
	}
}
