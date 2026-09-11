package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"

	"google.golang.org/adk/v2/plugin"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/mcpserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/piiwebhook"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/state"
)

// serveA2A runs the deployed Agent2Agent contract until the context is
// canceled, which is what a Ctrl-C and a Kubernetes SIGTERM both arrive as.
func serveA2A(ctx context.Context, arguments []string, console io.Writer) error {
	if len(arguments) > 0 {
		return fmt.Errorf(
			"%s: unexpected argument %q; the A2A server is configured by %s and the AGENT_A2A_* settings",
			a2aCommand, arguments[0], config.EnvEntrypoint,
		)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading the configuration (run `%s` for the resolved settings): %w", configCheckCommand, err)
	}
	recovered, err := recoverRuntimeState(cfg.StateDir)
	if err != nil {
		return err
	}
	server, assembled, err := newA2AServer(ctx, cfg, recovered, console)
	if err != nil {
		return err
	}
	defer assembled.close(ctx)

	if err := server.Serve(ctx); err != nil {
		return fmt.Errorf("serving the A2A contract: %w", err)
	}
	return nil
}

// newA2AServer assembles the A2A process after the caller has recovered its
// state generation. It opens the closeable session pool but neither migrates a
// schema nor binds a port; those remain a2aserver.Server.Start steps.
//
// This process is the single writer of runtime state. The a2aserver package
// owns the order its startup steps run in — recovery, then the read-only
// generation preflight, then publication, then the two schemas — and this
// function only supplies what each step does. Reordering them there is a course
// invariant violation, not a refactor: until recovery has run, a half-published
// generation is indistinguishable from a healthy one.
func newA2AServer(
	ctx context.Context, cfg config.Config, recovered recoveredState, console io.Writer,
) (*a2aserver.Server, *agentRuntime, error) {
	// This surface runs no ADK launcher, so nothing else in the process will
	// build the tracer and logger providers.
	assembled, err := newAgentRuntime(ctx, cfg, recovered, console, processInstallsProviders)
	if err != nil {
		return nil, nil, err
	}
	// Every failure from here on has to release the observability plane the
	// runtime just installed, because a caller that is handed no runtime has
	// nothing left to release it with. A deferred cleanup cannot do this: the
	// failing returns are the ones that null out the runtime it would need.
	var sessions *sessionStore
	fail := func(err error) (*a2aserver.Server, *agentRuntime, error) {
		if sessions != nil {
			err = errors.Join(err, sessions.Close())
		}
		assembled.close(ctx)
		return nil, nil, err
	}

	root, err := assembled.compositions.RootAgent()
	if err != nil {
		return fail(err)
	}
	governancePlugin, err := assembled.governance.Plugin()
	if err != nil {
		return fail(err)
	}
	// Opened, not migrated: creating ADK's schema is a startup step the server
	// sequences after recovery, so it must not have happened already.
	sessions, err = openSessionStore(cfg, recovered)
	if err != nil {
		return fail(err)
	}
	var promptGuard http.Handler
	var promptGuardReadiness a2aserver.Step
	if cfg.PIIModelEnabled && cfg.PIIModelBaseURL != "" {
		detector, detectorErr := piiwebhook.NewOpenAICompatibleDetector(ctx, piiwebhook.OpenAIConfig{
			BaseURL: cfg.PIIModelBaseURL,
			APIKey:  cfg.OpenAIAPIKey.Reveal(),
			Model:   cfg.PIIModel,
			Timeout: cfg.PIIModelTimeout.Duration(),
		})
		if detectorErr != nil {
			return fail(fmt.Errorf("building the named-entity detector: %w", detectorErr))
		}
		promptGuard, detectorErr = piiwebhook.New(piiwebhook.Config{
			Detector: detector,
			Timeout:  cfg.PIIModelTimeout.Duration(),
		})
		if detectorErr != nil {
			return fail(fmt.Errorf("building the prompt-guard webhook: %w", detectorErr))
		}
		promptGuardReadiness = detector.Ready
	}

	server, err := a2aserver.New(a2aserver.Config{
		RootAgent:            root,
		SessionService:       sessions,
		MemoryService:        assembled.memories.Service(),
		PromptGuardHandler:   promptGuard,
		PromptGuardReadiness: promptGuardReadiness,
		RecoverState: func(context.Context) error {
			return state.RecoverInterruptedRestore(cfg.StateDir, state.RecoverOptions{})
		},
		PrepareDataset: func(ctx context.Context) error {
			_, prepareErr := assembled.store.PrepareRuntimeDatabase(ctx)
			return prepareErr
		},
		MigrateSessions: func(context.Context) error { return sessions.migrate(cfg.StateDir) },
		ProbeDataset: func(ctx context.Context) error {
			_, probeErr := assembled.store.ProbeRuntimeDatabase(ctx)
			return probeErr
		},
		// Nil under the file backend, where a2aserver probes runtime.db itself;
		// the shared server is reachable only through the pool this process owns.
		ProbeSessions: sessions.readinessProbe(),
		// The same single plugin the launcher attaches, so an A2A turn is
		// governed by exactly the policy a console turn is.
		Plugins: []*plugin.Plugin{governancePlugin},
		Options: a2aserver.OptionsFrom(cfg, assembled.build.Version),
	})
	if err != nil {
		return fail(err)
	}
	return server, assembled, nil
}

// serveMCP runs the read-only MCP server until the context is canceled.
//
// The transport, address and Host allowlist come from the MCP_* variables the
// deployment sets, exactly as they do on the Python track, so `agent mcp` is
// stdio and the same command with MCP_TRANSPORT=streamable-http is HTTP. That
// is why this subcommand takes no flags of its own: a second spelling of the
// same setting is a second place for it to drift.
func serveMCP(ctx context.Context, arguments []string, console io.Writer) error {
	if len(arguments) > 0 {
		return fmt.Errorf(
			"%s: unexpected argument %q; the transport is selected by %s (%s, %s or %s)",
			mcpCommand, arguments[0], mcpserver.EnvTransport,
			mcpserver.TransportStdio, mcpserver.TransportSSE, mcpserver.TransportStreamableHTTP,
		)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading the configuration (run `%s` for the resolved settings): %w", configCheckCommand, err)
	}
	server, flush, err := newMCPServer(ctx, cfg, console)
	// Registered before the error is checked, because the flush is never nil:
	// an install that failed halfway still has a provider to release.
	defer flushTelemetry(ctx, flush)
	if err != nil {
		return err
	}
	if err := server.Serve(ctx); err != nil {
		return fmt.Errorf("serving the MCP surface: %w", err)
	}
	return nil
}

// newMCPServer assembles the MCP process and returns it with the flush that
// releases its observability plane. The flush is never nil.
func newMCPServer(
	ctx context.Context, cfg config.Config, console io.Writer,
) (*mcpserver.Server, func(context.Context) error, error) {
	options, err := mcpserver.OptionsFromEnv(cfg.DrainTimeout.Duration())
	if err != nil {
		return nil, noFlush, err
	}

	// No model is built here and no governance plugin is attached: this process
	// reads a database and answers, and both the policy callbacks and the
	// resilience policy belong to the agent process that calls it. What is
	// built is the redactor, because the tool constructors take a persistence
	// boundary seam and wiring a pass-through into one would be
	// indistinguishable from forgetting to wire it at all.
	governance, err := newPolicy(cfg, nil)
	if err != nil {
		return nil, noFlush, err
	}
	build, err := buildinfo.Current()
	if err != nil {
		return nil, noFlush, fmt.Errorf("reading build information: %w", err)
	}
	// This surface runs no ADK launcher either, so it owns its own providers.
	flush, _, err := installTelemetry(ctx, console, governance, build, processInstallsProviders)
	if err != nil {
		return nil, flush, err
	}

	store := data.New(data.Config{DataDir: cfg.DataDir, StateDir: cfg.StateDir})
	surface, _, err := newToolSurface(cfg, store, passThroughGuard, governance.PersistedRedactor(ctx))
	if err != nil {
		return nil, flush, err
	}

	server, err := mcpserver.New(mcpserver.Config{
		// Read-only, always: the probe reports an unprepared or corrupt runtime
		// database as unready and never repairs it. Publication and migration
		// belong to the A2A process, which is the single writer, and a replica
		// that migrated state under a running agent would be a data race across
		// processes.
		Probe: func(ctx context.Context) error {
			_, probeErr := store.ProbeRuntimeDatabase(ctx)
			return probeErr
		},
		// The six read tools in the order the client allowlist names them, and
		// nothing else: the server refuses to start on a seventh.
		Tools:   slices.Concat(surface.ReadTools(), surface.KnowledgeTools()),
		Version: build.Version,
		Options: options,
	})
	if err != nil {
		return nil, flush, err
	}
	return server, flush, nil
}

// passThroughGuard runs a read tool call with no deadline, no retries and no
// circuit breaker.
//
// It is the MCP server's guard, and only the MCP server's. That process carries
// no resilience policy on purpose: the ADK client supplies its own timeout, and
// a server that retried on the client's behalf would double the load on a
// dependency that is already struggling.
func passThroughGuard(ctx context.Context, _ string, call func(context.Context) error) error {
	return call(ctx)
}

// stateEnvironment resolves the process-level values the state command
// deliberately does not read for itself, so an operation on durable state stays
// a function of its arguments.
func stateEnvironment() state.CommandEnvironment {
	environment := state.CommandEnvironment{
		Timestamp: os.Getenv(state.EnvBackupTimestamp),
	}
	// A pointer, not a string: the command tells an unset variable apart from
	// one set to something unparseable, and refuses the second rather than
	// silently falling back to the default retention.
	if keep, set := os.LookupEnv(state.EnvBackupKeep); set {
		environment.Keep = &keep
	}
	return environment
}
