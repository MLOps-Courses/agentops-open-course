package freshness

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/conventions"
)

type fakeFetcher struct{}

func (fakeFetcher) JSON(_ context.Context, endpoint string, _ http.Header) (any, error) {
	if strings.Contains(endpoint, "/releases?") {
		tag := "v1.0.0"
		if strings.Contains(endpoint, "k3s-io") {
			tag = "v1.36.2+k3s1"
		} else if strings.Contains(endpoint, "ollama/ollama") {
			tag = "v0.32.5"
		} else if strings.Contains(endpoint, "jdx/mise") {
			tag = "v2026.7.18"
		}
		return []any{map[string]any{"tag_name": tag, "html_url": "https://example.test/release", "assets": []any{}}}, nil
	}
	// The Go module proxy answers every hand-moved module with a version the
	// manifests cannot be requiring, so the fixture asserts a REVIEW row rather
	// than whatever this repository happens to pin today.
	if strings.Contains(endpoint, "proxy.golang.org") {
		return map[string]any{"Version": "v99.0.0"}, nil
	}
	return map[string]any{"token": "fake"}, nil
}

func (fakeFetcher) Get(_ context.Context, _ string, _ http.Header) ([]byte, http.Header, error) {
	header := make(http.Header)
	header.Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
	return nil, header, nil
}

func TestReportIsDeterministicAndReadOnlyOffline(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Stub one real pin as behind, reading its version from mise.toml rather than
	// naming one: an empty map here used to render all 30 rows CURRENT, so this
	// fixture certified the bug it was supposed to catch, and a hard-coded version
	// would only re-break the fixture on the next pin bump.
	pins, err := misePins(root)
	if err != nil {
		t.Fatal(err)
	}
	const behind = "hugo-extended"
	pinned, ok := pins[behind]
	if !ok {
		t.Fatalf("mise.toml no longer pins %s; point this fixture at a tool it does pin", behind)
	}
	newer := pinned + "-newer"
	document, err := Report(context.Background(), Options{
		Root: root, RunID: "fixture-42", GeneratedAt: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
		Fetcher: fakeFetcher{},
		MiseOutdated: func(context.Context, string) (map[string]MiseUpdate, error) {
			return map[string]MiseUpdate{behind: {Requested: pinned, Latest: newer}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		"<!-- freshness-report:fixture-42 -->",
		"## Automated freshness snapshot — 2026-08-08",
		"### Go module authorities",
		"### Go module compatibility holds",
		"chromedp/cdproto",
		"tools/go.mod",
		"HELD",
		"### Hand-moved Go modules",
		"| `agents/go/go.mod` | `google.golang.org/adk/v2` | `" + requiredModuleVersion(root, "agents/go/go.mod", "google.golang.org/adk/v2") + "` | `v99.0.0` |",
		"REVIEW",
		"### Static external image pins",
		"| `" + behind + "` | `" + pinned + "` | `" + newer + "` | [mise registry](https://mise.jdx.dev/registry.html) |",
		"This reporter never updates a pin or opens a pull request.",
	} {
		if !strings.Contains(document, wanted) {
			t.Fatalf("report is missing %q", wanted)
		}
	}
	// The copied-prose gate must describe itself with the checker's own subset. A
	// literal sentence here would outlive the subset it describes the moment a
	// source-derived check joins or leaves CheckCopiedSources.
	summary := conventions.CopiedSourceSummary()
	if strings.Contains(document, "PASS — ") && !strings.Contains(document, "PASS — "+summary+" match their owners.") {
		t.Fatalf("copied-prose source gate did not name the checker subset %q", summary)
	}
}

func TestStaticImageInventoryUsesGoAgentDockerfile(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	images, err := staticImageReferences(root)
	if err != nil {
		t.Fatal(err)
	}
	wantedSources := map[string]bool{
		"agents/go/Dockerfile":           false,
		".github/workflows/platform.yml": false,
		"infra/scripts/gateway-host.sh":  false,
	}
	for _, sources := range images {
		for _, source := range sources {
			if _, ok := wantedSources[source]; ok {
				wantedSources[source] = true
			}
		}
	}
	for source, found := range wantedSources {
		if !found {
			t.Fatalf("static image inventory did not include %s", source)
		}
	}
}

func TestStaticImageInventoryIncludesWorkflowAndProductionInfraScriptAuthorities(t *testing.T) {
	root := t.TempDir()
	digest := strings.Repeat("a", 64)
	files := map[string]string{
		".github/workflows/platform.yml": "env:\n  CURL_IMAGE: curlimages/curl:8.21.0@sha256:" + digest + "\n",
		"infra/scripts/gateway-host.sh":  "readonly image=\"cr.agentgateway.dev/agentgateway:v1.4.1@sha256:" + digest + "\"\n",
		"infra/scripts/test-fixture.sh":  "FAKE_IMAGE=\"registry.invalid/not-an-authority:v1@sha256:" + digest + "\"\n",
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
	images, err := staticImageReferences(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"curlimages/curl:8.21.0@sha256:" + digest:                  ".github/workflows/platform.yml",
		"cr.agentgateway.dev/agentgateway:v1.4.1@sha256:" + digest: "infra/scripts/gateway-host.sh",
	}
	for reference, source := range want {
		if !slices.Contains(images[reference], source) {
			t.Fatalf("images[%q] = %#v, want source %q", reference, images[reference], source)
		}
	}
	for reference := range images {
		if strings.Contains(reference, "not-an-authority") {
			t.Fatalf("test fixture was inventoried as an authority: %q", reference)
		}
	}
}

// TestDependabotLeavesTheCompatibilityFamiliesAlone pins both halves of the
// agent's Dependabot contract, because either half alone is insufficient.
//
// Excluding a dependency from a group does not stop Dependabot from proposing
// it: it opens a separate pull request for it instead of folding it into the
// batch. Those four families are held at versions agents/go/go.mod records as
// structured compatibility-hold comments, which Dependabot never rewrites, so
// every such pull request fails check:freshness by construction — that is how
// five permanently red ones accumulated. The `ignore` list is what actually
// stops them; the `exclude-patterns` list keeps them out of the weekly batch if
// a hold is ever lifted. Both are asserted so neither can be dropped by hand.
//
// The /tools entry carries the same exposure through tools/go.mod's chromedp
// hold, so its `ignore` list is pinned too — cdproto for the constraint half and
// chromedp for the owner half, since a bump of either one alone turns the hold
// into a MISMATCH.
func TestDependabotLeavesTheCompatibilityFamiliesAlone(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, ".github", "dependabot.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Updates []struct {
			Groups map[string]struct {
				ExcludePatterns []string `yaml:"exclude-patterns"`
			} `yaml:"groups"`
			Ecosystem string `yaml:"package-ecosystem"`
			Directory string `yaml:"directory"`
			Ignore    []struct {
				DependencyName string `yaml:"dependency-name"`
			} `yaml:"ignore"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	var exclusions, ignored, toolsIgnored []string
	for _, update := range document.Updates {
		if update.Ecosystem != "gomod" {
			continue
		}
		var names []string
		for _, entry := range update.Ignore {
			names = append(names, entry.DependencyName)
		}
		switch update.Directory {
		case "/agents/go":
			exclusions = update.Groups["go-agent-dependencies"].ExcludePatterns
			ignored = names
		case "/tools":
			toolsIgnored = names
		}
	}
	for _, dependency := range []string{
		"github.com/openai/openai-go/v3",
		"go.opentelemetry.io/otel*",
		"google.golang.org/adk/v2",
		"google.golang.org/genai",
	} {
		if !slices.Contains(exclusions, dependency) {
			t.Errorf("agent dependency exclusions = %#v, want %q", exclusions, dependency)
		}
		if !slices.Contains(ignored, dependency) {
			t.Errorf("agent dependency ignore list = %#v, want %q", ignored, dependency)
		}
	}
	for _, dependency := range []string{
		"github.com/chromedp/cdproto",
		"github.com/chromedp/chromedp",
	} {
		if !slices.Contains(toolsIgnored, dependency) {
			t.Errorf("tool dependency ignore list = %#v, want %q", toolsIgnored, dependency)
		}
	}
}
