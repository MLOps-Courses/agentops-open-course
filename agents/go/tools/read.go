package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// The bounds of one log search. The lower bound is what makes limit=0 a
// refusal rather than an empty answer: zero lines is never what a diagnostic
// read meant to ask for, and answering it silently would hide the mistake.
const (
	minLogSearchLimit     = 1
	maxLogSearchLimit     = 100
	defaultLogSearchLimit = 20
)

// logExtension is the suffix of a sample log file in the dataset.
const logExtension = ".log"

// The tool descriptions the model reads. They carry the prose of the Python
// Google-style docstrings, minus the per-argument text, which lives on the
// schema properties instead — see [describeProperties].
const (
	listIncidentsDescription = "List incidents on the platform, most recent first.\n\n" +
		"Returns a dict with \"count\" and an \"incidents\" list " +
		"(id, service, title, severity, status)."

	getIncidentDescription = "Get the full details of one incident by its id.\n\n" +
		"Returns {\"incident\": {...}} with the record (including \"runbook\" and \"summary\"), " +
		"or an \"error\" if unknown. Wait for this result, then reuse its returned " +
		"\"service\" and \"runbook\" values verbatim in dependent tool calls."

	getServiceStatusDescription = "Get the current status of a service and its open incidents.\n\n" +
		"Returns {\"service\": {...}, \"open_incidents\": [...]}, or an \"error\" if unknown."

	searchServiceLogsDescription = "Search deterministic sample logs for one service, " +
		"newest matching lines first.\n\n" +
		"Returns a dict with the normalized service, match count, and matching log lines; or an error."
)

// IncidentSummary is the incident projection [Tools.ListIncidents] returns.
//
// It deliberately omits runbook and resolved_at: a listing is a survey, and
// the agent is expected to open the one incident it cares about with
// get_incident rather than plan a remediation from a list row.
type IncidentSummary struct {
	ID       string `json:"id"`
	Service  string `json:"service"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	OpenedAt string `json:"opened_at"`
	Summary  string `json:"summary"`
}

// OpenIncident is the smaller projection [Tools.GetServiceStatus] returns
// alongside a service: enough to see what is wrong, not enough to act on.
type OpenIncident struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
}

// ListIncidentsArgs are the arguments of the list_incidents tool. Both filters
// are optional, and an empty one means "no filter" rather than "match empty".
type ListIncidentsArgs struct {
	Status  string `json:"status,omitempty"`
	Service string `json:"service,omitempty"`
}

// ListIncidentsResult is what list_incidents returns.
//
// Exactly one shape reaches the model: either Error alone, or Count with
// Incidents. Every field is `omitzero`, so a refusal carries no stray zero
// values — and that is why Count is a pointer, because a successful listing
// that matched nothing must still report "count": 0 rather than omit it.
//
// Field order here, and in every result and argument struct below, is a
// memory-layout concern only: Go lays a struct out in declaration order and go
// vet's fieldalignment check wants the pointer-dense fields first. The contract
// is the set of JSON keys, and JSON objects are unordered.
type ListIncidentsResult struct {
	Count     *int              `json:"count,omitzero"`
	Error     string            `json:"error,omitzero"`
	Incidents []IncidentSummary `json:"incidents,omitzero"`
}

// GetIncidentArgs are the arguments of the get_incident tool.
type GetIncidentArgs struct {
	IncidentID string `json:"incident_id"`
}

// GetIncidentResult is what get_incident returns: the full record, or an error.
type GetIncidentResult struct {
	Incident *domain.IncidentRow `json:"incident,omitzero"`
	Error    string              `json:"error,omitzero"`
}

// GetServiceStatusArgs are the arguments of the get_service_status tool.
type GetServiceStatusArgs struct {
	Name string `json:"name"`
}

// GetServiceStatusResult is what get_service_status returns.
type GetServiceStatusResult struct {
	Service       *domain.ServiceRow `json:"service,omitzero"`
	Error         string             `json:"error,omitzero"`
	OpenIncidents []OpenIncident     `json:"open_incidents,omitzero"`
}

// SearchServiceLogsArgs are the arguments of the search_service_logs tool.
//
// Limit is a pointer because its three states are distinct and each has its own
// behavior: absent means the default, a value in range is honored, and a
// value out of range — zero especially — is refused. Collapsing "absent" onto
// zero would turn that refusal into a silent default.
type SearchServiceLogsArgs struct {
	Limit   *int   `json:"limit,omitempty"`
	Service string `json:"service"`
	Query   string `json:"query,omitempty"`
}

// SearchServiceLogsResult is what search_service_logs returns.
type SearchServiceLogsResult struct {
	Count   *int     `json:"count,omitzero"`
	Error   string   `json:"error,omitzero"`
	Service string   `json:"service,omitzero"`
	Lines   []string `json:"lines,omitzero"`
}

// runListIncidents is the list_incidents handler.
//
// Each read follows the same two-layer shape: the handler ADK calls puts the
// work through the [Guard], and the inner method does that work against a
// context the Guard controls, so a deadline it installs actually reaches the
// database.
func (t *Tools) runListIncidents(ctx agent.Context, args ListIncidentsArgs) (ListIncidentsResult, error) {
	var result ListIncidentsResult
	err := t.guard(ctx, ListIncidentsToolName, func(ctx context.Context) error {
		var err error
		result, err = t.readIncidents(ctx, args)
		return err
	})
	return result, err
}

func (t *Tools) readIncidents(ctx context.Context, args ListIncidentsArgs) (ListIncidentsResult, error) {
	var filter data.IncidentFilter
	// A blank-but-present status is a mistake, not an absent filter: Python
	// treated any non-empty string as a filter to parse, and "   " parses to
	// nothing. Keeping that means the model is told it typed something wrong.
	if args.Status != "" {
		status, err := domain.ParseIncidentStatus(strings.ToLower(strings.TrimSpace(args.Status)))
		if err != nil {
			return ListIncidentsResult{Error: fmt.Sprintf(
				"Invalid incident status %q. Expected one of: %s.", args.Status, incidentStatusList(),
			)}, nil
		}
		filter.Status = status
	}
	if args.Service != "" {
		service, err := domain.NormalizeSlug(args.Service)
		if err != nil {
			return ListIncidentsResult{Error: invalidServiceName(args.Service)}, nil
		}
		filter.Service = service
	}

	incidents, err := t.store.ListIncidents(ctx, filter)
	if err != nil {
		return ListIncidentsResult{}, fmt.Errorf("list incidents: %w", err)
	}
	// Allocated non-nil so an empty listing serializes as [] rather than being
	// omitted: "nothing matched your filter" and "this tool told you nothing"
	// are different answers.
	summaries := make([]IncidentSummary, 0, len(incidents))
	for _, incident := range incidents {
		summaries = append(summaries, IncidentSummary{
			ID:       string(incident.ID()),
			Service:  string(incident.Service()),
			Title:    incident.Title(),
			Severity: string(incident.Severity()),
			Status:   string(incident.Status()),
			OpenedAt: incident.OpenedAt(),
			Summary:  incident.Summary(),
		})
	}
	count := len(summaries)
	return ListIncidentsResult{Count: &count, Incidents: summaries}, nil
}

// runGetIncident is the get_incident handler.
// --8<-- [start:get-incident]
func (t *Tools) runGetIncident(ctx agent.Context, args GetIncidentArgs) (GetIncidentResult, error) {
	var result GetIncidentResult
	err := t.guard(ctx, GetIncidentToolName, func(ctx context.Context) error {
		var err error
		result, err = t.readIncident(ctx, args)
		return err
	})
	return result, err
}

// --8<-- [end:get-incident]

func (t *Tools) readIncident(ctx context.Context, args GetIncidentArgs) (GetIncidentResult, error) {
	incidentID, err := domain.NormalizeIncidentID(args.IncidentID)
	if err != nil {
		return GetIncidentResult{Error: invalidIncidentID(args.IncidentID)}, nil
	}
	incident, err := t.store.GetIncident(ctx, incidentID)
	if err != nil {
		return GetIncidentResult{}, fmt.Errorf("get incident %s: %w", incidentID, err)
	}
	if incident == nil {
		return GetIncidentResult{Error: fmt.Sprintf("No incident found with id %q.", incidentID)}, nil
	}
	row := incident.Row()
	return GetIncidentResult{Incident: &row}, nil
}

// runGetServiceStatus is the get_service_status handler.
func (t *Tools) runGetServiceStatus(ctx agent.Context, args GetServiceStatusArgs) (GetServiceStatusResult, error) {
	var result GetServiceStatusResult
	err := t.guard(ctx, GetServiceStatusToolName, func(ctx context.Context) error {
		var err error
		result, err = t.readServiceStatus(ctx, args)
		return err
	})
	return result, err
}

func (t *Tools) readServiceStatus(ctx context.Context, args GetServiceStatusArgs) (GetServiceStatusResult, error) {
	name, err := domain.NormalizeSlug(args.Name)
	if err != nil {
		return GetServiceStatusResult{Error: invalidServiceName(args.Name)}, nil
	}
	service, err := t.store.GetService(ctx, name)
	if err != nil {
		return GetServiceStatusResult{}, fmt.Errorf("get service %s: %w", name, err)
	}
	if service == nil {
		// Naming the services that do exist turns a dead end into a next step:
		// the model mistyped a name, and the answer tells it which ones are real.
		services, listErr := t.store.ListServices(ctx)
		if listErr != nil {
			return GetServiceStatusResult{}, fmt.Errorf("list the known services: %w", listErr)
		}
		known := make([]string, 0, len(services))
		for _, row := range services {
			known = append(known, string(row.Name()))
		}
		return GetServiceStatusResult{Error: fmt.Sprintf(
			"No service named %q. Known services: %s.", name, strings.Join(known, ", "),
		)}, nil
	}

	incidents, err := t.store.ListIncidents(ctx, data.IncidentFilter{Service: name})
	if err != nil {
		return GetServiceStatusResult{}, fmt.Errorf("list the incidents of service %s: %w", name, err)
	}
	open := make([]OpenIncident, 0, len(incidents))
	for _, incident := range incidents {
		if incident.Status() == domain.IncidentStatusResolved {
			continue
		}
		open = append(open, OpenIncident{
			ID:       string(incident.ID()),
			Title:    incident.Title(),
			Severity: string(incident.Severity()),
			Status:   string(incident.Status()),
		})
	}
	row := service.Row()
	return GetServiceStatusResult{Service: &row, OpenIncidents: open}, nil
}

// runSearchServiceLogs is the search_service_logs handler.
func (t *Tools) runSearchServiceLogs(ctx agent.Context, args SearchServiceLogsArgs) (SearchServiceLogsResult, error) {
	var result SearchServiceLogsResult
	err := t.guard(ctx, SearchServiceLogsToolName, func(ctx context.Context) error {
		var err error
		result, err = t.readServiceLogs(ctx, args)
		return err
	})
	return result, err
}

// readServiceLogs honors the Guard's per-attempt context at every point it can,
// which is a narrower promise than the database reads make — and the difference
// is worth stating rather than glossing over.
//
// Those reads hand ctx to the driver, so AGENT_TOOL_TIMEOUT_S aborts a query
// already in flight. The log corpus comes from the filesystem instead, and
// [Store.ReadServiceLogs] wraps os.ReadFile, which takes no context: the
// deadline therefore decides whether each filesystem read is *started*, but
// cannot preempt one already blocked on a stalled mount. Moving the read into a
// goroutine would not change that — the goroutine and its buffer would outlive
// every attempt and leak for as long as the mount hangs — so the checkpoints
// below are the real bound, and the limit is documented rather than implied
// away.
func (t *Tools) readServiceLogs(ctx context.Context, args SearchServiceLogsArgs) (SearchServiceLogsResult, error) {
	service, err := domain.NormalizeSlug(args.Service)
	if err != nil {
		return SearchServiceLogsResult{Error: invalidServiceName(args.Service)}, nil
	}
	// The bound is checked after the slug, exactly as in Python: a caller who got
	// both wrong hears about the name first, because that is the one that decides
	// whether there is anything to read at all.
	limit := defaultLogSearchLimit
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < minLogSearchLimit || limit > maxLogSearchLimit {
		return SearchServiceLogsResult{Error: fmt.Sprintf(
			"Log search limit must be between %d and %d.", minLogSearchLimit, maxLogSearchLimit,
		)}, nil
	}

	// The last point at which an exhausted budget can still stop the work rather
	// than pay for it: once the read below is under way, nothing can call it back.
	if expired := ctx.Err(); expired != nil {
		return SearchServiceLogsResult{}, fmt.Errorf("read the logs of service %s: %w", service, expired)
	}
	lines, found, err := t.store.ReadServiceLogs(string(service))
	if err != nil {
		return SearchServiceLogsResult{}, fmt.Errorf("read the logs of service %s: %w", service, err)
	}
	if !found {
		available, err := t.availableLogs(ctx)
		if err != nil {
			return SearchServiceLogsResult{}, err
		}
		return SearchServiceLogsResult{Error: fmt.Sprintf(
			"No logs for service %q. Available logs: %s.", service, strings.Join(available, ", "),
		)}, nil
	}

	needle := strings.ToLower(strings.TrimSpace(args.Query))
	matches := make([]string, 0, min(limit, len(lines)))
	// Newest first, so the walk runs backwards and stops as soon as the limit is
	// filled — the freshest matching lines are the diagnostic ones.
	for index := len(lines) - 1; index >= 0 && len(matches) < limit; index-- {
		if needle == "" || strings.Contains(strings.ToLower(lines[index]), needle) {
			matches = append(matches, lines[index])
		}
	}
	count := len(matches)
	return SearchServiceLogsResult{Service: string(service), Count: &count, Lines: matches}, nil
}

// availableLogs lists the services that have a sample log, sorted.
//
// The listing lives here rather than in the data package because it exists only
// to compose a message for the model. os.ReadDir sorts, so the sentence is the
// same on every host — Python's glob returned directory order, which was not.
func (t *Tools) availableLogs(ctx context.Context) ([]string, error) {
	directory := t.store.LogsDir()
	// The second filesystem read of the tool, and the same checkpoint as before
	// it: os.ReadDir cannot be canceled either, so a budget that expired while
	// the log read ran has to be caught here rather than during the listing.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list the sample logs in %s: %w", directory, err)
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		// No corpus at all is a legal, if unhelpful, state: the refusal above
		// still tells the model this service has no logs, with an empty list.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the sample logs in %s: %w", directory, err)
	}
	available := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, logExtension) {
			available = append(available, strings.TrimSuffix(name, logExtension))
		}
	}
	return available, nil
}

// incidentStatusList renders the accepted statuses in declaration order, which
// is the order the refusal and the argument description both present them.
func incidentStatusList() string {
	statuses := []domain.IncidentStatus{
		domain.IncidentStatusOpen,
		domain.IncidentStatusInvestigating,
		domain.IncidentStatusResolved,
	}
	rendered := make([]string, len(statuses))
	for index, status := range statuses {
		rendered[index] = string(status)
	}
	return strings.Join(rendered, ", ")
}

// invalidServiceName and invalidIncidentID are the two refusals the read and
// the write side share verbatim. Sharing them is deliberate: a model that
// learns the shape of a refusal from one tool should recognize it from another.
func invalidServiceName(name string) string {
	return fmt.Sprintf("Invalid service name %q; expected lowercase kebab-case.", name)
}

func invalidIncidentID(incidentID string) string {
	return fmt.Sprintf(
		"Invalid incident id %q; expected an id like %s.",
		incidentID, domain.Reference().Incidents.InventoryDown,
	)
}
