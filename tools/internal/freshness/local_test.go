package freshness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGoCompatibilityHoldRequiresPinnedOwnerAndValidator(t *testing.T) {
	root := t.TempDir()
	toolsDir := filepath.Join(root, "tools")
	if err := os.Mkdir(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	goMod := `module example.test/tools

go 1.26.5

require (
	github.com/chromedp/cdproto v0.0.0-20260714215040-dc233986426f // compatibility hold: owner=chromedp@v0.16.0 constraint=dc233986426f validator=real Chrome accessibility acceptance
	github.com/chromedp/chromedp v0.16.0
)
`
	if err := os.WriteFile(filepath.Join(toolsDir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}

	holds, err := goCompatibilityHolds(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 {
		t.Fatalf("holds = %d, want 1", len(holds))
	}
	if status := goCompatibilityHoldStatus(root, holds[0]); status != "HELD" {
		t.Fatalf("status = %q, want HELD", status)
	}

	holds[0].Owner = "chromedp@v0.17.0"
	if status := goCompatibilityHoldStatus(root, holds[0]); status != "MISMATCH" {
		t.Fatalf("status with unpinned owner = %q, want MISMATCH", status)
	}
	holds[0].Owner = "chromedp@v0.16.0"
	holds[0].Validator = ""
	if status := goCompatibilityHoldStatus(root, holds[0]); status != "MISMATCH" {
		t.Fatalf("status without validator = %q, want MISMATCH", status)
	}
}

func TestGoCompatibilityHoldsDiscoverEveryModuleAndCrossNamespaceOwner(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"agents/go/go.mod": `module example.test/agent

go 1.26.5

require (
	github.com/openai/openai-go/v3 v3.8.1 // compatibility hold: owner=google.golang.org/adk/v2@v2.1.0 constraint=v3.8.1 validator=agent check and test
	google.golang.org/adk/v2 v2.1.0
)
`,
		"tools/go.mod": `module example.test/tools

go 1.26.5

require (
	github.com/chromedp/cdproto v0.0.0-20260714215040-dc233986426f // compatibility hold: owner=github.com/chromedp/chromedp@v0.16.0 constraint=dc233986426f validator=real Chrome accessibility acceptance
	github.com/chromedp/chromedp v0.16.0
)
`,
	}
	for path, content := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	holds, err := goCompatibilityHolds(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 2 {
		t.Fatalf("holds = %#v, want two", holds)
	}
	for _, hold := range holds {
		if status := goCompatibilityHoldStatus(root, hold); status != "HELD" {
			t.Fatalf("%s status = %q, want HELD", hold.Module, status)
		}
	}
}

func TestGoCompatibilityHoldsRejectMalformedStructuredComment(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "evals")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	goMod := "module example.test/evals\n\ngo 1.26.5\n\nrequire example.test/held v1.0.0 // compatibility hold: owner=missing-fields\n"
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := goCompatibilityHolds(root); err == nil || !strings.Contains(err.Error(), "evals/go.mod:5: malformed compatibility hold") {
		t.Fatalf("goCompatibilityHolds error = %v", err)
	}
}

func TestRepositoryCompatibilityHoldsAreStructuredAndValidated(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	holds, err := goCompatibilityHolds(root)
	if err != nil {
		t.Fatal(err)
	}
	// One hold remains. ADK Go v2.3.0 moved its own logs onto the OTel 1.45 / log
	// 0.21 family, which retired the four holds agents/go carried through v2.2.0;
	// chromedp still owns cdproto, and that one is validated by a real Chrome run
	// rather than by a compiler, which is why it cannot lift on a version check.
	wanted := map[string]bool{
		"github.com/chromedp/cdproto": false,
	}
	for _, hold := range holds {
		if _, ok := wanted[hold.Module]; !ok {
			continue
		}
		if status := goCompatibilityHoldStatus(root, hold); status != "HELD" {
			t.Fatalf("%s status = %q, want HELD", hold.Module, status)
		}
		wanted[hold.Module] = true
	}
	for module, found := range wanted {
		if !found {
			t.Fatalf("repository compatibility holds omit %s", module)
		}
	}
}

// MiseResult reads an absent row as "nothing newer". That inference is sound
// only while mise is asked for the newest version rather than for whether the
// exact request is satisfied, so the flag that asks is a contract, not a detail.
func TestMiseOutdatedAsksForTheNewestVersion(t *testing.T) {
	if !slices.Contains(miseOutdatedArgs, "--bump") {
		t.Fatalf("miseOutdatedArgs = %v, want --bump so exact pins are resolved rather than rubber-stamped", miseOutdatedArgs)
	}
	if !slices.Contains(miseOutdatedArgs, "--json") {
		t.Fatalf("miseOutdatedArgs = %v, want --json for a parseable answer", miseOutdatedArgs)
	}
}

// A tool mise cannot resolve is omitted from the JSON while mise still exits 0,
// so the warning on stderr is the only evidence the question went unanswered.
// The fixture is mise 2026.9.3's real wording, truncated after the cause.
func TestUnresolvedMiseToolsNamesEveryPinMiseCouldNotAnswer(t *testing.T) {
	const diagnostics = "mise WARN  Failed to resolve tool version list for github:agentgateway/agentgateway: " +
		"[/repo/mise.toml] github:agentgateway/agentgateway@1.4.1: HTTP status client error (403 Forbidden)\n" +
		"mise WARN  Failed to resolve tool version list for age: [/repo/mise.toml] age@1.3.2: rate limited\n" +
		"mise WARN  something else entirely\n"
	got := unresolvedMiseTools(diagnostics)
	want := []string{"github:agentgateway/agentgateway", "age"}
	if !slices.Equal(got, want) {
		t.Fatalf("unresolvedMiseTools() = %v, want %v", got, want)
	}
	if names := unresolvedMiseTools("mise WARN  nothing this parser recognizes\n"); len(names) != 0 {
		t.Fatalf("unrecognized warning produced %v, want no names", names)
	}
}

// The two halves have to meet: an unresolved tool is recorded with no version,
// and MiseResult renders exactly that as the UNKNOWN gap rather than as CURRENT.
func TestAnUnresolvedPinRendersAsAGapRatherThanCurrent(t *testing.T) {
	update := MiseUpdate{}
	latest, status := MiseResult("1.4.1", &update, true)
	if latest != "unchecked" || status != "UNKNOWN" {
		t.Fatalf("unresolved pin = %q, %q, want unchecked/UNKNOWN", latest, status)
	}
}
