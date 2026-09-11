package conventions

// Source-owned course contracts. Each check reads a shipped file — a script, a
// manifest, a Go type — and requires the page documenting it to still match.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func compareContract(where, label, expected, actual string) []Problem {
	if expected == "" {
		return []Problem{problem(where, "could not resolve authoritative %s", label)}
	}
	if expected != actual {
		return []Problem{problem(where, "%s drifted: expected %q, found %q", label, expected, actual)}
	}
	return nil
}

func requireTokens(where, text string, tokens ...string) []Problem {
	problems := make([]Problem, 0, len(tokens))
	for _, token := range tokens {
		if !strings.Contains(text, token) {
			problems = append(problems, problem(where, "source contract is missing %q", token))
		}
	}
	return problems
}

func checkCurrentSourceContracts(root string, pages pageSet) []Problem {
	problems := make([]Problem, 0, 8)
	problems = append(problems, checkDependencyAuditContract(root, pages)...)
	problems = append(problems, checkStateCourseContract(root, pages)...)
	problems = append(problems, checkRetrievalCourseContract(root, pages)...)
	problems = append(problems, checkDomainCourseContract(root, pages)...)
	problems = append(problems, checkCapacityContract(root, pages)...)
	problems = append(problems, checkProviderContract(root, pages)...)
	problems = append(problems, checkArtifactRetentionContract(root, pages)...)
	problems = append(problems, checkDerivedFigures(root, pages)...)
	problems = append(problems, checkRepositorySlug(root)...)
	return problems
}

func checkDependencyAuditContract(root string, pages pageSet) []Problem {
	const licenseScriptWhere = "scripts/check-licenses.sh"
	const licensePageWhere = "content/8. Community/8.1. License.md"
	const lintPageWhere = "content/4. Quality/4.1. Linting.md"
	licenseScript, _ := readFile(filepath.Join(root, filepath.FromSlash(licenseScriptWhere)))
	licensePage := pages[licensePageWhere]
	lintPage := pages[lintPageWhere]
	problems := requireTokens(licenseScriptWhere, licenseScript,
		"check_repository_licenses", `"${lib_dir}/trivy-repository.sh" licenses`,
	)
	problems = append(problems, requireTokens(licensePageWhere, licensePage,
		"trivy-repository.sh", "three Go modules", "agents/go/go.mod", "evals/go.mod", "tools/go.mod",
	)...)
	problems = append(problems, requireTokens(lintPageWhere, lintPage,
		"govulncheck", "check:vuln", "agents/go", "evals", "tools",
	)...)
	for _, stale := range []string{"pip-licenses", "lock-owned profiles", "MLflow", "Python compatibility"} {
		if strings.Contains(licensePage, stale) {
			problems = append(problems, problem(licensePageWhere, "documents retired Python dependency auditing through %q", stale))
		}
	}
	return problems
}

func checkStateCourseContract(root string, pages pageSet) []Problem {
	const where = "content/6. Platform/6.6. Platform Delivery.md"
	stateSource, _ := readFile(filepath.Join(root, "agents", "go", "state", "state.go"))
	text := pages[where]
	var problems []Problem
	for label, pattern := range map[string]*regexp.Regexp{
		"manifest":          regexp.MustCompile(`(?m)^\s*manifestName\s*=\s*"([^"]+)"$`),
		"completion marker": regexp.MustCompile(`(?m)^\s*completeName\s*=\s*"([^"]+)"$`),
	} {
		match := pattern.FindStringSubmatch(stateSource)
		if match == nil {
			problems = append(problems, problem("agents/go/state/state.go", "could not resolve snapshot %s filename", label))
		} else {
			problems = append(problems, requireTokens(where, text, match[1])...)
		}
	}
	problems = append(problems, requireTokens(where, text, "manifest_sha256", "format_version", `args: ["state", "restore",`)...)
	if strings.Contains(text, "databases=") {
		problems = append(problems, problem(where, "documents the retired databases=N completion-marker format"))
	}
	cronjob, _ := readFile(filepath.Join(root, "infra", "k8s", "base", "state-backup.yaml"))
	if !strings.Contains(cronjob, "- state\n") || !strings.Contains(cronjob, "- backup\n") || strings.Contains(cronjob, "command:") {
		problems = append(problems, problem("infra/k8s/base/state-backup.yaml", "backup CronJob must use the shared agent state CLI"))
	}
	drill, _ := readFile(filepath.Join(root, "infra", "scripts", "backup-drill.sh"))
	match := regexp.MustCompile(`(?m)^echo "([^"]+)"$`).FindStringSubmatch(drill)
	if match == nil {
		problems = append(problems, problem("infra/scripts/backup-drill.sh", "could not resolve the drill completion line"))
	} else if !strings.Contains(text, match[1]) {
		problems = append(problems, problem(where, "state drill completion line drifted: expected %q, found %q", match[1], ""))
	}
	return problems
}

func goStructFields(path, typeName string) []string {
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil
	}
	var fields []string
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			structure, structureOK := typeSpec.Type.(*ast.StructType)
			if !ok || !structureOK || typeSpec.Name.Name != typeName {
				continue
			}
			for _, field := range structure.Fields.List {
				for _, name := range field.Names {
					fields = append(fields, name.Name)
				}
			}
		}
	}
	return fields
}

func checkRetrievalCourseContract(root string, pages pageSet) []Problem {
	const where = "content/3. Capabilities/3.4. Memory.md"
	fields := goStructFields(filepath.Join(root, "agents", "go", "memory", "retrieval.go"), "Provenance")
	if len(fields) == 0 {
		return []Problem{problem("agents/go/memory/retrieval.go", "could not resolve semantic index provenance fields")}
	}
	text := pages[where]
	var problems []Problem
	for _, field := range fields {
		if !strings.Contains(text, field) && !strings.Contains(strings.ToLower(text), strings.ToLower(field)) {
			problems = append(problems, problem(where, "source contract is missing %q", "`"+field+"`"))
		}
	}
	if !strings.Contains(text, "Historical checkpoint (") || strings.Contains(text, "\nMeasured checkpoint (") {
		problems = append(problems, problem(where, "the superseded retrieval result must be labeled as a historical checkpoint"))
	}
	return problems
}

func checkDomainCourseContract(root string, pages pageSet) []Problem {
	const where = "content/8. Community/8.7. Capstone.md"
	fields := goStructFields(filepath.Join(root, "agents", "go", "domain", "vocabulary.go"), "Vocabulary")
	if len(fields) == 0 {
		return []Problem{problem("agents/go/domain/vocabulary.go", "could not resolve Vocabulary fields")}
	}
	text := pages[where]
	var problems []Problem
	for _, field := range fields {
		if !strings.Contains(text, "`"+field+"`") {
			problems = append(problems, problem(where, "source contract is missing %q", "`"+field+"`"))
		}
	}
	problems = append(problems, requireTokens(where, text, "agents/go/domain/vocabulary.go", "domain/pivot_test.go", "Reference", "Pivot", "AdaptDataset")...)
	if regexp.MustCompile(`(?i)reference does not ship (?:that|the) seam`).MatchString(text) {
		problems = append(problems, problem(where, "claims the shipped portability seam is absent"))
	}
	return problems
}

// sectionAfterHeading returns the body under one H2, and refuses when that H2 is
// not there.
//
// Cutting a page on literal heading text is how several contract checks find the
// section they grade, and it fails open: rename the heading and `strings.Cut`
// quietly returns an empty section, which every "does the section mention X"
// check then passes. `checkOutcomeEvidenceContract` died that way and nobody
// noticed, because a check that cannot fail looks exactly like a check that
// passes. Reporting the missing heading turns a silent disarm into a build error
// naming the heading a rename has to preserve.
func sectionAfterHeading(where, text, heading string) (string, []Problem) {
	_, section, found := strings.Cut(text, heading)
	if !found {
		return "", []Problem{problem(where, "expected the heading %q, which a contract check reads its section from", heading)}
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return section, nil
}

func checkCapacityContract(root string, pages pageSet) []Problem {
	support, _ := readFile(filepath.Join(root, "SUPPORT.md"))
	match := regexp.MustCompile(`(?m)^<!-- local-platform-capacity: total-ram-gib=(\d+) free-disk-gib=(\d+) -->$`).FindAllStringSubmatch(support, -1)
	if len(match) != 1 {
		return []Problem{problem("SUPPORT.md", "expected exactly one machine-readable local-platform-capacity contract")}
	}
	ram, disk := match[0][1], match[0][2]
	problems := requireTokens("SUPPORT.md", support, "**"+ram+" GiB total RAM**", "**"+disk+" GiB free disk**")
	doctor, _ := readFile(filepath.Join(root, "scripts", "doctor.sh"))
	problems = append(problems, requireTokens("scripts/doctor.sh", doctor, "local-platform-capacity:", "platform_total_ram_gib", "platform_free_disk_gib")...)
	for where, text := range pages {
		for _, value := range []string{ram + " GiB total RAM", disk + " GiB free disk"} {
			if strings.Contains(text, value) {
				problems = append(problems, problem(where, "capacity literal %q belongs only in SUPPORT.md", value))
			}
		}
	}
	return problems
}

func checkProviderContract(root string, pages pageSet) []Problem {
	const envWhere = ".env.example"
	const pageWhere = "content/1. Setup/1.4. Providers.md"
	environment, _ := readFile(filepath.Join(root, envWhere))
	page := pages[pageWhere]
	problems := requireTokens(envWhere, environment, "GOOGLE_CLOUD_PROJECT=your-gcp-project-id")
	problems = append(problems, requireTokens(pageWhere, page,
		"GOOGLE_CLOUD_PROJECT=your-gcp-project-id",
		"GCP_PROJECT_ID=your-gcp-project-id mise run doctor:gcp",
		"does not load `GOOGLE_CLOUD_PROJECT` from `.env`",
	)...)
	assignment := regexp.MustCompile(`(?m)^\s*#?\s*(?:GOOGLE_CLOUD_PROJECT|GCP_PROJECT_ID)\s*=\s*agentops-open-course\s*$`)
	for where, text := range map[string]string{envWhere: environment, pageWhere: page} {
		if assignment.MatchString(text) {
			problems = append(problems, problem(where, "learner GCP example targets the maintainer-owned project"))
		}
	}
	return problems
}

func checkArtifactRetentionContract(root string, pages pageSet) []Problem {
	agents, _ := readFile(filepath.Join(root, "AGENTS.md"))
	match := regexp.MustCompile(`caps artifact and log retention at \*\*(\d+) days\*\*`).FindStringSubmatch(agents)
	if match == nil {
		return []Problem{problem("AGENTS.md", "Actions artifact retention policy is missing")}
	}
	limit, _ := strconv.Atoi(match[1])
	var problems []Problem
	// Pair each retention value with the step that sets it, rather than counting the two
	// literals per file: a workflow with two uploads and one retention-days used to
	// balance out to a pass, and an over-limit value named no job an author could go fix.
	for _, path := range workflowFiles(root) {
		where := relative(root, path)
		content, readErr := readFile(path)
		if readErr != nil {
			problems = append(problems, problem(where, "could not read workflow: %v", readErr))
			continue
		}
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct {
					With struct {
						// The value is `any` because YAML admits both `retention-days: 7` and a
						// quoted expression; anything that is not a plain number is reported
						// rather than silently read as zero.
						RetentionDays any `yaml:"retention-days"`
					} `yaml:"with"`
					Uses string `yaml:"uses"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if parseErr := yaml.Unmarshal([]byte(content), &workflow); parseErr != nil {
			problems = append(problems, problem(where, "could not parse workflow: %v", parseErr))
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(workflow.Jobs)) {
			for _, step := range workflow.Jobs[name].Steps {
				if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
					continue
				}
				days, ok := step.With.RetentionDays.(int)
				if !ok {
					problems = append(problems, problem(where,
						"job %q uploads an artifact without a numeric retention-days value", name))
					continue
				}
				if days > limit {
					problems = append(problems, problem(where,
						"job %q keeps Actions artifact retention %d days, which exceeds the %d-day policy", name, days, limit))
				}
			}
		}
	}
	const pageWhere = "content/8. Community/8.2. Releases.md"
	problems = append(problems, requireTokens(pageWhere, pages[pageWhere], fmt.Sprintf("**%d days**", limit), "immutable GitHub release", "OCI image")...)
	return problems
}
