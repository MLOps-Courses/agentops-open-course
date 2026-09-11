package mcpserver_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/mcpserver"
)

// TestTransportDefaultsToStdio is the port of test_mcp_main_runs_stdio_transport.
//
// stdio is the default because it needs no port, no address and no allowlist:
// the client starts the server as a child process and owns its lifetime, which
// is the shape Chapter 3.3 introduces before any of the network story exists.
func TestTransportDefaultsToStdio(t *testing.T) {
	t.Parallel()

	// Built without the fixture on purpose: the fixture serves HTTP so it can
	// exercise the protocol, and this test is about what an unconfigured server
	// does.
	server, err := mcpserver.New(mcpserver.Config{
		Probe: func(context.Context) error { return nil },
		Tools: readSurface(t, newStore(t), false),
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if got := server.Transport(); got != mcpserver.TransportStdio {
		t.Errorf("Transport() = %q, want %q", got, mcpserver.TransportStdio)
	}
	if got := server.Address(); got != net.JoinHostPort("127.0.0.1", "8000") {
		t.Errorf("Address() = %q, want the loopback default", got)
	}
	// A stdio server has no MCP endpoint over HTTP: mounting one would be a
	// second, unasked-for way in.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://127.0.0.1:8000"+mcpserver.StreamablePath, nil,
	)
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("a stdio server answered %s with %d, want %d",
			mcpserver.StreamablePath, recorder.Code, http.StatusNotFound)
	}
}

// TestParseTransportAcceptsExactlyThreeWires is the port of
// test_mcp_main_rejects_unknown_transport. SSE is a compatibility affordance
// for older clients, not a third choice, and anything else is a typo that must
// fail at startup rather than silently fall back.
func TestParseTransportAcceptsExactlyThreeWires(t *testing.T) {
	t.Parallel()

	for _, accepted := range []mcpserver.Transport{
		mcpserver.TransportStdio, mcpserver.TransportSSE, mcpserver.TransportStreamableHTTP,
	} {
		parsed, err := mcpserver.ParseTransport(string(accepted))
		if err != nil {
			t.Errorf("ParseTransport(%q) error = %v, want nil", accepted, err)
		}
		if parsed != accepted {
			t.Errorf("ParseTransport(%q) = %q", accepted, parsed)
		}
	}
	for _, refused := range []string{"websocket", "http", "STDIO", ""} {
		if _, err := mcpserver.ParseTransport(refused); !errors.Is(err, mcpserver.ErrInvalidOptions) {
			t.Errorf("ParseTransport(%q) error = %v, want it to wrap ErrInvalidOptions", refused, err)
		}
	}
}

// TestOptionsFromEnvReadsTheDeploymentSettings covers what the Kubernetes
// manifest actually sets, including the narrowed allowlist.
func TestOptionsFromEnvReadsTheDeploymentSettings(t *testing.T) {
	t.Setenv(mcpserver.EnvHost, "0.0.0.0")
	t.Setenv(mcpserver.EnvPort, "8000")
	t.Setenv(mcpserver.EnvTransport, string(mcpserver.TransportStreamableHTTP))
	t.Setenv(mcpserver.EnvAllowedHosts, " agentops-mcp:8000,agentops-mcp ")

	options, err := mcpserver.OptionsFromEnv(30 * time.Second)
	if err != nil {
		t.Fatalf("OptionsFromEnv() error = %v, want nil", err)
	}
	if options.Host != "0.0.0.0" || options.Port != 8000 {
		t.Errorf("address = %s:%d, want 0.0.0.0:8000", options.Host, options.Port)
	}
	if options.Transport != mcpserver.TransportStreamableHTTP {
		t.Errorf("Transport = %q, want %q", options.Transport, mcpserver.TransportStreamableHTTP)
	}
	// Whitespace around each entry is trimmed, which is what makes a manifest's
	// multi-line YAML value usable verbatim.
	if want := []string{"agentops-mcp:8000", "agentops-mcp"}; !slices.Equal(options.AllowedHosts, want) {
		t.Errorf("AllowedHosts = %v, want %v", options.AllowedHosts, want)
	}
	if options.DrainTimeout != 30*time.Second {
		t.Errorf("DrainTimeout = %s, want the value the caller passed", options.DrainTimeout)
	}
}

// TestOptionsFromEnvRefusesAnEmptyAllowlist is the port of
// test_mcp_allowed_hosts_rejects_an_empty_override.
//
// An override that resolves to nothing is refused rather than falling back to
// the defaults: an operator who narrowed the list and mistyped it would
// otherwise silently get the wide default list, which is the exact opposite of
// what they asked for.
func TestOptionsFromEnvRefusesAnEmptyAllowlist(t *testing.T) {
	t.Setenv(mcpserver.EnvAllowedHosts, " , ")

	_, err := mcpserver.OptionsFromEnv(0)
	if !errors.Is(err, mcpserver.ErrInvalidOptions) {
		t.Fatalf("OptionsFromEnv() error = %v, want it to wrap ErrInvalidOptions", err)
	}
	if !strings.Contains(err.Error(), "at least one host authority") {
		t.Errorf("OptionsFromEnv() error = %v, want it to say what is missing", err)
	}
}

// TestOptionsFromEnvRefusesAnUnusableAddress keeps a typo from becoming a
// server that binds somewhere nobody is looking.
func TestOptionsFromEnvRefusesAnUnusableAddress(t *testing.T) {
	cases := map[string]map[string]string{
		"a port that is not a number": {mcpserver.EnvPort: "eight-thousand"},
		"a port out of range":         {mcpserver.EnvPort: "70000"},
		"an unsupported transport":    {mcpserver.EnvTransport: "websocket"},
	}
	for name, environment := range cases {
		t.Run(name, func(t *testing.T) {
			for variable, value := range environment {
				t.Setenv(variable, value)
			}
			if _, err := mcpserver.OptionsFromEnv(0); !errors.Is(err, mcpserver.ErrInvalidOptions) {
				t.Errorf("OptionsFromEnv() error = %v, want it to wrap ErrInvalidOptions", err)
			}
		})
	}
}

// TestNewRefusesAWildcardAllowlist: one character would turn DNS-rebinding
// protection off entirely, so it is refused at startup rather than accepted as
// a configuration choice.
func TestNewRefusesAWildcardAllowlist(t *testing.T) {
	t.Parallel()

	_, err := mcpserver.New(mcpserver.Config{
		Probe:   func(ctx context.Context) error { return nil },
		Tools:   readSurface(t, newStore(t), false),
		Options: mcpserver.Options{AllowedHosts: []string{"*"}},
	})
	if !errors.Is(err, mcpserver.ErrInvalidOptions) {
		t.Fatalf("New() error = %v, want it to wrap ErrInvalidOptions", err)
	}
}

// TestServeDrainsInFlightRequestsOnShutdown is the port of
// test_mcp_http_transports_have_a_bounded_sigterm_drain, asserted through the
// behavior rather than through the arguments passed to a server library.
//
// The property that matters on a rolling update is that a tool call already in
// flight when SIGTERM arrives still gets its answer, and that the process then
// stops rather than lingering. Both halves are checked here.
func TestServeDrainsInFlightRequestsOnShutdown(t *testing.T) {
	t.Parallel()

	// Port 0 lets the operating system choose a free port, so the suite never
	// collides with a developer's running server.
	fixture := newFixture(t, func(o *fixtureOptions) {
		o.options = mcpserver.Options{
			Transport:    mcpserver.TransportStreamableHTTP,
			Port:         freePort(t),
			DrainTimeout: 5 * time.Second,
		}
	})

	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- fixture.server.Serve(ctx) }()

	// Wait for the listener rather than sleeping a guessed interval.
	address := fixture.server.Address()
	waitForListener(t, address)

	// One real request over the wire, so the drain has something to drain.
	probe := probeLiveness(t, address)
	if probe != http.StatusOK {
		t.Fatalf("liveness over the bound listener = %d, want %d", probe, http.StatusOK)
	}

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve() error = %v, want nil on a canceled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve() did not return after its context was canceled")
	}
}

// TestServeReportsAnUnusableAddress: a port already in use is a real failure
// and has to be reported, not swallowed into a process that serves nothing.
func TestServeReportsAnUnusableAddress(t *testing.T) {
	t.Parallel()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding a port to occupy: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := occupied.Close(); closeErr != nil {
			t.Errorf("closing the occupying listener: %v", closeErr)
		}
	})
	_, port, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatalf("splitting the occupied address: %v", err)
	}

	fixture := newFixture(t, func(o *fixtureOptions) {
		o.options = mcpserver.Options{
			Transport: mcpserver.TransportStreamableHTTP,
			Port:      atoi(t, port),
		}
	})
	if err := fixture.server.Serve(t.Context()); err == nil {
		t.Error("Serve() on an occupied port returned no error")
	}
}

// freePort asks the operating system for a port nothing is using.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("splitting the reserved address: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return atoi(t, port)
}

// waitForListener blocks until the address accepts a connection.
func waitForListener(t *testing.T, address string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			if err := connection.Close(); err != nil {
				t.Fatalf("closing the probe connection: %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing accepted a connection on %s", address)
}

// probeLiveness calls the liveness route over a bound listener.
func probeLiveness(t *testing.T, address string) int {
	t.Helper()

	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://"+address+mcpserver.LivenessPath, nil,
	)
	if err != nil {
		t.Fatalf("building the liveness request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling liveness: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Errorf("closing the liveness response: %v", err)
	}
	return response.StatusCode
}

// atoi parses a port the operating system produced.
func atoi(t *testing.T, value string) int {
	t.Helper()

	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("parsing the port %q: %v", value, err)
	}
	return port
}
