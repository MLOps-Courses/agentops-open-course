package conventions

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// probeContract is one workload's authoritative kubelet-probe shape. Every
// probed course workload answers a real HTTP handler on a named port: a data
// port accepts TCP as soon as its listener binds, which is before backends,
// policies, and stores are usable, so a tcpSocket probe would report Ready
// while every request still failed.
type probeContract struct {
	path      string // repository-relative manifest that owns the workload
	container string // container name inside the pod template
	port      string // named containerPort every probe of this workload targets
	startup   string // httpGet path, or "" when the workload declares no startupProbe
	readiness string // httpGet path; never empty
	liveness  string // httpGet path; never empty
	number    int    // containerPort number the named port must resolve to
	// exposed records whether the workload's Service publishes that port.
	// agentgateway (:15021) and the collector (:13133) deliberately keep theirs
	// pod-local: the kubelet dials the pod IP, so no cluster-reachable port is
	// needed and none is offered.
	exposed bool
}

// probeContracts is keyed by Deployment metadata.name, the identity a namespace
// cannot duplicate, which is the role the port number plays in portContracts.
// scripts/check-infra.sh asserts the same shapes on the *rendered* overlays;
// this registry is what binds them to the course prose, and the two must agree.
var probeContracts = map[string]probeContract{
	"agentgateway": {
		path: "infra/k8s/base/agentgateway.yaml", container: "agentgateway",
		port: "readiness", number: 15021, exposed: false,
		readiness: "/healthz/ready", liveness: "/healthz/ready",
	},
	"agentops-mcp": {
		path: "infra/k8s/base/mcp.yaml", container: "mcp",
		port: "mcp", number: 8000, exposed: true,
		startup: "/livez", readiness: "/healthz", liveness: "/livez",
	},
	"otel-collector": {
		path: "infra/k8s/base/otel-collector.yaml", container: "collector",
		port: "health", number: 13133, exposed: false,
		readiness: "/", liveness: "/",
	},
	"tempo": {
		path: "infra/k8s/base/tempo.yaml", container: "tempo",
		port: "http", number: 3200, exposed: true,
		startup: "/ready", readiness: "/ready", liveness: "/ready",
	},
	"loki": {
		path: "infra/k8s/base/loki.yaml", container: "loki",
		port: "http", number: 3100, exposed: true,
		startup: "/ready", readiness: "/ready", liveness: "/ready",
	},
	"prometheus": {
		path: "infra/k8s/overlays/local/prometheus.yaml", container: "prometheus",
		port: "http", number: 9090, exposed: true,
		readiness: "/-/ready", liveness: "/-/healthy",
	},
	"alertmanager": {
		path: "infra/k8s/overlays/local/alertmanager.yaml", container: "alertmanager",
		port: "http", number: 9093, exposed: true,
		readiness: "/-/ready", liveness: "/-/healthy",
	},
}

// probeSources are the manifest directories the inventory sweep walks, so a
// Deployment added without a reviewed probe contract fails as drift instead of
// shipping an unexamined health shape.
var probeSources = []string{
	"infra/k8s/base",
	"infra/k8s/overlays/local",
	"infra/k8s/overlays/gke",
}

const (
	startupProbeField   = "startupProbe"
	readinessProbeField = "readinessProbe"
	livenessProbeField  = "livenessProbe"

	byoAgentWhere     = "infra/kagent/agent.yaml"
	gatewayProbeWhere = "content/6. Platform/6.5. Platform Gateway.md"
	probeTableWhere   = "content/6. Platform/6.3. Platform Agents.md"
)

// probeFields are the three kubelet probe keys, in kubelet order.
var probeFields = []string{startupProbeField, readinessProbeField, livenessProbeField}

// probeHandler pairs one kubelet probe field with the path the registry demands
// and the probe the manifest actually declares.
type probeHandler struct {
	probe  *manifestProbe
	field  string
	wanted string
}

// probeHandlers keeps the field-to-contract wiring in one place, so the owner
// check and the documentation check can never disagree about which probes a
// workload is supposed to declare.
func probeHandlers(contract probeContract, container manifestContainer) []probeHandler {
	return []probeHandler{
		{field: startupProbeField, wanted: contract.startup, probe: container.StartupProbe},
		{field: readinessProbeField, wanted: contract.readiness, probe: container.ReadinessProbe},
		{field: livenessProbeField, wanted: contract.liveness, probe: container.LivenessProbe},
	}
}

// manifestProbe decodes one kubelet probe. httpGet.port is an IntOrString, so
// it stays a yaml.Node: decoding it into a string would fail the entire
// document the moment somebody writes a bare number, and that bare number is
// exactly what this registry has to reject with a targeted message.
type manifestProbe struct {
	HTTPGet *struct {
		Path string    `yaml:"path"`
		Port yaml.Node `yaml:"port"`
	} `yaml:"httpGet"`
	TCPSocket *yaml.Node `yaml:"tcpSocket"`
	Exec      *yaml.Node `yaml:"exec"`
	GRPC      *yaml.Node `yaml:"grpc"`
}

type manifestContainerPort struct {
	Name          string `yaml:"name"`
	ContainerPort int    `yaml:"containerPort"`
}

type manifestContainer struct {
	StartupProbe   *manifestProbe          `yaml:"startupProbe"`
	ReadinessProbe *manifestProbe          `yaml:"readinessProbe"`
	LivenessProbe  *manifestProbe          `yaml:"livenessProbe"`
	Name           string                  `yaml:"name"`
	Ports          []manifestContainerPort `yaml:"ports"`
}

// manifestServicePort carries targetPort as a yaml.Node for the same
// IntOrString reason; every course Service names its target today.
type manifestServicePort struct {
	TargetPort yaml.Node `yaml:"targetPort"`
	Port       int       `yaml:"port"`
}

// manifestWorkload decodes the Deployment and Service shapes together. They
// share the `spec` key and live in the same document stream, so one pass keeps
// the probe half and the Service half provably reading the same file.
type manifestWorkload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Ports    []manifestServicePort `yaml:"ports"`
		Template struct {
			Spec struct {
				Containers []manifestContainer `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func checkProbeContracts(root string, pages pageSet) []Problem {
	var problems []Problem
	for _, workload := range sortedRegistryKeys(probeContracts) {
		contract := probeContracts[workload]
		documents, err := decodeManifests[manifestWorkload](root, contract.path)
		if err != nil {
			problems = append(problems, problem(contract.path, "could not read probe owner for %s: %v", workload, err))
			continue
		}
		problems = append(problems, checkProbeOwner(workload, contract, documents)...)
	}
	problems = append(problems, checkProbeInventory(root)...)
	problems = append(problems, checkUnprobedByoAgent(root)...)
	problems = append(problems, checkGatewayProbeProse(pages)...)
	problems = append(problems, checkProbeWiringTable(pages)...)
	return problems
}

func checkProbeOwner(workload string, contract probeContract, documents []manifestWorkload) []Problem {
	var container *manifestContainer
	var service *manifestWorkload
	for index := range documents {
		document := &documents[index]
		if document.Metadata.Name != workload {
			continue
		}
		switch document.Kind {
		case "Deployment":
			for position := range document.Spec.Template.Spec.Containers {
				if document.Spec.Template.Spec.Containers[position].Name == contract.container {
					container = &document.Spec.Template.Spec.Containers[position]
				}
			}
		case "Service":
			service = document
		}
	}
	if container == nil {
		return []Problem{problem(contract.path, "probe owner %s declares no container %q", workload, contract.container)}
	}
	problems := checkProbeTarget(workload, contract, *container)
	for _, handler := range probeHandlers(contract, *container) {
		problems = append(problems, checkProbeHandler(workload, contract, handler)...)
	}
	return append(problems, checkProbeService(workload, contract, service)...)
}

func checkProbeTarget(workload string, contract probeContract, container manifestContainer) []Problem {
	for _, port := range container.Ports {
		if port.Name != contract.port {
			continue
		}
		if port.ContainerPort != contract.number {
			return []Problem{problem(contract.path, "probe target %q on %s must be containerPort %d, found %d", contract.port, workload, contract.number, port.ContainerPort)}
		}
		return nil
	}
	return []Problem{problem(contract.path, "%s declares no named container port %q for its probes", workload, contract.port)}
}

func checkProbeHandler(workload string, contract probeContract, handler probeHandler) []Problem {
	if handler.wanted == "" {
		if handler.probe != nil {
			return []Problem{problem(contract.path, "%s must not declare a %s; the reviewed shape has none", workload, handler.field)}
		}
		return nil
	}
	if handler.probe == nil {
		return []Problem{problem(contract.path, "authoritative %s httpGet %s for %s is missing", handler.field, handler.wanted, workload)}
	}
	probe := handler.probe
	if probe.TCPSocket != nil || probe.Exec != nil || probe.GRPC != nil || probe.HTTPGet == nil {
		return []Problem{problem(contract.path, "%s %s must use httpGet; a tcpSocket, exec, or grpc handler reports Ready before backends, policies, and stores are usable", workload, handler.field)}
	}
	var problems []Problem
	if probe.HTTPGet.Path != handler.wanted {
		problems = append(problems, problem(contract.path, "%s %s path drifted: expected %q, found %q", workload, handler.field, handler.wanted, probe.HTTPGet.Path))
	}
	// A named port survives a renumbering of the container port; a bare number
	// silently keeps probing whatever answers there afterwards.
	if probe.HTTPGet.Port.Tag != "!!str" {
		problems = append(problems, problem(contract.path, "%s %s must target the named port %q, not a bare number", workload, handler.field, contract.port))
	} else if probe.HTTPGet.Port.Value != contract.port {
		problems = append(problems, problem(contract.path, "%s %s port drifted: expected %q, found %q", workload, handler.field, contract.port, probe.HTTPGet.Port.Value))
	}
	return problems
}

func checkProbeService(workload string, contract probeContract, service *manifestWorkload) []Problem {
	published := false
	if service != nil {
		for _, port := range service.Spec.Ports {
			published = published || port.TargetPort.Value == contract.port || port.Port == contract.number
		}
	}
	switch {
	case published && !contract.exposed:
		return []Problem{problem(contract.path, "the %s Service must not publish probe port %q: the kubelet dials the pod IP, so no cluster-reachable port is needed", workload, contract.port)}
	case !published && contract.exposed:
		return []Problem{problem(contract.path, "the %s Service must keep publishing probe port %q", workload, contract.port)}
	}
	return nil
}

// checkProbeInventory sweeps every declared workload directory so a Deployment
// cannot enter the course without a reviewed probe contract. It reads source
// manifests, not rendered overlays: the ipBlock exceptions the overlays patch
// in are invisible here and stay owned by scripts/check-infra.sh.
//
// The sweep reads `.yaml` only, and the naming that makes that safe is enforced
// here rather than assumed: every Kubernetes manifest in the repository is
// `.yaml`, and scripts/check-infra.sh names its inputs by that spelling too. A
// workload added as `.yml` would be rendered by kustomize — which reads whatever
// its kustomization.yaml lists — while remaining invisible to this inventory, so
// it would ship with no reviewed probe contract and no failure. Accepting both
// extensions would close that hole for this one check while leaving the split
// convention for every other tool to rediscover; rejecting the spelling closes it
// once, at the name.
func checkProbeInventory(root string) []Problem {
	expected := make(map[string]bool, len(probeContracts))
	for workload := range probeContracts {
		expected[workload] = true
	}
	found := make(map[string]bool, len(probeContracts))
	var problems []Problem
	for _, source := range probeSources {
		walkErr := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(source)), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if filepath.Ext(path) == ".yml" {
				problems = append(problems, problem(relative(root, path),
					"Kubernetes manifests must be named .yaml; a .yml workload is rendered by kustomize but invisible to the probe inventory"))
				return nil
			}
			if filepath.Ext(path) != ".yaml" {
				return nil
			}
			where := relative(root, path)
			documents, decodeErr := decodeManifests[manifestWorkload](root, where)
			if decodeErr != nil {
				problems = append(problems, problem(where, "could not read workload manifest: %v", decodeErr))
				return nil
			}
			for _, document := range documents {
				if document.Kind == "Deployment" {
					found[document.Metadata.Name] = true
				}
			}
			return nil
		})
		if walkErr != nil {
			problems = append(problems, problem(source, "could not walk workload manifests: %v", walkErr))
		}
	}
	if !mapsEqual(found, expected) {
		problems = append(problems, problem("infra/k8s", "every Deployment must own a reviewed probe contract (missing: %s; unregistered: %s)",
			formatSet(sortedDifference(expected, found)), formatSet(sortedDifference(found, expected))))
	}
	return problems
}

// checkUnprobedByoAgent keeps the documented asymmetry honest: the pinned
// kagent v1alpha2 BYO deployment schema exposes no probe fields, so a probe
// written here would be silently dropped while the course claims it is wired.
func checkUnprobedByoAgent(root string) []Problem {
	text, err := readFile(filepath.Join(root, filepath.FromSlash(byoAgentWhere)))
	if err != nil {
		return []Problem{problem(byoAgentWhere, "could not read the BYO Agent: %v", err)}
	}
	var problems []Problem
	for _, field := range probeFields {
		if strings.Contains(text, field) {
			problems = append(problems, problem(byoAgentWhere, "the BYO Agent must not declare %s; the pinned v1alpha2 schema exposes no probe fields", field))
		}
	}
	return problems
}

// checkGatewayProbeProse binds the gateway paragraph to the manifest. Its
// claims are derived from the registry, so changing the probe without changing
// the page — or the reverse — fails here instead of teaching a stale shape.
func checkGatewayProbeProse(pages pageSet) []Problem {
	const workload = "agentgateway"
	contract := probeContracts[workload]
	paragraph := ""
	for _, part := range strings.Split(pages[gatewayProbeWhere], "\n\n") {
		if strings.HasPrefix(part, "Readiness and liveness probes") {
			paragraph = part
			break
		}
	}
	if paragraph == "" {
		return []Problem{problem(gatewayProbeWhere, "expected a paragraph starting \"Readiness and liveness probes\" that states the reviewed %s probe shape", workload)}
	}
	tokens := []string{fmt.Sprintf("`GET %s`", contract.readiness), fmt.Sprintf("`:%d`", contract.number), contract.port + " port"}
	if contract.liveness != contract.readiness {
		tokens = append(tokens, fmt.Sprintf("`GET %s`", contract.liveness))
	}
	if !contract.exposed {
		tokens = append(tokens, "absent from the Service")
	}
	problems := make([]Problem, 0, len(tokens))
	for _, token := range tokens {
		if !strings.Contains(paragraph, token) {
			problems = append(problems, problem(gatewayProbeWhere, "the gateway probe paragraph must cite %q from %s", token, contract.path))
		}
	}
	return problems
}

// checkProbeWiringTable binds the MCP-versus-BYO asymmetry table to the two
// manifests it describes: the static Deployment wires all three probes, and the
// BYO manifest declares none — the kagent controller supplies a readiness probe
// of its own on the agent card, which the row has to say, because a row that
// only denies the three fields reads as "nothing polls this pod" and that is
// not what the pinned chart renders.
func checkProbeWiringTable(pages pageSet) []Problem {
	mcpRow := tableRow(pages[probeTableWhere], "| `agentops-mcp`")
	agentRow := tableRow(pages[probeTableWhere], "| `agentops-agent`")
	if mcpRow == "" || agentRow == "" {
		return []Problem{problem(probeTableWhere, "expected the probe-wiring table to keep one row for `agentops-mcp` and one for `agentops-agent`")}
	}
	var problems []Problem
	for _, field := range wiredProbeFields(probeContracts["agentops-mcp"]) {
		if !strings.Contains(mcpRow, "`"+field+"`") {
			problems = append(problems, problem(probeTableWhere, "the probe-wiring table must name `%s` for agentops-mcp", field))
		}
	}
	for _, field := range probeFields {
		if strings.Contains(agentRow, field) {
			problems = append(problems, problem(probeTableWhere, "the probe-wiring table must keep the BYO A2A workload unwired, but its row names %s", field))
		}
	}
	// The positive half. Without it the row can drift back to claiming nothing polls
	// the BYO pod while the pinned controller renders a readiness probe on the card.
	for _, wanted := range []string{"agent-card.json", "controller"} {
		if !strings.Contains(agentRow, wanted) {
			problems = append(problems, problem(probeTableWhere, "the probe-wiring table's BYO row must say the controller polls the agent card, but it does not mention %q", wanted))
		}
	}
	return problems
}

func wiredProbeFields(contract probeContract) []string {
	wired := make([]string, 0, len(probeFields))
	for _, handler := range probeHandlers(contract, manifestContainer{}) {
		if handler.wanted != "" {
			wired = append(wired, handler.field)
		}
	}
	return wired
}

func tableRow(text, prefix string) string {
	for _, line := range splitLines(text) {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
