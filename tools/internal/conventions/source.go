package conventions

// What a page copies out of a shipped source file. This file holds the entry point
// the scheduled freshness report calls and the surfaces a page must quote rather
// than retype; the checks themselves live beside the source they read, in tasks.go,
// versions.go, ports.go, and the files next to them.

import (
	"fmt"
	"strings"
)

var sourceSnippetSurfaces = map[string][2]string{
	"Go module":  {"content/1. Setup/1.1. Go.md", "agents/go/go.mod:runtime-dependencies"},
	"YAML":       {"content/5. Gateway/5.0. Gateway.md", "infra/agentgateway/host/config.yaml:host-mcp-gateway"},
	"shell":      {"content/5. Gateway/5.1. Gateway Setup.md", "infra/scripts/gateway-host.sh:hardened-container-args"},
	"Dockerfile": {"content/6. Platform/6.1. Containers.md", "agents/go/Dockerfile:runtime-base"},
	"Helm":       {"content/6. Platform/6.2. Platform Install.md", "infra/helmfile.yaml:kagent-crds-release"},
	"workflow":   {"content/8. Community/8.2. Releases.md", ".github/workflows/release.yml:release-dispatch-authority"},
}

// CheckCopiedSources runs the source-derived subset used by the scheduled
// freshness report: the registry entries that declared a summary, in the order
// CheckDocs runs them. It deliberately excludes page-shape and rendered checks,
// and it cannot name a check the full set no longer has, because it never names
// one — copiedSourceChecks filters the same list CheckDocs walks.
func CheckCopiedSources(root string) []Problem {
	pages, err := loadPages(root)
	if err != nil {
		return []Problem{problem("content", "could not read Markdown pages: %v", err)}
	}
	return runChecks(copiedSourceChecks(), root, pages)
}

func checkSourceSnippetCoverage(pages pageSet) []Problem {
	var problems []Problem
	for formatName, surface := range sourceSnippetSurfaces {
		source, region, _ := strings.Cut(surface[1], ":")
		wanted := fmt.Sprintf(`{{< include path="%s" region="%s"`, source, region)
		if !strings.Contains(pages[surface[0]], wanted) {
			problems = append(problems, problem(surface[0], "source-backed %s example must include %q", formatName, surface[1]))
		}
	}
	return problems
}
