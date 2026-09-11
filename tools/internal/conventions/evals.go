package conventions

// The evaluation contract. evals/mise.toml is the authority for the thresholds and
// for the task surface the course teaches by quoting the command line itself.

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml"
)

// checkEvalThresholds pins two pages to the eval task's command line.
//
// The course teaches the thresholds by pointing at that line rather than at a policy
// file — there is no policy file — so the line is the contract. 4.4 quotes the flags
// verbatim in the sentence that tells a learner to read them there; 7.0 states the same
// three numbers in prose. Both are checked against the task, not against each other.
func checkEvalThresholds(root string, pages pageSet) []Problem {
	const evaluationsWhere = "content/4. Quality/4.4. Evaluations.md"
	const reproducibilityWhere = "content/7. Observability/7.0. Reproducibility.md"
	const anchor = "Read the threshold off the task's command line"
	fields := strings.Fields(strings.Join(taskCommands(filepath.Join(root, "evals", "mise.toml"), "eval"), "\n"))
	flag := func(name string) string {
		for index, field := range fields {
			if field == name && index+1 < len(fields) {
				return fields[index+1]
			}
		}
		return ""
	}
	repeat, passRate, required := flag("--repeat"), flag("--min-pass-rate"), flag("--required-cases")
	if repeat == "" || passRate == "" || required == "" {
		return []Problem{problem("evals/mise.toml", "the eval task must declare --repeat, --min-pass-rate, and --required-cases")}
	}
	cases := len(strings.Split(required, ","))
	var problems []Problem
	// 4.4's own sentence is the one that claims to reproduce the command line.
	sentence := ""
	for _, line := range splitLines(pages[evaluationsWhere]) {
		if strings.HasPrefix(line, anchor) {
			sentence = line
		}
	}
	if sentence == "" {
		problems = append(problems, problem(evaluationsWhere, "expected the sentence that reads the thresholds off the eval task's command line"))
	} else {
		for _, quoted := range []string{"`--repeat " + repeat + "`", "`--min-pass-rate " + passRate + "`", "`--required-cases`"} {
			if !strings.Contains(sentence, quoted) {
				problems = append(problems, problem(evaluationsWhere, "the threshold sentence must quote %s", quoted))
			}
		}
		if !strings.Contains(sentence, englishNumber(cases)+" names") {
			problems = append(problems, problem(evaluationsWhere, "the threshold sentence must name %s required cases", englishNumber(cases)))
		}
	}
	for _, wanted := range []string{englishNumber(mustAtoi(repeat)) + " samples", passRate, englishNumber(cases) + " required"} {
		if !strings.Contains(pages[reproducibilityWhere], wanted) {
			problems = append(problems, problem(reproducibilityWhere, "the eval description must state %q, which the eval task declares", wanted))
		}
	}

	// 4.3 pastes the task's whole command line as a verbatim capture and then counts the
	// cases in prose. It was left out of this check and drifted a case behind the task,
	// on the page whose own argument is that the task's output *is* the release policy.
	const metricsWhere = "content/4. Quality/4.3. Metrics.md"
	if !strings.Contains(pages[metricsWhere], "--required-cases "+required) {
		problems = append(problems, problem(metricsWhere, "the captured eval command must quote --required-cases %s verbatim, as the task declares it", required))
	}
	if !strings.Contains(pages[metricsWhere], "names "+englishNumber(cases)+" cases") {
		problems = append(problems, problem(metricsWhere, "the prose under the capture must name %s cases, which the eval task declares", englishNumber(cases)))
	}
	return problems
}

// taskFlag returns the value following a flag on a task's command line, or "" when the
// flag is absent. Reading the value as a whole field is what keeps callers from
// prefix-matching a comma-separated list and accepting a longer one.
func taskFlag(manifest, task, name string) string {
	fields := strings.Fields(strings.Join(taskCommands(manifest, task), "\n"))
	for index, field := range fields {
		if field == name && index+1 < len(fields) {
			return fields[index+1]
		}
	}
	return ""
}

func mustAtoi(text string) int {
	value, err := strconv.Atoi(text)
	if err != nil {
		return -1
	}
	return value
}

// checkEvalContract pins the numbers the course teaches. The evaluation threshold used
// to live in a release-policy state machine that no learner could satisfy; it now lives on
// the command line, which means the page that quotes it and the task that runs it can drift
// apart. This is the check that stops them.
func checkEvalContract(root string) []Problem {
	manifest := filepath.Join(root, "evals", "mise.toml")
	var problems []Problem

	// The taught command, term by term. A learner reads the required cases as the safety
	// floor, so silently dropping one would leave the prose telling a lie.
	command := taskExpansion(manifest, "eval")
	for _, required := range []string{
		"--repeat 3",
		"--min-pass-rate 0.33",
		"--judge",
		"--output results.json",
	} {
		if strings.Count(command, required) != 1 {
			problems = append(problems, problem("evals/mise.toml", "the eval task must state %q exactly once, because the course teaches it by quoting this command", required))
		}
	}

	// The safety floor is compared as a whole value, never as a substring. It used to be
	// asserted with strings.Count over a four-name literal, which a five-name list still
	// satisfied as a prefix — so a fifth case was added, the gate stayed green, and the
	// pages quoting the command drifted a release behind it.
	const safetyFloor = "investigation-recalls-context,remediation-loads-skill,restart-needs-approval,resolve-needs-approval,restart-approval-verified"
	if actual := taskFlag(manifest, "eval", "--required-cases"); actual != safetyFloor {
		problems = append(problems, problem("evals/mise.toml", "the eval task's --required-cases is %q, want exactly %q; changing the safety floor is a deliberate review, and every page quoting it must be re-captured", actual, safetyFloor))
	}

	// Four tasks, no more. Every capability beyond them is a documented flag on `eval`.
	expected := []string{"eval", "eval:ab", "eval:judge-calibration", "eval:validate"}
	if actual := evalTaskNames(manifest); !slices.Equal(actual, expected) {
		problems = append(problems, problem("evals/mise.toml", "evaluation tasks are %v, want exactly %v", actual, expected))
	}

	// A model-backed task needs the redacted root .env; an offline one must not read it,
	// or an artifact-only comparison starts depending on a credential.
	for _, task := range []string{"eval", "eval:judge-calibration"} {
		if !taskLoadsRedactedEnv(manifest, task, "../.env") {
			problems = append(problems, problem("evals/mise.toml", "live task %s must load the redacted root .env", task))
		}
	}
	for _, task := range []string{"eval:validate", "eval:ab"} {
		if taskLoadsRedactedEnv(manifest, task, "../.env") {
			problems = append(problems, problem("evals/mise.toml", "offline or artifact-only task %s must not load the root .env", task))
		}
	}

	promptCommand := taskExpansion(manifest, "eval:ab")
	if !strings.Contains(promptCommand, "--require-distinct-source") || !taskAcceptsPromptArtifacts(manifest, "eval:ab") {
		problems = append(problems, problem("evals/mise.toml", "eval:ab must accept explicit prompt artifacts from distinct source revisions"))
	}
	rootManifest := filepath.Join(root, "mise.toml")
	rootPromptCommand := taskExpansion(rootManifest, "eval:ab")
	if !taskAcceptsPromptArtifacts(rootManifest, "eval:ab") || !strings.Contains(rootPromptCommand, "mise run eval:ab --") {
		problems = append(problems, problem("mise.toml", "root eval:ab must forward explicit prompt artifacts to the eval module"))
	}
	return problems
}

// evalTaskNames lists every declared eval* task, sorted, so the set can be compared exactly.
func evalTaskNames(path string) []string {
	document, err := loadTOML(path)
	if err != nil {
		return nil
	}
	tasks, ok := document.GetPath([]string{"tasks"}).(*toml.Tree)
	if !ok {
		return nil
	}
	var names []string
	for _, name := range tasks.Keys() {
		if name == "eval" || strings.HasPrefix(name, "eval:") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func taskAcceptsPromptArtifacts(path, name string) bool {
	document, err := loadTOML(path)
	if err != nil {
		return false
	}
	usage, ok := document.GetPath([]string{"tasks", name, "usage"}).(string)
	if !ok || !strings.Contains(usage, `flag "--baseline <path>"`) ||
		!strings.Contains(usage, `flag "--candidate <path>"`) {
		return false
	}
	command := taskExpansion(path, name)
	return strings.Count(command, `--baseline "$usage_baseline"`) == 1 &&
		strings.Count(command, `--candidate "$usage_candidate"`) == 1 &&
		strings.Count(command, "--baseline") == 1 && strings.Count(command, "--candidate") == 1
}

func taskLoadsRedactedEnv(path, name, expectedPath string) bool {
	document, err := loadTOML(path)
	if err != nil {
		return false
	}
	pathValue, pathOK := document.GetPath([]string{"tasks", name, "env", "_", "file", "path"}).(string)
	redact, redactOK := document.GetPath([]string{"tasks", name, "env", "_", "file", "redact"}).(bool)
	return pathOK && redactOK && pathValue == expectedPath && redact
}
