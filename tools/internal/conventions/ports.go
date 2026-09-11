package conventions

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type portOwner struct {
	path    string
	pattern string
}

var portContracts = map[int]portOwner{
	3000:  {"infra/agentgateway/host/config.yaml", `(?m)^\s*mcp:\s*\n\s*port:\s*3000\s*$`},
	3001:  {"infra/agentgateway/host/config.yaml", `(?m)^\s*a2a:\s*\n\s*port:\s*3001\s*$`},
	4000:  {"infra/agentgateway/host/config.yaml", `(?m)^\s*llm:\s*\n\s*port:\s*4000\s*$`},
	15020: {"infra/scripts/gateway-host.sh", `AGENTOPS_GATEWAY_METRICS_PORT:-15020`},
	15021: {"infra/scripts/gateway-host.sh", `AGENTOPS_GATEWAY_READINESS_PORT:-15021`},
	8000:  {"agents/go/mise.toml", `MCP_PORT = "8000"`},
	8080:  {"agents/go/config/config.go", `A2APort\s+int[^\n]+envDefault:"8080"`},
	8083:  {"agents/go/cmd/kagent-invoke/main.go", `defaultEndpoint\s+= "http://127\.0\.0\.1:8083/mcp"`},
	8001:  {"mise.toml", `static-server --host 127\.0\.0\.1 --port 8001`},
	8002:  {"agents/go/mise.toml", `web -port 8002`},
	8003:  {"mise.toml", `hugo server --bind 127\.0\.0\.1 --port 8003`},
	11434: {"agents/go/config/config.go", `envDefault:"http://127\.0\.0\.1:11434/v1"`},
	13133: {"infra/k8s/base/otel-collector-config.yaml", `endpoint: 0\.0\.0\.0:13133`},
	3200:  {"infra/observability/compose.yaml", `TEMPO_PORT:-3200`},
	4317:  {"infra/observability/compose.yaml", `OTEL_GRPC_PORT:-4317`},
	4318:  {"infra/observability/compose.yaml", `OTEL_HTTP_PORT:-4318`},
	8889:  {"infra/observability/otel-collector.yaml", `endpoint: 0\.0\.0\.0:8889`},
	9090:  {"infra/observability/compose.yaml", `PROMETHEUS_PORT:-9090`},
	9093:  {"infra/observability/compose.yaml", `ALERTMANAGER_PORT:-9093`},
	3002:  {"infra/observability/compose.yaml", `GRAFANA_PORT:-3002`},
	3100:  {"infra/observability/compose.yaml", `LOKI_PORT:-3100`},
	5050:  {"infra/k3d.yaml", `hostPort: "5050"`},
}

func checkPortContracts(root string, pages pageSet) []Problem {
	expected := make(map[int]bool)
	var problems []Problem
	for port, owner := range portContracts {
		expected[port] = true
		text, err := readFile(filepath.Join(root, filepath.FromSlash(owner.path)))
		if err != nil {
			problems = append(problems, problem(owner.path, "could not read port owner for :%d: %v", port, err))
			continue
		}
		if !regexp.MustCompile(owner.pattern).MatchString(text) {
			problems = append(problems, problem(owner.path, "authoritative pattern for stable port :%d is missing", port))
		}
	}
	const ecosystemWhere = "content/0. Overview/0.4. Ecosystem.md"
	heading := `{{% collapsible note "Deeper: every port the course binds" %}}`
	// Read the section through the shared cutter, which names the heading a rename has to
	// preserve. Cutting inline reported the same rename as "every port is missing", which
	// sends an author to the port table instead of to the line that actually moved.
	section, missing := sectionAfterHeading(ecosystemWhere, pages[ecosystemWhere], heading)
	if len(missing) > 0 {
		return append(problems, missing...)
	}
	documented := make(map[int]bool)
	for _, match := range regexp.MustCompile("`:(\\d+)`").FindAllStringSubmatch(section, -1) {
		value, err := strconv.Atoi(match[1])
		if err == nil {
			documented[value] = true
		}
	}
	if !integerMapsEqual(documented, expected) {
		missing := integerDifference(expected, documented)
		extra := integerDifference(documented, expected)
		problems = append(problems, problem(ecosystemWhere, "stable port inventory drifted (missing: %s; extra: %s)", formatPorts(missing), formatPorts(extra)))
	}
	agents, _ := readFile(filepath.Join(root, "AGENTS.md"))
	paragraph := ""
	for _, part := range strings.Split(agents, "\n\n") {
		if strings.HasPrefix(part, "This file owns the stable network inventory") {
			paragraph = part
			break
		}
	}
	agentsPorts := make(map[int]bool)
	for _, match := range regexp.MustCompile(`:(\d+)`).FindAllStringSubmatch(paragraph, -1) {
		value, err := strconv.Atoi(match[1])
		if err == nil {
			agentsPorts[value] = true
		}
	}
	if !integerMapsEqual(agentsPorts, expected) {
		problems = append(problems, problem("AGENTS.md", "stable network paragraph must match the executable port inventory"))
	}
	return problems
}

func integerMapsEqual(left, right map[int]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

func integerDifference(left, right map[int]bool) map[int]bool {
	result := make(map[int]bool)
	for value := range left {
		if !right[value] {
			result[value] = true
		}
	}
	return result
}
