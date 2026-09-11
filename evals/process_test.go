package evals

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentProcessOwnsItsChildFromReadinessToShutdown(t *testing.T) {
	t.Parallel()

	// The child here is a stand-in that only holds the process slot, and the
	// readiness contract is answered by a local server bound to the port the
	// harness was given. What is under test is the harness's own lifecycle —
	// start the candidate, probe the documented path, publish the loopback base
	// URL, stop the child — which needs neither a model nor a real agent.
	port, probedPath := serveReadiness(t, http.StatusOK)
	binary := filepath.Join(t.TempDir(), "agent")
	writeExecutableScript(t, binary, "#!/bin/sh\nexec sleep 30\n")

	process, err := StartAgentProcess(t.Context(), AgentProcessConfig{
		Binary: binary, Transport: "rest", Entrypoint: "agent",
		DataDir: "data", StateDir: t.TempDir(), Port: port, Output: io.Discard,
	})
	if err != nil {
		t.Fatalf("StartAgentProcess() error = %v", err)
	}
	// The REST base URL carries ADK's /api mount. Without it the readiness probe and
	// every later request 404 against a server that is running perfectly.
	if got, want := process.BaseURL(), "http://127.0.0.1:"+strconv.Itoa(port)+"/api"; got != want {
		t.Fatalf("BaseURL() = %q, want the ADK API mount %q", got, want)
	}
	if got := probedPath.Load(); got != "/api/list-apps" {
		t.Fatalf("readiness path = %v, want the REST contract", got)
	}
	if err := process.Close(); err != nil {
		t.Fatalf("Close() error = %v, want a clean interrupt of the child", err)
	}
	// Cleanup runs from both the case body and a deferred join, so a second Close
	// must return the same verdict instead of signaling a process that is gone.
	if err := process.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want the first verdict", err)
	}
}

func TestStartAgentProcessBoundsStartupAndProbesTheTransportContract(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), "agent")
	writeExecutableScript(t, binary, "#!/bin/sh\nexec sleep 30\n")
	// REST probes under ADK's /api mount; A2A serves /healthz at the root.
	for name, wantPath := range map[string]string{"rest": "/api/list-apps", "a2a": "/healthz"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The stand-in never serves the contract, so startup must end at the
			// deadline rather than waiting forever on a candidate that never came up.
			// The budget spans several of the harness's 250ms probes so a loaded
			// machine still records at least one before the deadline arrives.
			port, probedPath := serveReadiness(t, http.StatusServiceUnavailable)
			_, err := StartAgentProcess(t.Context(), AgentProcessConfig{
				Binary: binary, Transport: name, Entrypoint: "agent",
				DataDir: "data", StateDir: t.TempDir(), Port: port, Output: io.Discard,
				StartupTimeout: 2 * time.Second, ShutdownTimeout: time.Second,
			})
			if err == nil || !strings.Contains(err.Error(), "did not answer /"+strings.TrimPrefix(strings.TrimPrefix(wantPath, "/api/"), "/")) {
				t.Fatalf("StartAgentProcess() error = %v, want a bounded %s readiness failure", err, wantPath)
			}
			if got := probedPath.Load(); got != wantPath {
				t.Fatalf("readiness path = %v, want %q for the %s transport", got, wantPath, name)
			}
		})
	}
}

func TestStartAgentProcessRefusesAnUnderspecifiedCandidate(t *testing.T) {
	t.Parallel()

	valid := AgentProcessConfig{
		Binary: "/tmp/agent", Transport: "rest", Entrypoint: "agent",
		DataDir: "data", StateDir: "state", Port: 43200,
	}
	for name, test := range map[string]struct {
		mutate func(*AgentProcessConfig)
		want   string
	}{
		"binary":      {mutate: func(c *AgentProcessConfig) { c.Binary = "" }, want: "needs a binary"},
		"data":        {mutate: func(c *AgentProcessConfig) { c.DataDir = "" }, want: "needs a binary"},
		"state":       {mutate: func(c *AgentProcessConfig) { c.StateDir = "" }, want: "needs a binary"},
		"port":        {mutate: func(c *AgentProcessConfig) { c.Port = 0 }, want: "positive port"},
		"transport":   {mutate: func(c *AgentProcessConfig) { c.Transport = "grpc" }, want: `unsupported agent transport "grpc"`},
		"absent file": {mutate: func(c *AgentProcessConfig) { c.Binary = "/nonexistent/agent" }, want: "start agent process"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config := valid
			test.mutate(&config)
			if _, err := StartAgentProcess(t.Context(), config); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("StartAgentProcess() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestAgentProcessCloseReportsAChildThatAlreadyExited(t *testing.T) {
	t.Parallel()

	// A candidate that died mid-run invalidates the results collected after it,
	// so cleanup reports the exit instead of silently succeeding. The process is
	// built directly because Close answers from the exit channel before it ever
	// touches the command, which is exactly the path under test.
	done := make(chan error, 1)
	done <- errors.New("exit status 1")
	close(done)
	process := &AgentProcess{done: done, shutdownTimeout: time.Second}
	if err := process.Close(); err == nil || !strings.Contains(err.Error(), "agent process exited") {
		t.Fatalf("Close() error = %v, want the child's exit to be reported", err)
	}
}

func TestWaitForReadinessStopsWhenTheAgentOrTheRunGoesAway(t *testing.T) {
	t.Parallel()

	t.Run("agent exits cleanly", func(t *testing.T) {
		t.Parallel()
		done := make(chan error, 1)
		done <- nil
		err := waitForReadiness(t.Context(), done, "http://127.0.0.1:1", "/healthz", time.Minute)
		if err == nil || !strings.Contains(err.Error(), "agent exited before becoming ready") {
			t.Fatalf("waitForReadiness() error = %v, want an early-exit report", err)
		}
	})

	t.Run("agent exits with a failure", func(t *testing.T) {
		t.Parallel()
		done := make(chan error, 1)
		done <- errors.New("exit status 2")
		err := waitForReadiness(t.Context(), done, "http://127.0.0.1:1", "/healthz", time.Minute)
		if err == nil || !strings.Contains(err.Error(), "exit status 2") {
			t.Fatalf("waitForReadiness() error = %v, want the child's exit status", err)
		}
	})

	t.Run("run is canceled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := waitForReadiness(ctx, make(chan error), "http://127.0.0.1:1", "/healthz", time.Minute)
		if err == nil || !strings.Contains(err.Error(), "wait for agent readiness") {
			t.Fatalf("waitForReadiness() error = %v, want the canceled run to end the wait", err)
		}
	})
}

// serveReadiness binds a local server to a free port and returns that port plus
// the last path it was asked for, so a test can assert which readiness contract
// the harness probed without running an agent.
func serveReadiness(t *testing.T, status int) (int, *atomic.Value) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		t.Fatalf("listener address = %T, want a TCP address", listener.Addr())
	}
	probedPath := &atomic.Value{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		probedPath.Store(request.URL.Path)
		writer.WriteHeader(status)
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return address.Port, probedPath
}
