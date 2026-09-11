//go:build unix

package safefile

import (
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A symlink and a directory are the refusals a portable test can build. These
// two are the ones that show why the inspection has to come before the open at
// all: opening a FIFO with no writer blocks indefinitely, and opening a socket
// as a file fails with an error that says nothing about a hostile path. Neither
// is a hypothetical — the state directory is a mount point an operator controls.
func TestOpenRefusesAFinalEntryThatIsNotAFileAtAll(t *testing.T) {
	tests := []struct {
		plant func(t *testing.T, path string)
		name  string
	}{
		{
			name: "named pipe",
			plant: func(t *testing.T, path string) {
				if err := syscall.Mkfifo(filepath.Clean(path), 0o600); err != nil {
					t.Skipf("create a named pipe on this platform: %v", err)
				}
			},
		},
		{
			name: "unix socket",
			plant: func(t *testing.T, path string) {
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Skipf("create a unix socket on this platform: %v", err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, path := hostileParent(t)
			test.plant(t, path)

			// Reaching this assertion at all is half the result: a refusal that
			// happened after the open would still be hanging on the pipe.
			if err := refuse(t, path); !strings.Contains(err.Error(), "must be a regular file") {
				t.Fatalf("Open() error = %v, want the no-follow regular-file refusal", err)
			}
		})
	}
}
