package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureManifest is the same file the renderer's golden test uses, so the CLI
// and the renderer cannot drift apart over what a valid manifest looks like.
const fixtureManifest = "../../internal/coursecertificate/testdata/manifest.json"

func TestExecuteWritesACertificateAndNamesTheDigestItUsed(t *testing.T) {
	output := filepath.Join(t.TempDir(), "nested", "certificate.svg")
	var printed bytes.Buffer
	arguments := []string{
		"--name", "Ada Lovelace", "--tier", "gold",
		"--manifest", fixtureManifest, "--output", output,
	}
	if err := execute(arguments, &printed); err != nil {
		t.Fatalf("execute() error = %v", err)
	}
	rendered, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	if !strings.HasPrefix(string(rendered), "<?xml") {
		t.Errorf("the certificate does not start as an SVG document:\n%.80s", rendered)
	}
	// The printed digest is what a reader retypes into `sha256sum`, so the command
	// has to print the same string it wrote onto the certificate.
	digest, found := strings.CutPrefix(strings.TrimSpace(lastLine(printed.String())), "course certificate: manifest sha256 ")
	if !found {
		t.Fatalf("execute() printed %q, want the manifest digest on the last line", printed.String())
	}
	if !strings.Contains(string(rendered), digest) {
		t.Errorf("the certificate does not carry the printed digest %q", digest)
	}
	if !strings.Contains(printed.String(), output) {
		t.Errorf("execute() printed %q, want it to name %s", printed.String(), output)
	}
}

func TestExecuteRefusesIncompleteOrUnknownArguments(t *testing.T) {
	cases := map[string][]string{
		"missing tier":        {"--name", "Ada Lovelace", "--manifest", fixtureManifest},
		"missing name":        {"--tier", "gold", "--manifest", fixtureManifest},
		"unknown tier":        {"--name", "Ada", "--tier", "platinum", "--manifest", fixtureManifest},
		"missing manifest":    {"--name", "Ada", "--tier", "gold", "--manifest", "testdata/absent.json"},
		"positional argument": {"--name", "Ada", "--tier", "gold", "--manifest", fixtureManifest, "extra"},
	}
	for name, arguments := range cases {
		t.Run(name, func(t *testing.T) {
			var printed bytes.Buffer
			if err := execute(append(arguments, "--output", filepath.Join(t.TempDir(), "out.svg")), &printed); err == nil {
				t.Errorf("execute() accepted %s", name)
			}
		})
	}
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines[len(lines)-1]
}
