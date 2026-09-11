package conventions

// Figures a page states in prose or inside a capture, each derived here from the
// file that settles it, so a count cannot be hand-edited into agreement.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	englishUnits = []string{
		"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
		"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen",
		"seventeen", "eighteen", "nineteen",
	}
	englishTens = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
)

// englishNumber spells 0-99, which covers every count a course page states in words.
func englishNumber(value int) string {
	switch {
	case value < 0 || value > 99:
		return strconv.Itoa(value)
	case value < len(englishUnits):
		return englishUnits[value]
	case value%10 == 0:
		return englishTens[value/10]
	default:
		return englishTens[value/10] + "-" + englishUnits[value%10]
	}
}

func parseCountedNumber(text string) (int, bool) {
	if value, err := strconv.Atoi(text); err == nil {
		return value, true
	}
	for value := 0; value <= 99; value++ {
		if englishNumber(value) == strings.ToLower(text) {
			return value, true
		}
	}
	return 0, false
}

var (
	// Deliberately narrow. Pages state small subset counts in words ("Four settings
	// decide the model"), and only the resolved total is ever written as digits or in
	// the forty-/fifty- range this contract lives in.
	settingsClaim = regexp.MustCompile(`(?i)\b([0-9]+|forty-\w+|fifty-\w+)\s+(?:resolved\s+)?settings\b`)
	alertName     = regexp.MustCompile(`(?m)^\s*- alert:\s*(\S+)\s*$`)
)

// checkDerivedFigures ties every count a capture prints back to a file that settles it.
//
// checkExactCountClaims already bans brittle counts, but it iterates linesOutsideFences,
// so every figure inside a ```text capture was deliberately exempt — which is how eight
// pages came to print a test total wrong by 47 and 7.2b came to contradict its own rule
// file by five rules. Suite totals and coverage cannot be derived without running the
// suites, and check:docs runs before test, so those come from the committed manifest
// `mise run docs:captures` writes; everything else is derived from source here.
func checkDerivedFigures(root string, pages pageSet) []Problem {
	problems := checkSettingsFigure(root, pages)
	problems = append(problems, checkAlertingFigures(root, pages)...)
	return append(problems, checkCaptureManifest(root, pages)...)
}

func checkSettingsFigure(root string, pages pageSet) []Problem {
	const where = "agents/go/config/config.go"
	source, err := readFile(filepath.Join(root, filepath.FromSlash(where)))
	if err != nil {
		return []Problem{problem(where, "could not read the configuration contract: %v", err)}
	}
	// One env tag per field is what `config:check` resolves and prints.
	settings := strings.Count(source, `env:"`)
	if settings == 0 {
		return []Problem{problem(where, "expected the configuration contract to declare env-tagged settings")}
	}
	var problems []Problem
	for page, text := range pages {
		for _, line := range splitLines(text) {
			for _, match := range settingsClaim.FindAllStringSubmatch(line, -1) {
				value, ok := parseCountedNumber(match[1])
				if ok && value != settings {
					problems = append(problems, problem(page, "states %s settings; %s resolves %d", match[1], where, settings))
				}
			}
		}
	}
	slices.SortFunc(problems, func(left, right Problem) int { return strings.Compare(left.String(), right.String()) })
	return problems
}

func checkAlertingFigures(root string, pages pageSet) []Problem {
	const rulesWhere = "infra/k8s/overlays/local/prometheus-rules.yaml"
	const pageWhere = "content/7. Observability/7.2b. Alerting.md"
	rules, err := readFile(filepath.Join(root, filepath.FromSlash(rulesWhere)))
	if err != nil {
		return []Problem{problem(rulesWhere, "could not read alerting rules: %v", err)}
	}
	records := len(regexp.MustCompile(`(?m)^\s*- record:\s*\S+\s*$`).FindAllString(rules, -1))
	alerts := alertName.FindAllStringSubmatch(rules, -1)
	const anchor = "That is both commands run back to back"
	page := pages[pageWhere]
	problems := requireTokens(pageWhere, page, fmt.Sprintf("SUCCESS: %d rules found", records+len(alerts)))
	sentence := ""
	for _, line := range splitLines(page) {
		if strings.HasPrefix(line, anchor) {
			sentence = line
		}
	}
	if sentence == "" {
		// A count rule whose only trigger is an English sentence disarms itself the
		// moment someone rewords or deletes that sentence, and the page then states
		// its two halves with nothing checking them. A missing anchor is therefore a
		// failure that names what it needed, not a silent skip.
		problems = append(problems, problem(pageWhere,
			"expected the sentence starting %q, which states the recording-rule and alert counts derived from %s", anchor, rulesWhere))
	} else {
		// Ordered rather than ranged over a map, so a drifted page reports its halves
		// in the same order on every run.
		for _, half := range []struct {
			label string
			count int
		}{{"recording rules", records}, {"alerts", len(alerts)}} {
			if !strings.Contains(sentence, englishNumber(half.count)+" "+half.label) {
				problems = append(problems, problem(pageWhere, "the rules-run sentence must state %s %s", englishNumber(half.count), half.label))
			}
		}
	}
	// Every shipped alert has to be answerable somewhere in the chapter that owns
	// alerting. This is the rule that would have caught the three cost alerts.
	var unnamed []string
	for _, match := range alerts {
		named := false
		for where, text := range pages {
			if strings.HasPrefix(where, "content/7. Observability/") && strings.Contains(text, "`"+match[1]+"`") {
				named = true
				break
			}
		}
		if !named {
			unnamed = append(unnamed, match[1])
		}
	}
	slices.Sort(unnamed)
	if len(unnamed) > 0 {
		problems = append(problems, problem(rulesWhere, "chapter 7 never names these shipped alerts: %s", strings.Join(unnamed, ", ")))
	}
	return problems
}
