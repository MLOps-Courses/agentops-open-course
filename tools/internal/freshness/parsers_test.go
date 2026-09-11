package freshness

import (
	"strings"
	"testing"
)

func TestLatestStableReleaseRejectsMislabeledBetaAndRC(t *testing.T) {
	document := []any{
		map[string]any{"tag_name": "v0.10.0-rc1", "html_url": "https://example.test/rc", "draft": false, "prerelease": false},
		map[string]any{"tag_name": "v0.10.0-beta11", "html_url": "https://example.test/beta", "draft": false, "prerelease": false},
		map[string]any{"tag_name": "v0.9.12", "html_url": "https://example.test/stable", "draft": false, "prerelease": false},
		map[string]any{"tag_name": "v0.9.13", "html_url": "https://example.test/draft", "draft": true, "prerelease": false},
	}
	release, ok := LatestStableRelease(document)
	if !ok || release.Tag != "v0.9.12" || release.URL != "https://example.test/stable" {
		t.Fatalf("release = %#v, %v", release, ok)
	}
}

func TestLatestStableK3sRevisionSortsNumerically(t *testing.T) {
	document := []any{
		map[string]any{"tag_name": "v1.35.6+k3s1", "html_url": "https://example.test/one"},
		map[string]any{"tag_name": "v1.35.6+k3s2", "html_url": "https://example.test/two"},
	}
	release, ok := LatestStableRelease(document)
	if !ok || release.Tag != "v1.35.6+k3s2" {
		t.Fatalf("release = %#v, %v", release, ok)
	}
}

func TestReleaseResultDetectsK3sAndOllamaDrift(t *testing.T) {
	if ReleaseResult("k3s", "v1.35.6-k3s1", "v1.36.2+k3s1") != "REVIEW" {
		t.Fatal("k3s drift accepted")
	}
	if ReleaseResult("Ollama", "v0.31.2", "v0.32.5") != "REVIEW" {
		t.Fatal("Ollama drift accepted")
	}
}

func TestParseMiseOutdatedKeepsActionableStrings(t *testing.T) {
	updates, err := ParseMiseOutdated(map[string]any{
		"hugo":   map[string]any{"requested": "0.11.0", "latest": "0.12.0", "current": "0.11.0"},
		"broken": map[string]any{"requested": nil, "latest": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates["hugo"] != (MiseUpdate{Requested: "0.11.0", Latest: "0.12.0"}) {
		t.Fatalf("updates = %#v", updates)
	}
}

func TestMiseResultDetectsDrift(t *testing.T) {
	latest, status := MiseResult("2.96.0", &MiseUpdate{Requested: "2.96.0", Latest: "2.97.0"}, true)
	if latest != "2.97.0" || status != "REVIEW" {
		t.Fatalf("result = %q, %q", latest, status)
	}
}

// A row that arrives carrying no version is mise answering without answering,
// and it must not read as a confirmation.
func TestMiseResultFlagsAnEmptyAnswer(t *testing.T) {
	latest, status := MiseResult("0.164.0", &MiseUpdate{Requested: "0.164.0"}, true)
	if latest != "unchecked" || status != "UNKNOWN" {
		t.Fatalf("result = %q, %q, want an unchecked/UNKNOWN gap", latest, status)
	}
}

// Absence means "nothing newer" only because miseOutdatedArgs carries --bump,
// and a failed mise run outranks every row.
func TestMiseResultReadsAbsenceAndUnavailability(t *testing.T) {
	if latest, status := MiseResult("0.164.0", nil, true); latest != "0.164.0" || status != "CURRENT" {
		t.Fatalf("absent result = %q, %q, want the pin reported CURRENT", latest, status)
	}
	if latest, status := MiseResult("0.164.0", &MiseUpdate{Requested: "0.164.0", Latest: "0.166.0"}, false); latest != "unavailable" || status != "UNAVAILABLE" {
		t.Fatalf("result = %q, %q, want UNAVAILABLE to win over any row", latest, status)
	}
}

func TestParseHelmChartsRetainsNamesSourcesAndDigests(t *testing.T) {
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	fixture := "# kagent-chart-version: 0.9.12\nreleases:\n  - name: kagent-crds\n    chart: oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds@sha256:" + first + "\n  - name: kagent\n    chart: oci://ghcr.io/kagent-dev/kagent/helm/kagent@sha256:" + second + "\n"
	version, charts := ParseHelmCharts(fixture)
	if version != "0.9.12" || len(charts) != 2 || charts[0].Name != "kagent-crds" || charts[1].Source != "ghcr.io/kagent-dev/kagent/helm/kagent" {
		t.Fatalf("version/charts = %q, %#v", version, charts)
	}
}

func TestOllamaAssetBindsURLAndChecksum(t *testing.T) {
	digest := strings.Repeat("f", 64)
	fixture := `archive="${RUNNER_TEMP}/ollama-linux-amd64.tar.zst"
curl "https://github.com/ollama/ollama/releases/download/v0.32.5/ollama-linux-amd64.tar.zst"
echo "` + digest + `  ${archive}" | sha256sum --check -
`
	pin, ok := OllamaAssetFromWorkflow(fixture)
	if !ok || pin.Tag != "v0.32.5" || pin.Name != "ollama-linux-amd64.tar.zst" || pin.Digest != "sha256:"+digest {
		t.Fatalf("pin = %#v, %v", pin, ok)
	}
}

func TestParseImageReferenceAppliesDockerLibraryDefault(t *testing.T) {
	digest := strings.Repeat("a", 64)
	image, ok := ParseImageReference("alpine:3.22@sha256:" + digest)
	if !ok || image.Registry != "docker.io" || image.Repository != "library/alpine" || image.Tag != "3.22" || image.Digest != "sha256:"+digest {
		t.Fatalf("image = %#v, %v", image, ok)
	}
}
