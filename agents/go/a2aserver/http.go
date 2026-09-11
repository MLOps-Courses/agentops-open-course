package a2aserver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/piiwebhook"
)

// health is the body both probes return.
type health struct {
	Status   string   `json:"status"`
	Problems []string `json:"problems,omitempty"`
}

// Handler returns the server's whole HTTP surface.
//
// It is exported so the surface can be exercised over net/http/httptest — the
// protocol, the identity binding and the probes all tested through the same
// handler a real client reaches, without binding a port. Before
// [Server.Start] has run it answers every path with 503: the task store is
// opened after the state preflight, so there is genuinely nothing to serve yet,
// and answering "unready" is more honest than a nil-pointer panic.
func (s *Server) Handler() http.Handler {
	if s.handler == nil {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writeJSON(writer, request, s.logger, http.StatusServiceUnavailable,
				health{Status: statusUnready, Problems: []string{ErrNotStarted.Error()}})
		})
	}
	return s.handler
}

// buildHandler wires the routes. It runs at the end of [Server.Start], once the
// task store exists.
func (s *Server) buildHandler() (http.Handler, error) {
	options, err := s.A2AOptions()
	if err != nil {
		return nil, err
	}
	requestHandler := a2asrv.NewHandler(s.executor(), options...)

	a2aMux := http.NewServeMux()
	// The probes are registered outside the protocol on purpose: kubelet speaks
	// HTTP, not JSON-RPC, and a probe that had to open a task would report that
	// task's health rather than the server's.
	a2aMux.HandleFunc("GET "+ReadinessPath, s.handleReadiness)
	a2aMux.HandleFunc("GET "+LivenessPath, s.handleLiveness)
	a2aMux.HandleFunc("GET "+CardPath, s.handleCard)

	// Protocol 1.0 and 0.3 over the same request handler, so the two bindings
	// can never diverge in behavior — only in wire shape.
	a2aMux.Handle("POST "+InvokePath, a2asrv.NewJSONRPCHandler(requestHandler))
	a2aMux.Handle("POST "+CompatInvokePath, a2av0.NewJSONRPCHandler(requestHandler))
	// "{$}" anchors the pattern to the exact root path. A bare "POST /" would
	// be a catch-all that answered every unrouted POST with the protocol —
	// including a typo'd gateway route, which would then look like it worked.
	a2aMux.Handle("POST "+RootPath+"{$}", a2av0.NewJSONRPCHandler(requestHandler))

	var a2aHandler http.Handler = a2aMux
	// Outermost on the A2A surface, so every protocol request is marked as a
	// network call before execution. The prompt-guard webhook is a private
	// machine-to-machine route and deliberately sits outside this middleware.
	a2aHandler = bindVerifiedIdentity(s.options.TrustedIdentityHeader, s.logger, a2aHandler)

	mux := http.NewServeMux()
	if s.promptGuardHandler != nil {
		mux.Handle("POST "+piiwebhook.RequestPath, s.promptGuardHandler)
		mux.Handle("POST "+piiwebhook.ResponsePath, s.promptGuardHandler)
	}
	mux.Handle(RootPath, a2aHandler)
	return mux, nil
}

// handleCard serves the public agent card.
//
// It is served by this package rather than by a2asrv.NewAgentCardHandler for
// one reason: that handler answers every request with
// Access-Control-Allow-Origin reflecting the caller's Origin, plus
// Allow-Credentials. The Python server sends no CORS headers at all, and the
// browser client is expected to reach the agent through the gateway, which is
// where the origin allowlist lives. Reflecting any origin here would quietly
// make the gateway's CORS policy optional.
func (s *Server) handleCard(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	if _, err := writer.Write(s.card); err != nil {
		s.logger.ErrorContext(request.Context(), "writing the agent card", "error", err)
	}
}

// handleReadiness reports whether this replica can actually serve.
//
// Every problem is accumulated rather than short-circuited, because an operator
// fixing a bad deployment wants the whole list, not the first item. Each entry
// names a failure class and never a message: the readiness body is served to
// anyone who can reach the port, and a driver error can carry a path or a
// query. The full error goes to the log, where it belongs.
func (s *Server) handleReadiness(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	var problems []string

	if err := s.probeDataset(ctx); err != nil {
		s.logger.WarnContext(ctx, "readiness: the dataset is unavailable", "error", err)
		problems = append(problems, fmt.Sprintf("dataset unavailable: %T", err))
	}
	if err := stateDirectoryIsWritable(s.options.StateDir); err != nil {
		s.logger.WarnContext(ctx, "readiness: the state directory is not writable", "error", err)
		// The path, deliberately: it is already in the deployment manifest, and
		// it is the one detail that tells an operator which mount to look at.
		problems = append(problems, err.Error())
	}
	// A bounded deadline of its own, so a wedged filesystem makes this replica
	// unready rather than holding the probe open until kubelet's own timeout.
	probeCtx, cancel := context.WithTimeout(ctx, storeProbeTimeout)
	defer cancel()
	if err := probeStateStores(probeCtx, s.options.StateDir, s.probeSessions); err != nil {
		s.logger.WarnContext(ctx, "readiness: the session or task store is invalid", "error", err)
		problems = append(problems, fmt.Sprintf("session/task store invalid: %T", err))
	}
	if s.promptGuardReadiness != nil {
		if err := s.promptGuardReadiness(ctx); err != nil {
			s.logger.WarnContext(ctx, "readiness: the named-entity model is unavailable", "error", err)
			problems = append(problems, fmt.Sprintf("named-entity model unavailable: %T", err))
		}
	}

	if len(problems) > 0 {
		writeJSON(writer, request, s.logger, http.StatusServiceUnavailable,
			health{Status: statusUnready, Problems: problems})
		return
	}
	writeJSON(writer, request, s.logger, http.StatusOK, health{Status: statusReady})
}

// handleLiveness answers as long as the process can serve a request.
//
// Trivial by design. Liveness restarts the container, and a restart only helps
// a wedged process — wiring it to the database would turn a database outage
// into a restart loop that makes the outage worse.
func (s *Server) handleLiveness(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, request, s.logger, http.StatusOK, health{Status: statusAlive})
}
