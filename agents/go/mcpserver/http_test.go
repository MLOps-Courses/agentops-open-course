package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/mcpserver"
)

// get sends one GET through the server's handler without binding a port.
//
// It returns the recorder rather than an http.Response so nothing has a body to
// close: the handler under test is the subject, and the transport is not.
func (f *fixture) get(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://127.0.0.1:8000"+path, nil,
	)
	for name, value := range headers {
		if name == "Host" {
			// Go carries the Host header on the request field, not the header
			// map, and setting only the map would leave the guard reading the
			// URL's host instead.
			request.Host = value
			continue
		}
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// decodeHealth reads a probe body.
func decodeHealth(t *testing.T, recorded *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.NewDecoder(recorded.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the probe body: %v", err)
	}
	return body
}

// TestDefaultAllowlistIsNarrowAndOwnsItsEntries is the port of
// test_mcp_transport_security_uses_a_narrow_host_allowlist. The entries are a
// deployment contract — the Kubernetes manifest and the three gateway profiles
// address this server by exactly these names — and "*" would turn the whole
// control off in one character.
func TestDefaultAllowlistIsNarrowAndOwnsItsEntries(t *testing.T) {
	t.Parallel()

	allowed := mcpserver.DefaultAllowedHosts()
	for _, required := range []string{
		"localhost", "localhost:*",
		"127.0.0.1", "127.0.0.1:*",
		"[::1]", "[::1]:*",
		"host.docker.internal", "host.docker.internal:*",
		"agentgateway", "agentgateway:*",
		"agentgateway.agentops.svc.cluster.local", "agentgateway.agentops.svc.cluster.local:*",
		"agentops-mcp", "agentops-mcp:*",
		"agentops-mcp.agentops.svc.cluster.local", "agentops-mcp.agentops.svc.cluster.local:*",
	} {
		if !slices.Contains(allowed, required) {
			t.Errorf("the default allowlist is missing %q", required)
		}
	}
	if slices.Contains(allowed, "*") {
		t.Error("the default allowlist contains \"*\", which disables the control entirely")
	}

	// The returned slice is a copy: a caller that truncates it must not be able
	// to narrow — or widen — what the next caller sees.
	allowed[0] = "attacker.example"
	if mcpserver.DefaultAllowedHosts()[0] == "attacker.example" {
		t.Error("DefaultAllowedHosts() returns shared state a caller can edit")
	}
}

// TestAuthorityGuardAcceptsEveryDocumentedAddress is the port of
// test_mcp_transport_security_accepts_expected_authorities: every way the
// deployment legitimately addresses this server has to work, or a chapter
// breaks.
func TestAuthorityGuardAcceptsEveryDocumentedAddress(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"localhost",
		"localhost:8000",
		"127.0.0.1:8000",
		"host.docker.internal:8000",
		"agentgateway:3000",
		"agentgateway.agentops.svc.cluster.local:3000",
		"agentops-mcp:8000",
		"agentops-mcp.agentops.svc.cluster.local",
		"agentops-mcp.agentops.svc.cluster.local:8000",
	} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			response := newFixture(t).get(t, mcpserver.LivenessPath,
				map[string]string{"Host": host})
			if response.Code != http.StatusOK {
				t.Errorf("Host %q got status %d, want %d", host, response.Code, http.StatusOK)
			}
		})
	}
}

// TestAuthorityGuardRejectsEverythingElse is the port of
// test_mcp_transport_security_rejects_untrusted_host, plus the near-misses a
// prefix check would have let through.
//
// 421 Misdirected Request is literal here: the request reached a server that is
// not the one its authority names. It is also the status the Python transport
// security middleware returns, so the two tracks fail identically.
func TestAuthorityGuardRejectsEverythingElse(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"attacker.example",
		"attacker.example:8000",
		// A trailing colon and a non-numeric port are what a naive prefix check
		// would accept, and they are exactly how a rebinding attempt is dressed
		// up as an allowed authority.
		"agentgateway:",
		"agentgateway:evil.example",
		"agentgateway.attacker.example",
		"evil-agentgateway",
	} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			response := newFixture(t).get(t, mcpserver.LivenessPath,
				map[string]string{"Host": host})
			if response.Code != http.StatusMisdirectedRequest {
				t.Errorf("Host %q got status %d, want %d",
					host, response.Code, http.StatusMisdirectedRequest)
			}
		})
	}
}

// TestOriginGuardRejectsACrossSiteRequest closes the browser half of the same
// hole: nothing in this course serves a page that talks to the MCP endpoint, so
// any Origin that is not loopback is a cross-site request that should not be
// here.
func TestOriginGuardRejectsACrossSiteRequest(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	allowed := fixture.get(t, mcpserver.LivenessPath, map[string]string{
		"Host": "127.0.0.1:8000", "Origin": "http://localhost:8001",
	})
	if allowed.Code != http.StatusOK {
		t.Errorf("a loopback origin got status %d, want %d", allowed.Code, http.StatusOK)
	}

	refused := fixture.get(t, mcpserver.LivenessPath, map[string]string{
		"Host": "127.0.0.1:8000", "Origin": "https://attacker.example",
	})
	if refused.Code != http.StatusForbidden {
		t.Errorf("a cross-site origin got status %d, want %d", refused.Code, http.StatusForbidden)
	}
}

// TestAllowlistCanBeNarrowedByConfiguration is the port of
// test_mcp_allowed_hosts_can_be_narrowed_by_environment, asserted through the
// behavior rather than through the parsed value: the Kubernetes manifest
// narrows the list to the in-cluster names, and what matters is that everything
// else is then refused.
func TestAllowlistCanBeNarrowedByConfiguration(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *fixtureOptions) {
		o.options = mcpserver.Options{AllowedHosts: []string{"agentops-mcp:8000"}}
	})

	kept := fixture.get(t, mcpserver.LivenessPath,
		map[string]string{"Host": "agentops-mcp:8000"})
	if kept.Code != http.StatusOK {
		t.Errorf("the narrowed authority got status %d, want %d", kept.Code, http.StatusOK)
	}

	// Loopback is on the default list and must be gone once the list is
	// narrowed, or "narrowed" would mean "widened by union".
	dropped := fixture.get(t, mcpserver.LivenessPath,
		map[string]string{"Host": "127.0.0.1:8000"})
	if dropped.Code != http.StatusMisdirectedRequest {
		t.Errorf("a dropped authority got status %d, want %d",
			dropped.Code, http.StatusMisdirectedRequest)
	}
}

// TestProbesAreRegisteredAndUnauthenticated is the port of
// test_mcp_health_routes_are_registered and test_mcp_livez_is_trivially_alive.
//
// kubelet speaks HTTP, not MCP: a probe that had to initialize a protocol
// session would be reporting the session's health, not the server's.
func TestProbesAreRegisteredAndUnauthenticated(t *testing.T) {
	t.Parallel()

	response := newFixture(t).get(t, mcpserver.LivenessPath,
		map[string]string{"Host": "127.0.0.1:8000"})
	if response.Code != http.StatusOK {
		t.Fatalf("%s status = %d, want %d", mcpserver.LivenessPath, response.Code, http.StatusOK)
	}
	if got := decodeHealth(t, response)["status"]; got != "alive" {
		t.Errorf("%s status = %v, want \"alive\"", mcpserver.LivenessPath, got)
	}
}

// TestReadinessReportsUnpreparedStateWithoutInitializingIt is the port of
// test_mcp_healthz_fails_on_fresh_state_without_initializing_it and
// test_mcp_probe_and_read_tools_do_not_prepare_writable_state.
//
// Two things at once, and the second is the load-bearing one: an unprepared
// runtime database is reported unready, and probing does not create it. The MCP
// replica is a reader; the A2A startup path is the single writer that
// publishes and migrates state, and a replica that migrated under a running
// agent would be a data race across processes.
func TestReadinessReportsUnpreparedStateWithoutInitializingIt(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	response := fixture.get(t, mcpserver.ReadinessPath,
		map[string]string{"Host": "127.0.0.1:8000"})

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("%s status = %d, want %d",
			mcpserver.ReadinessPath, response.Code, http.StatusServiceUnavailable)
	}
	body := decodeHealth(t, response)
	if got := body["status"]; got != "unready" {
		t.Errorf("status = %v, want \"unready\"", got)
	}
	problems, ok := body["problems"].([]any)
	if !ok || len(problems) == 0 {
		t.Errorf("an unready probe reported no problems: %v", body)
	}
	if fixture.probes != 1 {
		t.Errorf("the probe ran %d times, want exactly 1", fixture.probes)
	}
	// Nothing was published: the read path falls back to the committed seed and
	// the runtime database still does not exist.
	if _, err := os.Stat(fixture.store.RuntimePath()); err == nil {
		t.Error("probing readiness published runtime state")
	}
}

// TestReadinessReportsReadyWhenStateIsUsable covers the other branch, with the
// probe seam standing in for a prepared database.
func TestReadinessReportsReadyWhenStateIsUsable(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *fixtureOptions) {
		o.probe = func(context.Context) error { return nil }
	})
	response := fixture.get(t, mcpserver.ReadinessPath,
		map[string]string{"Host": "127.0.0.1:8000"})

	if response.Code != http.StatusOK {
		t.Errorf("%s status = %d, want %d", mcpserver.ReadinessPath, response.Code, http.StatusOK)
	}
	body := decodeHealth(t, response)
	if got := body["status"]; got != "ready" {
		t.Errorf("status = %v, want \"ready\"", got)
	}
	if _, reported := body["problems"]; reported {
		t.Errorf("a ready probe reported problems: %v", body)
	}
}

// TestReadinessNamesTheFailureTypeAndNotItsMessage keeps a readiness body from
// becoming an information leak: it is served to anyone who can reach the port,
// and a driver message can name a path, a query or a credential.
func TestReadinessNamesTheFailureTypeAndNotItsMessage(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *fixtureOptions) {
		o.probe = func(context.Context) error { return errProbeRefused }
	})
	response := fixture.get(t, mcpserver.ReadinessPath,
		map[string]string{"Host": "127.0.0.1:8000"})

	body := decodeHealth(t, response)
	problems, ok := body["problems"].([]any)
	if !ok || len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", body["problems"])
	}
	reported, ok := problems[0].(string)
	if !ok {
		t.Fatalf("problem = %v, want a string", problems[0])
	}
	if reported == errProbeRefused.Error() {
		t.Errorf("the probe reported the error's message verbatim: %q", reported)
	}
	if !strings.Contains(reported, "dataset unavailable") {
		t.Errorf("problem = %q, want it to name the failure class", reported)
	}
}
