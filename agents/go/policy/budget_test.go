package policy

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// This file is the Go port of tests/test_budget.py.
//
// The Python suite asserted the span attributes the callback set; here it
// asserts the [SessionUsage] the recorder seam receives, which carries the same
// four numbers. The attribute names themselves belong to the exporter — see
// [UsageRecorder] for why, and for what the telemetry wiring must preserve.

// usageResponse builds a model response carrying the usual token metadata.
func usageResponse(promptTokens, completionTokens int32) *model.LLMResponse {
	return &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     promptTokens,
		CandidatesTokenCount: completionTokens,
		TotalTokenCount:      promptTokens + completionTokens,
	}}
}

// budgetPolicy builds a policy with an optional budget and prices.
func budgetPolicy(t *testing.T, cfg Config) *Policy {
	t.Helper()
	return newPolicy(t, cfg)
}

func TestRecordAccumulatesUsageAcrossTurns(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	ctx := newContext()

	for _, response := range []*model.LLMResponse{usageResponse(100, 40), usageResponse(50, 10)} {
		replacement, err := policy.RecordTokenUsage(ctx, response, nil)
		if err != nil {
			t.Fatalf("RecordTokenUsage() error = %v, want nil", err)
		}
		// Returning nil is deliberate: the redaction guard must still see the
		// response.
		if replacement != nil {
			t.Errorf("RecordTokenUsage() = %v, want nil", replacement)
		}
	}

	inputTokens, outputTokens, err := policy.SessionUsage(ctx)
	if err != nil {
		t.Fatalf("SessionUsage() error = %v, want nil", err)
	}
	if inputTokens != 150 || outputTokens != 50 {
		t.Errorf("SessionUsage() = (%d, %d), want (150, 50)", inputTokens, outputTokens)
	}
}

// TestRecordCountsToolPromptReasoningAndTotalOnlyUsage covers the two
// providers this course actually meets: one that classifies everything, and one
// that reports only a total.
func TestRecordCountsToolPromptReasoningAndTotalOnlyUsage(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	ctx := newContext()

	classified := &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        100,
		ToolUsePromptTokenCount: 25,
		CandidatesTokenCount:    40,
		ThoughtsTokenCount:      10,
		TotalTokenCount:         175,
	}}
	totalOnly := &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
		TotalTokenCount: 75,
	}}
	for _, response := range []*model.LLMResponse{classified, totalOnly} {
		if _, err := policy.RecordTokenUsage(ctx, response, nil); err != nil {
			t.Fatalf("RecordTokenUsage() error = %v, want nil", err)
		}
	}

	inputTokens, outputTokens, err := policy.SessionUsage(ctx)
	if err != nil {
		t.Fatalf("SessionUsage() error = %v, want nil", err)
	}
	// The unclassified 75 lands entirely in the output bucket, which keeps the
	// budget fail-closed rather than silently recording zero.
	if inputTokens != 125 || outputTokens != 125 {
		t.Errorf("SessionUsage() = (%d, %d), want (125, 125)", inputTokens, outputTokens)
	}
}

// TestRecordSerializesInterleavedUpdates is the reason the per-session lock
// exists: without it both callbacks keep the same pre-update snapshot and one
// turn's tokens vanish.
//
// The two contexts share one state and one logical session identity, which is
// exactly the shape two concurrent callbacks for the same conversation take.
func TestRecordSerializesInterleavedUpdates(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	state := newState(nil)
	first, second := newContextWithState(state), newContextWithState(state)

	var group sync.WaitGroup
	for _, call := range []struct {
		ctx      *testContext
		response *model.LLMResponse
	}{
		{first, usageResponse(100, 40)},
		{second, usageResponse(50, 10)},
	} {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := policy.RecordTokenUsage(call.ctx, call.response, nil); err != nil {
				t.Errorf("RecordTokenUsage() error = %v, want nil", err)
			}
		}()
	}
	group.Wait()

	inputTokens, outputTokens, err := policy.SessionUsage(first)
	if err != nil {
		t.Fatalf("SessionUsage() error = %v, want nil", err)
	}
	if inputTokens != 150 || outputTokens != 50 {
		t.Errorf("SessionUsage() = (%d, %d), want (150, 50): an update was lost", inputTokens, outputTokens)
	}
}

// TestSessionLocksAreReleasedAndReclaimed proves the registry is reference
// counted rather than an unbounded map keyed by every session ever seen.
func TestSessionLocksAreReleasedAndReclaimed(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	ctx := newContext()
	if _, err := policy.RecordTokenUsage(ctx, usageResponse(1, 1), nil); err != nil {
		t.Fatalf("RecordTokenUsage() error = %v, want nil", err)
	}
	if got := len(policy.sessionLocks.locks); got != 0 {
		t.Errorf("the lock registry holds %d entries after the last release, want 0", got)
	}
}

func TestRecordIgnoresResponsesWithoutUsage(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	state := newState(nil)
	ctx := newContextWithState(state)

	if _, err := policy.RecordTokenUsage(ctx, &model.LLMResponse{}, nil); err != nil {
		t.Fatalf("RecordTokenUsage() error = %v, want nil", err)
	}
	if state.has(inputTokensKey) {
		t.Error("a response with no usage metadata wrote a counter, want nothing written")
	}
}

// TestRecordPublishesTheSessionAccounting is the Go home of the Python span
// attribute assertions.
func TestRecordPublishesTheSessionAccounting(t *testing.T) {
	t.Parallel()

	var published SessionUsage
	policy := budgetPolicy(t, Config{
		InputPricePer1K:  0.5,
		OutputPricePer1K: 1.0,
		RecordUsage:      func(_ context.Context, usage SessionUsage) { published = usage },
	})

	if _, err := policy.RecordTokenUsage(newContext(), usageResponse(1000, 500), nil); err != nil {
		t.Fatalf("RecordTokenUsage() error = %v, want nil", err)
	}
	want := SessionUsage{
		TurnInput: 1000, TurnOutput: 500,
		SessionInput: 1000, SessionOutput: 500,
		CostEstimate: 1.0,
	}
	if published != want {
		t.Errorf("published usage = %+v, want %+v", published, want)
	}
}

// TestUsageRecorderIsOptional keeps a composition without telemetry working.
func TestUsageRecorderIsOptional(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	if _, err := policy.RecordTokenUsage(newContext(), usageResponse(10, 5), nil); err != nil {
		t.Errorf("RecordTokenUsage() error = %v, want nil", err)
	}
}

// TestUnreadableCountersFailLoudly keeps a corrupted budget from silently
// resetting to zero, which would turn the hard limit into no limit at all.
func TestUnreadableCountersFailLoudly(t *testing.T) {
	t.Parallel()

	limit := 1000
	policy := budgetPolicy(t, Config{MaxTokensPerSession: &limit})
	ctx := newContextWithState(newState(map[string]any{inputTokensKey: []string{"not a number"}}))

	if _, err := policy.EnforceTokenBudget(ctx, nil); err == nil {
		t.Error("EnforceTokenBudget() error = nil, want a failure the runtime can see")
	}
}

// TestJSONRoundTrippedCountersAreRead covers the database-backed session
// service, which hands integers back as floating-point numbers.
func TestJSONRoundTrippedCountersAreRead(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	ctx := newContextWithState(newState(map[string]any{
		inputTokensKey:  float64(120),
		outputTokensKey: json.Number("30"),
	}))

	inputTokens, outputTokens, err := policy.SessionUsage(ctx)
	if err != nil {
		t.Fatalf("SessionUsage() error = %v, want nil", err)
	}
	if inputTokens != 120 || outputTokens != 30 {
		t.Errorf("SessionUsage() = (%d, %d), want (120, 30)", inputTokens, outputTokens)
	}
}

func TestBudgetIsDisabledByDefault(t *testing.T) {
	t.Parallel()

	policy := budgetPolicy(t, Config{})
	ctx := newContextWithState(newState(map[string]any{inputTokensKey: 1_000_000_000}))

	refusal, err := policy.EnforceTokenBudget(ctx, nil)
	if err != nil || refusal != nil {
		t.Errorf("EnforceTokenBudget() = (%v, %v), want (nil, nil)", refusal, err)
	}
}

// TestBudgetAdmitsTheCallThatCrossesTheThreshold pins the documented edge: the
// comparison is strictly less-than, so the last admitted call may cross.
func TestBudgetAdmitsTheCallThatCrossesTheThreshold(t *testing.T) {
	t.Parallel()

	limit := 1000
	policy := budgetPolicy(t, Config{MaxTokensPerSession: &limit})
	ctx := newContextWithState(newState(map[string]any{
		inputTokensKey:  500,
		outputTokensKey: 499,
	}))

	refusal, err := policy.EnforceTokenBudget(ctx, nil)
	if err != nil || refusal != nil {
		t.Errorf("EnforceTokenBudget() = (%v, %v), want (nil, nil) at 999 of 1000", refusal, err)
	}
}

func TestBudgetBlocksWithAnActionableMessage(t *testing.T) {
	t.Parallel()

	limit := 1000
	policy := budgetPolicy(t, Config{MaxTokensPerSession: &limit})
	ctx := newContextWithState(newState(map[string]any{
		inputTokensKey:  800,
		outputTokensKey: 200,
	}))

	refusal, err := policy.EnforceTokenBudget(ctx, nil)
	if err != nil {
		t.Fatalf("EnforceTokenBudget() error = %v, want nil", err)
	}
	if refusal == nil {
		t.Fatal("EnforceTokenBudget() = nil, want a refusal at 1000 of 1000")
	}
	if refusal.ErrorCode != tokenBudgetExhaustedCode {
		t.Errorf("ErrorCode = %q, want %q", refusal.ErrorCode, tokenBudgetExhaustedCode)
	}
	text := refusal.Content.Parts[0].Text
	for _, want := range []string{"1000 of 1000 tokens used", config.EnvMaxTokensPerSession} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal text = %q, want it to contain %q", text, want)
		}
	}
}

func TestCostEstimateUsesConfiguredPrices(t *testing.T) {
	t.Parallel()

	priced := budgetPolicy(t, Config{InputPricePer1K: 0.25, OutputPricePer1K: 2.0})
	if got, want := priced.EstimateCost(4000, 1000), 0.25*4+2.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("EstimateCost(4000, 1000) = %v, want %v", got, want)
	}

	// The reference path is local Ollama, which bills nothing.
	local := budgetPolicy(t, Config{})
	if got := local.EstimateCost(10_000, 10_000); got != 0 {
		t.Errorf("EstimateCost() = %v at the default prices, want 0", got)
	}
}

// TestNewRejectsAnUnusableBudget keeps a limit that cannot admit any work from
// reaching the runtime as a silent freeze.
func TestNewRejectsAnUnusableBudget(t *testing.T) {
	t.Parallel()

	zero := 0
	if _, err := New(Config{MaxTokensPerSession: &zero}); err == nil {
		t.Error("New() error = nil for a zero budget, want a configuration failure")
	}
}
