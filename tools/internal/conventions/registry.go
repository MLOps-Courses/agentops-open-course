package conventions

// The one registry of corpus-level documentation checks.
//
// Three files used to spell the source-derived subset independently: CheckDocs
// listed the calls it makes, CheckCopiedSources re-listed four of them, and the
// scheduled freshness reporter described those same four in an English sentence.
// Nothing bound the three, so a fifth source-derived check could join CheckDocs
// while the report kept claiming it had compared only the original four, and a
// renamed check would have left the subset calling something that no longer
// existed. The subset is now derived from this list, and the report's sentence is
// derived from the same entries, so all three move together or not at all.

import "strings"

// checkFunc is the one shape every corpus-level check is adapted to. Per-page
// checks stay out of this registry on purpose: they take (where, text) and run
// inside CheckDocs's sorted page loop, and no subset draws from them.
type checkFunc func(root string, pages pageSet) []Problem

// namedCheck is one corpus-level check. summary is non-empty exactly when the
// scheduled freshness report also runs the check, and it holds the words that
// report prints for it — so membership in the subset and the prose describing the
// subset are the same fact, recorded once.
type namedCheck struct {
	run     checkFunc
	name    string
	summary string
}

// docsCheck registers a check that only the docs gate runs.
//
// Two constructors exist so subset membership is a decision at the call site
// rather than a default: adding a source-derived check to the full set while the
// freshness subset stays behind is the exact drift this registry closes. The
// constructors make that decision visible, not mandatory — a bare namedCheck
// literal still compiles inside this package. What fails an author who skips the
// decision is reviewedCorpusCheckCount in registry_test.go.
func docsCheck(name string, run checkFunc) namedCheck {
	return namedCheck{name: name, run: run}
}

// copiedSourceCheck registers a check that the scheduled freshness report runs as
// well. summary is the noun phrase that report prints for it.
func copiedSourceCheck(name, summary string, run checkFunc) namedCheck {
	return namedCheck{name: name, summary: summary, run: run}
}

// pagesOnly and rootOnly adapt the two narrower check shapes. Adapting at the
// registry keeps each check's own signature honest about what it reads, rather
// than growing every check an unused parameter to satisfy the list.
func pagesOnly(run func(pages pageSet) []Problem) checkFunc {
	return func(_ string, pages pageSet) []Problem { return run(pages) }
}

func rootOnly(run func(root string) []Problem) checkFunc {
	return func(root string, _ pageSet) []Problem { return run(root) }
}

// pageRouteProblems drops the route table buildPageRoutes also returns. CheckDocs
// wants only its problems; CheckRendered builds its own table separately.
func pageRouteProblems(pages pageSet) []Problem {
	_, problems := buildPageRoutes(pages)
	return problems
}

// corpusChecks is the ordered full set of corpus-level checks. The order is
// load-bearing twice over: it is the order a failing `conventions docs` run prints
// its problems in, and the order the freshness subset inherits by being filtered
// out of this same list.
var corpusChecks = []namedCheck{
	docsCheck("index kinds", pagesOnly(checkIndexKinds)),
	docsCheck("page routes", pagesOnly(pageRouteProblems)),
	docsCheck("permalink config", rootOnly(checkPermalinkConfig)),
	docsCheck("navigation", checkNavigation),
	docsCheck("chapter index coverage", pagesOnly(checkChapterIndexCoverage)),
	docsCheck("image files", checkImageFiles),
	docsCheck("closing cadence", pagesOnly(checkClosingCadence)),
	docsCheck("source snippet coverage", pagesOnly(checkSourceSnippetCoverage)),
	docsCheck("literal repository paths", checkLiteralRepositoryPaths),
	docsCheck("documented Go packages", checkDocumentedGoPackages),
	copiedSourceCheck("documented tasks", "documented task names", checkDocumentedTasks),
	docsCheck("offline module checks", rootOnly(checkOfflineModuleChecks)),
	docsCheck("serialized module checks", rootOnly(checkSerializedModuleChecks)),
	docsCheck("read-only installs", rootOnly(checkReadOnlyInstalls)),
	docsCheck("nested mise trust", pagesOnly(checkNestedMiseTrust)),
	docsCheck("Hugo extended tool", rootOnly(checkHugoExtendedTool)),
	docsCheck("local action checkouts", rootOnly(checkLocalActionCheckouts)),
	docsCheck("Pages deployment", rootOnly(checkPagesDeployment)),
	docsCheck("workflow shell expressions", rootOnly(checkWorkflowShellExpressions)),
	copiedSourceCheck("task expansions", "task expansions", checkTaskExpansions),
	docsCheck("eval thresholds", checkEvalThresholds),
	docsCheck("theme pin", rootOnly(checkThemePin)),
	copiedSourceCheck("source versions", "source versions", checkSourceVersions),
	docsCheck("current source contracts", checkCurrentSourceContracts),
	copiedSourceCheck("port contracts", "stable ports", checkPortContracts),
	docsCheck("probe contracts", checkProbeContracts),
	docsCheck("network policy contracts", checkNetworkPolicyContracts),
	docsCheck("cost owner", pagesOnly(checkCostOwner)),
	docsCheck("quickstarts", checkQuickstarts),
	docsCheck("maintainer drift", checkMaintainerDrift),
	docsCheck("GCP runbook", rootOnly(checkGCPRunbook)),
	docsCheck("Skaffold runbooks", rootOnly(checkSkaffoldRunbooks)),
	docsCheck("eval contract", rootOnly(checkEvalContract)),
}

// runChecks executes an ordered slice of checks against one already-loaded page
// set. Both callers share it so the subset cannot acquire its own traversal.
func runChecks(checks []namedCheck, root string, pages pageSet) []Problem {
	problems := make([]Problem, 0, len(checks))
	for _, check := range checks {
		problems = append(problems, check.run(root, pages)...)
	}
	return problems
}

// copiedSourceChecks is the source-derived subset, filtered out of corpusChecks so
// it keeps the full set's relative order without restating it.
func copiedSourceChecks() []namedCheck {
	subset := make([]namedCheck, 0, len(corpusChecks))
	for _, check := range corpusChecks {
		if check.summary != "" {
			subset = append(subset, check)
		}
	}
	return subset
}

// CopiedSourceSummary names what CheckCopiedSources compared, in the order it ran
// the comparisons. The freshness report prints it instead of a hand-written list,
// which is what stops that report from naming a check the subset no longer runs.
func CopiedSourceSummary() string {
	checks := copiedSourceChecks()
	summaries := make([]string, len(checks))
	for index, check := range checks {
		summaries[index] = check.summary
	}
	switch len(summaries) {
	case 0:
		return "nothing"
	case 1:
		return summaries[0]
	case 2:
		return summaries[0] + " and " + summaries[1]
	default:
		return strings.Join(summaries[:len(summaries)-1], ", ") + ", and " + summaries[len(summaries)-1]
	}
}
