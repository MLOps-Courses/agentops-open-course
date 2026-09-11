package tools

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/principal"
)

// TestApprovedRestartFlipsTheStatusAndAudits is the guarded write's happy path:
// the state changes and the evidence that it was allowed to change lands with
// it.
func TestApprovedRestartFlipsTheStatusAndAudits(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	if before := fixture.serviceStatus(t, inventoryService); before != domain.ServiceStatusDown {
		t.Fatalf("the seeded service status is %q, want %q", before, domain.ServiceStatusDown)
	}

	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(),
		approvedContext(t, "the service is down during the incident"),
		map[string]any{"name": inventoryService})

	if result.Error != "" {
		t.Fatalf("Error = %q, want none", result.Error)
	}
	contains(t, result.Result, "restarted", "Result")
	if after := fixture.serviceStatus(t, inventoryService); after != domain.ServiceStatusOperational {
		t.Errorf("service status = %q, want %q", after, domain.ServiceStatusOperational)
	}
	if result.Audit == nil {
		t.Fatal("Audit = nil, want the evidence row")
	}
	if result.Audit.Action != RestartServiceToolName {
		t.Errorf("Audit.Action = %q, want %q", result.Audit.Action, RestartServiceToolName)
	}
	if result.Audit.ApprovedBy != testApprover {
		t.Errorf("Audit.ApprovedBy = %q, want %q", result.Audit.ApprovedBy, testApprover)
	}
	if result.Audit.SchemaVersion != domain.CurrentAuditSchemaVersion {
		t.Errorf("Audit.SchemaVersion = %d, want %d", result.Audit.SchemaVersion, domain.CurrentAuditSchemaVersion)
	}
}

// TestReplayedApprovedRestartReturnsTheOriginalAuditAndChangesStateOnce is the
// course invariant this whole package exists to protect.
//
// A redelivered approval — a retried A2A request, a resumed session, a client
// that fired twice — must return the row the first delivery wrote and must not
// apply the mutation again. The service is moved to a *newer* state between the
// two deliveries, so a second UPDATE would be visible: if the replay silently
// re-ran, the service would be operational again and the test would say so.
func TestReplayedApprovedRestartReturnsTheOriginalAuditAndChangesStateOnce(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := approvedContext(t, "the service is down during the incident")

	first := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
		map[string]any{"name": inventoryService})
	if first.Audit == nil {
		t.Fatalf("Audit = nil, want the evidence row; error was %q", first.Error)
	}

	fixture.execFixture(t, "UPDATE services SET status = ? WHERE name = ?",
		string(domain.ServiceStatusDegraded), inventoryService)

	replay := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
		map[string]any{"name": inventoryService})

	if replay.Audit == nil {
		t.Fatalf("replay Audit = nil, want the original row; error was %q", replay.Error)
	}
	if *replay.Audit != *first.Audit {
		t.Errorf("replay Audit = %+v, want the original %+v", *replay.Audit, *first.Audit)
	}
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDegraded {
		t.Errorf("service status = %q, want %q: the replay re-applied the mutation",
			status, domain.ServiceStatusDegraded)
	}
	if count := fixture.auditCount(t); count != 1 {
		t.Errorf("audit rows = %d, want 1: the replay appended a second record", count)
	}
}

// TestApprovedResolutionMarksTheIncidentResolved is the resolution side of the
// happy path.
func TestApprovedResolutionMarksTheIncidentResolved(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	if before := fixture.incidentStatus(t, inventoryIncident); before != domain.IncidentStatusOpen {
		t.Fatalf("the seeded incident status is %q, want %q", before, domain.IncidentStatusOpen)
	}

	result := mustRun[ResolveIncidentResult](t, fixture.tools.ResolveIncident(),
		approvedContext(t, "the rollback restored service"),
		map[string]any{"incident_id": inventoryIncident})

	if result.Error != "" {
		t.Fatalf("Error = %q, want none", result.Error)
	}
	if after := fixture.incidentStatus(t, inventoryIncident); after != domain.IncidentStatusResolved {
		t.Errorf("incident status = %q, want %q", after, domain.IncidentStatusResolved)
	}
	incident, err := fixture.store.GetIncident(t.Context(), mustIncidentID(t, inventoryIncident))
	if err != nil || incident == nil {
		t.Fatalf("GetIncident() = %v, %v, want the resolved incident", incident, err)
	}
	if resolvedAt, ok := incident.ResolvedAt(); !ok || resolvedAt == "" {
		t.Errorf("ResolvedAt() = %q, %t, want a timestamp", resolvedAt, ok)
	}
	if result.Audit == nil || result.Audit.Action != ResolveIncidentToolName {
		t.Errorf("Audit = %+v, want an entry for %q", result.Audit, ResolveIncidentToolName)
	}
}

// TestResolvingAnAlreadyResolvedIncidentIsRefused proves the no-op is reported
// as one. Appending a second audit row claiming a resolution that did not
// happen would corrupt the trail with a plausible lie.
func TestResolvingAnAlreadyResolvedIncidentIsRefused(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	result := mustRun[ResolveIncidentResult](t, fixture.tools.ResolveIncident(),
		approvedContext(t, "confirming the resolved incident"),
		map[string]any{"incident_id": resolvedIncident})

	want := `Incident "` + resolvedIncident + `" is already resolved.`
	if result.Error != want {
		t.Errorf("Error = %q, want %q", result.Error, want)
	}
	if result.Audit != nil {
		t.Errorf("Audit = %+v, want no evidence for an action that did not happen", result.Audit)
	}
}

// TestGuardedWritesRefuseUnknownOrMalformedTargets is the fail-closed table: a
// target the agent invented, and a target shaped like a path traversal, are
// both refused before any approval is even considered.
func TestGuardedWritesRefuseUnknownOrMalformedTargets(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := approvedContext(t, "approved during the incident call")

	cases := []struct {
		name string
		tool tool.Tool
		args map[string]any
		want string
	}{
		{
			name: "unknown service",
			tool: fixture.tools.RestartService(),
			args: map[string]any{"name": "ghost"},
			want: `No service named "ghost"; nothing to restart.`,
		},
		{
			name: "traversal-shaped service",
			tool: fixture.tools.RestartService(),
			args: map[string]any{"name": "../../" + inventoryService},
			want: `Invalid service name "../../` + inventoryService + `"; expected lowercase kebab-case.`,
		},
		{
			name: "unknown incident",
			tool: fixture.tools.ResolveIncident(),
			args: map[string]any{"incident_id": "INC-999"},
			want: `No incident with id "INC-999".`,
		},
		{
			name: "traversal-shaped incident id",
			tool: fixture.tools.ResolveIncident(),
			args: map[string]any{"incident_id": "INC-../../passwd"},
			want: `Invalid incident id "INC-../../passwd"; expected an id like ` + inventoryIncident + `.`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			raw, err := run(t, testCase.tool, ctx, testCase.args)
			if err != nil {
				t.Fatalf("Run() error = %v, want a refusal", err)
			}
			if got, _ := raw["error"].(string); got != testCase.want {
				t.Errorf("error = %q, want %q", got, testCase.want)
			}
			// Nothing was written, so nothing published runtime state.
			fixture.assertNoRuntimeState(t)
		})
	}
}

// TestGuardedWritesSurviveLosingTheRaceToTheTransaction covers the window
// between the tool's own check and the transaction: the target can disappear,
// or be resolved by someone else, in between. The transaction reports that it
// changed nothing, and the tool must believe it.
func TestGuardedWritesSurviveLosingTheRaceToTheTransaction(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.store = func(real Store) Store {
			return stubStore{
				Store:   real,
				restart: func() (*domain.AuditEntry, error) { return nil, nil },
				resolve: func() (*domain.AuditEntry, error) { return nil, nil },
			}
		}
	})
	ctx := approvedContext(t, "approved during the incident call")

	t.Run("the service was removed", func(t *testing.T) {
		result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
			map[string]any{"name": inventoryService})

		want := `No service named "` + inventoryService + `"; nothing to restart.`
		if result.Error != want {
			t.Errorf("Error = %q, want %q", result.Error, want)
		}
	})

	t.Run("the incident was resolved by someone else", func(t *testing.T) {
		result := mustRun[ResolveIncidentResult](t, fixture.tools.ResolveIncident(), ctx,
			map[string]any{"incident_id": inventoryIncident})

		want := `Incident "` + inventoryIncident + `" is already resolved.`
		if result.Error != want {
			t.Errorf("Error = %q, want %q", result.Error, want)
		}
	})
}

// TestAuditRecordsTheApproverRationaleAndDecisionContext is what makes the
// trail auditable rather than merely present: who approved, why, and what was
// true at the moment the action ran.
func TestAuditRecordsTheApproverRationaleAndDecisionContext(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	rationale := "the service is hard down; approved in the incident call"

	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(),
		approvedContext(t, rationale), map[string]any{"name": inventoryService})

	if result.Audit == nil {
		t.Fatalf("Audit = nil, want the evidence row; error was %q", result.Error)
	}
	audit := *result.Audit
	if audit.Actor != auditActor {
		t.Errorf("Audit.Actor = %q, want %q", audit.Actor, auditActor)
	}
	if audit.ApprovedBy != testApprover {
		t.Errorf("Audit.ApprovedBy = %q, want %q", audit.ApprovedBy, testApprover)
	}
	if audit.SessionID != testSessionID {
		t.Errorf("Audit.SessionID = %q, want %q", audit.SessionID, testSessionID)
	}
	if audit.InvocationID != testInvocationID {
		t.Errorf("Audit.InvocationID = %q, want %q", audit.InvocationID, testInvocationID)
	}
	if audit.Rationale != rationale {
		t.Errorf("Audit.Rationale = %q, want %q verbatim", audit.Rationale, rationale)
	}
	// The context is read under the write lock, so it describes the rows the
	// write is about to change — including the incident that justified it.
	contains(t, audit.ContextSummary, inventoryService+" was "+string(domain.ServiceStatusDown), "Audit.ContextSummary")
	contains(t, audit.ContextSummary, inventoryIncident, "Audit.ContextSummary")
	if !strings.HasSuffix(audit.Ts, "Z") {
		t.Errorf("Audit.Ts = %q, want a UTC timestamp ending in Z", audit.Ts)
	}
}

// TestABareStringPayloadIsAValidApproval keeps the console path working: a
// human answering a yes/no prompt types a sentence, not a JSON object.
func TestABareStringPayloadIsAValidApproval(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{confirmed: true, payload: "fixed by the 09:40 rollback"}, identity{})

	result := mustRun[ResolveIncidentResult](t, fixture.tools.ResolveIncident(), ctx,
		map[string]any{"incident_id": inventoryIncident})

	if result.Audit == nil {
		t.Fatalf("Audit = nil, want the evidence row; error was %q", result.Error)
	}
	if result.Audit.Rationale != "fixed by the 09:40 rollback" {
		t.Errorf("Audit.Rationale = %q, want the bare string", result.Audit.Rationale)
	}
	contains(t, result.Audit.ContextSummary, string(domain.SeveritySev1), "Audit.ContextSummary")
}

// TestTheRationaleIsRedactedBeforeItReachesTheAuditTrail proves the redaction
// seam is on the persistence path, not beside it. The audit trail is
// append-only, so a credential that lands there can never be edited out.
func TestTheRationaleIsRedactedBeforeItReachesTheAuditTrail(t *testing.T) {
	t.Parallel()

	// A deliberately crude stand-in for the policy plane's redactor: the point of
	// this test is that whatever it returns is what gets persisted, not how it
	// decides. The redaction matrix itself belongs to the package that owns it.
	fixture := newFixture(t, func(o *options) {
		o.redact = func(text string) string {
			return strings.ReplaceAll(text, "super-secret-token", "<SECRET>")
		}
	})
	rationale := "approved with token=super-secret-token"

	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(),
		approvedContext(t, rationale), map[string]any{"name": inventoryService})

	if result.Audit == nil {
		t.Fatalf("Audit = nil, want the evidence row; error was %q", result.Error)
	}
	if strings.Contains(result.Audit.Rationale, "super-secret-token") {
		t.Errorf("Audit.Rationale = %q, want the secret gone", result.Audit.Rationale)
	}
	contains(t, result.Audit.Rationale, "token=<SECRET>", "Audit.Rationale")
	// The redactor saw the raw text exactly once, before the row was written.
	if len(fixture.redacted) != 1 || fixture.redacted[0] != rationale {
		t.Errorf("the redactor was handed %q, want exactly [%q]", fixture.redacted, rationale)
	}
	// And the persisted row, not just the returned one, carries the redaction.
	var persisted string
	if err := fixture.runtimeDB(t).QueryRowContext(t.Context(),
		"SELECT rationale FROM audit_log WHERE id = ?", result.Audit.ID).Scan(&persisted); err != nil {
		t.Fatalf("read the persisted rationale: %v", err)
	}
	if persisted != result.Audit.Rationale {
		t.Errorf("the stored rationale is %q, want the redacted %q", persisted, result.Audit.Rationale)
	}
}

// TestAnOverlongRationaleIsRefusedWithoutMutationOrAudit covers both length
// checks, and the second one is the interesting one: a redactor rewrites rather
// than deletes, so a rationale that fits before redaction can stop fitting
// after it.
func TestAnOverlongRationaleIsRefusedWithoutMutationOrAudit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		redact    Redactor
		rationale string
		want      string
	}{
		{
			name:      "too long as written",
			rationale: strings.Repeat("x", domain.MaxAuditRationaleLength+1),
			want:      "the approval rationale exceeds 500 characters",
		},
		{
			name:      "too long only after redaction",
			redact:    func(text string) string { return text + strings.Repeat("!", domain.MaxAuditRationaleLength) },
			rationale: "approved",
			want:      "the redacted approval rationale exceeds 500 characters",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fixture := newFixture(t, func(o *options) { o.redact = testCase.redact })

			result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(),
				approvedContext(t, testCase.rationale), map[string]any{"name": inventoryService})

			contains(t, result.Error, testCase.want, "Error")
			if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
				t.Errorf("service status = %q, want the refused action to have changed nothing", status)
			}
			fixture.assertNoRuntimeState(t)
		})
	}
}

// TestAnApprovalWithoutARationaleIsRefused enumerates the payload shapes that
// carry no justification. Approval is attributable change management, not a
// yes/no click, so every one of them fails closed.
func TestAnApprovalWithoutARationaleIsRefused(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	cases := []struct {
		payload any
		name    string
	}{
		{nil, "no payload at all"},
		{map[string]any{}, "an empty object"},
		{map[string]any{"rationale": "   "}, "a blank rationale"},
		{map[string]any{"rationale": nil}, "an explicitly null rationale"},
		{"", "an empty string"},
		{[]any{"approved"}, "a payload of the wrong shape"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, _ := toolContext(t, confirmation{confirmed: true, payload: testCase.payload}, identity{})

			result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
				map[string]any{"name": inventoryService})

			contains(t, result.Error, "rationale", "Error")
			// The refusal says how to succeed, or it just gets retried the same way.
			contains(t, result.Error, `{"rationale": "why this action is appropriate now"}`, "Error")
		})
	}
	// The resolution side refuses the same way. Both guarded writes validate the
	// approval themselves, so neither can be the one that forgot.
	t.Run("the resolution side refuses identically", func(t *testing.T) {
		ctx, _ := toolContext(t, confirmation{confirmed: true, payload: map[string]any{}}, identity{})

		result := mustRun[ResolveIncidentResult](t, fixture.tools.ResolveIncident(), ctx,
			map[string]any{"incident_id": inventoryIncident})

		contains(t, result.Error, "Refusing resolution of", "Error")
		contains(t, result.Error, "rationale", "Error")
	})

	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
		t.Errorf("service status = %q, want every refused approval to have changed nothing", status)
	}
	if status := fixture.incidentStatus(t, inventoryIncident); status != domain.IncidentStatusOpen {
		t.Errorf("incident status = %q, want every refused approval to have changed nothing", status)
	}
	fixture.assertNoRuntimeState(t)
}

// TestAConfirmedActionMustBeAttributable proves an approval nobody can be
// pinned to is worth nothing: without an approver, a session and an invocation,
// the audit row would record a decision with no decider — and the invocation is
// also the idempotency key, so losing it would break replay safety.
func TestAConfirmedActionMustBeAttributable(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	cases := []struct {
		name string
		who  identity
		want string
	}{
		{"no approver", identity{userID: pointer("")}, "approver identity"},
		{"no session", identity{sessionID: pointer("")}, "session id"},
		{"no invocation", identity{invocationID: pointer("")}, "invocation id"},
		{"blank approver", identity{userID: pointer("   ")}, "approver identity"},
		{
			name: "nothing at all",
			who:  identity{userID: pointer(""), sessionID: pointer(""), invocationID: pointer("")},
			want: "approver identity, session id, invocation id",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, _ := toolContext(t, confirmation{
				confirmed: true,
				payload:   map[string]any{"rationale": "approved"},
			}, testCase.who)

			result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
				map[string]any{"name": inventoryService})

			contains(t, result.Error, "the confirmed action is missing "+testCase.want, "Error")
			if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
				t.Errorf("service status = %q, want the refused action to have changed nothing", status)
			}
		})
	}
	fixture.assertNoRuntimeState(t)
}

// TestAConfirmedNetworkActionRequiresAnAuthenticatedPrincipal proves that
// confirmation and an identity-shaped framework string are not authorization.
// ADK assigns anonymous A2A calls a non-empty A2A_USER_* value, so the explicit
// network provenance must fail closed before mutation or audit.
func TestAConfirmedNetworkActionRequiresAnAuthenticatedPrincipal(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContextWithBase(t, principal.MarkNetwork(t.Context()), confirmation{
		confirmed: true,
		payload:   map[string]any{"rationale": "approved over the anonymous A2A session"},
	}, identity{userID: pointer("A2A_USER_context-123")})

	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
		map[string]any{"name": inventoryService})

	contains(t, result.Error, "network action requires an authenticated principal", "Error")
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
		t.Errorf("service status = %q, want the unauthenticated action to change nothing", status)
	}
	fixture.assertNoRuntimeState(t)
}

func TestAuthenticatedNetworkPrincipalMustOwnTheInvocation(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	authenticated, err := principal.NewAuthenticated("alice@example.test")
	if err != nil {
		t.Fatalf("NewAuthenticated() error = %v, want nil", err)
	}
	base := principal.BindNetwork(principal.MarkNetwork(t.Context()), authenticated)
	ctx, _ := toolContextWithBase(t, base, confirmation{
		confirmed: true,
		payload:   map[string]any{"rationale": "approved by a different invocation user"},
	}, identity{userID: pointer("mallory@example.test")})

	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
		map[string]any{"name": inventoryService})

	contains(t, result.Error, "authenticated principal does not own the invocation", "Error")
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
		t.Errorf("service status = %q, want the mismatched principal to change nothing", status)
	}
	fixture.assertNoRuntimeState(t)
}

func TestAuthenticatedNetworkPrincipalCanApproveOnce(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	authenticated, err := principal.NewAuthenticated("alice@example.test")
	if err != nil {
		t.Fatalf("NewAuthenticated() error = %v, want nil", err)
	}
	base := principal.BindNetwork(principal.MarkNetwork(t.Context()), authenticated)
	ctx, _ := toolContextWithBase(t, base, confirmation{
		confirmed: true,
		payload:   map[string]any{"rationale": "verified gateway subject approved the simulated restart"},
	}, identity{userID: pointer("alice@example.test")})

	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
		map[string]any{"name": inventoryService})

	if result.Error != "" {
		t.Fatalf("Error = %q, want none", result.Error)
	}
	if result.Audit == nil || result.Audit.ApprovedBy != authenticated.Subject() {
		t.Fatalf("Audit = %+v, want approved_by %q", result.Audit, authenticated.Subject())
	}
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusOperational {
		t.Errorf("service status = %q, want %q", status, domain.ServiceStatusOperational)
	}
}

// TestAGuardedWriteFailsClosedOutsideTheConfirmationFlow reaches the handler
// directly, with no agent context at all, which is the one call shape ADK
// cannot produce and a future refactor could.
//
// The ordering matters as much as the refusal: a malformed target is still
// reported as a malformed target, because the argument checks run before the
// approval is looked for.
func TestAGuardedWriteFailsClosedOutsideTheConfirmationFlow(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// A typed nil, so the call reads as "there is no context" rather than as an
	// untyped nil the compiler had to guess about.
	var noContext agent.Context

	t.Run("a well formed target still needs a confirmation flow", func(t *testing.T) {
		result, err := fixture.tools.runRestartService(noContext, RestartServiceArgs{Name: inventoryService})
		if err != nil {
			t.Fatalf("runRestartService() error = %v, want a refusal", err)
		}
		contains(t, result.Error, "confirmation flow", "Error")
		if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
			t.Errorf("service status = %q, want the refused action to have changed nothing", status)
		}
	})

	t.Run("a malformed target is judged first", func(t *testing.T) {
		result, err := fixture.tools.runResolveIncident(noContext, ResolveIncidentArgs{IncidentID: "INC-../../passwd"})
		if err != nil {
			t.Fatalf("runResolveIncident() error = %v, want a refusal", err)
		}
		contains(t, result.Error, "Invalid incident id", "Error")
	})

	fixture.assertNoRuntimeState(t)
}

// TestAnUnconfirmedCallPausesForApproval is the HITL pause itself.
//
// ADK signals the pause through an error rather than a result map — the Go
// framework's shape, where Python returned a dict — so the assertion is on the
// sentinel, not on a string. What matters is the same in both: the human was
// asked, and the function body never ran.
func TestAnUnconfirmedCallPausesForApproval(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	for _, testCase := range []struct {
		tool tool.Tool
		args map[string]any
		name string
	}{
		{fixture.tools.RestartService(), map[string]any{"name": inventoryService}, RestartServiceToolName},
		{fixture.tools.ResolveIncident(), map[string]any{"incident_id": inventoryIncident}, ResolveIncidentToolName},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, actions := toolContext(t, confirmation{absent: true}, identity{})

			_, err := run(t, testCase.tool, ctx, testCase.args)

			if !errors.Is(err, tool.ErrConfirmationRequired) {
				t.Fatalf("Run() error = %v, want it to wrap %v", err, tool.ErrConfirmationRequired)
			}
			pending, asked := actions.RequestedToolConfirmations["function-call-1"]
			if !asked {
				t.Fatal("no confirmation was requested, so no human was ever asked")
			}
			if pending.Confirmed {
				t.Error("the pending confirmation is already marked confirmed")
			}
			// Without this the agent loop would run on and summarize a pause as if
			// it were an answer.
			if !actions.SkipSummarization {
				t.Error("SkipSummarization = false, want the agent loop to stop at the pause")
			}
		})
	}
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
		t.Errorf("service status = %q, want nothing to have run before approval", status)
	}
	if status := fixture.incidentStatus(t, inventoryIncident); status != domain.IncidentStatusOpen {
		t.Errorf("incident status = %q, want nothing to have run before approval", status)
	}
	fixture.assertNoRuntimeState(t)
}

// TestARejectedConfirmationBlocksTheAction is the other half of the pause: the
// human said no, and no is enforced by the framework before the body runs.
func TestARejectedConfirmationBlocksTheAction(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{confirmed: false, payload: map[string]any{"rationale": "not approved"}},
		identity{})

	_, err := run(t, fixture.tools.RestartService(), ctx, map[string]any{"name": inventoryService})

	if !errors.Is(err, tool.ErrConfirmationRejected) {
		t.Fatalf("Run() error = %v, want it to wrap %v", err, tool.ErrConfirmationRejected)
	}
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
		t.Errorf("service status = %q, want a rejected action to have changed nothing", status)
	}
	fixture.assertNoRuntimeState(t)
}

// TestTheHandlerRechecksConfirmationItself proves the tool does not simply
// trust the framework. Reaching the handler with an unconfirmed context — which
// is what a direct call or a future in-process caller looks like — is refused
// by the handler's own check.
func TestTheHandlerRechecksConfirmationItself(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{confirmed: false, payload: map[string]any{"rationale": "not approved"}},
		identity{})

	result, err := fixture.tools.runRestartService(ctx, RestartServiceArgs{Name: inventoryService})
	if err != nil {
		t.Fatalf("runRestartService() error = %v, want a refusal", err)
	}
	contains(t, result.Error, "has not been confirmed", "Error")
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
		t.Errorf("service status = %q, want the refused action to have changed nothing", status)
	}
}

// TestTheKillSwitchFreezesEveryGuardedWrite covers AGENT_WRITES_DISABLED: an
// operator freezes mutations while an incident is contained, and reads keep
// working so the agent can still investigate.
func TestTheKillSwitchFreezesEveryGuardedWrite(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) { o.writesDisabled = true })
	ctx := approvedContext(t, "the runbook says so")

	for _, testCase := range []struct {
		tool tool.Tool
		args map[string]any
		name string
	}{
		{fixture.tools.RestartService(), map[string]any{"name": inventoryService}, RestartServiceToolName},
		{fixture.tools.ResolveIncident(), map[string]any{"incident_id": inventoryIncident}, ResolveIncidentToolName},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw, err := run(t, testCase.tool, ctx, testCase.args)
			if err != nil {
				t.Fatalf("Run() error = %v, want a refusal", err)
			}
			message, _ := raw["error"].(string)
			// The variable is named so an operator reading the transcript knows
			// which switch to clear.
			contains(t, message, "AGENT_WRITES_DISABLED", "error")
			contains(t, message, "reads still work", "error")
		})
	}

	t.Run("reads keep working while writes are frozen", func(t *testing.T) {
		readCtx, _ := toolContext(t, confirmation{absent: true}, identity{})
		result := mustRun[ListIncidentsResult](t, fixture.tools.ListIncidents(), readCtx, map[string]any{})
		if result.Count == nil || *result.Count < seededIncidents {
			t.Errorf("Count = %v, want the reads to be unaffected", result.Count)
		}
	})

	// Nothing was written, and nothing was audited: no runtime state exists at
	// all, which is a stronger claim than an unchanged row count.
	fixture.assertNoRuntimeState(t)
}

// TestGuardedWritesNeverRunThroughTheResilienceGuard is the deliberate
// asymmetry with the reads: retrying a write can cross an unknown commit
// boundary, so a write is never automatically retried even though the audit
// trail would deduplicate the redelivery.
func TestGuardedWritesNeverRunThroughTheResilienceGuard(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := approvedContext(t, "the runbook says so")

	if _, err := run(t, fixture.tools.RestartService(), ctx, map[string]any{"name": inventoryService}); err != nil {
		t.Fatalf("restart_service Run() error = %v, want nil", err)
	}
	if _, err := run(t, fixture.tools.ResolveIncident(), ctx,
		map[string]any{"incident_id": inventoryIncident}); err != nil {
		t.Fatalf("resolve_incident Run() error = %v, want nil", err)
	}

	if len(fixture.guard.names) != 0 {
		t.Errorf("the guard wrapped %v, want no guarded write to be retryable", fixture.guard.names)
	}
}

// TestTheAuditTrailIsAppendOnlyAfterAToolWrite proves the guarantee end to end:
// after a real approved write through the tool, the schema itself refuses to
// let the row be rewritten or removed.
func TestTheAuditTrailIsAppendOnlyAfterAToolWrite(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(),
		approvedContext(t, "the service is hard down"), map[string]any{"name": inventoryService})
	if result.Audit == nil {
		t.Fatalf("Audit = nil, want the evidence row; error was %q", result.Error)
	}

	for _, statement := range []string{
		"UPDATE audit_log SET actor = 'attacker'",
		"DELETE FROM audit_log",
	} {
		_, err := fixture.runtimeDB(t).ExecContext(t.Context(), statement)
		if err == nil {
			t.Fatalf("%q succeeded, want the append-only trigger to abort it", statement)
		}
		contains(t, err.Error(), "append-only", "the rejection")
	}
	if count := fixture.auditCount(t); count != 1 {
		t.Errorf("audit rows = %d, want the original row intact", count)
	}
}

// TestTheActionAndItsAuditRollBackTogether is the transactional guarantee: if
// the evidence cannot be recorded, the action does not happen either. A
// mutation without its audit row is exactly the state this design exists to
// make impossible.
func TestTheActionAndItsAuditRollBackTogether(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.prepareRuntime(t)
	fixture.execFixture(t,
		"CREATE TRIGGER reject_audit BEFORE INSERT ON audit_log "+
			"BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END")

	_, err := run(t, fixture.tools.RestartService(),
		approvedContext(t, "the service is hard down"), map[string]any{"name": inventoryService})

	if !errors.Is(err, data.ErrDataAccess) {
		t.Fatalf("Run() error = %v, want it to wrap %v", err, data.ErrDataAccess)
	}
	if status := fixture.serviceStatus(t, inventoryService); status != domain.ServiceStatusDown {
		t.Errorf("service status = %q, want the mutation rolled back with its audit row", status)
	}
	if count := fixture.auditCount(t); count != 0 {
		t.Errorf("audit rows = %d, want none", count)
	}
}

// TestNewRefusesAnIncompleteConfiguration keeps the two security seams from
// being silently absent. A nil guard or a nil redactor is a wiring bug, and the
// only safe moment to report one is startup.
func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	identityRedactor := func(text string) string { return text }
	noopGuard := func(ctx context.Context, _ string, call func(context.Context) error) error { return call(ctx) }
	store := data.New(data.Config{DataDir: t.TempDir(), StateDir: t.TempDir()})

	cases := []struct {
		name   string
		config Config
		want   string
	}{
		{"no store", Config{Guard: noopGuard, Redact: identityRedactor}, "Store is required"},
		{"no guard", Config{Store: store, Redact: identityRedactor}, "Guard is required"},
		{"no redactor", Config{Store: store, Guard: noopGuard}, "Redact is required"},
		{"nothing at all", Config{}, "Store is required"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			built, err := New(testCase.config)
			if built != nil {
				t.Errorf("New() = %v, want nil", built)
			}
			if !errors.Is(err, ErrIncompleteConfig) {
				t.Fatalf("New() error = %v, want it to wrap %v", err, ErrIncompleteConfig)
			}
			contains(t, err.Error(), testCase.want, "the error")
		})
	}

	t.Run("a complete configuration builds", func(t *testing.T) {
		t.Parallel()

		if _, err := New(Config{Store: store, Guard: noopGuard, Redact: identityRedactor}); err != nil {
			t.Errorf("New() error = %v, want nil", err)
		}
	})
}

// assertNoRuntimeState fails when anything published disposable runtime state.
// Only a write does, so its absence proves no mutation and no audit append ran.
func (f *fixture) assertNoRuntimeState(t *testing.T) {
	t.Helper()

	if _, err := os.Stat(f.store.RuntimePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("runtime state exists at %s (stat error %v), want no write to have happened",
			f.store.RuntimePath(), err)
	}
}

// prepareRuntime publishes and migrates runtime state without writing anything
// to it, so a fixture can reach the database before the first real write.
func (f *fixture) prepareRuntime(t *testing.T) {
	t.Helper()

	if _, err := f.store.PrepareRuntimeDatabase(t.Context()); err != nil {
		t.Fatalf("prepare the runtime database: %v", err)
	}
}

// execFixture runs one setup statement against the runtime database.
func (f *fixture) execFixture(t *testing.T, statement string, args ...any) {
	t.Helper()

	if _, err := f.runtimeDB(t).ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("fixture statement %q: %v", statement, err)
	}
}
