package evals

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// RunArtifactSchemaVersion is 6: schema 5 gained `expected_case_samples`, which is
// the count a truncated artifact needs to admit that it is one. It is a count, so
// the sanitization boundary is unchanged — see TestRunArtifactCarriesNoContent.
const RunArtifactSchemaVersion = 6

type ModelEvidence struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Digest   string `json:"digest,omitempty"`
}

type EvalSetEvidence struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

type CaseResult struct {
	Scores map[string]float64 `json:"scores"`
	ID     string             `json:"id"`
	Usage  Usage              `json:"usage"`
	// DurationMillis is wall clock for this sample. Chapter 7 teaches latency
	// budgets that the evaluation evidence could not corroborate without it, and
	// `eval:ab` could not flag a candidate that answers correctly and twice as slow.
	DurationMillis int64 `json:"duration_ms"`
	Sample         int   `json:"sample"`
	Passed         bool  `json:"passed"`
	// DeterministicPassed drops judged verdicts. It never serializes: the artifact
	// keeps one `passed` per sample, and this decides `required_cases_passed` alone.
	DeterministicPassed bool `json:"-"`
}

type RunSummary struct {
	// CaseConsistency is how many samples of each case passed, out of how many ran.
	//
	// A pass rate flattens the one thing `--repeat` was bought for: a case that
	// passes two runs in three is not a passing case with a rounding error, it is a
	// one-in-three case caught on a good day, and the aggregate cannot tell the two
	// apart. Sorted by case id so two artifacts diff cleanly.
	CaseConsistency     []CaseConsistency `json:"case_consistency"`
	Passed              int               `json:"passed"`
	Failed              int               `json:"failed"`
	PassRate            float64           `json:"pass_rate"`
	MinimumPassRate     float64           `json:"minimum_pass_rate"`
	RequiredCasesPassed bool              `json:"required_cases_passed"`
}

// CaseConsistency is one case's per-sample record: "2 of 3 samples passed".
type CaseConsistency struct {
	ID      string `json:"id"`
	Passed  int    `json:"passed"`
	Samples int    `json:"samples"`
}

// Flaky reports a case that neither always passed nor always failed, which is the
// only reading of a repeated run that a single rate cannot express.
func (c CaseConsistency) Flaky() bool {
	return c.Passed > 0 && c.Passed < c.Samples
}

// RunArtifact is deliberately content-free: prompts, answers, tool arguments,
// tool results, rationales, URLs, and errors never cross this boundary. What
// remains is what a reader needs to reproduce the run — the checkout, the
// model, the evalset, the per-case scores, and the per-case usage.
type RunArtifact struct {
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   time.Time       `json:"completed_at"`
	Model         ModelEvidence   `json:"model"`
	EvalSet       EvalSetEvidence `json:"evalset"`
	RunID         string          `json:"run_id"`
	Source        SourceEvidence  `json:"source"`
	Transport     string          `json:"transport"`
	Cases         []CaseResult    `json:"cases"`
	Summary       RunSummary      `json:"summary"`
	SchemaVersion int             `json:"schema_version"`
	// ExpectedCaseSamples is how many case samples the run was asked for — the evalset's
	// case count times `--repeat` — and it is stamped before the first case runs.
	//
	// It is the only field that can tell a complete artifact from one a failed run left
	// behind. Every other number describes the rows that exist: a run that died on case
	// two of sixteen writes a summary over case one, and a pass rate of 1 over one case
	// is indistinguishable from a clean run of the whole evalset. Passed() reads this
	// first, so the file itself refuses to claim a run that never finished.
	ExpectedCaseSamples int `json:"expected_case_samples"`
}

type ClientFactory func(context.Context, EvalCase, int) (AgentClient, func() error, error)

type RunnerConfig struct {
	Domain          Domain
	Recorder        EvidenceRecorder
	Judge           VerdictJudge
	Clock           func() time.Time
	ClientFactory   ClientFactory
	Model           ModelEvidence
	RunID           string
	Transport       string
	EvalSet         EvalSet
	Source          SourceEvidence
	RequiredCases   []string
	MinimumPassRate float64
	Repeat          int
	RequireSchema   bool
	RequireGrounded bool
}

type caseScores struct {
	values map[string]Score
	usage  Usage
}

func Run(ctx context.Context, config RunnerConfig) (RunArtifact, error) {
	if err := validateRunnerConfig(config); err != nil {
		return RunArtifact{}, err
	}
	clock := config.clock()
	repeat := config.Repeat
	if repeat == 0 {
		repeat = 1
	}
	artifact := RunArtifact{
		SchemaVersion: RunArtifactSchemaVersion,
		RunID:         config.RunID,
		Source:        config.Source,
		Model:         config.Model,
		EvalSet:       EvalSetEvidence{ID: config.EvalSet.ID, Digest: config.EvalSet.Digest},
		Transport:     config.Transport,
		StartedAt:     clock().UTC(),
		Cases:         make([]CaseResult, 0, len(config.EvalSet.Cases)*repeat),
		// Stamped here rather than at the end, so the artifact a failed run hands back
		// carries the same expectation as a complete one and cannot read as complete.
		ExpectedCaseSamples: len(config.EvalSet.Cases) * repeat,
	}
	runCtx, endRun, err := config.Recorder.StartRun(ctx, RunEvidence{
		RunID: config.RunID, Source: config.Source,
		Model: config.Model.Name, EvalSet: config.EvalSet.ID, Transport: config.Transport,
	})
	if err != nil {
		return RunArtifact{}, fmt.Errorf("start run evidence: %w", err)
	}
	runPassed := false
	defer func() { endRun(RunOutcome{Passed: runPassed}) }()

	for sample := 1; sample <= repeat; sample++ {
		for _, evalCase := range config.EvalSet.Cases {
			result, err := runCase(runCtx, config, evalCase, sample)
			if err != nil {
				// Hand back the cases that did finish rather than dropping them. A
				// model-backed run costs hours, and the caller can write them out as
				// triage evidence. It is never release evidence: the summary describes
				// the samples that ran, not the evalset the run was asked for. The
				// artifact says so itself — its `expected_case_samples` still names the
				// whole evalset, so Passed() is false however well the rows read — and
				// the error remains the outcome the caller must act on.
				artifact.Summary = summarizeCases(artifact.Cases, config.RequiredCases, config.MinimumPassRate)
				artifact.CompletedAt = clock().UTC()
				return artifact, fmt.Errorf("case %q sample %d: %w", evalCase.ID, sample, err)
			}
			artifact.Cases = append(artifact.Cases, result)
		}
	}
	artifact.Summary = summarizeCases(artifact.Cases, config.RequiredCases, config.MinimumPassRate)
	artifact.CompletedAt = clock().UTC()
	runPassed = artifact.Passed()
	return artifact, nil
}

// Passed reports a run that cleared both thresholds over the whole evalset.
//
// Completeness is tested first because the two thresholds cannot test it: both are
// computed over the samples the artifact holds, and `required_cases_passed` is a
// lookup that only ever sees the required cases that ran. An artifact of one passing
// case out of sixteen satisfies both.
func (a RunArtifact) Passed() bool {
	return a.Complete() && a.Summary.PassRate >= a.Summary.MinimumPassRate && a.Summary.RequiredCasesPassed
}

// Complete reports that the artifact holds every case sample its run was asked for.
//
// An unstamped expectation is not a complete run: it is an artifact written before
// the field existed or assembled by hand, and neither can say what it was meant to
// cover. Reading that as incomplete is the direction that cannot publish a false
// pass.
func (a RunArtifact) Complete() bool {
	return a.ExpectedCaseSamples > 0 && len(a.Cases) == a.ExpectedCaseSamples
}

func runCase(ctx context.Context, config RunnerConfig, evalCase EvalCase, sample int) (result CaseResult, err error) {
	caseCtx, endCase, err := config.Recorder.StartCase(ctx, CaseEvidence{CaseID: evalCase.ID, Sample: sample})
	if err != nil {
		return CaseResult{}, fmt.Errorf("start case evidence: %w", err)
	}
	outcome := CaseOutcome{}
	defer func() { endCase(outcome) }()

	client, cleanup, err := config.ClientFactory(caseCtx, evalCase, sample)
	if err != nil {
		return CaseResult{}, fmt.Errorf("start isolated agent client: %w", err)
	}
	defer func() {
		err = errors.Join(err, client.Close(), cleanup())
	}()
	sessionID, err := client.CreateSession(caseCtx)
	if err != nil {
		return CaseResult{}, err
	}
	evaluated := caseScores{values: make(map[string]Score)}
	questions := make([]string, 0, len(evalCase.Conversation))
	answers := make([]string, 0, len(evalCase.Conversation))
	references := make([]string, 0, len(evalCase.Conversation))
	started := config.clock()()
	var previous Turn
	for _, invocation := range evalCase.Conversation {
		question := invocation.UserContent.Text()
		turn, err := sendInvocation(caseCtx, client, sessionID, invocation, previous)
		if errors.Is(err, errNothingToConfirm) {
			// The agent declined to propose the guarded action, so there is nothing for
			// the human to approve and the remaining turns cannot be replayed. That is
			// behavior, not infrastructure: the case stops here and scores as a failed
			// confirmation, which is what keeps a model's refusal scoreable instead of
			// aborting the run and discarding every case that already passed.
			//
			// The previous turn's own confirmation score has already failed for the same
			// reason, and merge() keeps the lowest, so recording it again changes nothing
			// there — it is what makes the outcome explicit for any evalset whose earlier
			// turn carries no required confirmation.
			evaluated.merge(NewBinaryScore(
				"confirmation", false, "the agent never proposed the guarded action this turn answers",
			))
			break
		}
		if err != nil {
			return CaseResult{}, err
		}
		previous = turn
		if turn.Failed() {
			// A refused confirmation is graded above; a provider failure is not. The
			// difference is who produced it: an error code comes from the gateway or the
			// runtime rather than from the agent's reasoning, so scoring it would record
			// an outage as a model regression. Remote error codes are also
			// provider-controlled and can contain echoed content, which is why the error
			// carries none of it.
			return CaseResult{}, errors.New("provider turn failed")
		}
		evaluated.usage, err = evaluated.usage.add(turn.Usage)
		if err != nil {
			return CaseResult{}, fmt.Errorf("aggregate usage: %w", err)
		}
		evaluated.merge(TrajectoryScore(turn, invocation))
		for _, score := range DeterministicControlScores(turn, invocation.DeterministicChecks) {
			evaluated.merge(score)
		}
		if config.RequireGrounded {
			evaluated.merge(GroundednessScore(turn, question, config.Domain))
		}
		if config.RequireSchema {
			evaluated.merge(SchemaScore(turn))
		}
		questions = append(questions, question)
		answers = append(answers, turn.Text)
		references = append(references, invocation.FinalResponse.Text())
	}
	// A case that stopped early is still judged, on the turns that did happen:
	// `eval:ab` refuses two artifacts whose samples carry different score names, so
	// dropping the verdict here would make a refused confirmation uncomparable.
	if config.Judge != nil {
		verdict, err := config.Judge.Judge(caseCtx, JudgeInput{
			Questions: questions, Answers: answers, ReferenceAnswers: references,
		})
		if err != nil {
			return CaseResult{}, fmt.Errorf("judge: %w", err)
		}
		evaluated.merge(NewStochasticBinaryScore(JudgeScoreName, verdict.Passed, verdict.Rationale))
	}
	result = CaseResult{
		ID: evalCase.ID, Sample: sample, Passed: evaluated.passed(),
		DeterministicPassed: evaluated.deterministicPassed(),
		Scores:              evaluated.sanitized(), Usage: evaluated.usage,
		// Wall clock for the whole case, including the judge call when one runs.
		// It is a duration rather than a rate because that is what Chapter 7's
		// latency budgets are stated in, and it is content-free by construction.
		DurationMillis: config.clock()().Sub(started).Milliseconds(),
	}
	outcome = CaseOutcome{Passed: result.Passed, Usage: result.Usage}
	for _, name := range sortedScoreNames(evaluated.values) {
		if err := config.Recorder.RecordScore(caseCtx, evaluated.values[name]); err != nil {
			return CaseResult{}, err
		}
	}
	return result, nil
}

// clock returns the configured clock, or the wall clock.
func (c RunnerConfig) clock() func() time.Time {
	if c.Clock == nil {
		return time.Now
	}
	return c.Clock
}

// sendInvocation drives one conversation entry: a question, or a human's answer
// to the approval the previous entry left pending.
//
// The two are different wire messages — user text against an ADK function
// response — and only the second can carry an approver and a rationale, which is
// why an evalset says which one it means rather than leaving it to be inferred
// from whether a turn happens to be awaiting confirmation.
func sendInvocation(
	ctx context.Context, client AgentClient, sessionID string, invocation Invocation, previous Turn,
) (Turn, error) {
	if invocation.Confirmation == nil {
		return client.Send(ctx, sessionID, invocation.UserContent.Text())
	}
	if previous.AwaitingConfirmation == nil {
		// Not an infrastructure failure: the agent declined to propose the guarded
		// action, so there is nothing to approve. runCase grades this as a failed
		// confirmation rather than crashing the run, which is what keeps a model's
		// refusal scoreable.
		return Turn{}, errNothingToConfirm
	}
	return client.Confirm(ctx, sessionID, previous, invocation.Confirmation.Approved(), invocation.Confirmation.Rationale)
}

var errNothingToConfirm = errors.New("the case answers a confirmation the agent never asked for")

func validateRunnerConfig(config RunnerConfig) error {
	var problems []error
	if err := config.EvalSet.Validate(); err != nil {
		problems = append(problems, err)
	}
	if config.EvalSet.Digest == "" {
		problems = append(problems, errors.New("evalset digest is required"))
	}
	if config.RunID == "" || config.Model.Provider == "" || config.Model.Name == "" {
		problems = append(problems, errors.New("run id, model provider, and model name are required"))
	}
	if err := config.Source.Validate(); err != nil {
		problems = append(problems, err)
	}
	if config.Transport != "rest" && config.Transport != "a2a" {
		problems = append(problems, fmt.Errorf("unsupported transport %q", config.Transport))
	}
	if config.Repeat < 0 {
		problems = append(problems, errors.New("repeat must not be negative"))
	}
	if math.IsNaN(config.MinimumPassRate) || math.IsInf(config.MinimumPassRate, 0) ||
		config.MinimumPassRate < 0 || config.MinimumPassRate > 1 {
		problems = append(problems, errors.New("minimum pass rate must be between 0 and 1"))
	}
	if config.ClientFactory == nil || config.Recorder == nil {
		problems = append(problems, errors.New("client factory and evidence recorder are required"))
	}
	caseIDs := make(map[string]struct{}, len(config.EvalSet.Cases))
	for _, evalCase := range config.EvalSet.Cases {
		caseIDs[evalCase.ID] = struct{}{}
	}
	for _, caseID := range config.RequiredCases {
		if _, found := caseIDs[caseID]; !found {
			problems = append(problems, fmt.Errorf("required case %q is not in evalset", caseID))
		}
	}
	return errors.Join(problems...)
}

func summarizeCases(results []CaseResult, required []string, minimumPassRate float64) RunSummary {
	summary := RunSummary{MinimumPassRate: minimumPassRate, RequiredCasesPassed: true}
	// Folded per case rather than per sample, and with AND rather than OR: a
	// required case that passed twice and failed once has already shown it can
	// fail, so a later sample may not restore an earlier one. The judged verdict
	// is excluded upstream, in CaseResult.DeterministicPassed.
	deterministicPass := make(map[string]bool)
	consistency := make(map[string]CaseConsistency)
	order := make([]string, 0, len(results))
	for _, result := range results {
		if result.Passed {
			summary.Passed++
		} else {
			summary.Failed++
		}
		priorDeterministic, seen := deterministicPass[result.ID]
		deterministicPass[result.ID] = result.DeterministicPassed && (!seen || priorDeterministic)
		record, counted := consistency[result.ID]
		if !counted {
			record.ID = result.ID
			order = append(order, result.ID)
		}
		record.Samples++
		if result.Passed {
			record.Passed++
		}
		consistency[result.ID] = record
	}
	slices.Sort(order)
	summary.CaseConsistency = make([]CaseConsistency, 0, len(order))
	for _, caseID := range order {
		summary.CaseConsistency = append(summary.CaseConsistency, consistency[caseID])
	}
	if len(results) > 0 {
		summary.PassRate = float64(summary.Passed) / float64(len(results))
	}
	// A required case is a safety claim, so it folds over deterministic scores only.
	// The judge still costs the run its pass rate above.
	for _, caseID := range required {
		if !deterministicPass[caseID] {
			summary.RequiredCasesPassed = false
		}
	}
	return summary
}

// merge keeps the *lowest* score seen under a name, which is a decision rather
// than an accident.
//
// A multi-turn case scores every turn, so one name arrives more than once, and
// the honest reading of "was this case safe?" is the worst turn rather than the
// average or the last. Averaging would let a clean second turn pay for a first
// one that crossed a write boundary, and last-wins would forget it entirely.
func (s *caseScores) merge(score Score) {
	if previous, found := s.values[score.Name]; !found || score.Value < previous.Value {
		s.values[score.Name] = score
	}
}

func (s caseScores) passed() bool {
	for _, score := range s.values {
		if !score.Passed {
			return false
		}
	}
	return len(s.values) > 0
}

// deterministicPassed answers the same question as passed(), ignoring judged verdicts.
//
// A required case is a safety claim — confirmation was asked for, the skill was loaded,
// prior context was recalled — and every one of those is decidable by a rule. Letting a
// 4B model judging its own family veto that claim would make the strictest gate in the
// harness the least reliable one. Like passed(), an empty map is not a pass.
func (s caseScores) deterministicPassed() bool {
	found := false
	for _, score := range s.values {
		if score.Stochastic {
			continue
		}
		found = true
		if !score.Passed {
			return false
		}
	}
	return found
}

func (s caseScores) sanitized() map[string]float64 {
	result := make(map[string]float64, len(s.values))
	for name, score := range s.values {
		result[name] = score.Value
	}
	return result
}

func sortedScoreNames(scores map[string]Score) []string {
	names := make([]string, 0, len(scores))
	for name := range scores {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
