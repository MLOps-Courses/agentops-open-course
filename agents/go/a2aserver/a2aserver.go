// Package a2aserver serves the agent over Agent2Agent — the deployed contract
// a kagent BYO deployment, the browser client and the evaluation harness all
// speak.
//
// # This is a rebuild, not a translation
//
// The Python server exists in the shape it does because it subclasses three ADK
// Python internals: a session service (to survive a get-then-create race), a
// runner (to serialize overlapping invocations of one session), and an A2A
// executor (to publish the terminal event ADK omitted on cancellation). None of
// those classes exist in Go, so each behavior is re-earned at the seam ADK Go
// actually offers:
//
//	Python subclass                 Go seam
//	------------------------------  ------------------------------------------
//	_A2ASessionService              [recoveringSessions], a session.Service wrapper
//	_SessionSerializingRunner       adka2a.ExecutorConfig.RunnerProvider + [serializingRunner]
//	_CancelableA2AExecutor          nothing — adka2a's Executor.Cancel already
//	                                emits the terminal TaskStateCanceled event
//	VerifiedIdentityMiddleware      [bindVerifiedIdentity] + [identityInterceptor]
//	RunConfig.max_llm_calls         [newCallBudgetPlugin] — see the type's doc
//	DatabaseTaskStore               [TaskStore] — see G-4
//
// # What the process owns
//
// The A2A process is the single writer of runtime state. It recovers an
// interrupted snapshot restore, refuses an incompatible state generation,
// publishes and migrates the incident database, and creates both store schemas
// — in that order, before it will report ready. Every other replica (the MCP
// server, a second A2A replica behind a readiness gate) observes that state
// read-only. Reordering the startup sequence is a course invariant violation,
// not a refactor: recovery has to run before anything reads or publishes,
// because until it does, a half-published generation is indistinguishable from
// a healthy one.
//
// # What it deliberately does not own
//
// No CORS and no rate limiting. Both are gateway concerns in this course: the
// raw A2A server sends no CORS headers, which is exactly why the browser client
// requires agentgateway in front of it, and the gateway is where a rate limit
// can see all replicas. Adding either here would teach that a policy plane is
// optional. The one HTTP-layer control this server does keep is the trusted
// identity binding, because it is the gateway's *output* rather than its
// policy — see [bindVerifiedIdentity].
//
// # Configuration
//
// Like every package in this module, the settings arrive as a value; there is
// no package-level singleton. [OptionsFrom] is the one place the mapping from
// config.Config is written.
package a2aserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/plugin"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
)

// The public identity of the served agent. These are the strings a client sees
// on the card, so they are constants rather than literals typed twice.
const (
	// CardName is the agent's name on its public card.
	CardName = "AgentOps Agent"
	// CardDescription is the agent's one-line summary. The operating
	// instruction never appears on the card: a public card is discovery
	// metadata, and publishing the system prompt hands an attacker the exact
	// text to talk around.
	CardDescription = "Runbook-grounded incident triage and guarded remediation for the AgentOps Open Course."
	// TriageSkillID and RemediationSkillID name the two advertised skills.
	TriageSkillID      = "incident-triage"
	RemediationSkillID = "remediation"
)

// UnknownVersion is retained for API compatibility; development binaries now
// report the build authority's explicit development version.
const UnknownVersion = buildinfo.DevelopmentVersion

// The HTTP surface. Every path here is named in a manifest, a gateway route or
// a checked-in client that this package cannot see, so they are constants.
const (
	// InvokePath is the protocol 1.0 JSON-RPC endpoint, matching the path ADK's
	// own a2a sublauncher mounts.
	InvokePath = "/a2a/v1/invoke"
	// CompatInvokePath is the protocol 0.3 JSON-RPC endpoint, likewise.
	CompatInvokePath = "/a2a/invoke"
	// RootPath is the protocol 0.3 JSON-RPC endpoint at the server root. It is
	// what the Python server served and what clients/web/index.html posts to;
	// dropping it would break the checked-in browser client.
	RootPath = "/"
	// CardPath is the A2A discovery path.
	CardPath = a2asrv.WellKnownAgentCardPath
	// ReadinessPath and LivenessPath are the Kubernetes probes.
	ReadinessPath = "/healthz"
	LivenessPath  = "/livez"
)

// Readiness and liveness payload values, asserted by the smoke tests and read
// by an operator on a bad day.
const (
	statusReady   = "ready"
	statusUnready = "unready"
	statusAlive   = "alive"
)

// Transport and lifecycle defaults.
const (
	// DefaultBindHost is loopback: a server that binds every interface by
	// default is one misconfiguration away from being on the network. The
	// container image opts into 0.0.0.0 explicitly.
	DefaultBindHost = "127.0.0.1"
	// DefaultHost is the advertised host. Never advertise the bind address:
	// 0.0.0.0 is a listener, not a callable endpoint.
	DefaultHost = "localhost"
	// DefaultProtocol and DefaultPort complete the advertised URL.
	DefaultProtocol = "http"
	DefaultPort     = 8080
	// defaultDrainTimeout matches AGENT_DRAIN_TIMEOUT_S's own default.
	defaultDrainTimeout = 10 * time.Second
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers. Without it one idle connection can hold a handler forever.
	readHeaderTimeout = 10 * time.Second
)

// ErrIncompleteConfig marks a [Config] that cannot produce a working server. It
// is a sentinel so a caller can tell "this server is wired wrong" — a startup
// bug — apart from a runtime failure.
var ErrIncompleteConfig = errors.New("incomplete A2A server configuration")

// ErrNotStarted is returned by operations that need the startup sequence to
// have run.
var ErrNotStarted = errors.New("the A2A server has not started")

// Step is one startup or readiness action the runtime supplies.
//
// They are function seams rather than concrete types because this package must
// not decide the order in which *other* packages initialize themselves — it
// decides only the order in which they run here, which is the invariant. The
// four the runtime always fills, plus the one the shared session backend adds,
// are:
//
//	RecoverState:    state.RecoverInterruptedRestore(cfg.StateDir, …)
//	PrepareDataset:  store.PrepareRuntimeDatabase(ctx)
//	MigrateSessions: database.AutoMigrate(sessions)
//	ProbeDataset:    store.ProbeRuntimeDatabase(ctx)
//	ProbeSessions:   ProbePostgresSessionStore(ctx, pool) — Postgres only
type Step func(ctx context.Context) error

// Options are the transport, lifecycle and policy settings.
//
// Field order follows what go vet's fieldalignment check wants rather than how
// the fields group by meaning; the grouping a reader wants is in [OptionsFrom].
type Options struct {
	// StateDir holds disposable runtime state: this package's task database
	// under every backend, and ADK's session database under the SQLite one.
	// Required.
	StateDir string

	// SessionBackend names the engine holding ADK's sessions. Empty means
	// config.SessionBackendSQLite, the file inside StateDir that this package
	// probes itself. Any other backend puts the sessions somewhere this package
	// cannot open on its own, and Config.ProbeSessions is then required — see
	// [New].
	SessionBackend config.SessionBackend

	// BindHost is the interface to listen on. Empty means [DefaultBindHost].
	BindHost string
	// Host, Protocol and Port build the URL the public card advertises. Empty
	// or zero means the Default* constants.
	Host     string
	Protocol string

	// TrustedIdentityHeader names the request header carrying a
	// gateway-verified caller identity. Empty means the header is never read;
	// see [bindVerifiedIdentity] for the trust assumption when it is set.
	TrustedIdentityHeader string

	// Version is the version the public card reports. Empty resolves it from
	// the build info, falling back to [UnknownVersion].
	Version string

	// Entrypoint is the composition the runtime selected. Only
	// config.EntrypointAgent may be served; see [New].
	Entrypoint config.Entrypoint

	// DrainTimeout bounds how long in-flight requests may finish after a
	// shutdown signal. Zero means [defaultDrainTimeout].
	DrainTimeout time.Duration

	// Port is the port to bind and to advertise. Zero means [DefaultPort].
	Port int

	// MaxLLMCalls bounds the model calls one A2A turn may make. Zero or less
	// leaves a turn unbounded; see [callBudget] for why this is not a
	// framework feature in Go.
	MaxLLMCalls int

	// Streaming turns on per-token model streaming for A2A requests. Off by
	// default: chunked output weakens redaction (an entity spanning a chunk
	// boundary has already been sent) and the gateway reports no usage on
	// streamed responses. Off still gives message/stream clients server-sent
	// events of whole task updates — only the model runs non-streaming.
	Streaming bool
}

// OptionsFrom maps the agent configuration onto this package's settings.
//
// One field per line, in one place, so a new A2A setting has exactly one
// plausible home and the mapping can be asserted by a test.
func OptionsFrom(cfg config.Config, version string) Options {
	options := Options{
		StateDir:       cfg.StateDir,
		SessionBackend: cfg.SessionBackend,
		BindHost:       cfg.A2ABindHost,
		Host:           cfg.A2AHost,
		Protocol:       cfg.A2AProtocol,
		Version:        version,
		Entrypoint:     cfg.Entrypoint,
		DrainTimeout:   cfg.DrainTimeout.Duration(),
		Port:           cfg.A2APort,
		MaxLLMCalls:    cfg.A2AMaxLLMCalls,
		Streaming:      cfg.A2AStreaming,
	}
	if cfg.TrustedIdentityHeader != nil {
		options.TrustedIdentityHeader = *cfg.TrustedIdentityHeader
	}
	return options
}

// resolve fills the defaults and refuses settings the server cannot honor.
func (o Options) resolve() (Options, error) {
	resolved := o
	var problems []error
	// The empty value is the zero value of an Options built in code rather than
	// mapped from the environment, whose own default is sqlite. Resolving the two
	// the same way keeps "unset means the account-free default" true from both
	// directions, and leaves an unknown value to be refused below.
	if resolved.SessionBackend == "" {
		resolved.SessionBackend = config.SessionBackendSQLite
	}
	if resolved.BindHost == "" {
		resolved.BindHost = DefaultBindHost
	}
	if resolved.Host == "" {
		resolved.Host = DefaultHost
	}
	if resolved.Protocol == "" {
		resolved.Protocol = DefaultProtocol
	}
	if resolved.Port == 0 {
		resolved.Port = DefaultPort
	}
	if resolved.DrainTimeout == 0 {
		resolved.DrainTimeout = defaultDrainTimeout
	}
	if resolved.Version == "" {
		info, err := buildinfo.Current()
		if err != nil {
			problems = append(problems, fmt.Errorf("reading build information: %w", err))
		} else {
			resolved.Version = info.Version
		}
	}

	if resolved.StateDir == "" {
		problems = append(problems, fmt.Errorf(
			"%w: Options.StateDir is required; it is where the task database lives, "+
				"and the session database too under %s=%s",
			ErrIncompleteConfig, config.EnvSessionBackend, config.SessionBackendSQLite,
		))
	}
	if !resolved.SessionBackend.Valid() {
		// Refusing beats defaulting: a backend this server does not recognize would
		// otherwise be probed as if it were the local file, which is a readiness
		// check that passes while the store it reports on is somewhere else.
		problems = append(problems, fmt.Errorf(
			"%w: Options.SessionBackend is %q, want one of %v",
			ErrIncompleteConfig, resolved.SessionBackend, config.SessionBackends(),
		))
	}
	if resolved.Port < 1 || resolved.Port > 65535 {
		problems = append(problems, fmt.Errorf(
			"%w: Options.Port is %d, want 1 to 65535", ErrIncompleteConfig, resolved.Port,
		))
	}
	if resolved.DrainTimeout < 0 {
		problems = append(problems, fmt.Errorf(
			"%w: Options.DrainTimeout is %s, want a non-negative duration",
			ErrIncompleteConfig, resolved.DrainTimeout,
		))
	}
	if resolved.Entrypoint != config.EntrypointAgent {
		// The Python server refused any other entrypoint because only the
		// conversational composition was a servable agent there. In Go all three
		// are agent.Agent values, so the compiler no longer enforces it — and the
		// guarantee still has to hold, because the published card describes the
		// conversational agent's two skills.
		problems = append(problems, fmt.Errorf(
			"%w: the A2A server requires %s=%s, got %q",
			ErrIncompleteConfig, config.EnvEntrypoint, config.EntrypointAgent, resolved.Entrypoint,
		))
	}
	return resolved, errors.Join(problems...)
}

// Config is everything the server needs from the rest of the runtime.
type Config struct {
	// RootAgent is the composition to serve. Required.
	RootAgent agent.Agent

	// SessionService is the persistent session store. Required. It is wrapped
	// in [recoveringSessions] on the way in, so a caller cannot forget the
	// first-use race recovery.
	SessionService session.Service

	// MemoryService backs the long-term memory tools. Optional.
	MemoryService memory.Service

	// PromptGuardHandler serves agentgateway's fixed /request and /response
	// webhook paths on this process's existing private listener. Optional, but
	// when present PromptGuardReadiness is required.
	PromptGuardHandler http.Handler

	// Logger receives the server's own diagnostics. Nil uses slog.Default.
	Logger *slog.Logger

	// RecoverState finishes or reverses an interrupted snapshot restore. It is
	// the first thing startup does, and it is required: a server that skipped
	// it could publish on top of a half-restored generation.
	RecoverState Step

	// PrepareDataset publishes and migrates the agent-owned incident database.
	// Required: the A2A process is the single writer that owns first boot.
	PrepareDataset Step

	// MigrateSessions creates ADK's session schema. Required, because ADK
	// creates its tables lazily and readiness must not report success before
	// they exist.
	MigrateSessions Step

	// ProbeDataset backs the readiness route's dataset check. Required: a
	// replica that reports ready without checking anything is worse than one
	// with no probe at all, because Kubernetes will send it traffic.
	ProbeDataset Step

	// ProbeSessions backs the readiness route's session-store check when the
	// sessions do not live in a file this package can open.
	//
	// Required exactly when Options.SessionBackend is not
	// config.SessionBackendSQLite, and refused otherwise. The pairing is the
	// guarantee: without it, a deployment that moved its sessions to PostgreSQL
	// would still have its readiness answered by the runtime.db nobody writes —
	// a probe that reports on the wrong database, which is worse than no probe,
	// because it is believed. See [ProbePostgresSessionStore] for the check the
	// shared backend supplies here.
	ProbeSessions Step

	// PromptGuardReadiness checks the configured NER model catalog without
	// running inference. It must be set exactly when PromptGuardHandler is set.
	PromptGuardReadiness Step

	// Plugins are the application-wide policy plugins, attached to every
	// invocation this server runs. The call budget is prepended to them.
	Plugins []*plugin.Plugin

	// Options are the transport, lifecycle and policy settings.
	Options Options
}

// Server is the A2A server. Build it with [New], run the startup sequence with
// [Server.Start], and serve it with [Server.Serve] or over [Server.Handler].
// The zero value is not usable.
type Server struct {
	rootAgent          agent.Agent
	sessions           session.Service
	memories           memory.Service
	logger             *slog.Logger
	promptGuardHandler http.Handler

	// sessionCloser is the session service the caller handed in, kept for
	// shutdown. It cannot be recovered from the `sessions` field: that one is
	// the [recoveringSessions] wrapper, and embedding an interface promotes only
	// that interface's own methods — session.Service declares no Close, so a
	// type assertion on the wrapper would silently never find one.
	sessionCloser io.Closer

	recoverState         Step
	prepareDataset       Step
	migrateSessions      Step
	probeDataset         Step
	probeSessions        Step
	promptGuardReadiness Step

	plugins []*plugin.Plugin
	gate    *sessionGate

	tasks   *TaskStore
	handler http.Handler
	card    []byte

	appName string
	options Options
}

// New validates cfg and builds the server.
//
// Nothing here touches the filesystem: construction is a pure wiring decision,
// and every disk operation belongs to [Server.Start], where its order relative
// to crash recovery is the guarantee.
func New(cfg Config) (*Server, error) {
	options, optionsErr := cfg.Options.resolve()

	problems := []error{optionsErr}
	if cfg.RootAgent == nil {
		problems = append(problems, fmt.Errorf("%w: RootAgent is required", ErrIncompleteConfig))
	}
	if cfg.SessionService == nil {
		problems = append(problems, fmt.Errorf(
			"%w: SessionService is required; A2A sessions are durable state, not a cache",
			ErrIncompleteConfig,
		))
	}
	if (cfg.PromptGuardHandler == nil) != (cfg.PromptGuardReadiness == nil) {
		problems = append(problems, fmt.Errorf(
			"%w: PromptGuardHandler and PromptGuardReadiness must be configured together",
			ErrIncompleteConfig,
		))
	}
	// Which database readiness reports on is decided here, once, from the backend
	// the deployment configured — never from what happens to be on disk.
	probeSessions := cfg.ProbeSessions
	switch {
	case options.SessionBackend == config.SessionBackendSQLite:
		if probeSessions != nil {
			problems = append(problems, fmt.Errorf(
				"%w: ProbeSessions must not be set under %s=%s; the session store is %s inside "+
					"Options.StateDir and this package probes it read-only itself",
				ErrIncompleteConfig, config.EnvSessionBackend, config.SessionBackendSQLite, SessionDatabaseName,
			))
		}
		probeSessions = func(ctx context.Context) error {
			return probeFileSessionStore(ctx, options.StateDir)
		}
	case probeSessions == nil:
		problems = append(problems, fmt.Errorf(
			"%w: ProbeSessions is required under %s=%s; the sessions are not in Options.StateDir, "+
				"so only the caller that owns the connection can report on them",
			ErrIncompleteConfig, config.EnvSessionBackend, options.SessionBackend,
		))
	}
	for _, step := range []struct {
		value Step
		name  string
	}{
		{cfg.RecoverState, "RecoverState"},
		{cfg.PrepareDataset, "PrepareDataset"},
		{cfg.MigrateSessions, "MigrateSessions"},
		{cfg.ProbeDataset, "ProbeDataset"},
	} {
		if step.value == nil {
			problems = append(problems, fmt.Errorf("%w: %s is required", ErrIncompleteConfig, step.name))
		}
	}
	for index, installed := range cfg.Plugins {
		if installed == nil {
			problems = append(problems, fmt.Errorf("%w: Plugins[%d] is nil", ErrIncompleteConfig, index))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// The call budget runs before the policy plane's own before-model chain: a
	// refused call needs neither compaction nor redaction, and counting a call
	// that was never made would be wrong.
	plugins := cfg.Plugins
	budget, err := newCallBudgetPlugin(options.MaxLLMCalls)
	if err != nil {
		return nil, err
	}
	if budget != nil {
		plugins = append([]*plugin.Plugin{budget}, cfg.Plugins...)
	}

	sessionCloser, _ := cfg.SessionService.(io.Closer)
	server := &Server{
		rootAgent:            cfg.RootAgent,
		sessions:             recoveringSessions{Service: cfg.SessionService},
		sessionCloser:        sessionCloser,
		memories:             cfg.MemoryService,
		logger:               logger,
		promptGuardHandler:   cfg.PromptGuardHandler,
		recoverState:         cfg.RecoverState,
		prepareDataset:       cfg.PrepareDataset,
		migrateSessions:      cfg.MigrateSessions,
		probeDataset:         cfg.ProbeDataset,
		probeSessions:        probeSessions,
		promptGuardReadiness: cfg.PromptGuardReadiness,
		plugins:              plugins,
		gate:                 newSessionGate(),
		appName:              policy.AppName,
		options:              options,
	}
	card, err := server.encodeCard()
	if err != nil {
		return nil, err
	}
	server.card = card
	return server, nil
}

// Options returns the resolved settings.
func (s *Server) Options() Options { return s.options }

// Address is the host:port this server binds.
func (s *Server) Address() string {
	return s.options.BindHost + ":" + strconv.Itoa(s.options.Port)
}

// URL is the endpoint the public card advertises. It is deliberately built from
// the advertised host, never from the bind address.
func (s *Server) URL() string {
	return fmt.Sprintf("%s://%s:%d", s.options.Protocol, s.options.Host, s.options.Port)
}

// TaskStore returns the persistent task store, or nil before [Server.Start].
func (s *Server) TaskStore() *TaskStore { return s.tasks }

// AgentCard builds the public card.
//
// The card is discovery metadata and nothing else. It carries the two skills a
// client can ask for, the transports it can ask over, and the version it is
// talking to — and never the agent's operating instruction. ADK's own
// adka2a.BuildAgentSkills would publish exactly that: it folds the agent's
// Instruction and GlobalInstruction into a skill description, rewritten into
// the first person. That is why this card is assembled by hand.
func (s *Server) AgentCard() *a2a.AgentCard {
	base := s.URL()
	return &a2a.AgentCard{
		Name:        CardName,
		Description: CardDescription,
		Version:     s.options.Version,
		// Two interfaces, both JSON-RPC. The 0.3 one is listed first and is what
		// a compat client sees as the preferred transport: it is the root path
		// the checked-in browser client posts to and the one the Python track
		// advertised. The 1.0 one is the path ADK's own launcher mounts, and is
		// the only binding that carries ListTasks.
		SupportedInterfaces: []*a2a.AgentInterface{
			{
				URL:             base + RootPath,
				ProtocolBinding: a2a.TransportProtocolJSONRPC,
				ProtocolVersion: a2av0.Version,
			},
			{
				URL:             base + InvokePath,
				ProtocolBinding: a2a.TransportProtocolJSONRPC,
				ProtocolVersion: a2a.Version,
			},
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{
			{
				ID:          TriageSkillID,
				Name:        "Incident triage",
				Description: "Prioritize incidents using service state, logs, and deterministic severity rules.",
				Tags:        []string{"incident", "triage", "operations"},
				Examples:    []string{"Triage the open incidents."},
			},
			{
				ID:          RemediationSkillID,
				Name:        "Guarded remediation",
				Description: "Recommend runbook-backed remediation and request confirmation before mock actions.",
				Tags:        []string{"runbook", "remediation", "approval"},
				Examples:    []string{"How should I remediate the open incident?"},
			},
		},
	}
}

// encodeCard renders the card once, in the 0.3/1.0 union form.
//
// The union is what lets one document serve both protocol generations: a 1.0
// client reads supportedInterfaces, a 0.3 client reads the top-level url and
// preferredTransport, and neither needs to know the other exists. Rendering it
// at construction also means a malformed card is a startup failure rather than
// a 500 on the discovery path.
func (s *Server) encodeCard() ([]byte, error) {
	producer, ok := a2av0.NewStaticAgentCardProducer(s.AgentCard()).(a2asrv.AgentCardJSONProducer)
	if !ok {
		return nil, fmt.Errorf("%w: the A2A compat card producer no longer renders JSON", ErrIncompleteConfig)
	}
	encoded, err := producer.CardJSON(context.Background())
	if err != nil {
		return nil, fmt.Errorf("%w: rendering the agent card: %w", ErrIncompleteConfig, err)
	}
	return encoded, nil
}

// A2AOptions returns the request-handler options this server installs.
//
// It is exported so the same task store and the same identity interceptor can
// be handed to ADK's own launcher through launcher.Config.A2AOptions, for a
// deployment that wants the launcher's web surface instead of this one. The
// server must have started, because the task store is opened after the state
// preflight and there is nothing to install before then.
func (s *Server) A2AOptions() ([]a2asrv.RequestHandlerOption, error) {
	if s.tasks == nil {
		return nil, ErrNotStarted
	}
	return []a2asrv.RequestHandlerOption{
		a2asrv.WithTaskStore(s.tasks),
		a2asrv.WithLogger(s.logger),
		a2asrv.WithCallInterceptors(identityInterceptor{}),
		// The card promises streaming, and the handler refuses a streaming
		// request when the capabilities it was given say otherwise. Passing the
		// card's own capabilities is what keeps the promise and the enforcement
		// the same statement.
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
	}, nil
}

// executor builds the ADK-to-A2A executor.
func (s *Server) executor() *adka2a.Executor {
	return adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerProvider: s.runnerProvider,
		RunConfig:      s.runConfig(),
	})
}

// runConfig is the ADK run configuration every A2A invocation uses.
// --8<-- [start:a2a-runtime]
func (s *Server) runConfig() agent.RunConfig {
	if s.options.Streaming {
		return agent.RunConfig{StreamingMode: agent.StreamingModeSSE}
	}
	// The default leaves model streaming off: message/stream clients still get
	// server-sent events of whole task updates, which is the trade-off Chapter
	// 3.6 documents.
	return agent.RunConfig{StreamingMode: agent.StreamingModeNone}
}

// --8<-- [end:a2a-runtime]

// Close releases the resources the server owns.
//
// Every close runs even when an earlier one fails, and the failures are joined
// rather than replaced: a task store left open because the session service
// failed to close would be a file handle leaked on every restart.
func (s *Server) Close() error {
	var problems []error
	if s.tasks != nil {
		problems = append(problems, s.tasks.Close())
		s.tasks = nil
	}
	if s.sessionCloser != nil {
		problems = append(problems, s.sessionCloser.Close())
		s.sessionCloser = nil
	}
	return errors.Join(problems...)
}
