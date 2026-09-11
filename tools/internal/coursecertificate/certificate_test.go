package coursecertificate

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/courseevidence"
)

// update rewrites the golden certificate. A deliberate flag rather than an
// environment variable, so regenerating evidence is something a maintainer types
// on purpose and shows up in the shell history that produced the diff.
var update = flag.Bool("update", false, "rewrite the golden certificate from the fixture manifest")

const goldenPath = "testdata/certificate.svg"

func fixtureInput(t *testing.T) Input {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "manifest.json"))
	if err != nil {
		t.Fatalf("reading the fixture manifest: %v", err)
	}
	manifest, err := courseevidence.Decode(content)
	if err != nil {
		t.Fatalf("the fixture manifest does not satisfy the evidence contract: %v", err)
	}
	return Input{
		Name:           "Ada Lovelace",
		Tier:           Gold,
		CourseVersion:  "0.7.0",
		ManifestDigest: Digest(content),
		Manifest:       manifest,
	}
}

// TestRenderMatchesTheGoldenCertificate is the whole point of a golden file here:
// the certificate is evidence, so any change to what it states — a moved line, a
// dropped disclosure, a different digest — has to arrive as a reviewable diff
// rather than as a silently different artifact learners are handing to people.
func TestRenderMatchesTheGoldenCertificate(t *testing.T) {
	rendered, err := Render(fixtureInput(t))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if *update {
		if writeErr := os.WriteFile(goldenPath, []byte(rendered), 0o600); writeErr != nil {
			t.Fatalf("writing the golden certificate: %v", writeErr)
		}
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading the golden certificate: %v", err)
	}
	if rendered != string(golden) {
		t.Errorf("Render() does not match %s; re-run with -update after reviewing the change", goldenPath)
	}
}

// TestRenderIsDeterministic states the property the certificate claims on its own
// face. It would fail the moment the renderer reached for a clock or a map range.
func TestRenderIsDeterministic(t *testing.T) {
	input := fixtureInput(t)
	first, err := Render(input)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	second, err := Render(input)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if first != second {
		t.Error("Render() produced two different documents from one input")
	}
}

// TestRenderNeedsNoNetwork is the automated half of the offline promise. The SVG
// namespace is the one absolute URL allowed, because renderers compare it as a
// string and never fetch it.
func TestRenderNeedsNoNetwork(t *testing.T) {
	rendered, err := Render(fixtureInput(t))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	withoutNamespace := strings.ReplaceAll(rendered, "http://www.w3.org/2000/svg", "")
	for _, forbidden := range []string{"http://", "https://", "@font-face", "<image", "<script", "url("} {
		if strings.Contains(withoutNamespace, forbidden) {
			t.Errorf("the certificate carries %q, which a browser would try to fetch or execute", forbidden)
		}
	}
}

// TestRenderEscapesTheHolderName covers the only place text from outside the
// repository reaches markup.
func TestRenderEscapesTheHolderName(t *testing.T) {
	input := fixtureInput(t)
	input.Name = `</text><script>alert(1)</script> & "Ada"`
	rendered, err := Render(input)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.Contains(rendered, "<script>") {
		t.Error("Render() emitted an unescaped script element from the holder name")
	}
	for _, wanted := range []string{"&lt;/text&gt;", "&amp;", "&#34;Ada&#34;"} {
		if !strings.Contains(rendered, wanted) {
			t.Errorf("Render() did not escape the holder name into %q", wanted)
		}
	}
}

func TestRenderStatesEveryCertifiedFact(t *testing.T) {
	input := fixtureInput(t)
	rendered, err := Render(input)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, wanted := range []string{
		"Ada Lovelace",
		"Gold tier",
		input.Manifest.Revision,
		input.Manifest.TreeDigest,
		input.ManifestDigest,
		"2026-08-13 UTC",
		"mise run check:core",
		"2 evidence artifacts hashed",
		"MLOps-Courses/agentops-open-course v0.7.0",
		"Nobody issued, graded, signed, or registered this certificate",
		"mise run course:evidence:verify",
	} {
		if !strings.Contains(rendered, wanted) {
			t.Errorf("the certificate does not state %q", wanted)
		}
	}
}

func TestRenderRejectsWhatItCannotStateHonestly(t *testing.T) {
	longName := strings.Repeat("a", maxNameRunes+1)
	cases := []struct {
		name    string
		mutate  func(*Input)
		wantErr string
	}{
		{"empty name", func(in *Input) { in.Name = "  " }, "needs a name"},
		{"control character", func(in *Input) { in.Name = "Ada\nLovelace" }, "control character"},
		{"name too long", func(in *Input) { in.Name = longName }, "room for"},
		{"unknown tier", func(in *Input) { in.Tier = Tier("Platinum") }, "not one of the capstone tiers"},
		{"missing version", func(in *Input) { in.CourseVersion = "" }, "VERSION"},
		{"short digest", func(in *Input) { in.ManifestDigest = "abcd" }, "hex SHA-256"},
		{"unparsable timestamp", func(in *Input) { in.Manifest.GeneratedAt = "yesterday" }, "RFC 3339"},
		{"no revision", func(in *Input) { in.Manifest.Revision = "" }, "no source revision"},
		{"no gates", func(in *Input) { in.Manifest.Gates = nil }, "no gates or artifacts"},
		{"no artifacts", func(in *Input) { in.Manifest.Artifacts = nil }, "no gates or artifacts"},
		{
			"gate command wider than the column",
			func(in *Input) { in.Manifest.Gates[0].Command = "mise run " + strings.Repeat("x", 120) },
			"certificate column is",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := fixtureInput(t)
			testCase.mutate(&input)
			_, err := Render(input)
			if err == nil {
				t.Fatalf("Render() accepted %s", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("Render() error = %v, want it to mention %q", err, testCase.wantErr)
			}
		})
	}
}

func TestParseTierAcceptsTheThreeCapstoneTiers(t *testing.T) {
	for value, wanted := range map[string]Tier{
		"bronze": Bronze, "Silver": Silver, " GOLD ": Gold,
	} {
		got, err := ParseTier(value)
		if err != nil || got != wanted {
			t.Errorf("ParseTier(%q) = %q, %v; want %q", value, got, err, wanted)
		}
	}
	if _, err := ParseTier("platinum"); err == nil {
		t.Error("ParseTier() invented a tier the capstone does not define")
	}
}

// TestNameFontSizeKeepsALongNameInsideTheColumn checks the one piece of geometry
// that is an estimate rather than a pinned width.
func TestNameFontSizeKeepsALongNameInsideTheColumn(t *testing.T) {
	for _, name := range []string{"Ada", "Ada Lovelace", strings.Repeat("W", maxNameRunes)} {
		size := nameFontSize(name)
		if width := 0.62 * size * float64(len([]rune(name))); width > contentWidth+0.001 {
			t.Errorf("name %q renders about %.0fpx wide in a %0.fpx column", name, width, contentWidth)
		}
	}
}

func TestWrapBreaksOnWordsWithinTheBudget(t *testing.T) {
	lines := wrap(disclosure(Gold), proseColumns)
	if len(lines) < 2 {
		t.Fatalf("wrap() returned %d lines, want the disclosure broken across several", len(lines))
	}
	for _, line := range lines {
		if len(line) > proseColumns {
			t.Errorf("wrap() produced a %d-character line over the %d budget: %q", len(line), proseColumns, line)
		}
	}
	if joined := strings.Join(lines, " "); joined != strings.Join(strings.Fields(disclosure(Gold)), " ") {
		t.Error("wrap() changed the disclosure text")
	}
}
