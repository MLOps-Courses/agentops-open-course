package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The readiness and liveness payloads. They are asserted by the smoke tests and
// read by an operator on a bad day, so they are constants rather than literals
// typed twice.
const (
	statusReady   = "ready"
	statusUnready = "unready"
	statusAlive   = "alive"
)

// health is the body both probes return.
type health struct {
	Status   string   `json:"status"`
	Problems []string `json:"problems,omitempty"`
}

// Handler returns the server's whole HTTP surface: the MCP endpoint for the
// configured transport, and the two Kubernetes probes.
//
// It is exported so the surface can be exercised over httptest — the protocol,
// the allowlist and the probes all tested through the same handler a real
// client reaches, without binding a port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// The probes are registered unauthenticated and outside the MCP endpoint on
	// purpose: kubelet speaks HTTP, not MCP, and a probe that had to initialize
	// a protocol session would report the session's health, not the server's.
	mux.HandleFunc("GET "+ReadinessPath, s.handleReadiness)
	mux.HandleFunc("GET "+LivenessPath, s.handleLiveness)

	switch s.options.Transport {
	case TransportStdio:
		// Stdio has no MCP endpoint over HTTP, and mounting one anyway would be
		// a second, unasked-for way in. The probes above are still served,
		// because a stdio server can still be asked whether its data is usable.
	case TransportSSE:
		mux.Handle(SSEPath, mcp.NewSSEHandler(s.serverFor, nil))
	case TransportStreamableHTTP:
		mux.Handle(StreamablePath, mcp.NewStreamableHTTPHandler(s.serverFor, &mcp.StreamableHTTPOptions{
			// Stateless, exactly as the Python server: every request carries
			// everything the server needs, so any replica can serve any call
			// and a rolling update loses no session.
			Stateless: true,
			Logger:    s.logger,
			// This server's own allowlist below replaces the SDK's localhost
			// heuristic, and is strictly narrower: the SDK's check only fires
			// for requests that arrive via a loopback address, while
			// [Server.guardAuthority] checks every request. The heuristic would
			// also reject the documented host.docker.internal path, where a
			// gateway container legitimately reaches a loopback listener under
			// a non-loopback name.
			DisableLocalhostProtection: true,
		}))
	}
	return s.guardAuthority(mux)
}

// serverFor returns the MCP server backing one HTTP request.
//
// One server for every session: the tools are stateless reads over a shared
// database handle, so there is nothing per-client to keep apart.
func (s *Server) serverFor(*http.Request) *mcp.Server { return s.mcp }

// guardAuthority rejects requests whose Host or Origin header is not on the
// allowlist.
//
// This is DNS-rebinding and cross-origin protection, and it wraps everything,
// probes included: a probe that answered on any authority would tell an
// attacker's page that the server exists and is healthy.
func (s *Server) guardAuthority(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !authorityAllowed(s.options.AllowedHosts, request.Host) {
			// 421 Misdirected Request, which is literally what this is: the
			// request reached a server that is not the one its authority names.
			// It is also what the Python transport-security middleware returns,
			// so the two tracks fail identically.
			s.refuse(writer, request, http.StatusMisdirectedRequest,
				fmt.Sprintf("host %q is not an allowed authority", request.Host))
			return
		}
		if origin := request.Header.Get("Origin"); origin != "" &&
			!authorityAllowed(s.options.AllowedOrigins, origin) {
			s.refuse(writer, request, http.StatusForbidden,
				fmt.Sprintf("origin %q is not allowed", origin))
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// refuse writes a rejection and records why.
//
// The reason is logged with the offending value and returned to the client as a
// bare status text. Telling a caller which authority would have been accepted
// would hand an attacker the allowlist one guess at a time.
func (s *Server) refuse(writer http.ResponseWriter, request *http.Request, status int, reason string) {
	s.logger.WarnContext(request.Context(), "refused an MCP request", "reason", reason, "status", status)
	http.Error(writer, http.StatusText(status), status)
}

// authorityAllowed reports whether value matches any allowlist entry.
//
// Two forms, and only two. An exact entry matches exactly. An entry ending in
// ":*" matches that authority followed by a numeric port — numeric on purpose,
// so "agentgateway:" and "agentgateway:evil" do not slip through a prefix
// check. There is deliberately no leading wildcard: "*.example.com" would be a
// standing invitation to widen the list one subdomain at a time.
func authorityAllowed(allowed []string, value string) bool {
	for _, pattern := range allowed {
		if pattern == value {
			return true
		}
		base, wildcard := strings.CutSuffix(pattern, ":*")
		if !wildcard {
			continue
		}
		port, found := strings.CutPrefix(value, base+":")
		if found && isPort(port) {
			return true
		}
	}
	return false
}

// isPort reports whether text is a non-empty run of digits.
func isPort(text string) bool {
	if text == "" {
		return false
	}
	for _, character := range text {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// handleReadiness reports whether the runtime state is usable.
//
// Every failure class is one answer — unready — because that is the only
// question kubelet asks, and the detail belongs in the body and the log rather
// than in a status code nobody parses. The probe never prepares, migrates or
// repairs anything: this replica is a reader, and the A2A startup path is the
// single writer that publishes state.
func (s *Server) handleReadiness(writer http.ResponseWriter, request *http.Request) {
	if err := s.probe(request.Context()); err != nil {
		s.logger.WarnContext(request.Context(), "readiness probe failed", "error", err)
		// The error's type, not its message: the message can name a path or a
		// driver detail, and a readiness body is served to anyone who can reach
		// the port.
		s.writeHealth(writer, request, http.StatusServiceUnavailable, health{
			Status:   statusUnready,
			Problems: []string{fmt.Sprintf("dataset unavailable: %T", err)},
		})
		return
	}
	s.writeHealth(writer, request, http.StatusOK, health{Status: statusReady})
}

// handleLiveness answers as long as the process can serve a request.
//
// Trivial by design. Liveness restarts the container, and a restart only helps
// a wedged process — wiring it to the database would turn a database outage
// into a restart loop that makes the outage worse.
func (s *Server) handleLiveness(writer http.ResponseWriter, request *http.Request) {
	s.writeHealth(writer, request, http.StatusOK, health{Status: statusAlive})
}

// writeHealth writes one probe response.
func (s *Server) writeHealth(
	writer http.ResponseWriter, request *http.Request, status int, body health,
) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(body); err != nil {
		// The status line is already on the wire, so there is nothing left to
		// correct for the client — it sees a truncated body. Logging is the
		// only honest response, and swallowing the error silently would hide a
		// probe that answers with nothing.
		s.logger.ErrorContext(request.Context(), "writing a probe response", "error", err)
	}
}
