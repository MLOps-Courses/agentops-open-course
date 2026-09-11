package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// seededIncidents is the floor the committed dataset guarantees. The suite
// asserts a lower bound rather than an exact count so adding an incident to the
// seed is not a test change.
const seededIncidents = 6

// TestListIncidentsSurveysTheDataset covers the unfiltered listing, both
// filters, and the projection the model receives.
func TestListIncidentsSurveysTheDataset(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	t.Run("every incident by default", func(t *testing.T) {
		result := mustRun[ListIncidentsResult](t, fixture.tools.ListIncidents(), ctx, map[string]any{})

		if result.Error != "" {
			t.Fatalf("Error = %q, want none", result.Error)
		}
		if result.Count == nil {
			t.Fatal("Count = nil, want a count")
		}
		if *result.Count != len(result.Incidents) {
			t.Errorf("Count = %d, want %d, the number of incidents returned", *result.Count, len(result.Incidents))
		}
		if *result.Count < seededIncidents {
			t.Errorf("Count = %d, want at least the %d seeded incidents", *result.Count, seededIncidents)
		}
		// The projection is deliberately narrower than the record: a listing must
		// not carry the runbook, or an agent will plan a remediation from a survey.
		first := result.Incidents[0]
		for field, value := range map[string]string{
			"id":        first.ID,
			"service":   first.Service,
			"title":     first.Title,
			"severity":  first.Severity,
			"status":    first.Status,
			"opened_at": first.OpenedAt,
			"summary":   first.Summary,
		} {
			if value == "" {
				t.Errorf("incident[0].%s is empty, want a value", field)
			}
		}
	})

	t.Run("filtered by status", func(t *testing.T) {
		result := mustRun[ListIncidentsResult](t, fixture.tools.ListIncidents(), ctx,
			map[string]any{"status": string(domain.IncidentStatusOpen)})

		if result.Count == nil || *result.Count < 1 {
			t.Fatalf("Count = %v, want at least one open incident", result.Count)
		}
		for _, incident := range result.Incidents {
			if incident.Status != string(domain.IncidentStatusOpen) {
				t.Errorf("incident %s has status %q, want %q", incident.ID, incident.Status, domain.IncidentStatusOpen)
			}
		}
	})

	t.Run("filtered by service", func(t *testing.T) {
		result := mustRun[ListIncidentsResult](t, fixture.tools.ListIncidents(), ctx,
			map[string]any{"service": checkoutService})

		if result.Count == nil || *result.Count < 1 {
			t.Fatalf("Count = %v, want at least one incident", result.Count)
		}
		for _, incident := range result.Incidents {
			if incident.Service != checkoutService {
				t.Errorf("incident %s belongs to %q, want %q", incident.ID, incident.Service, checkoutService)
			}
		}
	})
}

// TestListIncidentsRefusesMalformedFilters proves a bad filter is answered with
// advice rather than an empty list — an empty result would read as "there are
// no such incidents", which is a different and wrong answer.
func TestListIncidentsRefusesMalformedFilters(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "unknown status",
			args: map[string]any{"status": "pending"},
			want: `Invalid incident status "pending". Expected one of: open, investigating, resolved.`,
		},
		{
			name: "blank status is a mistake, not an absent filter",
			args: map[string]any{"status": "   "},
			want: `Invalid incident status "   ". Expected one of: open, investigating, resolved.`,
		},
		{
			name: "traversal-shaped service",
			args: map[string]any{"service": "../" + checkoutService},
			want: `Invalid service name "../` + checkoutService + `"; expected lowercase kebab-case.`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, raw := runListing(t, fixture, ctx, testCase.args)

			if result.Error != testCase.want {
				t.Errorf("Error = %q, want %q", result.Error, testCase.want)
			}
			// A refusal carries nothing else: a "count" of zero beside an error
			// would invite the model to believe the query ran.
			for _, key := range []string{"count", "incidents"} {
				if _, present := raw[key]; present {
					t.Errorf("refusal carries %q = %v, want it omitted", key, raw[key])
				}
			}
		})
	}
}

// runListing runs list_incidents and returns both the typed result and the raw
// map, so a test can assert on the keys that are absent as well as present.
func runListing(t *testing.T, fixture *fixture, ctx agent.Context, args map[string]any) (ListIncidentsResult, map[string]any) {
	t.Helper()

	raw, err := run(t, fixture.tools.ListIncidents(), ctx, args)
	if err != nil {
		t.Fatalf("list_incidents Run() error = %v, want nil", err)
	}
	return decode[ListIncidentsResult](t, raw), raw
}

// TestGetIncidentReturnsTheFullRecordOrSaysWhyNot covers the one read whose
// result the agent is told to reuse verbatim in dependent calls.
func TestGetIncidentReturnsTheFullRecordOrSaysWhyNot(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	t.Run("known incident", func(t *testing.T) {
		result := mustRun[GetIncidentResult](t, fixture.tools.GetIncident(), ctx,
			map[string]any{"incident_id": checkoutIncident})

		if result.Incident == nil {
			t.Fatalf("Incident = nil, want the record; error was %q", result.Error)
		}
		if result.Incident.ID != checkoutIncident {
			t.Errorf("Incident.ID = %q, want %q", result.Incident.ID, checkoutIncident)
		}
		// The runbook is the field the listing withholds and this tool exists to
		// supply, so it is the one worth pinning.
		if result.Incident.Runbook != highLatencyBook {
			t.Errorf("Incident.Runbook = %q, want %q", result.Incident.Runbook, highLatencyBook)
		}
	})

	t.Run("case-insensitive and padded ids are normalized", func(t *testing.T) {
		result := mustRun[GetIncidentResult](t, fixture.tools.GetIncident(), ctx,
			map[string]any{"incident_id": "  " + strings.ToLower(checkoutIncident) + " "})

		if result.Incident == nil {
			t.Fatalf("Incident = nil, want the record; error was %q", result.Error)
		}
		if result.Incident.ID != checkoutIncident {
			t.Errorf("Incident.ID = %q, want %q", result.Incident.ID, checkoutIncident)
		}
	})

	cases := []struct {
		name       string
		incidentID string
		want       string
	}{
		{
			name:       "well formed but unknown",
			incidentID: "INC-999",
			want:       `No incident found with id "INC-999".`,
		},
		{
			name:       "traversal-shaped",
			incidentID: "../" + checkoutIncident,
			want:       `Invalid incident id "../` + checkoutIncident + `"; expected an id like ` + inventoryIncident + `.`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := mustRun[GetIncidentResult](t, fixture.tools.GetIncident(), ctx,
				map[string]any{"incident_id": testCase.incidentID})

			if result.Error != testCase.want {
				t.Errorf("Error = %q, want %q", result.Error, testCase.want)
			}
			if result.Incident != nil {
				t.Errorf("Incident = %+v, want nil beside a refusal", result.Incident)
			}
		})
	}
}

// TestGetServiceStatusReportsHealthAndUnresolvedWork proves the resolved
// incidents are filtered out: a service's open work is what decides whether to
// act, and a resolved row in that list would argue for acting again.
func TestGetServiceStatusReportsHealthAndUnresolvedWork(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	result := mustRun[GetServiceStatusResult](t, fixture.tools.GetServiceStatus(), ctx,
		map[string]any{"name": checkoutService})

	if result.Service == nil {
		t.Fatalf("Service = nil, want the record; error was %q", result.Error)
	}
	if result.Service.Status != string(domain.ServiceStatusDegraded) {
		t.Errorf("Service.Status = %q, want %q", result.Service.Status, domain.ServiceStatusDegraded)
	}
	if len(result.OpenIncidents) == 0 {
		t.Fatal("OpenIncidents is empty, want the service's unresolved work")
	}
	for _, incident := range result.OpenIncidents {
		if incident.Status == string(domain.IncidentStatusResolved) {
			t.Errorf("incident %s is resolved, want it filtered out of the open list", incident.ID)
		}
	}
}

// TestGetServiceStatusNamesTheServicesThatDoExist turns a dead end into a next
// step for the model.
func TestGetServiceStatusNamesTheServicesThatDoExist(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	t.Run("unknown service", func(t *testing.T) {
		result := mustRun[GetServiceStatusResult](t, fixture.tools.GetServiceStatus(), ctx,
			map[string]any{"name": "nope"})

		contains(t, result.Error, `No service named "nope". Known services: `, "Error")
		contains(t, result.Error, checkoutService, "Error")
		if result.Service != nil {
			t.Errorf("Service = %+v, want nil beside a refusal", result.Service)
		}
	})

	t.Run("traversal-shaped service", func(t *testing.T) {
		result := mustRun[GetServiceStatusResult](t, fixture.tools.GetServiceStatus(), ctx,
			map[string]any{"name": "../" + checkoutService})

		want := `Invalid service name "../` + checkoutService + `"; expected lowercase kebab-case.`
		if result.Error != want {
			t.Errorf("Error = %q, want %q", result.Error, want)
		}
	})
}

// TestSearchServiceLogsFiltersNewestFirst pins the ordering as well as the
// filter: a diagnostic read wants the freshest matching lines, and returning
// the oldest two would be a plausible-looking wrong answer.
func TestSearchServiceLogsFiltersNewestFirst(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	result := mustRun[SearchServiceLogsResult](t, fixture.tools.SearchServiceLogs(), ctx,
		map[string]any{"service": inventoryService, "query": "ERROR", "limit": 2})

	if result.Count == nil || *result.Count != 2 {
		t.Fatalf("Count = %v, want 2 (the limit)", result.Count)
	}
	if result.Service != inventoryService {
		t.Errorf("Service = %q, want the normalized %q", result.Service, inventoryService)
	}
	for _, line := range result.Lines {
		if !strings.Contains(line, "ERROR") {
			t.Errorf("line %q does not contain the query", line)
		}
	}
	contains(t, result.Lines[0], "stock lookup", "the newest matching line")
}

// TestSearchServiceLogsDefaultsAndBounds covers the three states of the limit
// argument, which are the reason it is a pointer.
func TestSearchServiceLogsDefaultsAndBounds(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	t.Run("an omitted limit falls back to the default", func(t *testing.T) {
		result := mustRun[SearchServiceLogsResult](t, fixture.tools.SearchServiceLogs(), ctx,
			map[string]any{"service": inventoryService})

		if result.Error != "" {
			t.Fatalf("Error = %q, want none", result.Error)
		}
		if result.Count == nil || *result.Count > defaultLogSearchLimit {
			t.Errorf("Count = %v, want at most the default %d", result.Count, defaultLogSearchLimit)
		}
	})

	cases := []struct {
		name  string
		limit int
	}{
		{"zero is refused rather than answered with nothing", 0},
		{"negative is refused", -1},
		{"above the ceiling is refused", maxLogSearchLimit + 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := mustRun[SearchServiceLogsResult](t, fixture.tools.SearchServiceLogs(), ctx,
				map[string]any{"service": inventoryService, "limit": testCase.limit})

			want := "Log search limit must be between 1 and 100."
			if result.Error != want {
				t.Errorf("Error = %q, want %q", result.Error, want)
			}
			if result.Lines != nil {
				t.Errorf("Lines = %v, want nothing beside a refusal", result.Lines)
			}
		})
	}
}

// TestSearchServiceLogsRefusesWhatItCannotRead covers the two refusals, and
// pins the ordering between them: the slug is judged before the bound, so a
// caller that got both wrong hears about the one that decides whether there is
// anything to read at all.
func TestSearchServiceLogsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	t.Run("traversal-shaped service", func(t *testing.T) {
		result := mustRun[SearchServiceLogsResult](t, fixture.tools.SearchServiceLogs(), ctx,
			map[string]any{"service": "../../etc", "limit": 0})

		want := `Invalid service name "../../etc"; expected lowercase kebab-case.`
		if result.Error != want {
			t.Errorf("Error = %q, want the slug refusal %q to win over the limit refusal", result.Error, want)
		}
	})

	t.Run("a real service with no sample log", func(t *testing.T) {
		result := mustRun[SearchServiceLogsResult](t, fixture.tools.SearchServiceLogs(), ctx,
			map[string]any{"service": paymentsService})

		contains(t, result.Error, `No logs for service "`+paymentsService+`". Available logs: `, "Error")
		// The refusal names what can be read instead, so the model has somewhere
		// to go next.
		contains(t, result.Error, inventoryService, "Error")
	})
}

// TestSearchServiceLogsHonorsTheGuardsBudget pins the narrow promise
// AGENT_TOOL_TIMEOUT_S makes for this one tool. Its corpus is read with
// os.ReadFile, which cannot be called back once it is blocked, so the deadline
// is enforced at the two points where it still decides something: before each of
// the two filesystem reads. Without those checks the configured deadline would
// be inert here — the tool would run to completion on a budget that is already
// spent — while the doc comment claimed the Guard bounded it.
func TestSearchServiceLogsHonorsTheGuardsBudget(t *testing.T) {
	t.Parallel()

	t.Run("an expired budget stops the log read from starting", func(t *testing.T) {
		t.Parallel()

		reads := 0
		fixture := newFixture(t, func(opts *options) {
			opts.store = func(real Store) Store {
				return stubStore{Store: real, readLogs: func() ([]string, bool, error) {
					reads++
					return nil, false, nil
				}}
			}
		})
		fixture.guard.wrap = func(ctx context.Context) context.Context {
			expired, cancel := context.WithCancel(ctx)
			cancel()
			return expired
		}
		ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

		_, err := run(t, fixture.tools.SearchServiceLogs(), ctx, map[string]any{"service": inventoryService})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to wrap %v", err, context.Canceled)
		}
		if reads != 0 {
			t.Errorf("the store was read %d times, want the spent budget to stop the read", reads)
		}
	})

	t.Run("a budget spent during the log read stops the listing that follows", func(t *testing.T) {
		t.Parallel()

		attempt, spend := context.WithCancel(context.Background())
		t.Cleanup(spend)
		fixture := newFixture(t, func(opts *options) {
			opts.store = func(real Store) Store {
				// The budget runs out while the uncancelable log read is in flight,
				// which is what the second checkpoint exists for: composing the "no
				// logs for this service" refusal costs another filesystem read, and
				// nobody is waiting for the answer anymore.
				return stubStore{Store: real, readLogs: func() ([]string, bool, error) {
					spend()
					return nil, false, nil
				}}
			}
		})
		fixture.guard.wrap = func(context.Context) context.Context { return attempt }
		ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

		_, err := run(t, fixture.tools.SearchServiceLogs(), ctx, map[string]any{"service": inventoryService})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to wrap %v", err, context.Canceled)
		}
	})
}

// TestReadToolsComposeTheDefaultSurface pins the registration order, which the
// evalsets and the course prose both depend on, and proves each named accessor
// returns the same tool the slice carries.
func TestReadToolsComposeTheDefaultSurface(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	tools := fixture.tools

	want := []string{
		ListIncidentsToolName,
		GetIncidentToolName,
		GetServiceStatusToolName,
		SearchServiceLogsToolName,
	}
	got := make([]string, 0, len(tools.ReadTools()))
	for _, read := range tools.ReadTools() {
		got = append(got, read.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReadTools() = %v, want %v", got, want)
	}

	named := []any{tools.ListIncidents(), tools.GetIncident(), tools.GetServiceStatus(), tools.SearchServiceLogs()}
	for index, read := range tools.ReadTools() {
		if named[index] != any(read) {
			t.Errorf("ReadTools()[%d] is not the tool the named accessor returns", index)
		}
	}

	// A caller that mutates the returned slice must not be able to change what
	// the next caller sees.
	tools.ReadTools()[0] = nil
	if tools.ReadTools()[0] == nil {
		t.Error("ReadTools() returns a shared slice; a caller mutated the registry")
	}
}

// TestEveryReadRunsThroughTheResilienceGuard is the seam Python expressed as a
// decorator: reads are wrapped because they are idempotent.
func TestEveryReadRunsThroughTheResilienceGuard(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	for _, testCase := range []struct {
		args map[string]any
		tool string
	}{
		{map[string]any{}, ListIncidentsToolName},
		{map[string]any{"incident_id": checkoutIncident}, GetIncidentToolName},
		{map[string]any{"name": checkoutService}, GetServiceStatusToolName},
		{map[string]any{"service": inventoryService}, SearchServiceLogsToolName},
	} {
		fixture.guard.names = nil
		read := toolNamed(t, fixture, testCase.tool)
		if _, err := run(t, read, ctx, testCase.args); err != nil {
			t.Fatalf("%s Run() error = %v, want nil", testCase.tool, err)
		}
		if !slices.Equal(fixture.guard.names, []string{testCase.tool}) {
			t.Errorf("guard saw %v, want exactly [%s]", fixture.guard.names, testCase.tool)
		}
	}
}

// TestAGuardFailureSurfacesAsAToolError proves the seam is load-bearing in both
// directions: a circuit that is open, or a deadline that expired, must reach the
// policy plane as an error rather than as a plausible empty answer.
func TestAGuardFailureSurfacesAsAToolError(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.guard.before = func(string) error { return errUnavailable }
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	_, err := run(t, fixture.tools.ListIncidents(), ctx, map[string]any{})
	if !errors.Is(err, errUnavailable) {
		t.Errorf("Run() error = %v, want it to wrap %v", err, errUnavailable)
	}
}

// TestAGuardRetryReRunsTheRead proves the read really is re-executed, not
// merely re-wrapped — which is what makes wrapping only the idempotent tools a
// meaningful distinction.
func TestAGuardRetryReRunsTheRead(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.guard.repeat = 2
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	result := mustRun[ListIncidentsResult](t, fixture.tools.ListIncidents(), ctx, map[string]any{})
	if result.Count == nil || *result.Count < seededIncidents {
		t.Fatalf("Count = %v, want the listing to survive being re-run", result.Count)
	}
}
