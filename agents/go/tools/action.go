package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/principal"
)

// The two guarded writes describe themselves to the model in unusually blunt
// terms, and every sentence is load-bearing. A model that believes calling the
// tool *is* the restart will promise the user it happened; a model that
// believes prose can stand in for the confirmation request will never create
// one. Both failure modes were observed, and both are answered here.
const (
	restartServiceDescription = "Create an ADK confirmation request for a guarded mock restart.\n\n" +
		"Call this tool before approval when the evidence supports a restart. " +
		"The call does not restart the service: ADK pauses before this function runs. " +
		"Only a later approval with a rationale executes the restart and writes the audit entry. " +
		"A prose promise cannot create the confirmation request.\n\n" +
		"Returns, after approval, a dict describing the outcome; otherwise ADK pauses " +
		"before execution. Direct unapproved calls fail closed with an \"error\"."

	resolveIncidentDescription = "Create an ADK confirmation request for a guarded mock resolution.\n\n" +
		"Call this tool before approval when the evidence supports resolution. " +
		"The call does not resolve the incident: ADK pauses before this function runs. " +
		"Only a later approval with a rationale executes the resolution and writes the audit entry. " +
		"A prose promise cannot create the confirmation request.\n\n" +
		"Returns, after approval, a dict describing the outcome; otherwise ADK pauses " +
		"before execution. Direct unapproved calls fail closed with an \"error\"."
)

// errWritesDisabled is what every guarded write reports while the kill-switch
// is engaged. It is phrased as a reason rather than a sentence because it is
// interpolated into the refusal below, and it says what still works: freezing
// writes must not read as "the agent is broken".
//
// It carries no trailing period, unlike the Python original: the refusal below
// appends one, and reproducing that wart would render "…resume approvals.. Ask
// the approver…" to whoever reads the incident transcript.
//
// The wording itself is config's, next to the variable it names, so this refusal
// and the two in policy and memory cannot drift into three descriptions of one
// switch. config.WritesDisabledReason is the interpolable form this needs.
var errWritesDisabled = errors.New(config.WritesDisabledReason)

// RestartServiceArgs are the arguments of the restart_service tool.
type RestartServiceArgs struct {
	Name string `json:"name"`
}

// RestartServiceResult is what restart_service returns after approval: the
// outcome and the audit row that records it, or a refusal.
type RestartServiceResult struct {
	Audit  *domain.AuditEntryRow `json:"audit,omitzero"`
	Error  string                `json:"error,omitzero"`
	Result string                `json:"result,omitzero"`
}

// ResolveIncidentArgs are the arguments of the resolve_incident tool.
type ResolveIncidentArgs struct {
	IncidentID string `json:"incident_id"`
}

// ResolveIncidentResult is what resolve_incident returns.
type ResolveIncidentResult struct {
	Audit  *domain.AuditEntryRow `json:"audit,omitzero"`
	Error  string                `json:"error,omitzero"`
	Result string                `json:"result,omitzero"`
}

// approval is validated approval data, safe to carry into the mutation layer.
//
// Its fields are unexported and it is only ever produced by
// [Tools.validatedApproval], so a value of this type is a proof: the action was
// confirmed by a named human, in a known session and invocation, with a
// rationale short enough and clean enough to persist.
type approval struct {
	approvedBy   string
	rationale    string
	sessionID    string
	invocationID string
}

// runRestartService is the restart_service handler. It runs only after ADK has
// paused for a human, or not at all — see [Tools.validatedApproval].
//
// It is deliberately not wrapped in a [Guard]: a retry could re-deliver a write
// across an unknown commit boundary. The idempotency key in the audit trail
// makes re-delivery safe, but that is a safety net, not a license to retry.
func (t *Tools) runRestartService(ctx agent.Context, args RestartServiceArgs) (RestartServiceResult, error) {
	// A direct call with no context has to reach the checks below rather than
	// panic on the first accessor: the ordering is observable — a malformed name
	// is reported as a malformed name even when there is no confirmation to find
	// — and the reads need a context of their own to get there.
	queryCtx := queryContext(ctx)

	if t.writesDisabled {
		// The raw argument, not the normalized one: nothing has been validated
		// yet, and echoing what the model actually sent is what makes the refusal
		// legible.
		return RestartServiceResult{
			Error: approvalRefusal("restart of "+quoted(args.Name), errWritesDisabled),
		}, nil
	}
	name, err := domain.NormalizeSlug(args.Name)
	if err != nil {
		return RestartServiceResult{Error: invalidServiceName(args.Name)}, nil
	}
	service, err := t.store.GetService(queryCtx, name)
	if err != nil {
		return RestartServiceResult{}, fmt.Errorf("get service %s: %w", name, err)
	}
	if service == nil {
		return RestartServiceResult{Error: unknownService(name)}, nil
	}
	approved, err := t.validatedApproval(ctx)
	if err != nil {
		return RestartServiceResult{
			Error: approvalRefusal("restart of "+quoted(string(name)), err),
		}, nil
	}

	entry, err := t.store.RestartServiceWithAudit(queryCtx, name, data.AuditIdentity{
		Actor:        auditActor,
		ApprovedBy:   approved.approvedBy,
		Rationale:    approved.rationale,
		SessionID:    approved.sessionID,
		InvocationID: approved.invocationID,
	})
	if err != nil {
		return RestartServiceResult{}, fmt.Errorf("restart service %s: %w", name, err)
	}
	if entry == nil {
		// The service was removed between the lookup above and the transaction.
		// Nothing changed and nothing was audited, so the honest answer is the
		// same one an unknown service gets.
		return RestartServiceResult{Error: unknownService(name)}, nil
	}
	row := entry.Row()
	return RestartServiceResult{
		Result: fmt.Sprintf("Service %q restarted and marked operational.", name),
		Audit:  &row,
	}, nil
}

// runResolveIncident is the resolve_incident handler. It mirrors
// [Tools.runRestartService] step for step, including the deliberate absence of
// a [Guard].
func (t *Tools) runResolveIncident(ctx agent.Context, args ResolveIncidentArgs) (ResolveIncidentResult, error) {
	queryCtx := queryContext(ctx)

	if t.writesDisabled {
		return ResolveIncidentResult{
			Error: approvalRefusal("resolution of "+quoted(args.IncidentID), errWritesDisabled),
		}, nil
	}
	incidentID, err := domain.NormalizeIncidentID(args.IncidentID)
	if err != nil {
		return ResolveIncidentResult{Error: invalidIncidentID(args.IncidentID)}, nil
	}
	incident, err := t.store.GetIncident(queryCtx, incidentID)
	if err != nil {
		return ResolveIncidentResult{}, fmt.Errorf("get incident %s: %w", incidentID, err)
	}
	if incident == nil {
		return ResolveIncidentResult{Error: fmt.Sprintf("No incident with id %q.", incidentID)}, nil
	}
	approved, err := t.validatedApproval(ctx)
	if err != nil {
		return ResolveIncidentResult{
			Error: approvalRefusal("resolution of "+quoted(string(incidentID)), err),
		}, nil
	}

	entry, err := t.store.ResolveIncidentWithAudit(queryCtx, incidentID, data.AuditIdentity{
		Actor:        auditActor,
		ApprovedBy:   approved.approvedBy,
		Rationale:    approved.rationale,
		SessionID:    approved.sessionID,
		InvocationID: approved.invocationID,
	})
	if err != nil {
		return ResolveIncidentResult{}, fmt.Errorf("resolve incident %s: %w", incidentID, err)
	}
	if entry == nil {
		// Resolving twice is not an error, it is a no-op — and saying so is more
		// useful than a generic refusal, because the agent's goal is already met.
		return ResolveIncidentResult{Error: fmt.Sprintf("Incident %q is already resolved.", incidentID)}, nil
	}
	row := entry.Row()
	return ResolveIncidentResult{
		Result: fmt.Sprintf("Incident %q marked resolved.", incidentID),
		Audit:  &row,
	}, nil
}

// validatedApproval parses a confirmed, attributable approval, or returns the
// reason it refuses.
//
// The human approves by answering the confirmation request with a payload like
// {"rationale": "why this is safe now"}; a bare string works too. Every check
// is repeated here rather than trusted to the framework, because the value of a
// guarded action is exactly that it fails closed: confirmation, approver
// identity, session, invocation and rationale are all required, and a call that
// arrives without one of them changes nothing.
//
// The returned error is a refusal to show a human, never a failure to
// propagate: the caller folds its text into a result the model reads.
// --8<-- [start:validated-approval]
func (t *Tools) validatedApproval(ctx agent.Context) (approval, error) {
	if ctx == nil {
		return approval{}, errors.New("the action must run through an ADK confirmation flow")
	}
	confirmation := ctx.ToolConfirmation()
	if confirmation == nil || !confirmation.Confirmed {
		return approval{}, errors.New("the action has not been confirmed")
	}

	// Session() is one of the accessors ADK stubs out inside a tool context, so
	// the identity is read from SessionID()/UserID(), which are not.
	identities := []struct{ label, value string }{
		{"approver identity", ctx.UserID()},
		{"session id", ctx.SessionID()},
		{"invocation id", ctx.InvocationID()},
	}
	var missing []string
	for _, identity := range identities {
		if strings.TrimSpace(identity.value) == "" {
			missing = append(missing, identity.label)
		}
	}
	if len(missing) > 0 {
		// All of them, not the first: an operator fixing a broken client wants the
		// whole list in one round trip.
		return approval{}, fmt.Errorf("the confirmed action is missing %s", strings.Join(missing, ", "))
	}
	approvedBy := strings.TrimSpace(ctx.UserID())
	networkPrincipal, networkState := principal.Network(ctx)
	switch networkState {
	case principal.NetworkUnauthenticated:
		return approval{}, errors.New("a network action requires an authenticated principal in addition to confirmation")
	case principal.NetworkAuthenticated:
		if networkPrincipal.Subject() != approvedBy {
			return approval{}, errors.New("the authenticated principal does not own the invocation")
		}
	}

	rationale := rationaleFrom(confirmation.Payload)
	if rationale == "" {
		return approval{}, errors.New("the approval carried no rationale")
	}
	if !fitsRationale(rationale) {
		return approval{}, fmt.Errorf(
			"the approval rationale exceeds %d characters", domain.MaxAuditRationaleLength,
		)
	}
	// Redaction runs before the row is written, because the audit trail is
	// append-only: a credential that lands there cannot be edited out afterwards.
	rationale = t.redact(rationale)
	// And the bound is re-checked afterwards, because a redactor rewrites rather
	// than deletes — "<EMAIL_ADDRESS>" is longer than most addresses it replaces.
	if !fitsRationale(rationale) {
		return approval{}, fmt.Errorf(
			"the redacted approval rationale exceeds %d characters", domain.MaxAuditRationaleLength,
		)
	}

	return approval{
		approvedBy:   approvedBy,
		rationale:    rationale,
		sessionID:    strings.TrimSpace(ctx.SessionID()),
		invocationID: strings.TrimSpace(ctx.InvocationID()),
	}, nil
}

// --8<-- [end:validated-approval]

// rationaleFrom extracts the approver's justification from a confirmation
// payload.
//
// Two shapes are accepted, and both are real: a client that answers with the
// documented {"rationale": "..."} object, and a console that answers with the
// bare string a human typed. Anything else carries no rationale, which the
// caller refuses.
func rationaleFrom(payload any) string {
	switch value := payload.(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		// The payload arrives as decoded JSON, so the value under the key can be
		// any JSON type. A number or a boolean is rendered rather than rejected —
		// it is a strange rationale, but it is one a human wrote — while an absent
		// key and an explicit null are both "no rationale at all".
		switch rationale := value["rationale"].(type) {
		case nil:
			return ""
		case string:
			return strings.TrimSpace(rationale)
		default:
			return strings.TrimSpace(fmt.Sprint(rationale))
		}
	default:
		return ""
	}
}

// fitsRationale reports whether the justification is short enough to persist.
// The bound counts characters rather than bytes, because it exists to keep a
// human explanation short and an accented word is not two letters long.
func fitsRationale(rationale string) bool {
	return len([]rune(rationale)) <= domain.MaxAuditRationaleLength
}

// approvalRefusal renders a refused action, and tells the approver exactly what
// to send instead. A refusal that does not say how to succeed just gets retried
// the same way.
func approvalRefusal(action string, reason error) string {
	return fmt.Sprintf(
		`Refusing %s: %s. Ask the approver to confirm through the runtime `+
			`with a payload such as {"rationale": "why this action is appropriate now"}.`,
		action, reason,
	)
}

// unknownService is the refusal both the pre-check and the lost race return, so
// a service that disappears mid-transaction is indistinguishable from one that
// was never there — which is exactly what it is, from here.
func unknownService(name domain.Slug) string {
	return fmt.Sprintf("No service named %q; nothing to restart.", name)
}

// quoted renders a value the way every refusal in this package renders one.
func quoted(value string) string { return fmt.Sprintf("%q", value) }

// queryContext yields a context the dataset reads can use even when the tool
// was called outside ADK, where there is no agent context at all.
func queryContext(ctx agent.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
