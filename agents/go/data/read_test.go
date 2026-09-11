package data

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// incidentIDs renders a listing as its identifiers, which is what the ordering
// and filtering assertions are actually about.
func incidentIDs(incidents []domain.Incident) []string {
	ids := make([]string, len(incidents))
	for i, incident := range incidents {
		ids[i] = string(incident.ID())
	}
	return ids
}

func TestListIncidentsFiltersAndOrders(t *testing.T) {
	t.Parallel()
	reference := domain.Reference()
	tests := []struct {
		name   string
		filter IncidentFilter
		want   []string
	}{
		{
			// The zero filter is Python's two None arguments: everything,
			// newest first.
			name: "no filter returns every incident newest first",
			want: []string{
				reference.Incidents.GatewayMemory, reference.Incidents.CheckoutCascade,
				reference.Incidents.DatabaseCascade, reference.Incidents.CacheMemory,
				reference.Incidents.InventoryDown, reference.Incidents.CheckoutLatency,
				reference.Incidents.SearchLatency, reference.Incidents.PaymentsErrors,
				reference.Incidents.AuthErrors, reference.Incidents.CheckoutDisk,
			},
		},
		{
			name:   "by status",
			filter: IncidentFilter{Status: domain.IncidentStatusOpen},
			want: []string{
				reference.Incidents.GatewayMemory, reference.Incidents.InventoryDown,
				reference.Incidents.SearchLatency,
			},
		},
		{
			name:   "by service",
			filter: IncidentFilter{Service: checkoutService},
			want: []string{
				reference.Incidents.CheckoutCascade, reference.Incidents.CheckoutLatency,
				reference.Incidents.CheckoutDisk,
			},
		},
		{
			name: "by status and service together",
			filter: IncidentFilter{
				Status:  domain.IncidentStatusInvestigating,
				Service: checkoutService,
			},
			want: []string{reference.Incidents.CheckoutCascade, reference.Incidents.CheckoutLatency},
		},
		{
			name:   "a filter that matches nothing",
			filter: IncidentFilter{Service: mustSlug("nonexistent")},
			want:   nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			incidents, err := store.ListIncidents(t.Context(), test.filter)
			if err != nil {
				t.Fatalf("list incidents: %v", err)
			}
			if got := incidentIDs(incidents); !slices.Equal(got, test.want) {
				t.Errorf("incidents = %v, want %v", got, test.want)
			}
		})
	}
}

func TestReadsFallBackToTheSeedWithoutPublishingState(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	incidents, err := store.ListIncidents(t.Context(), IncidentFilter{})
	if err != nil {
		t.Fatalf("list incidents with no state published: %v", err)
	}
	if len(incidents) == 0 {
		t.Fatal("the seed fallback returned no incidents")
	}
	// This is the property that lets a learner run every read tool on a fresh
	// checkout: reads never publish, so the state directory is still absent.
	if _, err := os.Stat(store.StateDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a read published state: stat error %v", err)
	}
}

func TestReadsSeeRuntimeStateOnceItIsPublished(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	execFixture(t, openFixture(t, path),
		"UPDATE services SET status = 'down' WHERE name = ?", string(checkoutService))

	service, err := store.GetService(t.Context(), checkoutService)
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if service == nil {
		t.Fatal("the seeded service disappeared from runtime state")
	}
	if service.Status() != domain.ServiceStatusDown {
		t.Errorf("status = %q, want %q: the read did not prefer runtime state",
			service.Status(), domain.ServiceStatusDown)
	}
}

func TestGetIncidentReturnsNilForAnUnknownID(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	incident, err := store.GetIncident(t.Context(), inventoryIncident)
	if err != nil {
		t.Fatalf("get a known incident: %v", err)
	}
	if incident == nil {
		t.Fatal("the seeded incident was not found")
	}
	if incident.Service() != inventoryService {
		t.Errorf("service = %q, want %q", incident.Service(), inventoryService)
	}
	if incident.Severity() != domain.SeveritySev1 {
		t.Errorf("severity = %q, want %q", incident.Severity(), domain.SeveritySev1)
	}
	if _, resolved := incident.ResolvedAt(); resolved {
		t.Error("an open incident carries a resolution timestamp")
	}

	// A well-formed but unknown identifier is "no such row", not an error: the
	// caller decides how to phrase that for the model.
	missing, err := store.GetIncident(t.Context(), mustIncidentID("INC-999"))
	if err != nil {
		t.Fatalf("get an unknown incident: %v", err)
	}
	if missing != nil {
		t.Errorf("an unknown incident returned %+v, want nil", missing)
	}
}

func TestResolvedIncidentsCarryTheirResolutionTimestamp(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	incident, err := store.GetIncident(t.Context(), resolvedIncident)
	if err != nil {
		t.Fatalf("get the resolved incident: %v", err)
	}
	if incident == nil {
		t.Fatal("the seeded resolved incident was not found")
	}
	// NULL and "set" are distinguishable, which is the whole reason the row
	// type carries a pointer for this one column.
	resolvedAt, ok := incident.ResolvedAt()
	if !ok || resolvedAt == "" {
		t.Errorf("resolved_at = %q (set %t), want a timestamp", resolvedAt, ok)
	}
}

func TestListAndGetServices(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	services, err := store.ListServices(t.Context())
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	names := make([]string, len(services))
	for i, service := range services {
		names[i] = string(service.Name())
	}
	want := domain.Reference().Services.Values()
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("services = %v, want %v sorted by name", names, want)
	}

	service, err := store.GetService(t.Context(), checkoutService)
	if err != nil {
		t.Fatalf("get a known service: %v", err)
	}
	if service == nil || service.Name() != checkoutService {
		t.Fatalf("get service returned %+v, want %q", service, checkoutService)
	}
	missing, err := store.GetService(t.Context(), mustSlug("nonexistent"))
	if err != nil {
		t.Fatalf("get an unknown service: %v", err)
	}
	if missing != nil {
		t.Errorf("an unknown service returned %+v, want nil", missing)
	}
}

func TestReadsRejectARowTheDomainCannotRepresent(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	// The CHECK constraint is suspended so the row can exist at all. A snapshot
	// from a foreign writer could carry exactly this, and the read boundary has
	// to refuse it rather than hand a nonsense status to the model.
	fixture := openFixture(t, path)
	execFixture(t, fixture, "PRAGMA ignore_check_constraints = ON")
	execFixture(t, fixture, "UPDATE services SET status = 'melted' WHERE name = ?", string(checkoutService))

	_, err := store.ListServices(t.Context())
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("the domain's rejection was not preserved in the chain: %v", err)
	}
	if !strings.Contains(err.Error(), string(checkoutService)) {
		t.Errorf("error does not name the offending row: %v", err)
	}
}

func TestRunbookAccess(t *testing.T) {
	t.Parallel()
	reference := domain.Reference()
	tests := []struct {
		name      string
		slug      string
		wantFound bool
	}{
		{name: "a known runbook", slug: reference.Runbooks.ServiceDown, wantFound: true},
		// The tool boundary is lenient about case and surrounding whitespace,
		// because a language model emits both.
		{name: "normalized case and padding", slug: "  " + strings.ToUpper(reference.Runbooks.DiskFull) + " ", wantFound: true},
		{name: "an unknown but well-formed slug", slug: "no-such-runbook"},
		// A slug admits neither "/" nor ".", so traversal is rejected as a
		// malformed slug long before a path is built from it.
		{name: "path traversal", slug: "../../etc/passwd"},
		{name: "an absolute path", slug: "/etc/passwd"},
		{name: "an empty slug", slug: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			content, found, err := store.ReadRunbook(test.slug)
			if err != nil {
				t.Fatalf("read runbook %q: %v", test.slug, err)
			}
			if found != test.wantFound {
				t.Fatalf("found = %t, want %t for %q", found, test.wantFound, test.slug)
			}
			if found && !strings.HasPrefix(content, "# Runbook: ") {
				t.Errorf("runbook %q does not look like a runbook: %.40q", test.slug, content)
			}
			if !found && content != "" {
				t.Errorf("a miss returned content: %.40q", content)
			}
		})
	}
}

func TestListRunbookSlugs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	slugs, err := store.ListRunbookSlugs()
	if err != nil {
		t.Fatalf("list runbook slugs: %v", err)
	}
	want := domain.Reference().Runbooks.Values()
	slices.Sort(want)
	if !slices.Equal(slugs, want) {
		t.Errorf("slugs = %v, want %v", slugs, want)
	}

	// An absent knowledge base is a legal, if unhelpful, state — the callers
	// use the empty list to tell the model there is nothing to read.
	empty := New(Config{DataDir: filepath.Join(t.TempDir(), "missing"), StateDir: t.TempDir()})
	slugs, err = empty.ListRunbookSlugs()
	if err != nil {
		t.Fatalf("list runbook slugs with no knowledge base: %v", err)
	}
	if len(slugs) != 0 {
		t.Errorf("slugs = %v, want none", slugs)
	}
}

func TestReadServiceLogs(t *testing.T) {
	t.Parallel()
	reference := domain.Reference()
	tests := []struct {
		name      string
		service   string
		wantFound bool
		wantLines int
	}{
		{name: "a service with logs", service: reference.Services.Inventory, wantFound: true, wantLines: 6},
		{name: "normalized case and padding", service: " CHECKOUT ", wantFound: true, wantLines: 6},
		// Only four of the eight services ship logs; the rest are an absence,
		// not an error.
		{name: "a service without logs", service: reference.Services.Search},
		{name: "path traversal", service: "../" + reference.Services.Checkout},
		{name: "an unknown service", service: "no-such-service"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			lines, found, err := store.ReadServiceLogs(test.service)
			if err != nil {
				t.Fatalf("read logs for %q: %v", test.service, err)
			}
			if found != test.wantFound {
				t.Fatalf("found = %t, want %t for %q", found, test.wantFound, test.service)
			}
			if !found {
				if lines != nil {
					t.Errorf("a miss returned %d lines", len(lines))
				}
				return
			}
			if len(lines) != test.wantLines {
				t.Errorf("lines = %d, want %d", len(lines), test.wantLines)
			}
			// The trailing newline must not become an empty final element: the
			// tool layer counts these lines and shows them to the model.
			for i, line := range lines {
				if line == "" {
					t.Errorf("line %d is empty", i)
				}
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "empty input", text: "", want: nil},
		{name: "one line without a terminator", text: "a", want: []string{"a"}},
		{name: "a trailing newline is not an empty element", text: "a\nb\n", want: []string{"a", "b"}},
		{name: "no trailing newline", text: "a\nb", want: []string{"a", "b"}},
		{name: "CRLF terminators", text: "a\r\nb\r\n", want: []string{"a", "b"}},
		{name: "a blank line in the middle is kept", text: "a\n\nb\n", want: []string{"a", "", "b"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := splitLines(test.text); !slices.Equal(got, test.want) {
				t.Errorf("splitLines(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

func TestCorpusPathsAreRootedInTheDataDirectory(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	if want := filepath.Join(store.DataDir(), "runbooks"); store.RunbooksDir() != want {
		t.Errorf("runbooks dir = %q, want %q", store.RunbooksDir(), want)
	}
	if want := filepath.Join(store.DataDir(), "logs"); store.LogsDir() != want {
		t.Errorf("logs dir = %q, want %q", store.LogsDir(), want)
	}
	slug := mustSlug(domain.Reference().Runbooks.HighLatency)
	if want := filepath.Join(store.RunbooksDir(), string(slug)+".md"); store.RunbookPath(slug) != want {
		t.Errorf("runbook path = %q, want %q", store.RunbookPath(slug), want)
	}
	if _, err := os.Stat(store.RunbookPath(slug)); err != nil {
		t.Errorf("the runbook path does not resolve to a file: %v", err)
	}
}
