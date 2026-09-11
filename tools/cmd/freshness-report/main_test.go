package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunHelpMatchesSuccessfulCLIContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "-output") || !strings.Contains(stderr.String(), "-run-id") {
		t.Fatalf("help output is missing flags: %q", stderr.String())
	}
}
