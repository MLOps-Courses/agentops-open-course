package conventions

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPinnedADKTelemetrySpanMetricsContract(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// Inspect the selected module instead of copying ADK's explicitly unstable
	// telemetry contract into course prose or collector configuration.
	command := exec.Command("go", "list", "-mod=readonly", "-m", "-f", "{{.Version}}\n{{.Dir}}", "google.golang.org/adk/v2")
	command.Dir = filepath.Join(root, "agents", "go")
	command.Env = append(os.Environ(), "GOPROXY=off")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("locate installed ADK without network access: %v", err)
	}
	selected := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)
	if len(selected) != 2 || selected[0] != "v2.3.0" {
		t.Fatalf("selected ADK = %q; review the telemetry compatibility contract before changing v2.3.0", strings.TrimSpace(string(output)))
	}
	implementation, err := os.ReadFile(filepath.Join(selected[1], "internal", "telemetry", "telemetry.go"))
	if err != nil {
		t.Fatalf("read installed ADK telemetry implementation: %v", err)
	}
	source := string(implementation)
	for _, token := range []string{
		`systemName = "gcp.vertex.agent"`,
		"if err == nil {\n\t\treturn\n\t}",
		"span.RecordError(err)",
		"span.SetStatus(codes.Error, err.Error())",
	} {
		if !strings.Contains(source, token) {
			t.Fatalf("installed ADK telemetry no longer contains %q; review its scope and status behavior", token)
		}
	}
	if strings.Contains(source, `attribute.Key("error.type")`) {
		t.Fatal("installed ADK now emits error.type; review the bounded span-metric dimensions before adding it")
	}

	wantDimensions := []string{"gen_ai.operation.name", "gen_ai.request.model"}
	for _, where := range []string{
		"infra/observability/otel-collector.yaml",
		"infra/k8s/base/otel-collector-config.yaml",
	} {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(where)))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Connectors map[string]struct {
				Dimensions []struct {
					Name string `yaml:"name"`
				} `yaml:"dimensions"`
			} `yaml:"connectors"`
		}
		if err := yaml.Unmarshal(content, &document); err != nil {
			t.Fatalf("parse %s: %v", where, err)
		}
		dimensions := document.Connectors["span_metrics"].Dimensions
		got := make([]string, 0, len(dimensions))
		for _, dimension := range dimensions {
			got = append(got, dimension.Name)
		}
		if !slices.Equal(got, wantDimensions) {
			t.Errorf("%s span-metric dimensions = %v, want installed-ADK-supported %v", where, got, wantDimensions)
		}
	}

	for _, where := range []string{
		"content/7. Observability/7.1. Tracing.md",
		"content/7. Observability/7.2. Monitoring.md",
	} {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(where)))
		if err != nil {
			t.Fatal(err)
		}
		for _, stale := range []string{
			"google.adk.telemetry",
			"- name: error.type",
			"`error.type` attribute",
		} {
			if strings.Contains(string(content), stale) {
				t.Errorf("%s copies unsupported ADK telemetry detail %q", where, stale)
			}
		}
	}
}
