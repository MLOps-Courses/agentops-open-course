package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
)

// ComparisonArtifactSchemaVersion is 3: schema 2's `newly_flaky_cases` folded the
// judged verdict in with the rules, so it now reads the rule-decided scores alone and
// the judged half is reported beside it as `newly_flaky_judged_cases`.
const ComparisonArtifactSchemaVersion = 3

type RunReference struct {
	Model     ModelEvidence   `json:"model"`
	EvalSet   EvalSetEvidence `json:"evalset"`
	RunID     string          `json:"run_id"`
	Transport string          `json:"transport"`
	Source    SourceEvidence  `json:"source"`
}

type ComparisonArtifact struct {
	ScoreDeltas map[string]float64 `json:"score_deltas"`
	// NewlyFlakyCases names cases the candidate decided one way on some samples and the
	// other way on others while the baseline was consistent about them. A pass rate
	// cannot show this: a case that went from three-of-three to two-of-three moves the
	// rate by a third of one case and has changed from a guarantee into a coin toss.
	//
	// It folds the rule-decided scores alone, for the same reason DeterministicPass
	// does, and it is the one flakiness reading that fails the comparison.
	NewlyFlakyCases []string `json:"newly_flaky_cases"`
	// NewlyFlakyJudgedCases is the same reading of the model-produced verdicts, and it
	// is reported rather than gated: a 4B judge disagreeing with itself on one sample of
	// a repeated run is worth seeing and is never on its own proof that the candidate
	// changed. The score delta beside it averages that flip away, which is why the case
	// names are listed here.
	NewlyFlakyJudgedCases []string     `json:"newly_flaky_judged_cases"`
	Baseline              RunReference `json:"baseline"`
	Candidate             RunReference `json:"candidate"`
	BaselinePassRate      float64      `json:"baseline_pass_rate"`
	CandidatePassRate     float64      `json:"candidate_pass_rate"`
	PassRateDelta         float64      `json:"pass_rate_delta"`
	TotalTokensDelta      int64        `json:"total_tokens_delta"`
	ModelCallsDelta       int64        `json:"model_calls_delta"`
	// TotalDurationDelta is the candidate's wall clock minus the baseline's, summed
	// over every sample. Without it a candidate that answers just as well and twice
	// as slowly compares clean, which is the regression Chapter 7 budgets exist for.
	TotalDurationDelta int64 `json:"total_duration_ms_delta"`
	SchemaVersion      int   `json:"schema_version"`
	DeterministicPass  bool  `json:"deterministic_pass"`
}

func totalDuration(cases []CaseResult) int64 {
	var total int64
	for _, result := range cases {
		total += result.DurationMillis
	}
	return total
}

// newlyFlakyCases finds cases the candidate is inconsistent about and the
// baseline was not, under the per-sample outcome the caller hands it.
//
// It reads the per-sample rows rather than the serialized summary, for the same
// reason the score comparison does: an artifact's own summary is a claim, and the
// rows are the evidence. It recomputes each sample's outcome rather than reading
// `passed`, because that flag folds every score together and the two flakiness
// columns need the rule-decided and the judged halves apart. A case that appears in
// only one of the two runs is not flaky, it is new or removed, and the score
// comparison already reports that.
func newlyFlakyCases(baseline, candidate []CaseResult, passed func(map[string]float64) bool) []string {
	count := func(cases []CaseResult) map[string]CaseConsistency {
		byCase := make(map[string]CaseConsistency, len(cases))
		for _, result := range cases {
			record := byCase[result.ID]
			record.ID, record.Samples = result.ID, record.Samples+1
			if passed(result.Scores) {
				record.Passed++
			}
			byCase[result.ID] = record
		}
		return byCase
	}
	before, after := count(baseline), count(candidate)
	var flaky []string
	for caseID, candidateRecord := range after {
		baselineRecord, known := before[caseID]
		if known && !baselineRecord.Flaky() && candidateRecord.Flaky() {
			flaky = append(flaky, caseID)
		}
	}
	slices.Sort(flaky)
	return flaky
}

func LoadRunArtifact(path string) (RunArtifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return RunArtifact{}, fmt.Errorf("open evaluation artifact %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var artifact RunArtifact
	if decodeErr := decoder.Decode(&artifact); decodeErr != nil {
		return RunArtifact{}, fmt.Errorf("decode evaluation artifact %s: %w", path, decodeErr)
	}
	if eofErr := requireJSONEOF(decoder); eofErr != nil {
		return RunArtifact{}, fmt.Errorf("decode evaluation artifact %s: %w", path, eofErr)
	}
	if err := validateComparisonRunIdentity(artifact); err != nil {
		return RunArtifact{}, fmt.Errorf("evaluation artifact %s has incomplete identity", path)
	}
	if len(artifact.Cases) == 0 || artifact.Summary.MinimumPassRate < 0 || artifact.Summary.MinimumPassRate > 1 {
		return RunArtifact{}, fmt.Errorf("evaluation artifact %s has invalid cases or threshold", path)
	}
	if _, usageErr := totalUsage(artifact.Cases); usageErr != nil {
		return RunArtifact{}, fmt.Errorf("evaluation artifact %s has invalid usage: %w", path, usageErr)
	}
	return artifact, nil
}

func CompareRuns(baseline, candidate RunArtifact) (ComparisonArtifact, error) {
	if err := validateComparisonRunIdentity(baseline); err != nil {
		return ComparisonArtifact{}, fmt.Errorf("baseline comparison artifact has incomplete identity: %w", err)
	}
	if err := validateComparisonRunIdentity(candidate); err != nil {
		return ComparisonArtifact{}, fmt.Errorf("candidate comparison artifact has incomplete identity: %w", err)
	}
	if baseline.EvalSet != candidate.EvalSet {
		return ComparisonArtifact{}, errors.New("comparison artifacts must use the same evalset id and digest")
	}
	if err := validateComparisonScores("baseline", baseline.Cases); err != nil {
		return ComparisonArtifact{}, err
	}
	if err := validateComparisonScores("candidate", candidate.Cases); err != nil {
		return ComparisonArtifact{}, err
	}
	baselineContract, err := comparisonCaseContract("baseline", baseline.Cases)
	if err != nil {
		return ComparisonArtifact{}, err
	}
	candidateContract, err := comparisonCaseContract("candidate", candidate.Cases)
	if err != nil {
		return ComparisonArtifact{}, err
	}
	baselineCases := caseKeys(baselineContract)
	candidateCases := caseKeys(candidateContract)
	if !slices.Equal(baselineCases, candidateCases) {
		return ComparisonArtifact{}, errors.New("comparison artifacts must contain identical case samples")
	}
	for _, key := range baselineCases {
		if !slices.Equal(baselineContract[key], candidateContract[key]) {
			return ComparisonArtifact{}, fmt.Errorf(
				"comparison artifacts must contain identical score names for case sample %q", key,
			)
		}
	}
	baselineSummary := summarizeComparisonCases(baseline.Cases)
	candidateSummary := summarizeComparisonCases(candidate.Cases)
	result := ComparisonArtifact{
		SchemaVersion: ComparisonArtifactSchemaVersion,
		Baseline:      runReference(baseline), Candidate: runReference(candidate),
		BaselinePassRate: baselineSummary.passRate, CandidatePassRate: candidateSummary.passRate,
		PassRateDelta: candidateSummary.passRate - baselineSummary.passRate,
		ScoreDeltas:   make(map[string]float64),
		// Required-case identities are not part of the sanitized artifact schema,
		// so its serialized summary cannot be independently recomputed here. The
		// comparison trusts only the case results it can actually verify.
		//
		// The rate folded here is the deterministic one rather than the headline
		// `pass_rate` the fields above report: a field named `deterministic_pass`
		// must not turn red because a 4B judge changed its mind about one sample.
		DeterministicPass: candidateSummary.deterministicPassRate >= baselineSummary.deterministicPassRate,
	}
	for name, baselineValue := range baselineSummary.scores {
		candidateValue, found := candidateSummary.scores[name]
		if !found {
			result.DeterministicPass = false
			continue
		}
		delta := candidateValue - baselineValue
		result.ScoreDeltas[name] = delta
		// A judged verdict is still reported as a delta — it is the cheapest signal a
		// reader has that answer quality moved — but only a rule-decided score may
		// fail the comparison, for the same reason `required_cases_passed` folds over
		// deterministic scores alone.
		if delta < 0 && !IsStochasticScoreName(name) {
			result.DeterministicPass = false
		}
	}
	baselineUsage, err := totalUsage(baseline.Cases)
	if err != nil {
		return ComparisonArtifact{}, fmt.Errorf("baseline comparison artifact has invalid usage: %w", err)
	}
	candidateUsage, err := totalUsage(candidate.Cases)
	if err != nil {
		return ComparisonArtifact{}, fmt.Errorf("candidate comparison artifact has invalid usage: %w", err)
	}
	result.TotalTokensDelta = candidateUsage.TotalTokens - baselineUsage.TotalTokens
	result.ModelCallsDelta = candidateUsage.ModelCalls - baselineUsage.ModelCalls
	result.TotalDurationDelta = totalDuration(candidate.Cases) - totalDuration(baseline.Cases)
	result.NewlyFlakyCases = newlyFlakyCases(baseline.Cases, candidate.Cases, deterministicallyPassed)
	result.NewlyFlakyJudgedCases = newlyFlakyCases(baseline.Cases, candidate.Cases, judgedPassed)
	// A case that became a coin toss by rule is a regression even when the rate held:
	// it is the same evidence a single run would have shown as a pass. The judged
	// column never reaches this decision — it is the last place a flipped verdict could
	// still have failed a field named `deterministic_pass`.
	if len(result.NewlyFlakyCases) > 0 {
		result.DeterministicPass = false
	}
	return result, nil
}

func validateComparisonRunIdentity(artifact RunArtifact) error {
	if artifact.SchemaVersion != RunArtifactSchemaVersion || artifact.RunID == "" {
		return errors.New("schema version and run id are required")
	}
	if artifact.Source.Validate() != nil {
		return errors.New("a valid source identity is required")
	}
	if artifact.Model.Provider == "" || artifact.Model.Name == "" ||
		artifact.EvalSet.ID == "" || artifact.EvalSet.Digest == "" {
		return errors.New("model and evalset identities are required")
	}
	if artifact.Transport != "rest" && artifact.Transport != "a2a" {
		return errors.New("transport identity must be rest or a2a")
	}
	return nil
}

func validateComparisonScores(role string, cases []CaseResult) error {
	if len(cases) == 0 {
		return fmt.Errorf("%s comparison artifact has no cases", role)
	}
	for _, result := range cases {
		if len(result.Scores) == 0 {
			return fmt.Errorf("%s case %q sample %d has no deterministic scores", role, result.ID, result.Sample)
		}
		allScoresPassed := true
		for name, value := range result.Scores {
			if err := validateScoreValue(name, value); err != nil {
				return fmt.Errorf("%s case %q sample %d: %w", role, result.ID, result.Sample, err)
			}
			allScoresPassed = allScoresPassed && value == 1
		}
		if result.Passed != allScoresPassed {
			return fmt.Errorf(
				"%s case %q sample %d pass flag disagrees with its deterministic scores",
				role, result.ID, result.Sample,
			)
		}
	}
	return nil
}

// ComparePromptRuns keeps Git as the only instruction authority. Comparing two
// runs of the same revision would label model variance as a prompt change, and
// a dirty tree cannot name the instructions it ran.
func ComparePromptRuns(baseline, candidate RunArtifact) (ComparisonArtifact, error) {
	if !isCleanExactRevision(baseline.Source) || !isCleanExactRevision(candidate.Source) {
		return ComparisonArtifact{}, errors.New("prompt comparison artifacts must use clean exact revisions")
	}
	if baseline.Source.Revision == candidate.Source.Revision {
		return ComparisonArtifact{}, errors.New("prompt comparison artifacts must use distinct clean exact revisions")
	}
	if baseline.Model != candidate.Model {
		return ComparisonArtifact{}, errors.New("prompt comparison artifacts must use the same model identity")
	}
	if baseline.Transport != candidate.Transport {
		return ComparisonArtifact{}, errors.New("prompt comparison artifacts must use the same transport")
	}
	return CompareRuns(baseline, candidate)
}

func isCleanExactRevision(source SourceEvidence) bool {
	return source.Validate() == nil && !source.Dirty && source.Revision != ""
}

func runReference(artifact RunArtifact) RunReference {
	return RunReference{
		RunID: artifact.RunID, Source: artifact.Source,
		Model: artifact.Model, EvalSet: artifact.EvalSet, Transport: artifact.Transport,
	}
}

func comparisonCaseContract(role string, cases []CaseResult) (map[string][]string, error) {
	contract := make(map[string][]string, len(cases))
	for _, result := range cases {
		if result.ID == "" || result.Sample < 1 {
			return nil, fmt.Errorf("%s comparison artifact needs a valid case id and positive sample", role)
		}
		key := fmt.Sprintf("%s/%d", result.ID, result.Sample)
		if _, duplicate := contract[key]; duplicate {
			return nil, fmt.Errorf("%s comparison artifact contains duplicate case sample %q", role, key)
		}
		scoreNames := make([]string, 0, len(result.Scores))
		for name := range result.Scores {
			scoreNames = append(scoreNames, name)
		}
		slices.Sort(scoreNames)
		contract[key] = scoreNames
	}
	return contract, nil
}

func caseKeys(contract map[string][]string) []string {
	keys := make([]string, 0, len(contract))
	for key := range contract {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

type comparisonSummary struct {
	scores map[string]float64
	// passRate is the headline rate the artifact reports, judged verdicts included.
	passRate float64
	// deterministicPassRate is the same fraction over rule-decided scores only. The
	// artifact cannot hand it over — `DeterministicPassed` never serializes — so it is
	// recomputed here from the score names, which do.
	deterministicPassRate float64
}

func summarizeComparisonCases(cases []CaseResult) comparisonSummary {
	summary := comparisonSummary{scores: make(map[string]float64)}
	counts := make(map[string]int)
	for _, result := range cases {
		if result.Passed {
			summary.passRate++
		}
		if deterministicallyPassed(result.Scores) {
			summary.deterministicPassRate++
		}
		for name, value := range result.Scores {
			summary.scores[name] += value
			counts[name]++
		}
	}
	if len(cases) > 0 {
		summary.passRate /= float64(len(cases))
		summary.deterministicPassRate /= float64(len(cases))
	}
	for name := range summary.scores {
		summary.scores[name] /= float64(counts[name])
	}
	return summary
}

// deterministicallyPassed reads one serialized sample the way caseScores.deterministicPassed
// reads a live one: every rule-decided score passed. Like that method, a sample carrying
// nothing but a judged verdict is not a pass — there is no deterministic evidence in it.
func deterministicallyPassed(scores map[string]float64) bool {
	return samplePassed(scores, func(name string) bool { return !IsStochasticScoreName(name) })
}

// judgedPassed is its mirror: every model-produced verdict on this sample passed. A
// sample carrying none is not a pass, which keeps a run made without `--judge` out of
// the judged flakiness column rather than listing every case in it.
func judgedPassed(scores map[string]float64) bool {
	return samplePassed(scores, IsStochasticScoreName)
}

// samplePassed folds the half of one sample's scores that `include` selects. Both
// halves are read the same way — every selected score at 1, and at least one selected
// score present — so the two readings cannot drift apart as scorers are added.
func samplePassed(scores map[string]float64, include func(string) bool) bool {
	found := false
	for name, value := range scores {
		if !include(name) {
			continue
		}
		found = true
		if value != 1 {
			return false
		}
	}
	return found
}

func totalUsage(cases []CaseResult) (Usage, error) {
	var usage Usage
	for _, result := range cases {
		var err error
		usage, err = usage.add(result.Usage)
		if err != nil {
			return Usage{}, fmt.Errorf("case %q sample %d: %w", result.ID, result.Sample, err)
		}
	}
	return usage, nil
}
