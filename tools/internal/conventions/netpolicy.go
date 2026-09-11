package conventions

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// networkPeer is one end of a declared flow. At most one of pod/cidr is set;
// namespace is nil for an in-namespace peer and carries the namespaceSelector's
// labels when the rule crosses namespaces.
type networkPeer struct {
	namespace map[string]string // namespaceSelector.matchLabels
	pod       map[string]string // podSelector.matchLabels
	cidr      string            // ipBlock.cidr
}

// networkPort keeps the protocol explicit: dns-egress opens :53 on both UDP and
// TCP, so a bare port set would silently accept half of that rule.
type networkPort struct {
	protocol string
	number   int
}

// networkFlow is one directed, port-scoped allowance.
type networkFlow struct {
	peer  networkPeer
	ports []networkPort
}

// networkPolicyContract is one NetworkPolicy: the workload it selects and the
// exact set of flows it opens. Nil ingress and egress mean the policy
// deliberately opens nothing, which is how the default-deny pair and
// prometheus-ingress do their work.
type networkPolicyContract struct {
	path     string   // repository-relative manifest that owns the policy
	selector string   // app.kubernetes.io/name it selects; "" for the namespace-wide {} selector
	types    []string // spec.policyTypes, exactly as written
	ingress  []networkFlow
	egress   []networkFlow
}

const (
	policyNamespace = "agentops"
	basePolicies    = "infra/k8s/base/network-policies.yaml"
	localPolicies   = "infra/k8s/overlays/local/network-policies.yaml"
	matrixWhere     = "content/6. Platform/6.5. Platform Gateway.md"
)

func podPeer(name string) networkPeer {
	return networkPeer{pod: map[string]string{"app.kubernetes.io/name": name}}
}

// crossNamespace names another namespace by the label Kubernetes injects on
// every namespace object.
func crossNamespace(name string) map[string]string {
	return map[string]string{"kubernetes.io/metadata.name": name}
}

func tcp(numbers ...int) []networkPort {
	ports := make([]networkPort, 0, len(numbers))
	for _, number := range numbers {
		ports = append(ports, networkPort{number: number, protocol: "TCP"})
	}
	return ports
}

// kagentController is the only cross-namespace caller. Both selectors must
// match, so an unrelated pod in either namespace stays denied; dropping either
// half widens the rule to a whole namespace and shows up as matrix drift.
var kagentController = networkPeer{
	namespace: crossNamespace("kagent"),
	pod: map[string]string{
		"app.kubernetes.io/instance":  "kagent",
		"app.kubernetes.io/component": "controller",
	},
}

var kubeSystem = networkPeer{namespace: crossNamespace("kube-system")}

// networkPolicyContracts is keyed by NetworkPolicy metadata.name, which one
// namespace cannot hold twice.
//
// It covers declared policy documents only. The per-overlay model exceptions
// live as inline JSON-patch strings in infra/k8s/overlays/local/kustomization.yaml
// and infra/k8s/overlays/gke/kustomization.yaml — the GKE one still carrying an
// unresolved placeholder that only infra/scripts/render-gke.sh substitutes — so
// they are asserted on the rendered overlays by scripts/check-infra.sh instead.
var networkPolicyContracts = map[string]networkPolicyContract{
	"default-deny-ingress": {path: basePolicies, types: []string{"Ingress"}},
	"default-deny-egress":  {path: basePolicies, types: []string{"Egress"}},
	"dns-egress": {
		path: basePolicies, types: []string{"Egress"},
		egress: []networkFlow{{peer: kubeSystem, ports: []networkPort{
			{number: 53, protocol: "UDP"},
			{number: 53, protocol: "TCP"},
		}}},
	},
	"agentgateway-ingress": {
		path: basePolicies, selector: "agentgateway", types: []string{"Ingress"},
		ingress: []networkFlow{
			{peer: podPeer("agentops-agent"), ports: tcp(3000, 4000)},
			{peer: podPeer("otel-collector"), ports: tcp(15020)},
			{peer: kagentController, ports: tcp(3000, 4000)},
		},
	},
	"mcp-ingress": {
		path: basePolicies, selector: "agentops-mcp", types: []string{"Ingress"},
		ingress: []networkFlow{{peer: podPeer("agentgateway"), ports: tcp(8000)}},
	},
	"agent-a2a-ingress": {
		path: basePolicies, selector: "agentops-agent", types: []string{"Ingress"},
		ingress: []networkFlow{{peer: podPeer("agentgateway"), ports: tcp(8080)}},
	},
	"tempo-ingress": {
		path: basePolicies, selector: "tempo", types: []string{"Ingress"},
		ingress: []networkFlow{{peer: podPeer("otel-collector"), ports: tcp(4318)}},
	},
	"otel-collector-ingress": {
		path: basePolicies, selector: "otel-collector", types: []string{"Ingress"},
		ingress: []networkFlow{
			{peer: podPeer("agentgateway"), ports: tcp(4317)},
			{peer: podPeer("agentops-agent"), ports: tcp(4318)},
			{peer: kagentController, ports: tcp(4317)},
		},
	},
	"loki-ingress": {
		path: basePolicies, selector: "loki", types: []string{"Ingress"},
		ingress: []networkFlow{{peer: podPeer("otel-collector"), ports: tcp(3100)}},
	},
	"agentgateway-egress": {
		path: basePolicies, selector: "agentgateway", types: []string{"Egress"},
		egress: []networkFlow{
			{peer: podPeer("agentops-mcp"), ports: tcp(8000)},
			{peer: podPeer("agentops-agent"), ports: tcp(8080)},
			{peer: podPeer("otel-collector"), ports: tcp(4317)},
		},
	},
	"agent-egress": {
		path: basePolicies, selector: "agentops-agent", types: []string{"Egress"},
		egress: []networkFlow{
			{peer: podPeer("agentgateway"), ports: tcp(3000, 4000)},
			{peer: podPeer("otel-collector"), ports: tcp(4318)},
		},
	},
	"otel-collector-egress": {
		path: basePolicies, selector: "otel-collector", types: []string{"Egress"},
		egress: []networkFlow{
			{peer: podPeer("tempo"), ports: tcp(4318)},
			{peer: podPeer("loki"), ports: tcp(3100)},
			{peer: podPeer("agentgateway"), ports: tcp(15020)},
		},
	},
	"otel-collector-metrics-ingress": {
		path: localPolicies, selector: "otel-collector", types: []string{"Ingress"},
		ingress: []networkFlow{{peer: podPeer("prometheus"), ports: tcp(8889)}},
	},
	"prometheus-ingress": {path: localPolicies, selector: "prometheus", types: []string{"Ingress"}},
	"prometheus-egress": {
		path: localPolicies, selector: "prometheus", types: []string{"Egress"},
		egress: []networkFlow{
			{peer: podPeer("otel-collector"), ports: tcp(8889)},
			{peer: podPeer("alertmanager"), ports: tcp(9093)},
		},
	},
	"alertmanager-ingress": {
		path: localPolicies, selector: "alertmanager", types: []string{"Ingress"},
		ingress: []networkFlow{{peer: podPeer("prometheus"), ports: tcp(9093)}},
	},
}

// deniedIngressPorts are listeners the course promises no pod may enter. They
// are checked against the registry rather than against a rendered overlay, so
// the promise is reviewable in the same table a reader is looking at.
var deniedIngressPorts = map[int]string{
	3001: "the raw A2A listener is reached through the governed gateway service, never pod to pod",
	3200: "Tempo's query API stays reachable only through kubectl port-forward",
}

// matrixDiagramPods maps every mermaid node id on the allow-matrix page to the
// pod it draws. The agent appears twice (as the workload and as its A2A
// surface), and an empty value marks a node that is outside the namespace and
// therefore outside the symmetric in-namespace edge set.
var matrixDiagramPods = map[string]string{
	"GW":     "agentgateway",
	"AG":     "agentops-agent",
	"A2A":    "agentops-agent",
	"MCP":    "agentops-mcp",
	"OT":     "otel-collector",
	"TP":     "tempo",
	"LK":     "loki",
	"kagent": "",
	"Public": "",
}

var (
	matrixRowPattern  = regexp.MustCompile("(?m)^\\| `([a-z-]+)` *\\|([^|]*)\\|([^|]*)\\|\\s*$")
	matrixEdgePattern = regexp.MustCompile(`(?m)^\s*(\w+)(?:\[[^]]*\])?\s*-->\|"([^"]*)"\|\s*(\w+)`)
	matrixPortPattern = regexp.MustCompile(`:(\d+)`)
)

type manifestLabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type manifestPolicyPeer struct {
	PodSelector       *manifestLabelSelector `yaml:"podSelector"`
	NamespaceSelector *manifestLabelSelector `yaml:"namespaceSelector"`
	IPBlock           *struct {
		CIDR string `yaml:"cidr"`
	} `yaml:"ipBlock"`
}

// NetworkPolicy ports are IntOrString as well, but this repository never names
// one; keeping the field an int makes a future named port fail loudly at decode
// instead of being read as zero and compared as a real rule.
type manifestPolicyPort struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}

type manifestPolicyRule struct {
	From  []manifestPolicyPeer `yaml:"from"`
	To    []manifestPolicyPeer `yaml:"to"`
	Ports []manifestPolicyPort `yaml:"ports"`
}

type manifestNetworkPolicy struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		PodSelector manifestLabelSelector `yaml:"podSelector"`
		PolicyTypes []string              `yaml:"policyTypes"`
		Ingress     []manifestPolicyRule  `yaml:"ingress"`
		Egress      []manifestPolicyRule  `yaml:"egress"`
	} `yaml:"spec"`
}

func checkNetworkPolicyContracts(root string, pages pageSet) []Problem {
	var problems []Problem
	policies := make(map[string]map[string]manifestNetworkPolicy, 2)
	for _, path := range []string{basePolicies, localPolicies} {
		documents, err := decodeManifests[manifestNetworkPolicy](root, path)
		if err != nil {
			problems = append(problems, problem(path, "could not read the NetworkPolicy owner: %v", err))
			continue
		}
		indexed := make(map[string]manifestNetworkPolicy, len(documents))
		for _, document := range documents {
			if document.Kind == "NetworkPolicy" {
				indexed[document.Metadata.Name] = document
			}
		}
		policies[path] = indexed
	}
	for _, name := range sortedRegistryKeys(networkPolicyContracts) {
		contract := networkPolicyContracts[name]
		indexed, read := policies[contract.path]
		if !read {
			continue // the owner failed to decode; one problem per file is enough
		}
		document, found := indexed[name]
		if !found {
			problems = append(problems, problem(contract.path, "authoritative NetworkPolicy %s is missing", name))
			continue
		}
		problems = append(problems, checkPolicyDocument(name, contract, document)...)
	}
	problems = append(problems, checkPolicyInventory(policies)...)
	problems = append(problems, checkAllowMatrixSymmetry(networkPolicyContracts)...)
	problems = append(problems, checkDeniedIngress(networkPolicyContracts)...)
	problems = append(problems, checkAllowMatrixRows(networkPolicyContracts, pages)...)
	problems = append(problems, checkAllowMatrixDiagram(networkPolicyContracts, pages)...)
	return problems
}

func checkPolicyDocument(name string, contract networkPolicyContract, document manifestNetworkPolicy) []Problem {
	var problems []Problem
	if document.Metadata.Namespace != policyNamespace {
		problems = append(problems, problem(contract.path, "NetworkPolicy %s must live in the %s namespace, found %q", name, policyNamespace, document.Metadata.Namespace))
	}
	labels := document.Spec.PodSelector.MatchLabels
	switch {
	case contract.selector == "" && len(labels) != 0:
		problems = append(problems, problem(contract.path, "NetworkPolicy %s must select the whole namespace, found {%s}", name, labelKey(labels)))
	case contract.selector != "" && (len(labels) != 1 || labels["app.kubernetes.io/name"] != contract.selector):
		problems = append(problems, problem(contract.path, "NetworkPolicy %s selector drifted: expected app.kubernetes.io/name=%s, found {%s}", name, contract.selector, labelKey(labels)))
	}
	if !slices.Equal(document.Spec.PolicyTypes, contract.types) {
		problems = append(problems, problem(contract.path, "NetworkPolicy %s policyTypes drifted: expected %v, found %v", name, contract.types, document.Spec.PolicyTypes))
	}
	wanted := contractFlowKeys(contract)
	found := manifestFlowKeys(document)
	if !mapsEqual(found, wanted) {
		problems = append(problems, problem(contract.path, "NetworkPolicy %s allow-matrix drifted (missing: %s; extra: %s)", name,
			formatSet(sortedDifference(wanted, found)), formatSet(sortedDifference(found, wanted))))
	}
	return problems
}

func checkPolicyInventory(policies map[string]map[string]manifestNetworkPolicy) []Problem {
	var problems []Problem
	for _, path := range []string{basePolicies, localPolicies} {
		indexed, read := policies[path]
		if !read {
			continue
		}
		expected := make(map[string]bool, len(networkPolicyContracts))
		for name, contract := range networkPolicyContracts {
			if contract.path == path {
				expected[name] = true
			}
		}
		found := make(map[string]bool, len(indexed))
		for name := range indexed {
			found[name] = true
		}
		if !mapsEqual(found, expected) {
			problems = append(problems, problem(path, "NetworkPolicy inventory drifted (missing: %s; unregistered: %s)",
				formatSet(sortedDifference(expected, found)), formatSet(sortedDifference(found, expected))))
		}
	}
	return problems
}

// checkAllowMatrixSymmetry is the assertion a per-policy check cannot make: a
// default-deny namespace only lets a flow through when the source's egress and
// the destination's ingress both allow it, so a one-sided entry is a rule that
// looks open in review and fails at runtime.
func checkAllowMatrixSymmetry(contracts map[string]networkPolicyContract) []Problem {
	egress, ingress := allowMatrixEdges(contracts, basePolicies, localPolicies)
	if mapsEqual(egress, ingress) {
		return nil
	}
	return []Problem{problem(basePolicies, "the allow-matrix is one-sided (egress without ingress: %s; ingress without egress: %s)",
		formatSet(sortedDifference(egress, ingress)), formatSet(sortedDifference(ingress, egress)))}
}

// allowMatrixEdges reduces the registry to directed in-namespace pod-to-pod
// edges. Cross-namespace and ipBlock peers are excluded on purpose: the kagent
// controller and the model upstream live outside this namespace, own no
// agentops egress policy, and could never be symmetric.
func allowMatrixEdges(contracts map[string]networkPolicyContract, paths ...string) (egress, ingress map[string]bool) {
	egress, ingress = make(map[string]bool), make(map[string]bool)
	for _, name := range sortedRegistryKeys(contracts) {
		contract := contracts[name]
		if contract.selector == "" || !slices.Contains(paths, contract.path) {
			continue
		}
		for _, flow := range contract.egress {
			for _, key := range flowEdgeKeys(contract.selector, flow.peer.pod["app.kubernetes.io/name"], flow) {
				egress[key] = true
			}
		}
		for _, flow := range contract.ingress {
			for _, key := range flowEdgeKeys(flow.peer.pod["app.kubernetes.io/name"], contract.selector, flow) {
				ingress[key] = true
			}
		}
	}
	return egress, ingress
}

// flowEdgeKeys renders one flow as "<source> -> <target> <PROTO>:<port>" keys.
// Naming both ends explicitly is what lets an ingress rule and its matching
// egress rule reduce to the same directed key.
func flowEdgeKeys(source, target string, flow networkFlow) []string {
	if source == "" || target == "" || flow.peer.namespace != nil || flow.peer.cidr != "" {
		return nil
	}
	keys := make([]string, 0, len(flow.ports))
	for _, port := range flow.ports {
		keys = append(keys, fmt.Sprintf("%s -> %s %s:%d", source, target, port.protocol, port.number))
	}
	return keys
}

func checkDeniedIngress(contracts map[string]networkPolicyContract) []Problem {
	var problems []Problem
	for _, name := range sortedRegistryKeys(contracts) {
		contract := contracts[name]
		for _, flow := range contract.ingress {
			for _, port := range flow.ports {
				if reason, denied := deniedIngressPorts[port.number]; denied {
					problems = append(problems, problem(contract.path, "NetworkPolicy %s admits :%d: %s", name, port.number, reason))
				}
			}
		}
	}
	return problems
}

// checkAllowMatrixRows binds the published allow-matrix to the registry in both
// directions: every port the table cites must be opened by a rule, and every
// port a rule opens must be cited. A clause that promises nobody reaches a port
// is checked the other way round, so "nobody on `:3001`" is an assertion rather
// than a sentence.
func checkAllowMatrixRows(contracts map[string]networkPolicyContract, pages pageSet) []Problem {
	documented := make(map[string]bool)
	var problems []Problem
	for _, row := range matrixRowPattern.FindAllStringSubmatch(pages[matrixWhere], -1) {
		pod := row[1]
		documented[pod] = true
		egress, ingress := matrixPorts(contracts, pod)
		problems = append(problems, checkAllowMatrixCell(pod, "egress", row[2], egress)...)
		problems = append(problems, checkAllowMatrixCell(pod, "ingress", row[3], ingress)...)
	}
	// The table announces itself as a mirror of the base manifest, so the base
	// policies decide which pods get a row. An overlay-only pod contributes its
	// ports to an existing row (the collector cites Prometheus' :8889) without
	// earning one of its own.
	expected := make(map[string]bool)
	for _, contract := range contracts {
		if contract.path == basePolicies && contract.selector != "" {
			expected[contract.selector] = true
		}
	}
	if !mapsEqual(documented, expected) {
		problems = append(problems, problem(matrixWhere, "allow-matrix rows drifted (missing: %s; extra: %s)",
			formatSet(sortedDifference(expected, documented)), formatSet(sortedDifference(documented, expected))))
	}
	return problems
}

func checkAllowMatrixCell(pod, direction, cell string, opened map[int]bool) []Problem {
	var problems []Problem
	cited := make(map[int]bool)
	// Clauses are separated by ";", so one cell can carry both what a pod may
	// reach and what nothing may reach.
	for _, clause := range strings.Split(cell, ";") {
		denies := strings.Contains(clause, "nobody")
		for _, match := range matrixPortPattern.FindAllStringSubmatch(clause, -1) {
			port, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			switch {
			case denies && opened[port]:
				problems = append(problems, problem(matrixWhere, "allow-matrix row %s promises nobody reaches :%d, but its %s rules open it", pod, port, direction))
			case denies:
				continue
			case !opened[port]:
				problems = append(problems, problem(matrixWhere, "allow-matrix row %s documents :%d, which no %s rule opens", pod, port, direction))
			default:
				cited[port] = true
			}
		}
	}
	for _, port := range sortedPorts(opened) {
		if !cited[port] {
			problems = append(problems, problem(matrixWhere, "allow-matrix row %s omits :%d, which its %s rules open", pod, port, direction))
		}
	}
	return problems
}

// matrixPorts collects every port the registry opens for one pod, base and
// local overlay together, because the published table tabulates both.
func matrixPorts(contracts map[string]networkPolicyContract, pod string) (egress, ingress map[int]bool) {
	egress, ingress = make(map[int]bool), make(map[int]bool)
	for _, contract := range contracts {
		if contract.selector != pod {
			continue
		}
		for _, flow := range contract.egress {
			for _, port := range flow.ports {
				egress[port.number] = true
			}
		}
		for _, flow := range contract.ingress {
			for _, port := range flow.ports {
				ingress[port.number] = true
			}
		}
	}
	return egress, ingress
}

// checkAllowMatrixDiagram holds the diagram to the base registry exactly. The
// table summarizes; the diagram claims to draw every internal flow, so it is
// the surface where set equality is the honest assertion.
func checkAllowMatrixDiagram(contracts map[string]networkPolicyContract, pages pageSet) []Problem {
	blocks := fencedBlocks(pages[matrixWhere], "mermaid")
	if len(blocks) != 1 {
		return []Problem{problem(matrixWhere, "expected exactly one mermaid allow-matrix diagram, found %d", len(blocks))}
	}
	var problems []Problem
	drawn := make(map[string]bool)
	for _, edge := range matrixEdgePattern.FindAllStringSubmatch(strings.Join(blocks[0].body, "\n"), -1) {
		source, known := matrixDiagramPods[edge[1]]
		target, drawnTarget := matrixDiagramPods[edge[3]]
		if !known || !drawnTarget {
			problems = append(problems, problem(matrixWhere, "allow-matrix diagram names unknown node %q or %q", edge[1], edge[3]))
			continue
		}
		if source == "" || target == "" {
			continue // a cross-namespace or public edge; the table owns those
		}
		for _, match := range matrixPortPattern.FindAllStringSubmatch(edge[2], -1) {
			// Every in-namespace flow is TCP today; a future UDP flow surfaces
			// here as a diff and has to be drawn explicitly.
			drawn[fmt.Sprintf("%s -> %s TCP:%s", source, target, match[1])] = true
		}
	}
	expected, _ := allowMatrixEdges(contracts, basePolicies)
	if !mapsEqual(drawn, expected) {
		problems = append(problems, problem(matrixWhere, "allow-matrix diagram drifted (missing: %s; extra: %s)",
			formatSet(sortedDifference(expected, drawn)), formatSet(sortedDifference(drawn, expected))))
	}
	return problems
}

func sortedPorts(values map[int]bool) []int {
	ports := make([]int, 0, len(values))
	for port := range values {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	return ports
}

func contractFlowKeys(contract networkPolicyContract) map[string]bool {
	keys := make(map[string]bool, len(contract.ingress)+len(contract.egress))
	for _, flow := range contract.ingress {
		keys["in "+peerKey(flow.peer)+" "+portsKey(flow.ports)] = true
	}
	for _, flow := range contract.egress {
		keys["out "+peerKey(flow.peer)+" "+portsKey(flow.ports)] = true
	}
	return keys
}

// manifestFlowKeys flattens each rule into one key per peer: Kubernetes ORs the
// peers inside a rule and applies the rule's ports to all of them.
func manifestFlowKeys(document manifestNetworkPolicy) map[string]bool {
	keys := make(map[string]bool)
	for _, rule := range document.Spec.Ingress {
		for _, peer := range rule.From {
			keys["in "+peerKey(manifestPeer(peer))+" "+portsKey(manifestPorts(rule.Ports))] = true
		}
	}
	for _, rule := range document.Spec.Egress {
		for _, peer := range rule.To {
			keys["out "+peerKey(manifestPeer(peer))+" "+portsKey(manifestPorts(rule.Ports))] = true
		}
	}
	return keys
}

func manifestPeer(peer manifestPolicyPeer) networkPeer {
	result := networkPeer{}
	if peer.IPBlock != nil {
		result.cidr = peer.IPBlock.CIDR
	}
	if peer.NamespaceSelector != nil {
		// An empty namespaceSelector matches *every* namespace, so it must stay
		// distinguishable from an in-namespace peer rather than decaying to nil.
		result.namespace = peer.NamespaceSelector.MatchLabels
		if result.namespace == nil {
			result.namespace = map[string]string{}
		}
	}
	if peer.PodSelector != nil {
		result.pod = peer.PodSelector.MatchLabels
	}
	return result
}

func manifestPorts(ports []manifestPolicyPort) []networkPort {
	result := make([]networkPort, 0, len(ports))
	for _, port := range ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "TCP" // Kubernetes' own default
		}
		result = append(result, networkPort{number: port.Port, protocol: protocol})
	}
	return result
}

func peerKey(peer networkPeer) string {
	if peer.cidr != "" {
		return "ip(" + peer.cidr + ")"
	}
	parts := make([]string, 0, 2)
	if peer.namespace != nil {
		parts = append(parts, "ns("+labelKey(peer.namespace)+")")
	}
	if peer.pod != nil {
		parts = append(parts, "pod("+labelKey(peer.pod)+")")
	}
	if len(parts) == 0 {
		return "any()"
	}
	return strings.Join(parts, "+")
}

func portsKey(ports []networkPort) string {
	if len(ports) == 0 {
		return "every-port"
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, port.protocol+":"+strconv.Itoa(port.number))
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}
