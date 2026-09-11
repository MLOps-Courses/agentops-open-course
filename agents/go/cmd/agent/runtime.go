package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	adkagent "google.golang.org/adk/v2/agent"
	adktelemetry "google.golang.org/adk/v2/telemetry"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/memory"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/model"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/resilience"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// agentRuntime is every plane a conversational process needs, assembled once.
//
// The launcher path and the standalone A2A path draw from the same value: they
// serve the same agent, under the same policy, over the same state, and the
// only difference is which surface publishes it. Assembling them separately is
// how the two would eventually drift.
type agentRuntime struct {
	governance   *policy.Policy
	memories     *memory.Memory
	compositions *compose.Compose
	store        *data.Store

	// telemetryOptions carry the resource ADK attributes its own spans and log
	// records to. Empty when this process installed the providers itself.
	telemetryOptions []adktelemetry.Option

	// shutdown flushes the observability plane. Never nil once the runtime
	// exists, so [agentRuntime.close] needs no guard.
	shutdown func(context.Context) error

	// sessions is non-nil only for the ADK launcher family, whose lifecycle
	// this runtime owns. The standalone A2A server owns its own closeable store.
	sessions  *sessionStore
	recovered recoveredState

	build  buildinfo.Info
	config config.Config
}

// --8<-- [start:runtime-assembly]
// newAgentRuntime assembles every plane, in dependency order.
//
// The order is not incidental, and four constraints fix it.
//
// Skills before policy: the trust carve-out is keyed on the identity of the
// load_skill tool value that toolset built, never on its name.
//
// Policy before observability: installTelemetry redacts every durable log path
// through the policy, so the plane cannot be installed until the redactor
// exists. It is installed at the first line where that is true and no later,
// because a record written before slog.SetDefault reaches a handler that is
// neither correlated nor exported — nothing above installTelemetry may write a
// durable record.
//
// Policy before tools: a rationale is redacted by the policy before it reaches
// the append-only audit trail.
//
// Tools before compositions: least-privilege delegation is a statement about
// which tool values each agent holds.
//
// ownsProviders selects who installs the OpenTelemetry tracer and logger
// providers; see [processInstallsProviders].
// --8<-- [end:runtime-assembly]
func newAgentRuntime(
	ctx context.Context,
	cfg config.Config,
	recovered recoveredState,
	console io.Writer,
	ownsProviders bool,
) (assembled *agentRuntime, err error) {
	if recoveryErr := recovered.require(cfg.StateDir); recoveryErr != nil {
		return nil, recoveryErr
	}
	guard, err := newGuard(cfg)
	if err != nil {
		return nil, err
	}

	skills, err := compose.NewSkills(ctx, compose.SkillsDir(cfg.DataDir))
	if err != nil {
		return nil, err
	}

	// The carve-out is the exact tool value the locally built toolset produced,
	// never a name: a name is something a remote MCP server picks for itself,
	// and a trust boundary keyed on an attacker's choice is not a trust
	// boundary.
	governance, err := newPolicy(cfg, []tool.Tool{skills.LoadSkillTool()})
	if err != nil {
		return nil, err
	}

	build, err := buildinfo.Current()
	if err != nil {
		return nil, fmt.Errorf("reading build information: %w", err)
	}
	shutdown, options, telemetryErr := installTelemetry(ctx, console, governance, build, ownsProviders)
	// The flush is registered before the error is checked, because it is never
	// nil, and it runs on every failing path, because the records that describe
	// a startup failure are exactly the ones worth exporting. A caller that is
	// handed no runtime has nothing left to release the plane with.
	defer func() {
		if err != nil {
			flushTelemetry(ctx, shutdown)
		}
	}()
	if telemetryErr != nil {
		return nil, telemetryErr
	}

	llm, err := model.Build(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("building the model: %w", err)
	}

	store := data.New(data.Config{DataDir: cfg.DataDir, StateDir: cfg.StateDir})
	surface, memories, err := newToolSurface(cfg, store, guard.Run, governance.PersistedRedactor(ctx))
	if err != nil {
		return nil, err
	}

	mcpToolset, err := newMCPToolset(cfg)
	if err != nil {
		return nil, err
	}

	compositions, err := compose.New(compose.Config{
		Model:                 llm,
		GenerateContentConfig: model.GenerationConfig(cfg),
		Skills:                skills,
		MCP:                   mcpToolset,
		Tools:                 surface,
		// The two recall tools, in the order the agent registers them. They stay
		// local even when the six reads are served over MCP, because the note
		// store is scoped per user and per application.
		Memory:     memories.MemoryTools(),
		Entrypoint: cfg.Entrypoint,
	})
	if err != nil {
		return nil, fmt.Errorf("wiring the compositions: %w", err)
	}

	return &agentRuntime{
		governance:       governance,
		memories:         memories,
		compositions:     compositions,
		store:            store,
		telemetryOptions: options,
		shutdown:         shutdown,
		recovered:        recovered,
		build:            build,
		config:           cfg,
	}, nil
}

// close releases every resource this runtime owns. A launcher session pool is
// closed here; the standalone A2A server closes the pool handed to it itself.
func (r *agentRuntime) close(ctx context.Context) {
	if r.sessions != nil {
		if err := r.sessions.Close(); err != nil {
			slog.WarnContext(ctx, "releasing the session store failed", "error", err)
		}
		r.sessions = nil
	}
	flushTelemetry(ctx, r.shutdown)
}

// newToolSurface builds the eight function tools every composition draws from,
// and the memory plane behind two of them.
//
// The runbook reads come from the memory package rather than the tools package
// because they read the knowledge corpus and its derived vector index, not the
// incident database — and the MCP server needs exactly this pairing to serve
// the six-name read allowlist.
func newToolSurface(
	cfg config.Config, store *data.Store, guard tools.Guard, redact func(string) string,
) (compose.Tools, *memory.Memory, error) {
	agentTools, err := tools.New(tools.Config{
		Store:  store,
		Guard:  guard,
		Redact: redact,
		// Read once at startup, exactly as the setting documents itself: roll
		// the value out and restart to freeze mutations while reads keep
		// working.
		WritesDisabled: cfg.WritesDisabled,
	})
	if err != nil {
		return compose.Tools{}, nil, fmt.Errorf("building the tools: %w", err)
	}

	memories, err := memory.New(memory.Config{
		Store:  store,
		Guard:  memory.Guard(guard),
		Redact: redact,
		// Long-term memory is disposable runtime state: memory.db and
		// vectors.db live beside the runtime incident database, never in the
		// committed dataset.
		StateDir:          cfg.StateDir,
		EmbeddingsURL:     cfg.EmbeddingsURL,
		EmbeddingModel:    cfg.EmbeddingModel,
		EmbeddingTimeout:  cfg.EmbeddingTimeout.Duration(),
		SemanticRetrieval: cfg.SemanticRetrieval,
		WritesDisabled:    cfg.WritesDisabled,
	})
	if err != nil {
		return compose.Tools{}, nil, fmt.Errorf("building the memory plane: %w", err)
	}

	return compose.Tools{
		ListIncidents:     agentTools.ListIncidents(),
		GetIncident:       agentTools.GetIncident(),
		GetServiceStatus:  agentTools.GetServiceStatus(),
		SearchServiceLogs: agentTools.SearchServiceLogs(),
		GetRunbook:        memories.GetRunbook(),
		SearchRunbooks:    memories.SearchRunbooks(),
		RestartService:    agentTools.RestartService(),
		ResolveIncident:   agentTools.ResolveIncident(),
	}, memories, nil
}

// newGuard builds the read-tool resilience policy: a deadline, bounded retries,
// and — only when the setting is on — a per-tool circuit breaker.
func newGuard(cfg config.Config) (*resilience.Guard, error) {
	var breakers *resilience.Breakers
	if cfg.CircuitBreakerEnabled {
		var err error
		breakers, err = resilience.NewBreakers(resilience.BreakersConfig{
			FailureThreshold: cfg.CircuitFailureThreshold,
			ResetTimeout:     cfg.CircuitResetTimeout.Duration(),
		})
		if err != nil {
			return nil, fmt.Errorf("building the circuit breakers: %w", err)
		}
	}
	guard, err := resilience.NewGuard(resilience.Config{
		Breakers:     breakers,
		ToolTimeout:  cfg.ToolTimeout.Duration(),
		RetryBackoff: cfg.RetryBackoff.Duration(),
		MaxRetries:   cfg.MaxRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("building the resilience guard: %w", err)
	}
	return guard, nil
}

// newPolicy builds the governance plane over the configuration.
//
// trusted lists, by identity, the tools whose output is reviewed repository
// instruction rather than attacker-influenceable data. A process that runs no
// model passes none: there is no instruction boundary there to carve out.
func newPolicy(cfg config.Config, trusted []tool.Tool) (*policy.Policy, error) {
	governance, err := policy.New(policy.Config{
		MaxHistoryMessages:  cfg.MaxHistoryMessages,
		MaxTokensPerSession: cfg.MaxTokensPerSession,
		ActionableError:     actionableError,
		// The two counters Chapter 7.3 documents. The policy plane owns the
		// rules; the telemetry package owns the instrument names those rules
		// publish under.
		RecordUsage:             telemetry.RecordTokenUsage,
		RecordInjections:        telemetry.RecordInjectionsNeutralized,
		TrustedInstructionTools: trusted,
		InputPricePer1K:         cfg.InputPricePer1K,
		OutputPricePer1K:        cfg.OutputPricePer1K,
		WritesDisabled:          cfg.WritesDisabled,
		SanitizeToolOutput:      cfg.SanitizeToolOutput,
	})
	if err != nil {
		return nil, fmt.Errorf("building the policy plane: %w", err)
	}
	return governance, nil
}

// actionableError extracts repository-authored summaries from typed failures.
//
// The matched value, not the outer error chain, owns the summary. In particular,
// a retry error retains its last dependency failure for errors.Is/As but omits
// that untrusted cause from the text returned to the model. Broad data-access
// failures stay opaque because their fields can contain paths and driver text.
func actionableError(err error) (string, bool) {
	var circuitOpen *resilience.CircuitOpenError
	if errors.As(err, &circuitOpen) {
		return circuitOpen.Error(), true
	}
	var deadline *resilience.DeadlineError
	if errors.As(err, &deadline) {
		return deadline.Error(), true
	}
	var exhausted *resilience.RetriesExhaustedError
	if errors.As(err, &exhausted) {
		return exhausted.SafeSummary(), true
	}
	return "", false
}

// newMCPToolset builds the governed MCP route, or nil when no URL is set.
//
// Nil is the account-free default: every read runs in process, under the local
// deadline, retries and circuit breaker. Only the conversational entrypoint
// consumes this — see the compose package doc.
func newMCPToolset(cfg config.Config) (tool.Toolset, error) {
	if cfg.MCPURL == "" {
		return nil, nil
	}
	toolset, err := compose.NewMCPToolset(compose.MCPConfig{
		Endpoint: cfg.MCPURL,
		Token:    cfg.MCPToken,
		Timeout:  cfg.ToolTimeout.Duration(),
	})
	if err != nil {
		return nil, err
	}
	return toolset, nil
}

// newAgentLoader publishes the selected entrypoint as the root agent and the
// structured report beside it.
//
// The report is a second, independently addressable agent rather than a mode of
// the root one, which is what lets an evaluation drive the schema path over the
// REST surface without changing AGENT_ENTRYPOINT — the same property the Python
// track got from its separate discovery package. The console and the A2A card
// still serve the root agent alone.
func newAgentLoader(compositions *compose.Compose) (adkagent.Loader, error) {
	root, err := compositions.RootAgent()
	if err != nil {
		return nil, err
	}
	report, err := compositions.TriageReportAgent()
	if err != nil {
		return nil, err
	}
	loader, err := adkagent.NewMultiLoader(root, report)
	if err != nil {
		return nil, fmt.Errorf("publishing the agents: %w", err)
	}
	return loader, nil
}
