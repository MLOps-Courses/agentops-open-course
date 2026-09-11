package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probeManifests are the directories the probe registry reads: the workload
// manifests it owns plus the BYO Agent it deliberately leaves unprobed.
var probeManifests = []string{"infra/k8s", "infra/kagent"}

func TestProbeContractsMatchTheRepository(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	if problems := checkProbeContracts(root, repositoryPages(t)); len(problems) != 0 {
		t.Fatalf("reviewed probe problems = %#v", problems)
	}
}

func TestCheckProbeContractsRejectsHandlerAndTargetDrift(t *testing.T) {
	t.Parallel()
	pages := repositoryPages(t)
	tests := []struct {
		name        string
		where       string
		anchor      string
		replacement string
		want        string
	}{
		{
			name:        "tcp socket handler",
			where:       "infra/k8s/base/agentgateway.yaml",
			anchor:      "          readinessProbe:\n            httpGet:\n              path: /healthz/ready\n              port: readiness\n",
			replacement: "          readinessProbe:\n            tcpSocket:\n              port: readiness\n",
			want:        "must use httpGet",
		},
		{
			name:        "bare port number",
			where:       "infra/k8s/base/agentgateway.yaml",
			anchor:      "port: readiness",
			replacement: "port: 15021",
			want:        "not a bare number",
		},
		{
			name:        "drifted path",
			where:       "infra/k8s/base/mcp.yaml",
			anchor:      "path: /healthz\n",
			replacement: "path: /ready\n",
			want:        `readinessProbe path drifted: expected "/healthz", found "/ready"`,
		},
		{
			name:        "missing probe",
			where:       "infra/k8s/base/agentgateway.yaml",
			anchor:      "          readinessProbe:\n",
			replacement: "          unwiredProbe:\n",
			want:        "authoritative readinessProbe httpGet /healthz/ready for agentgateway is missing",
		},
		{
			name:        "unreviewed startup probe",
			where:       "infra/k8s/base/agentgateway.yaml",
			anchor:      "          readinessProbe:\n",
			replacement: "          startupProbe:\n            httpGet:\n              path: /healthz/ready\n              port: readiness\n          readinessProbe:\n",
			want:        "must not declare a startupProbe",
		},
		{
			name:        "renumbered container port",
			where:       "infra/k8s/base/agentgateway.yaml",
			anchor:      "            - name: readiness\n              containerPort: 15021\n",
			replacement: "            - name: readiness\n              containerPort: 15022\n",
			want:        "must be containerPort 15021, found 15022",
		},
		{
			name:        "removed named port",
			where:       "infra/k8s/base/agentgateway.yaml",
			anchor:      "            - name: readiness\n              containerPort: 15021\n",
			replacement: "",
			want:        `agentgateway declares no named container port "readiness"`,
		},
		{
			name:        "probe port published by the Service",
			where:       "infra/k8s/base/agentgateway.yaml",
			anchor:      "    - name: metrics\n      port: 15020\n      targetPort: metrics\n",
			replacement: "    - name: metrics\n      port: 15020\n      targetPort: metrics\n    - name: readiness\n      port: 15021\n      targetPort: readiness\n",
			want:        `must not publish probe port "readiness"`,
		},
		{
			name:        "probe port dropped from the Service",
			where:       "infra/k8s/base/mcp.yaml",
			anchor:      "    - name: mcp\n      port: 8000\n      targetPort: mcp\n",
			replacement: "",
			want:        `must keep publishing probe port "mcp"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := copyManifests(t, probeManifests...)
			mutateFile(t, root, test.where, test.anchor, test.replacement)
			messages := problemMessages(checkProbeContracts(root, pages))
			if !strings.Contains(messages, test.want) {
				t.Fatalf("problems = %s", messages)
			}
		})
	}
}

func TestCheckProbeContractsRejectsAnUnregisteredDeployment(t *testing.T) {
	t.Parallel()
	root := copyManifests(t, probeManifests...)
	manifest := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: shadow\n  namespace: agentops\nspec:\n  template:\n    spec:\n      containers:\n        - name: shadow\n"
	if err := os.WriteFile(filepath.Join(root, "infra", "k8s", "base", "shadow.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	messages := problemMessages(checkProbeContracts(root, repositoryPages(t)))
	if !strings.Contains(messages, "every Deployment must own a reviewed probe contract") || !strings.Contains(messages, "unregistered: shadow") {
		t.Fatalf("problems = %s", messages)
	}
}

func TestCheckProbeContractsRejectsProbesOnTheBYOAgent(t *testing.T) {
	t.Parallel()
	root := copyManifests(t, probeManifests...)
	mutateFile(t, root, "infra/kagent/agent.yaml", "      resources:\n", "      readinessProbe:\n        httpGet:\n          path: /healthz\n      resources:\n")
	messages := problemMessages(checkProbeContracts(root, repositoryPages(t)))
	if !strings.Contains(messages, "the BYO Agent must not declare readinessProbe") {
		t.Fatalf("problems = %s", messages)
	}
}

func TestCheckProbeContractsRejectsGatewayProseDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		anchor      string
		replacement string
		want        string
	}{
		{name: "port", anchor: "`:15021`", replacement: "`:15020`", want: `must cite "` + "`:15021`" + `"`},
		{name: "path", anchor: "`GET /healthz/ready`", replacement: "`GET /healthz`", want: `must cite "` + "`GET /healthz/ready`" + `"`},
		{name: "service", anchor: "deliberately absent from the Service", replacement: "published by the Service", want: `must cite "absent from the Service"`},
	}
	root := repositoryRoot(t)
	pages := repositoryPages(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutated := mutatePage(t, pages, gatewayProbeWhere, test.anchor, test.replacement)
			messages := problemMessages(checkProbeContracts(root, mutated))
			if !strings.Contains(messages, test.want) {
				t.Fatalf("problems = %s", messages)
			}
		})
	}
}

func TestCheckProbeContractsRejectsProbeTableDrift(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	pages := repositoryPages(t)
	unwired := mutatePage(t, pages, probeTableWhere, "Yes — `startupProbe`, `readinessProbe`, `livenessProbe`", "No")
	if messages := problemMessages(checkProbeContracts(root, unwired)); !strings.Contains(messages, "must name `startupProbe` for agentops-mcp") {
		t.Fatalf("unwired MCP row problems = %s", messages)
	}
	// Mutate the shortest phrase that is load-bearing rather than the whole cell:
	// the row's wording is prose and has already been rewritten once, while what
	// the checker actually reads is whether the row names a probe field.
	claimed := mutatePage(t, pages, probeTableWhere, "exposes no probe fields", "wires `readinessProbe`")
	if messages := problemMessages(checkProbeContracts(root, claimed)); !strings.Contains(messages, "must keep the BYO A2A workload unwired") {
		t.Fatalf("claimed BYO row problems = %s", messages)
	}
}
