package a2aserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/principal"
)

// The gateway-verified caller identity, carried from the HTTP boundary into the
// A2A call context.
//
// # Why this is two pieces and not one
//
// ADK Go reads the caller identity from exactly one place,
// a2asrv.CallContext.User.Name (server/adka2a/v2/metadata.go), and the A2A
// transport creates that CallContext itself, inside its own ServeHTTP, after
// any HTTP middleware has already run. A context value set on the request does
// reach every handler on that request, but it cannot reach a CallContext that
// does not exist yet, so the value has to be handed across in two steps:
//
//  1. [Server.bindVerifiedIdentity] reads and validates the header at the HTTP
//     boundary — this is where a duplicate header can still be answered with a
//     400 — and stores the subject in the request context.
//  2. [identityInterceptor], installed with a2asrv.WithCallInterceptors, runs
//     inside the A2A handler once the CallContext exists and promotes the
//     subject onto it.
//
// # What depends on it
//
// Three consumers read it:
//
//   - the run's user id, which becomes the audit row's approved_by, because
//     adka2a's toInvocationMeta prefers CallContext.User.Name over the
//     synthetic "A2A_USER_<contextId>";
//   - the per-user long-term memory key, which is the same user id;
//   - task ownership, because a2asrv.NewTaskStoreAuthenticator reads the same
//     field, so ListTasks is scoped to the verified caller.
//
// # The trust assumption
//
// Only set AGENT_TRUSTED_IDENTITY_HEADER when a trusted gateway validates the
// JWT and *sets* this header itself, overwriting any client-supplied copy. A
// raw client could otherwise forge it. When the setting is empty the header is
// never read, so an unconfigured deployment cannot be tricked into trusting one.

const (
	// duplicateIdentityHeader is the body returned when a request carries the
	// trusted header twice. Two values mean at least one of them was not set by
	// the gateway, and there is no safe way to pick between them.
	duplicateIdentityHeader = "duplicate trusted identity header"
	identityRequired        = "authenticated identity required"
)

// VerifiedSubject returns the gateway-verified caller identity bound to ctx, if
// any.
//
// It is exported because it is the honest way for anything downstream of the
// HTTP boundary to ask "who did the gateway say this is?" without re-reading a
// header it must not trust on its own.
func VerifiedSubject(ctx context.Context) (string, bool) {
	authenticated, state := principal.Network(ctx)
	return authenticated.Subject(), state == principal.NetworkAuthenticated
}

// bindVerifiedIdentity reads the trusted identity header once per request.
//
// The middleware always marks A2A requests as network-originated. When no
// header name is configured it reads no identity header and grants no principal,
// so guarded writes fail closed while read-only A2A remains available. Once a
// trusted header is configured, every protocol POST must carry exactly one
// valid subject; public card and probe GETs remain independent of caller auth.
// --8<-- [start:verified-identity]
func bindVerifiedIdentity(header string, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		request = request.WithContext(principal.MarkNetwork(request.Context()))
		if header == "" || request.Method != http.MethodPost {
			next.ServeHTTP(writer, request)
			return
		}
		// Header.Values canonicalizes its argument, so it collects the header
		// under any capitalization — the case-insensitive match the ASGI
		// middleware did by lowercasing bytes.
		values := request.Header.Values(header)
		if len(values) > 1 {
			logger.WarnContext(request.Context(), "refused an A2A request",
				"reason", duplicateIdentityHeader, "header", header, "status", http.StatusBadRequest)
			writeJSON(writer, request, logger, http.StatusBadRequest,
				map[string]string{"error": duplicateIdentityHeader})
			return
		}
		if len(values) != 1 {
			logger.WarnContext(request.Context(), "refused an A2A request",
				"reason", identityRequired, "header", header, "status", http.StatusUnauthorized)
			writeJSON(writer, request, logger, http.StatusUnauthorized,
				map[string]string{"error": identityRequired})
			return
		}
		authenticated, err := principal.NewAuthenticated(strings.TrimSpace(values[0]))
		if err != nil {
			// The raw value is attacker-controlled identity data. Keep it out of
			// both the response and durable logs; the local failure class is enough.
			logger.WarnContext(request.Context(), "refused an A2A request",
				"reason", identityRequired, "header", header, "status", http.StatusUnauthorized)
			writeJSON(writer, request, logger, http.StatusUnauthorized,
				map[string]string{"error": identityRequired})
			return
		}
		request = request.WithContext(principal.BindNetwork(request.Context(), authenticated))
		next.ServeHTTP(writer, request)
	})
}

// --8<-- [end:verified-identity]

// identityInterceptor promotes the verified subject onto the A2A call context.
//
// It embeds the SDK's passthrough so a new interceptor method added upstream
// does not silently become this type's responsibility.
type identityInterceptor struct {
	a2asrv.PassthroughCallInterceptor
}

// Before implements a2asrv.CallInterceptor.
//
// It runs for every protocol method, not only message sends, because task
// ownership is read on the listing path too: a caller who is authenticated well
// enough to create a task must be the same caller who can list it back.
func (identityInterceptor) Before(
	ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request,
) (context.Context, any, error) {
	subject, ok := VerifiedSubject(ctx)
	if !ok || callCtx == nil {
		// No verified subject means the unauthenticated synthetic identity
		// stands only for session and memory scoping. Guarded tools inspect
		// the typed network state separately and refuse it as write authority.
		return ctx, nil, nil
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(subject, nil)
	return ctx, nil, nil
}

// writeJSON writes one small JSON body, logging a write failure rather than
// swallowing it.
func writeJSON(writer http.ResponseWriter, request *http.Request, logger *slog.Logger, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(body); err != nil {
		// The status line is already on the wire, so the client sees a truncated
		// body and there is nothing left to correct. Logging is the only honest
		// response; silence would hide a probe that answers with nothing.
		logger.ErrorContext(request.Context(), "writing an A2A response body", "error", err)
	}
}
