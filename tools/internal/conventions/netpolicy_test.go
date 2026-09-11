package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kagentGatewayRule is the cross-namespace rule the course promises is scoped
// by namespace *and* pod labels; the tests below widen it on purpose.
const kagentGatewayRule = "        - namespaceSelector:\n            matchLabels:\n              kubernetes.io/metadata.name: kagent\n" +
	"          podSelector:\n            matchLabels:\n              app.kubernetes.io/instance: kagent\n              app.kubernetes.io/component: controller\n"

func TestNetworkPolicyContractsMatchTheRepository(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	if problems := checkNetworkPolicyContracts(root, repositoryPages(t)); len(problems) != 0 {
		t.Fatalf("reviewed allow-matrix problems = %#v", problems)
	}
}

func TestCheckNetworkPolicyContractsRejectsPolicyDrift(t *testing.T) {
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
			name:        "widened port set",
			where:       basePolicies,
			anchor:      "        - { port: 8000, protocol: TCP }\n",
			replacement: "        - { port: 8000, protocol: TCP }\n        - { port: 9000, protocol: TCP }\n",
			want:        "NetworkPolicy mcp-ingress allow-matrix drifted",
		},
		{
			name:        "removed flow",
			where:       basePolicies,
			anchor:      "    - from:\n        - podSelector:\n            matchLabels:\n              app.kubernetes.io/name: otel-collector\n      ports:\n        - { port: 15020, protocol: TCP }\n",
			replacement: "",
			want:        "missing: in pod(app.kubernetes.io/name=otel-collector) TCP:15020",
		},
		{
			name:        "cross-namespace rule without pod labels",
			where:       basePolicies,
			anchor:      kagentGatewayRule,
			replacement: "        - namespaceSelector:\n            matchLabels:\n              kubernetes.io/metadata.name: kagent\n",
			want:        "extra: in ns(kubernetes.io/metadata.name=kagent) TCP:3000,TCP:4000",
		},
		{
			name:        "cross-namespace rule without a namespace",
			where:       basePolicies,
			anchor:      kagentGatewayRule,
			replacement: "        - podSelector:\n            matchLabels:\n              app.kubernetes.io/instance: kagent\n              app.kubernetes.io/component: controller\n",
			want:        "NetworkPolicy agentgateway-ingress allow-matrix drifted",
		},
		{
			name:        "drifted selector",
			where:       basePolicies,
			anchor:      "      app.kubernetes.io/name: agentops-mcp\n  policyTypes: [Ingress]",
			replacement: "      app.kubernetes.io/name: mcp\n  policyTypes: [Ingress]",
			want:        "NetworkPolicy mcp-ingress selector drifted",
		},
		{
			name:        "drifted policy types",
			where:       basePolicies,
			anchor:      "  policyTypes: [Ingress]",
			replacement: "  policyTypes: [Ingress, Egress]",
			want:        "NetworkPolicy default-deny-ingress policyTypes drifted",
		},
		{
			name:        "namespace-wide selector narrowed",
			where:       basePolicies,
			anchor:      "  podSelector: {}\n  policyTypes: [Ingress]",
			replacement: "  podSelector:\n    matchLabels:\n      app.kubernetes.io/name: tempo\n  policyTypes: [Ingress]",
			want:        "NetworkPolicy default-deny-ingress must select the whole namespace",
		},
		{
			name:        "relocated policy",
			where:       basePolicies,
			anchor:      "  name: dns-egress\n  namespace: agentops",
			replacement: "  name: dns-egress\n  namespace: default",
			want:        `NetworkPolicy dns-egress must live in the agentops namespace, found "default"`,
		},
		{
			name:        "missing policy",
			where:       localPolicies,
			anchor:      "  name: prometheus-ingress",
			replacement: "  name: prometheus-ingress-renamed",
			want:        "authoritative NetworkPolicy prometheus-ingress is missing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := copyManifests(t, "infra/k8s")
			mutateFile(t, root, test.where, test.anchor, test.replacement)
			messages := problemMessages(checkNetworkPolicyContracts(root, pages))
			if !strings.Contains(messages, test.want) {
				t.Fatalf("problems = %s", messages)
			}
		})
	}
}

func TestCheckNetworkPolicyContractsRejectsAnUnregisteredPolicy(t *testing.T) {
	t.Parallel()
	root := copyManifests(t, "infra/k8s")
	path := filepath.Join(root, filepath.FromSlash(basePolicies))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	extra := "---\napiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: shadow-ingress\n  namespace: agentops\nspec:\n  podSelector: {}\n  policyTypes: [Ingress]\n"
	if err := os.WriteFile(path, append(content, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
	messages := problemMessages(checkNetworkPolicyContracts(root, repositoryPages(t)))
	if !strings.Contains(messages, "NetworkPolicy inventory drifted") || !strings.Contains(messages, "unregistered: shadow-ingress") {
		t.Fatalf("problems = %s", messages)
	}
}

func TestCheckAllowMatrixSymmetryRejectsAOneSidedFlow(t *testing.T) {
	t.Parallel()
	symmetric := map[string]networkPolicyContract{
		"alpha-egress": {
			path: basePolicies, selector: "alpha", types: []string{"Egress"},
			egress: []networkFlow{{peer: podPeer("beta"), ports: tcp(9000)}},
		},
		"beta-ingress": {
			path: basePolicies, selector: "beta", types: []string{"Ingress"},
			ingress: []networkFlow{{peer: podPeer("alpha"), ports: tcp(9000)}},
		},
	}
	if problems := checkAllowMatrixSymmetry(symmetric); len(problems) != 0 {
		t.Fatalf("symmetric matrix problems = %#v", problems)
	}
	delete(symmetric, "beta-ingress")
	messages := problemMessages(checkAllowMatrixSymmetry(symmetric))
	if !strings.Contains(messages, "one-sided") || !strings.Contains(messages, "egress without ingress: alpha -> beta TCP:9000") {
		t.Fatalf("one-sided matrix problems = %s", messages)
	}
}

func TestCheckDeniedIngressRejectsTheA2AAndTempoQueryListeners(t *testing.T) {
	t.Parallel()
	contracts := map[string]networkPolicyContract{
		"gateway-ingress": {
			path: basePolicies, selector: "agentgateway", types: []string{"Ingress"},
			ingress: []networkFlow{{peer: podPeer("agentops-agent"), ports: tcp(3001)}},
		},
		"tempo-ingress": {
			path: basePolicies, selector: "tempo", types: []string{"Ingress"},
			ingress: []networkFlow{{peer: podPeer("otel-collector"), ports: tcp(3200)}},
		},
	}
	messages := problemMessages(checkDeniedIngress(contracts))
	if !strings.Contains(messages, "NetworkPolicy gateway-ingress admits :3001") {
		t.Fatalf("A2A problems = %s", messages)
	}
	if !strings.Contains(messages, "NetworkPolicy tempo-ingress admits :3200") {
		t.Fatalf("Tempo query problems = %s", messages)
	}
}

func TestCheckAllowMatrixRowsBindsThePublishedTableToTheRegistry(t *testing.T) {
	t.Parallel()
	pages := repositoryPages(t)
	if problems := checkAllowMatrixRows(networkPolicyContracts, pages); len(problems) != 0 {
		t.Fatalf("reviewed allow-matrix row problems = %#v", problems)
	}
	tests := []struct {
		name        string
		anchor      string
		replacement string
		want        string
	}{
		{
			name:        "undocumented port",
			anchor:      "`otel-collector` `:15020`; ",
			replacement: "",
			want:        "allow-matrix row agentgateway omits :15020, which its ingress rules open",
		},
		{
			name:        "invented port",
			anchor:      "| `tempo`          | DNS only",
			replacement: "| `tempo`          | collector `:9999`",
			want:        "allow-matrix row tempo documents :9999, which no egress rule opens",
		},
		{
			name:        "removed row",
			anchor:      "| `loki`           | DNS only",
			replacement: "| ~~loki~~        | DNS only",
			want:        "allow-matrix rows drifted (missing: loki",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutated := mutatePage(t, pages, matrixWhere, test.anchor, test.replacement)
			messages := problemMessages(checkAllowMatrixRows(networkPolicyContracts, mutated))
			if !strings.Contains(messages, test.want) {
				t.Fatalf("problems = %s", messages)
			}
		})
	}
}

// The "nobody on :3001" clause is a promise about the manifest, so widening the
// registry — not the page — has to be what breaks it.
func TestCheckAllowMatrixRowsRejectsAdmittingADeniedListener(t *testing.T) {
	t.Parallel()
	contracts := make(map[string]networkPolicyContract, len(networkPolicyContracts))
	for name, contract := range networkPolicyContracts {
		contracts[name] = contract
	}
	admitted := contracts["agentgateway-ingress"]
	admitted.ingress = append(append([]networkFlow{}, admitted.ingress...), networkFlow{peer: podPeer("agentops-agent"), ports: tcp(3001)})
	contracts["agentgateway-ingress"] = admitted
	messages := problemMessages(checkAllowMatrixRows(contracts, repositoryPages(t)))
	if !strings.Contains(messages, "allow-matrix row agentgateway promises nobody reaches :3001, but its ingress rules open it") {
		t.Fatalf("problems = %s", messages)
	}
}

func TestCheckAllowMatrixDiagramRejectsMissingAndUnknownEdges(t *testing.T) {
	t.Parallel()
	pages := repositoryPages(t)
	if problems := checkAllowMatrixDiagram(networkPolicyContracts, pages); len(problems) != 0 {
		t.Fatalf("reviewed allow-matrix diagram problems = %#v", problems)
	}
	dropped := mutatePage(t, pages, matrixWhere, `    OT -->|":15020"| GW`+"\n", "")
	messages := problemMessages(checkAllowMatrixDiagram(networkPolicyContracts, dropped))
	if !strings.Contains(messages, "allow-matrix diagram drifted") || !strings.Contains(messages, "missing: otel-collector -> agentgateway TCP:15020") {
		t.Fatalf("dropped edge problems = %s", messages)
	}
	renamed := mutatePage(t, pages, matrixWhere, `    OT -->|":3100"| LK`, `    OT -->|":3100"| LOKI`)
	messages = problemMessages(checkAllowMatrixDiagram(networkPolicyContracts, renamed))
	if !strings.Contains(messages, "allow-matrix diagram names unknown node") {
		t.Fatalf("unknown node problems = %s", messages)
	}
}
