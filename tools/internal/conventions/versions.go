package conventions

// Version authorities and their copies. One file owns each version, and these
// checks prove the pages that repeat it still agree with that owner.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

func checkThemePin(root string) []Problem {
	modules, err := readFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return []Problem{problem("go.mod", "could not read the Hextra module pin: %v", err)}
	}
	var problems []Problem
	if !regexp.MustCompile(`(?m)^require github\.com/imfing/hextra v\d+\.\d+\.\d+`).MatchString(modules) {
		problems = append(problems, problem("go.mod", "the Hextra theme must be pinned to an exact version"))
	}
	const manifestWhere = "assets/js/vendor/versions.json"
	manifest, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifestWhere)))
	if err != nil {
		return append(problems, problem(manifestWhere, "could not read vendored bundle versions: %v", err))
	}
	var pins map[string]struct {
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal(manifest, &pins); err != nil {
		return append(problems, problem(manifestWhere, "could not read vendored bundle versions: %v", err))
	}
	names := make([]string, 0, len(pins))
	for name := range pins {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(root, "assets", "js", "vendor", name))
		if err != nil {
			problems = append(problems, problem(manifestWhere, "pinned bundle is missing: %s", name))
			continue
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(content))
		if digest != pins[name].SHA256 {
			problems = append(problems, problem("assets/js/vendor/"+name, "bundle does not match its pinned digest (%s)", pins[name].Version))
		}
	}
	return problems
}

func toolVersion(root, key string) string {
	document, err := loadTOML(filepath.Join(root, "mise.toml"))
	if err != nil {
		return ""
	}
	value, _ := document.GetPath([]string{"tools", key}).(string)
	return value
}

func sourceVersion(text string, pattern *regexp.Regexp) (string, bool) {
	matches := pattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return "", false
	}
	version := matches[0][1]
	for _, match := range matches[1:] {
		if match[1] != version {
			return "", false
		}
	}
	return version, true
}

func checkCopiedVersionInventory(
	pages pageSet,
	label string,
	expected string,
	surfaces map[string]int,
	pattern *regexp.Regexp,
) []Problem {
	actual := make(map[string]int)
	var problems []Problem
	for where, text := range pages {
		matches := pattern.FindAllStringSubmatch(text, -1)
		actual[where] = len(matches)
		for _, match := range matches {
			if match[1] != expected {
				problems = append(problems, problem(where, "%s version drifted: expected %q, found %q", label, expected, match[1]))
			}
		}
	}
	paths := make(map[string]bool, len(actual)+len(surfaces))
	for where := range actual {
		paths[where] = true
	}
	for where := range surfaces {
		paths[where] = true
	}
	ordered := make([]string, 0, len(paths))
	for where := range paths {
		ordered = append(ordered, where)
	}
	slices.Sort(ordered)
	for _, where := range ordered {
		if actual[where] != surfaces[where] {
			problems = append(problems, problem(where, "%s copy inventory drifted: expected %d, found %d", label, surfaces[where], actual[where]))
		}
	}
	return problems
}

func checkSourceVersions(root string, pages pageSet) []Problem {
	var problems []Problem
	setup := "content/1. Setup/1.3. Kubernetes.md"
	rowPattern := regexp.MustCompile(`(?m)^\|\s*([^|]+?)\s*\|\s*([v\d][^|]*?)\s*\|`)
	rows := make(map[string]string)
	for _, match := range rowPattern.FindAllStringSubmatch(pages[setup], -1) {
		rows[strings.ToLower(strings.TrimSpace(match[1]))] = strings.TrimSpace(match[2])
	}
	for label, key := range map[string]string{
		"k3d": "k3d", "kubectl": "kubectl", "helm": "helm", "helmfile": "helmfile",
		"skaffold": "skaffold", "kubeconform": "kubeconform", "kube-linter": "kube-linter",
		"agentgateway": "github:agentgateway/agentgateway",
	} {
		problems = append(problems, compareContract(setup, label+" table version", toolVersion(root, key), rows[label])...)
	}

	goPin := toolVersion(root, "go")
	goDirective := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`)
	// All four modules, including the root one. AGENTS.md calls the toolchain a
	// coordinated pin across every Go module, the root mise.toml and the Dockerfile
	// build stage; leaving the root go.mod out let a coordinated bump forget one of
	// the five and stay green.
	for _, where := range []string{"go.mod", "agents/go/go.mod", "evals/go.mod", "tools/go.mod"} {
		module, err := readFile(filepath.Join(root, filepath.FromSlash(where)))
		if err != nil {
			problems = append(problems, problem(where, "could not read Go version authority: %v", err))
			continue
		}
		actual, _ := sourceVersion(module, goDirective)
		problems = append(problems, compareContract(where, "Go toolchain version", goPin, actual)...)
	}
	dockerfile, err := readFile(filepath.Join(root, "agents", "go", "Dockerfile"))
	if err != nil {
		problems = append(problems, problem("agents/go/Dockerfile", "could not read Go build image authority: %v", err))
	} else {
		match := regexp.MustCompile(`(?m)^FROM golang:([^@\s]+)@sha256:`).FindStringSubmatch(dockerfile)
		actual := ""
		if match != nil {
			actual = strings.TrimSuffix(match[1], "-alpine")
		}
		problems = append(problems, compareContract("agents/go/Dockerfile", "Go build image version", goPin, actual)...)
	}

	rootMise, miseErr := readFile(filepath.Join(root, "mise.toml"))
	helmfile, helmErr := readFile(filepath.Join(root, "infra", "helmfile.yaml"))
	helmDiff, helmDiffErr := readFile(filepath.Join(root, "scripts", "install-helm-diff.sh"))
	authorities := []struct {
		pattern  *regexp.Regexp
		copies   *regexp.Regexp
		surfaces map[string]int
		readErr  error
		where    string
		label    string
		text     string
	}{
		{
			where: "infra/helmfile.yaml", label: "kagent chart", text: helmfile, readErr: helmErr,
			pattern: regexp.MustCompile(`(?m)^# kagent-chart-version: (\d+\.\d+\.\d+)$`),
			surfaces: map[string]int{
				"content/6. Platform/6.0. Platform.md":         1,
				"content/6. Platform/6.2. Platform Install.md": 2,
			},
			copies: regexp.MustCompile(`(?i)(?:pinned to|pinned stable chart|chart version|at exactly)\s+` + "`?" + `v?(\d+\.\d+\.\d+)`),
		},
		{
			where: "scripts/install-helm-diff.sh", label: "helm-diff", text: helmDiff, readErr: helmDiffErr,
			pattern: regexp.MustCompile(`(?m)^readonly expected=(\d+\.\d+\.\d+)$`),
			surfaces: map[string]int{
				"content/1. Setup/1.3. Kubernetes.md":          2,
				"content/6. Platform/6.2. Platform Install.md": 1,
			},
			copies: regexp.MustCompile(`(?i)helm-diff[^\n]{0,80}?(\d+\.\d+\.\d+)`),
		},
		{
			where: "mise.toml", label: "k6", text: rootMise, readErr: miseErr,
			pattern: regexp.MustCompile(`mise x k6@(\d+\.\d+\.\d+) -- k6 run`),
			surfaces: map[string]int{
				"content/7. Observability/7.2. Monitoring.md": 4,
			},
			copies: regexp.MustCompile(`(?i)k6@(\d+\.\d+\.\d+)`),
		},
	}
	for _, authority := range authorities {
		if authority.readErr != nil {
			problems = append(problems, problem(authority.where, "could not read the %s version authority: %v", authority.label, authority.readErr))
			continue
		}
		expected, ok := sourceVersion(authority.text, authority.pattern)
		if !ok {
			problems = append(problems, problem(authority.where, "could not parse one consistent %s version authority", authority.label))
			continue
		}
		problems = append(problems, checkCopiedVersionInventory(pages, authority.label, expected, authority.surfaces, authority.copies)...)
	}

	versionPatterns := []struct {
		pattern  *regexp.Regexp
		label    string
		expected string
	}{
		{regexp.MustCompile(`(?i)(?:agentgateway|pinned gateway)[^\n]{0,160}?v?(\d+\.\d+\.\d+)`), "agentgateway", toolVersion(root, "github:agentgateway/agentgateway")},
	}
	for _, contract := range versionPatterns {
		for where, text := range pages {
			for _, match := range contract.pattern.FindAllStringSubmatch(text, -1) {
				if match[1] != contract.expected {
					problems = append(problems, problem(where, "%s version drifted: expected %q, found %q", contract.label, contract.expected, match[1]))
				}
			}
		}
	}

	externalImage := regexp.MustCompile(`(?m)(?:--image=|^\s*image:\s+)((?:docker\.io|ghcr\.io|cgr\.dev|cr\.[^/\s]+\.dev)/[^\s` + "`" + `]+)`)
	for where, text := range pages {
		for _, match := range externalImage.FindAllStringSubmatch(text, -1) {
			image := strings.TrimRight(match[1], `"')],`)
			if !strings.Contains(image, "@sha256:") {
				problems = append(problems, problem(where, "external image %q must include an immutable sha256 digest", image))
			}
		}
	}
	return problems
}
