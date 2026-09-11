package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport selects how the server talks to its clients.
//
// A defined type rather than a bare string so an unsupported value is a
// compile-time impossibility at every call site inside this module, and a
// single, named failure at the one place a learner can supply one: the
// environment.
type Transport string

const (
	// TransportStdio serves one client over standard input and output. It is
	// the default because it needs no port, no address and no allowlist: the
	// client starts the server as a child process and owns its lifetime.
	TransportStdio Transport = "stdio"
	// TransportSSE serves the 2024-11-05 Server-Sent Events transport. It is a
	// compatibility affordance for older clients, not a third choice.
	TransportSSE Transport = "sse"
	// TransportStreamableHTTP serves the current HTTP transport. This is what
	// the gateway and the Kubernetes deployment use.
	TransportStreamableHTTP Transport = "streamable-http"
)

// The environment variables this package reads.
//
// They deliberately carry no AGENT_ prefix: they configure the MCP process's
// transport, not the agent's behavior, and they are set by the deployment that
// runs the process — the Kubernetes manifest sets all four. Everything that
// belongs to the agent is read by the config package and passed in as a value.
const (
	EnvHost         = "MCP_HOST"
	EnvPort         = "MCP_PORT"
	EnvTransport    = "MCP_TRANSPORT"
	EnvAllowedHosts = "MCP_ALLOWED_HOSTS"
)

// The HTTP surface. The MCP path is what the gateway routes to and the two
// probes are what Kubernetes polls; all three are named in manifests that this
// package cannot see, so they are constants, not settings.
const (
	StreamablePath = "/mcp"
	SSEPath        = "/sse"
	ReadinessPath  = "/healthz"
	LivenessPath   = "/livez"
)

// Transport and address defaults. Loopback and a fixed port, because a server
// that binds every interface by default is one misconfiguration away from
// being on the network.
const (
	DefaultHost         = "127.0.0.1"
	DefaultPort         = 8000
	defaultDrainTimeout = 10 * time.Second
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers. Without it a single idle connection can hold a handler forever.
	readHeaderTimeout = 10 * time.Second
)

// ErrInvalidOptions marks transport settings the server cannot honor.
var ErrInvalidOptions = errors.New("invalid MCP server options")

// DefaultAllowedHosts returns the Host header authorities this server answers
// to, in the order the deployment documents them.
//
// This is DNS-rebinding protection. A browser page on an attacker's site can be
// made to resolve a name it controls to 127.0.0.1 and then talk to a local
// server as if it were same-origin; checking the Host header against a closed
// list is what stops that, because the attacker's name is not on it. The list
// covers the four ways this server is legitimately addressed: loopback, the
// Docker host bridge, the gateway, and the in-cluster service.
//
// A fresh slice on every call, for the same reason [Server.ToolNames] returns
// one: a security list an importer can edit in place is not a security list.
func DefaultAllowedHosts() []string {
	return []string{
		"127.0.0.1",
		"127.0.0.1:*",
		"localhost",
		"localhost:*",
		"[::1]",
		"[::1]:*",
		"host.docker.internal",
		"host.docker.internal:*",
		"agentgateway",
		"agentgateway:*",
		"agentgateway.agentops.svc.cluster.local",
		"agentgateway.agentops.svc.cluster.local:*",
		"agentops-mcp",
		"agentops-mcp:*",
		"agentops-mcp.agentops.svc.cluster.local",
		"agentops-mcp.agentops.svc.cluster.local:*",
	}
}

// DefaultAllowedOrigins returns the browser origins this server accepts.
//
// Only loopback: nothing in this course serves a page that talks to the MCP
// endpoint from a browser, so any Origin header at all is either a developer
// tool on the same machine or a cross-site request that should not be here.
func DefaultAllowedOrigins() []string {
	return []string{"http://127.0.0.1:*", "http://localhost:*", "http://[::1]:*"}
}

// Options are the transport and security settings.
//
// Field order follows what go vet's fieldalignment check wants rather than how
// the fields group by meaning; the grouping is address, then allowlists, then
// lifecycle.
type Options struct {
	// Host is the interface to bind. Empty means [DefaultHost].
	Host string

	// Transport selects the wire. Empty means [TransportStdio].
	Transport Transport

	// AllowedHosts is the Host header allowlist. Empty means
	// [DefaultAllowedHosts]. An entry ending in ":*" matches that authority on
	// any port; every other entry matches exactly.
	AllowedHosts []string

	// AllowedOrigins is the Origin header allowlist, with the same wildcard
	// rule. Empty means [DefaultAllowedOrigins].
	AllowedOrigins []string

	// DrainTimeout bounds how long in-flight requests may finish after a
	// shutdown signal. Zero means ten seconds, which is what
	// AGENT_DRAIN_TIMEOUT_S defaults to; a caller with the agent's
	// configuration in hand should pass that value instead.
	DrainTimeout time.Duration

	// Port is the port to bind. Zero means [DefaultPort].
	Port int
}

// resolve fills the defaults and refuses a configuration that cannot work.
func (o Options) resolve() (Options, error) {
	resolved := o
	if resolved.Host == "" {
		resolved.Host = DefaultHost
	}
	if resolved.Port == 0 {
		resolved.Port = DefaultPort
	}
	if resolved.Transport == "" {
		resolved.Transport = TransportStdio
	}
	if len(resolved.AllowedHosts) == 0 {
		resolved.AllowedHosts = DefaultAllowedHosts()
	}
	if len(resolved.AllowedOrigins) == 0 {
		resolved.AllowedOrigins = DefaultAllowedOrigins()
	}
	if resolved.DrainTimeout == 0 {
		resolved.DrainTimeout = defaultDrainTimeout
	}

	var problems []error
	if resolved.Port < 1 || resolved.Port > 65535 {
		problems = append(problems, fmt.Errorf(
			"%w: Port is %d, want 1 to 65535", ErrInvalidOptions, resolved.Port,
		))
	}
	if resolved.DrainTimeout < 0 {
		problems = append(problems, fmt.Errorf(
			"%w: DrainTimeout is %s, want a non-negative duration",
			ErrInvalidOptions, resolved.DrainTimeout,
		))
	}
	if _, err := ParseTransport(string(resolved.Transport)); err != nil {
		problems = append(problems, err)
	}
	if slices.Contains(resolved.AllowedHosts, "*") {
		problems = append(problems, fmt.Errorf(
			"%w: AllowedHosts contains \"*\", which disables DNS-rebinding protection entirely; "+
				"name the authorities this server answers to", ErrInvalidOptions,
		))
	}
	return resolved, errors.Join(problems...)
}

// ParseTransport turns a configured string into a [Transport].
func ParseTransport(value string) (Transport, error) {
	switch transport := Transport(value); transport {
	case TransportStdio, TransportSSE, TransportStreamableHTTP:
		return transport, nil
	default:
		return "", fmt.Errorf(
			"%w: unsupported %s %q, want one of %s, %s or %s",
			ErrInvalidOptions, EnvTransport, value,
			TransportStdio, TransportSSE, TransportStreamableHTTP,
		)
	}
}

// OptionsFromEnv reads the transport settings from the process environment.
//
// drainTimeout is passed in rather than read here because it belongs to the
// agent's configuration (AGENT_DRAIN_TIMEOUT_S), which the config package owns;
// a second reader of the same variable is a second place for its default to
// drift. Pass zero to accept this package's own default.
func OptionsFromEnv(drainTimeout time.Duration) (Options, error) {
	options := Options{Host: os.Getenv(EnvHost), DrainTimeout: drainTimeout}

	var problems []error
	if raw, set := os.LookupEnv(EnvPort); set {
		port, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			problems = append(problems, fmt.Errorf(
				"%w: %s is %q, which is not a port number: %w", ErrInvalidOptions, EnvPort, raw, err,
			))
		}
		options.Port = port
	}
	if raw, set := os.LookupEnv(EnvTransport); set {
		transport, err := ParseTransport(raw)
		if err != nil {
			problems = append(problems, err)
		}
		options.Transport = transport
	}
	if raw, set := os.LookupEnv(EnvAllowedHosts); set {
		hosts, err := parseAllowedHosts(raw)
		if err != nil {
			problems = append(problems, err)
		}
		options.AllowedHosts = hosts
	}
	if err := errors.Join(problems...); err != nil {
		return Options{}, err
	}
	return options.resolve()
}

// parseAllowedHosts splits the comma-separated override.
//
// An override that resolves to nothing is refused rather than silently falling
// back to the defaults: an operator who narrowed the allowlist and mistyped it
// would otherwise get the wide default list and no warning, which is the exact
// opposite of what they asked for.
func parseAllowedHosts(raw string) ([]string, error) {
	hosts := make([]string, 0, strings.Count(raw, ",")+1)
	for _, entry := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			hosts = append(hosts, trimmed)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf(
			"%w: %s must contain at least one host authority", ErrInvalidOptions, EnvAllowedHosts,
		)
	}
	return hosts, nil
}

// Address is the host:port this server binds on the HTTP transports.
func (s *Server) Address() string {
	return net.JoinHostPort(s.options.Host, strconv.Itoa(s.options.Port))
}

// Transport is the wire this server was configured for.
func (s *Server) Transport() Transport { return s.options.Transport }

// Serve runs the server until ctx is canceled.
//
// Cancelation is a normal shutdown, not a failure: pressing Ctrl-C on a
// foreground server, and Kubernetes sending SIGTERM, both arrive here as a
// canceled context and both return nil. That is the Go shape of the Python
// entrypoint's decision to swallow KeyboardInterrupt.
// --8<-- [start:mcp-server-transport]
func (s *Server) Serve(ctx context.Context) error {
	switch s.options.Transport {
	case TransportStdio:
		return s.serveStdio(ctx)
	case TransportSSE, TransportStreamableHTTP:
		return s.serveHTTP(ctx)
	default:
		// Unreachable: New resolved and validated the transport. Reported as an
		// error anyway, because "unreachable" is a claim about today's code.
		return fmt.Errorf("%w: unsupported transport %q", ErrInvalidOptions, s.options.Transport)
	}
}

// --8<-- [end:mcp-server-transport]

// serveStdio serves one client over the process's standard streams.
func (s *Server) serveStdio(ctx context.Context) error {
	s.logger.InfoContext(ctx, "serving MCP over stdio", "tools", len(s.names))
	if err := s.mcp.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return fmt.Errorf("serving MCP over stdio: %w", err)
	}
	return nil
}

// serveHTTP serves the HTTP surface with a bounded shutdown drain.
//
// The drain is what makes a rolling update lossless: on SIGTERM the listener
// stops accepting, in-flight tool calls are given DrainTimeout to finish, and
// only then does the process exit. The Kubernetes deployment's termination
// grace period is set longer than this window on purpose.
func (s *Server) serveHTTP(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.Address(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	listening := make(chan error, 1)
	go func() { listening <- server.ListenAndServe() }()

	s.logger.InfoContext(ctx, "serving MCP over HTTP",
		"address", s.Address(), "transport", string(s.options.Transport), "tools", len(s.names))

	select {
	case err := <-listening:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving MCP on %s: %w", s.Address(), err)
	case <-ctx.Done():
		// WithoutCancel, because the drain deadline has to outlive the
		// cancellation that started it — a shutdown context that is already
		// canceled would drain for exactly zero seconds.
		drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.options.DrainTimeout)
		defer cancel()
		s.logger.InfoContext(ctx, "draining in-flight MCP requests",
			"timeout", s.options.DrainTimeout.String())
		if err := server.Shutdown(drain); err != nil {
			return fmt.Errorf("draining MCP connections within %s: %w", s.options.DrainTimeout, err)
		}
		return nil
	}
}
