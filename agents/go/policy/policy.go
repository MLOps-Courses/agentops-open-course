// Package policy is the agent's governance plane: one cross-cutting set of
// rules that every agent, sub-agent and workflow node in the application obeys.
//
// # Attached once, at the app boundary
//
// The whole package resolves to a single [Policy] value whose [Policy.Plugin]
// builds one *plugin.Plugin. That plugin goes on runner.PluginConfig (which the
// launcher takes as launcher.Config.PluginConfig) and its hooks then fire for
// every agent the application runs. Attaching the same rules to each agent by
// hand made it a clerical duty, and a tenth agent added by a learner silently
// shipped with no redaction and no budget while nothing failed. Adding an agent
// cannot lose the policy here, because the policy was never attached per agent.
// Never reintroduce a per-agent callback list.
//
// # Order is load-bearing
//
// The before-model chain is budget, then compaction, then redaction, and the
// first non-nil response short-circuits the rest. A refused call needs no
// further work; compaction then bounds the history; redaction then runs on only
// the messages that survived. The after-model chain is usage accounting, then
// redaction — the accountant returns nil on purpose so the redaction pass still
// sees every response. Both chains are declared in [Policy.beforeModelGuards]
// and [Policy.afterModelGuards], and TestModelGuardOrderIsLoadBearing pins them.
//
// # Trust is keyed on identity, never on a name
//
// A skill result is reviewed repository instruction, so it bypasses injection
// neutralization and spotlighting. Every other tool result is attacker-
// influenceable data and stays hardened. That carve-out is granted to the exact
// tool.Tool values listed in Config.TrustedInstructionTools, compared by
// identity — never by tool.Name(), which any MCP server could claim. PII and
// credential redaction still applies to both trust classes.
//
// # Configuration
//
// The package deliberately does not import the agent's configuration loader:
// [Config] is a value the composition builds from a config.Config, exactly as
// the tools package is wired. The mapping is one field per line:
//
//	WritesDisabled      <- cfg.WritesDisabled
//	SanitizeToolOutput  <- cfg.SanitizeToolOutput
//	MaxHistoryMessages  <- cfg.MaxHistoryMessages
//	MaxTokensPerSession <- cfg.MaxTokensPerSession
//	InputPricePer1K     <- cfg.InputPricePer1K
//	OutputPricePer1K    <- cfg.OutputPricePer1K
package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
)

const (
	// AppName is the ADK application name the policy plugin governs. It is the
	// name sessions, audit rows and telemetry all attribute work to, so it is
	// declared once here rather than spelled at each call site.
	AppName = "agentops-agent"

	// PluginName identifies the plugin to ADK's plugin manager, which rejects
	// two plugins sharing a name.
	PluginName = "agentops_policy"
)

// ErrIncompleteConfig marks a [Config] that cannot produce a working policy.
//
// It is a sentinel so a caller can tell "this composition is wired wrong" —
// which is a startup bug — apart from a runtime failure.
var ErrIncompleteConfig = errors.New("incomplete policy configuration")

// SessionUsage is one model response's token accounting, as the policy sees it.
//
// TurnInput and TurnOutput are this response's contribution; SessionInput and
// SessionOutput are the running totals the session has accumulated, and
// CostEstimate prices those totals with the configured per-1k rates.
type SessionUsage struct {
	TurnInput     int
	TurnOutput    int
	SessionInput  int
	SessionOutput int
	CostEstimate  float64
}

// UsageRecorder publishes the token accounting for one model response.
//
// It is the seam the telemetry package fills. The Python track emitted an OTel
// counter named agentops.tokens (unit "token", by direction) plus the span
// attributes agentops.tokens.session.{input,output,total} and
// agentops.cost.session.estimate; those names are Chapter 7.3's observable
// contract and belong with the exporter, not with the rule that produces them.
// Nil means the accounting is still enforced but not published.
type UsageRecorder func(ctx context.Context, usage SessionUsage)

// InjectionRecorder publishes how many injection markers were neutralized in
// one tool result.
//
// It is the counterpart of [UsageRecorder] for the OTel counter the Python
// track named agentops.guardrails.injections_neutralized (unit "1"). Nil means
// the neutralization still happens but is not counted.
type InjectionRecorder func(ctx context.Context, hits int)

// ActionableError extracts a repository-authored summary that is safe to hand
// back to the model.
//
// Error hygiene is classification, not silence. This repository authors a small
// number of typed failures whose summaries are first-party, carry no untrusted
// content, and name the setting to change. The entire error chain is never safe:
// even a first-party retry wrapper can retain a provider response body as its
// cause. Every unclassified error stays opaque.
//
// The composition supplies the classifier because the policy plane must not
// depend on every package that can fail. A nil classifier makes every error
// opaque, which is the safe direction but loses the actionable half. A caller
// must extract the matched typed error and build the summary from its trusted
// fields, never return err.Error():
//
//	ActionableError: func(err error) (string, bool) {
//		var circuitOpen *resilience.CircuitOpenError
//		if errors.As(err, &circuitOpen) {
//			return circuitOpen.Error(), true
//		}
//		return "", false
//	}
type ActionableError func(err error) (safeSummary string, ok bool)

// Config is everything the governance plane needs from the rest of the runtime.
//
// Field order follows what the compiler wants (govet's fieldalignment check),
// not what a reader would group; the concern-based grouping lives in the
// package doc's configuration mapping.
type Config struct {
	// Logger receives the policy's own diagnostics. Nil uses slog.Default.
	Logger *slog.Logger

	// MaxHistoryMessages bounds the conversation history sent to the model.
	// Nil sends ADK's full session history every turn. A non-nil value must be
	// at least 2: a window of one cannot hold a tool call and its result.
	MaxHistoryMessages *int

	// MaxTokensPerSession is the hard per-session token budget. Nil disables
	// enforcement. A non-nil value must be positive.
	MaxTokensPerSession *int

	// ActionableError extracts safe summaries from typed tool failures. See
	// [ActionableError].
	ActionableError ActionableError

	// RecordUsage publishes token accounting. See [UsageRecorder].
	RecordUsage UsageRecorder

	// RecordInjections publishes neutralization counts. See [InjectionRecorder].
	RecordInjections InjectionRecorder

	// TrustedInstructionTools lists, by identity, the tools whose output is
	// reviewed repository instruction rather than attacker-influenceable data.
	//
	// In practice this is exactly the load_skill tool obtained from the locally
	// constructed skilltoolset — capture the tool.Tool values that toolset
	// returns and pass those. Never list a tool because of its name: a remote
	// MCP server chooses the names it advertises, and a trust boundary keyed on
	// a name an attacker picks is not a trust boundary. Do not list
	// load_skill_resource: it returns arbitrary files from a skill directory,
	// which is a wider surface than a reviewed SKILL.md body.
	//
	// The listed values are used as map keys, so their dynamic types must be
	// comparable. Every tool ADK constructs is a pointer, which is.
	TrustedInstructionTools []tool.Tool

	// InputPricePer1K and OutputPricePer1K price the token totals. Both default
	// to 0 because the reference path is local Ollama, which bills nothing.
	InputPricePer1K  float64
	OutputPricePer1K float64

	// WritesDisabled is the AGENT_WRITES_DISABLED kill-switch. When true, the
	// before-tool guard refuses every mutating action before ADK can build a
	// confirmation request, so no approval prompt reaches a human.
	WritesDisabled bool

	// SanitizeToolOutput turns on injection neutralization and spotlighting for
	// untrusted tool output. It is opt-out (default true in the configuration)
	// because it is a Chapter 4.6 guarantee, not a preference.
	SanitizeToolOutput bool
}

// Policy is the governance plane. Build it once with [New] and attach it once
// with [Policy.Plugin]; the zero value is not usable.
type Policy struct {
	logger *slog.Logger

	maxHistoryMessages  *int
	maxTokensPerSession *int

	actionableError  ActionableError
	recordUsage      UsageRecorder
	recordInjections InjectionRecorder

	// trusted is the identity set behind the skill carve-out. A map, not a
	// name lookup — see Config.TrustedInstructionTools.
	trusted map[tool.Tool]bool

	// sessionLocks serializes the read-modify-write of one session's token
	// counters. See [sessionLockRegistry].
	sessionLocks *sessionLockRegistry

	inputPricePer1K  float64
	outputPricePer1K float64

	writesDisabled     bool
	sanitizeToolOutput bool
}

// New validates cfg and builds the policy.
//
// The bounds re-checked here are the same ones the configuration loader
// enforces. Repeating them is not redundancy: Config is an ordinary struct a
// caller can build by hand, and a window of one message or a budget of zero
// tokens would turn a guarantee into a silent malfunction.
func New(cfg Config) (*Policy, error) {
	var problems []error
	if cfg.MaxHistoryMessages != nil && *cfg.MaxHistoryMessages < minHistoryMessages {
		problems = append(problems, fmt.Errorf(
			"%w: MaxHistoryMessages is %d; a window smaller than %d cannot hold a tool call and its result",
			ErrIncompleteConfig, *cfg.MaxHistoryMessages, minHistoryMessages,
		))
	}
	if cfg.MaxTokensPerSession != nil && *cfg.MaxTokensPerSession < 1 {
		problems = append(problems, fmt.Errorf(
			"%w: MaxTokensPerSession is %d; a budget must admit at least one token",
			ErrIncompleteConfig, *cfg.MaxTokensPerSession,
		))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}

	trusted := make(map[tool.Tool]bool, len(cfg.TrustedInstructionTools))
	for _, trustedTool := range cfg.TrustedInstructionTools {
		if trustedTool == nil {
			return nil, fmt.Errorf("%w: TrustedInstructionTools contains a nil tool", ErrIncompleteConfig)
		}
		trusted[trustedTool] = true
	}

	return &Policy{
		logger:              cfg.Logger,
		maxHistoryMessages:  cfg.MaxHistoryMessages,
		maxTokensPerSession: cfg.MaxTokensPerSession,
		actionableError:     cfg.ActionableError,
		recordUsage:         cfg.RecordUsage,
		recordInjections:    cfg.RecordInjections,
		trusted:             trusted,
		sessionLocks:        newSessionLockRegistry(),
		inputPricePer1K:     cfg.InputPricePer1K,
		outputPricePer1K:    cfg.OutputPricePer1K,
		writesDisabled:      cfg.WritesDisabled,
		sanitizeToolOutput:  cfg.SanitizeToolOutput,
	}, nil
}

// activeLogger returns an explicitly injected logger, or the process default
// active at emission. Runtime assembly installs its sanitizing handler after it
// constructs the policy, so resolving nil in New would retain the startup sink.
func (p *Policy) activeLogger() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// Plugin builds the single ADK plugin that carries the whole policy.
//
// plugin.Config holds one function per hook rather than a list, so the explicit
// composition in [Policy.BeforeModel] and [Policy.AfterModel] is not merely
// idiomatic — it is the only way to run two guards on one hook, which is
// exactly the shape the Python track chose for the same reason.
// --8<-- [start:policy-plugin]
func (p *Policy) Plugin() (*plugin.Plugin, error) {
	built, err := plugin.New(plugin.Config{
		Name:                 PluginName,
		BeforeModelCallback:  p.BeforeModel,
		AfterModelCallback:   p.AfterModel,
		OnModelErrorCallback: p.HandleModelError,
		BeforeToolCallback:   p.ValidateActions,
		AfterToolCallback:    p.SecureToolOutput,
		OnToolErrorCallback:  p.HandleToolError,
	})
	if err != nil {
		return nil, fmt.Errorf("building the %s plugin: %w", PluginName, err)
	}
	return built, nil
}

// --8<-- [end:policy-plugin]

// Guard names. They exist so the composition order can be asserted by a test
// rather than by reading the source, and so a failing guard names itself.
const (
	budgetGuard     = "budget"
	compactionGuard = "compaction"
	redactionGuard  = "redaction"
	usageGuard      = "usage"
)

// beforeModelGuard is one step of the before-model policy, paired with its name.
type beforeModelGuard struct {
	run  llmagent.BeforeModelCallback
	name string
}

// afterModelGuard is one step of the after-model policy, paired with its name.
type afterModelGuard struct {
	run  llmagent.AfterModelCallback
	name string
}

// beforeModelGuards declares the before-model order. Budget first: a refused
// call needs no further work. Compaction second: it decides which messages
// survive. Redaction last: it runs on only those.
func (p *Policy) beforeModelGuards() []beforeModelGuard {
	return []beforeModelGuard{
		{name: budgetGuard, run: p.EnforceTokenBudget},
		{name: compactionGuard, run: p.CompactHistory},
		{name: redactionGuard, run: p.RedactRequest},
	}
}

// afterModelGuards declares the after-model order. The accountant runs first
// and returns nil on purpose, so redaction still sees every response.
func (p *Policy) afterModelGuards() []afterModelGuard {
	return []afterModelGuard{
		{name: usageGuard, run: p.RecordTokenUsage},
		{name: redactionGuard, run: p.RedactResponse},
	}
}

// BeforeModel runs the before-model policy: budget, compaction, redaction.
//
// The first guard to return a response short-circuits the rest and replaces the
// model call, which is how a budget refusal reaches the caller as an answer
// instead of as a silent failure.
func (p *Policy) BeforeModel(ctx agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	return runBeforeModelGuards(p.beforeModelGuards(), ctx, request)
}

// AfterModel runs the after-model policy: token accounting, then redaction.
func (p *Policy) AfterModel(
	ctx agent.Context, response *model.LLMResponse, responseErr error,
) (*model.LLMResponse, error) {
	return runAfterModelGuards(p.afterModelGuards(), ctx, response, responseErr)
}

// runBeforeModelGuards applies the guards in order and stops at the first one
// that answers.
//
// It takes the slice rather than reading it from the policy so the composition
// rule and the composition contents are separately expressible — and separately
// testable — without putting a test seam on the [Policy] itself.
func runBeforeModelGuards(
	guards []beforeModelGuard, ctx agent.Context, request *model.LLMRequest,
) (*model.LLMResponse, error) {
	for _, guard := range guards {
		response, err := guard.run(ctx, request)
		if err != nil {
			// Naming the guard matters: three of them run on one hook, and an
			// unattributed failure sends the reader to the wrong file.
			return nil, fmt.Errorf("before-model %s guard: %w", guard.name, err)
		}
		if response != nil {
			return response, nil
		}
	}
	return nil, nil
}

// runAfterModelGuards is [runBeforeModelGuards] for the return path.
func runAfterModelGuards(
	guards []afterModelGuard, ctx agent.Context, response *model.LLMResponse, responseErr error,
) (*model.LLMResponse, error) {
	for _, guard := range guards {
		replacement, err := guard.run(ctx, response, responseErr)
		if err != nil {
			return nil, fmt.Errorf("after-model %s guard: %w", guard.name, err)
		}
		if replacement != nil {
			return replacement, nil
		}
	}
	return nil, nil
}
