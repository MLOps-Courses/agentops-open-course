package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// Token budgets and per-session cost attribution (Chapter 7.3).
//
// Every production deployment has to answer two questions: how many tokens did
// this session consume, and what happens when it exceeds its budget.
// [Policy.RecordTokenUsage] accumulates each response's usage into session
// state, which the session service persists across turns; [Policy.
// EnforceTokenBudget] refuses the next model call once the tracked total
// reaches the configured limit.
//
// Two properties are documented rather than fixed. The last admitted call can
// cross the threshold, because the refusal is checked before a call whose cost
// is not yet known. And a response with no usage metadata cannot be counted at
// all — which is not hypothetical: a streaming response carries usage only on
// its final aggregated chunk, so every partial passes through here counting
// nothing.
//
// Costs are tokens times configurable per-1k prices, 0 by default, because the
// reference path is local Ollama and no vendor pricing is hardcoded.

// Session-state keys for the running totals.
//
// Neither carries the "temp:" prefix, so a database-backed session service
// persists them across turns — the budget covers the whole conversation, not
// one turn.
const (
	inputTokensKey  = "budget:input_tokens"
	outputTokensKey = "budget:output_tokens"
)

// sessionLockRegistry hands out one lock per logical session, reference
// counted so an idle session leaves nothing behind.
//
// The lock exists because reading the totals, adding this turn's tokens and
// writing them back is a read-modify-write, and two callbacks for the same
// session can interleave inside one process. Without it both would keep the
// same pre-update snapshot and one turn's tokens would vanish. It is a
// process-local guard, not a distributed one: a second replica has its own
// state through the session service.
type sessionLockRegistry struct {
	locks map[sessionKey]*countedLock
	guard sync.Mutex
}

// sessionKey is ADK's stable logical session identity.
type sessionKey struct {
	appName   string
	userID    string
	sessionID string
}

// countedLock is one session's lock plus the number of holders and waiters.
type countedLock struct {
	mutex sync.Mutex
	refs  int
}

func newSessionLockRegistry() *sessionLockRegistry {
	return &sessionLockRegistry{locks: make(map[sessionKey]*countedLock)}
}

// lock acquires the session's lock and returns the function that releases it.
func (r *sessionLockRegistry) lock(key sessionKey) func() {
	r.guard.Lock()
	entry, ok := r.locks[key]
	if !ok {
		entry = &countedLock{}
		r.locks[key] = entry
	}
	entry.refs++
	r.guard.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		r.guard.Lock()
		defer r.guard.Unlock()
		if entry.refs--; entry.refs == 0 {
			delete(r.locks, key)
		}
	}
}

// keyOf reads the session identity from a callback context.
//
// ADK's callback context is a wrapper that stubs out Session(); the identity
// has to come from the three accessors that stay live inside a callback.
func keyOf(ctx agent.Context) sessionKey {
	return sessionKey{appName: ctx.AppName(), userID: ctx.UserID(), sessionID: ctx.SessionID()}
}

// EstimateCost prices a session's token totals with the configured per-1k
// rates. Both default to 0, so the local Ollama path costs exactly 0.
func (p *Policy) EstimateCost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)/1000*p.inputPricePer1K +
		float64(outputTokens)/1000*p.outputPricePer1K
}

// SessionUsage returns the session's accumulated input and output token counts.
func (p *Policy) SessionUsage(ctx agent.Context) (int, int, error) {
	state := ctx.State()
	inputTokens, err := stateInt(state, inputTokensKey)
	if err != nil {
		return 0, 0, err
	}
	outputTokens, err := stateInt(state, outputTokensKey)
	if err != nil {
		return 0, 0, err
	}
	return inputTokens, outputTokens, nil
}

// stateInt reads one token counter out of session state.
//
// The counters are written as ints, but a session service that persists state
// through JSON hands them back as float64 or json.Number, so every numeric
// shape a round trip can produce is accepted. Anything else is an error rather
// than a silent zero: a budget that quietly forgets its total is not a budget.
func stateInt(state session.State, key string) (int, error) {
	raw, err := state.Get(key)
	if err != nil {
		if errors.Is(err, session.ErrStateKeyNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %q from session state: %w", key, err)
	}
	switch value := raw.(type) {
	case int:
		return value, nil
	case int32:
		return int(value), nil
	case int64:
		return int(value), nil
	case float32:
		return int(value), nil
	case float64:
		return int(value), nil
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("session state %q holds %q, which is not a token count: %w", key, value, err)
		}
		return int(parsed), nil
	case string:
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("session state %q holds %q, which is not a token count: %w", key, value, err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("session state %q holds %T, which is not a token count", key, raw)
	}
}

// EnforceTokenBudget is the before-model guard: it refuses work past the
// per-session token budget.
//
// Returning a response short-circuits the model call, so the engineer gets an
// actionable message instead of a silent failure or an open-ended bill.
func (p *Policy) EnforceTokenBudget(ctx agent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
	if p.maxTokensPerSession == nil {
		return nil, nil
	}
	unlock := p.sessionLocks.lock(keyOf(ctx))
	inputTokens, outputTokens, err := p.SessionUsage(ctx)
	unlock()
	if err != nil {
		return nil, fmt.Errorf("reading the session token budget: %w", err)
	}

	used := inputTokens + outputTokens
	// Strictly less than: the call that crosses the threshold is admitted,
	// because its cost is not known until it returns.
	if used < *p.maxTokensPerSession {
		return nil, nil
	}
	message := fmt.Sprintf(
		"This session has exhausted its token budget (%d of %d tokens used). "+
			"Start a new session to continue, or raise %s if the work warrants it.",
		used, *p.maxTokensPerSession, config.EnvMaxTokensPerSession,
	)
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  modelRole,
			Parts: []*genai.Part{{Text: message}},
		},
		ErrorCode:    tokenBudgetExhaustedCode,
		ErrorMessage: "Per-session token budget exhausted.",
	}, nil
}

// tokenBudgetExhaustedCode is the machine-readable half of the refusal above.
const tokenBudgetExhaustedCode = "TOKEN_BUDGET_EXHAUSTED"

// RecordTokenUsage is the after-model guard: it attributes this turn's tokens
// to the session.
//
// It accumulates into session state and publishes the accounting through the
// configured recorder. It returns nil on purpose, so the response continues to
// the redaction guard unchanged.
// --8<-- [start:record-session-usage]
func (p *Policy) RecordTokenUsage(
	ctx agent.Context, response *model.LLMResponse, responseErr error,
) (*model.LLMResponse, error) {
	if response == nil || responseErr != nil || response.UsageMetadata == nil {
		// A response with no usage metadata cannot be counted. Streaming makes
		// this the common case rather than the exception: usage arrives only on
		// the final aggregated chunk.
		return nil, nil
	}
	usage := response.UsageMetadata
	turnInput := int(usage.PromptTokenCount) + int(usage.ToolUsePromptTokenCount)
	turnOutput := int(usage.CandidatesTokenCount) + int(usage.ThoughtsTokenCount)

	// Some OpenAI-compatible providers report only a total. Assigning the
	// unclassified remainder to the output bucket keeps the budget fail-closed
	// rather than silently recording zero tokens.
	classified := turnInput + turnOutput
	total := int(usage.TotalTokenCount)
	if total == 0 {
		total = classified
	}
	turnOutput += max(total-classified, 0)

	unlock := p.sessionLocks.lock(keyOf(ctx))
	inputTokens, outputTokens, err := p.SessionUsage(ctx)
	if err == nil {
		inputTokens += turnInput
		outputTokens += turnOutput
		err = errors.Join(
			ctx.State().Set(inputTokensKey, inputTokens),
			ctx.State().Set(outputTokensKey, outputTokens),
		)
	}
	unlock()
	if err != nil {
		return nil, fmt.Errorf("recording the session token usage: %w", err)
	}

	if p.recordUsage != nil {
		p.recordUsage(ctx, SessionUsage{
			TurnInput:     turnInput,
			TurnOutput:    turnOutput,
			SessionInput:  inputTokens,
			SessionOutput: outputTokens,
			CostEstimate:  p.EstimateCost(inputTokens, outputTokens),
		})
	}
	return nil, nil
}

// --8<-- [end:record-session-usage]
