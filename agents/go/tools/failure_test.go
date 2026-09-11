package tools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// errUnreachable stands in for a dataset that cannot be read at all.
var errUnreachable = errors.New("the dataset is unreachable")

// TestGenuineFailuresStayGoErrors is the other half of the package's central
// distinction, and the half a test can easily miss.
//
// A refusal is a result the model reads and acts on. A failure — the database
// is gone, the audit insert was rejected — is a Go error, because the policy
// plane classifies errors and decides what is safe to show. Folding a failure
// into an "error" key would route it around that classification and hand the
// model a driver message to reason about.
func TestGenuineFailuresStayGoErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		stub     func(Store) Store
		tool     func(*Tools) tool.Tool
		args     map[string]any
		sentinel error
		name     string
	}{
		{
			name: "the incident listing fails",
			stub: func(real Store) Store {
				return stubStore{Store: real, listIncidents: func() ([]domain.Incident, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).ListIncidents,
			args:     map[string]any{},
			sentinel: errUnreachable,
		},
		{
			name: "the incident lookup fails",
			stub: func(real Store) Store {
				return stubStore{Store: real, getIncident: func() (*domain.Incident, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).GetIncident,
			args:     map[string]any{"incident_id": checkoutIncident},
			sentinel: errUnreachable,
		},
		{
			name: "the service lookup fails",
			stub: func(real Store) Store {
				return stubStore{Store: real, getService: func() (*domain.Service, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).GetServiceStatus,
			args:     map[string]any{"name": checkoutService},
			sentinel: errUnreachable,
		},
		{
			name: "the known-service listing behind an unknown name fails",
			stub: func(real Store) Store {
				return stubStore{
					Store:        real,
					getService:   func() (*domain.Service, error) { return nil, nil },
					listServices: func() ([]domain.Service, error) { return nil, errUnreachable },
				}
			},
			tool:     (*Tools).GetServiceStatus,
			args:     map[string]any{"name": checkoutService},
			sentinel: errUnreachable,
		},
		{
			name: "the service behind a restart cannot be read",
			stub: func(real Store) Store {
				return stubStore{Store: real, getService: func() (*domain.Service, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).RestartService,
			args:     map[string]any{"name": inventoryService},
			sentinel: errUnreachable,
		},
		{
			name: "the open incidents behind a service status cannot be listed",
			stub: func(real Store) Store {
				return stubStore{Store: real, listIncidents: func() ([]domain.Incident, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).GetServiceStatus,
			args:     map[string]any{"name": checkoutService},
			sentinel: errUnreachable,
		},
		{
			name: "the log corpus cannot be listed",
			stub: func(real Store) Store {
				return stubStore{
					Store:    real,
					readLogs: func() ([]string, bool, error) { return nil, false, nil },
					// A regular file where the corpus directory should be: not
					// "absent", which is legal, but unreadable, which is not.
					logsDir: func() string { return notADirectory(t) },
				}
			},
			tool: (*Tools).SearchServiceLogs,
			args: map[string]any{"service": inventoryService},
			// No sentinel: the failure here is the filesystem's own, and the point
			// is that it still reaches the caller as an error rather than as a
			// refusal with an empty list of alternatives.
		},
		{
			name: "the log corpus cannot be read",
			stub: func(real Store) Store {
				return stubStore{Store: real, readLogs: func() ([]string, bool, error) {
					return nil, false, errUnreachable
				}}
			},
			tool:     (*Tools).SearchServiceLogs,
			args:     map[string]any{"service": inventoryService},
			sentinel: errUnreachable,
		},
		{
			name: "the restart transaction fails",
			stub: func(real Store) Store {
				return stubStore{Store: real, restart: func() (*domain.AuditEntry, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).RestartService,
			args:     map[string]any{"name": inventoryService},
			sentinel: errUnreachable,
		},
		{
			name: "the resolution transaction fails",
			stub: func(real Store) Store {
				return stubStore{Store: real, resolve: func() (*domain.AuditEntry, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).ResolveIncident,
			args:     map[string]any{"incident_id": inventoryIncident},
			sentinel: errUnreachable,
		},
		{
			name: "the incident behind a resolution cannot be read",
			stub: func(real Store) Store {
				return stubStore{Store: real, getIncident: func() (*domain.Incident, error) {
					return nil, errUnreachable
				}}
			},
			tool:     (*Tools).ResolveIncident,
			args:     map[string]any{"incident_id": inventoryIncident},
			sentinel: errUnreachable,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fixture := newFixture(t, func(o *options) { o.store = testCase.stub })
			ctx := approvedContext(t, "approved during the incident call")

			raw, err := run(t, testCase.tool(fixture.tools), ctx, testCase.args)

			if err == nil {
				t.Fatalf("Run() = %v, nil, want a failure rather than a refusal", raw)
			}
			// Whatever failed, the message names what was being attempted, so an
			// operator reading a trace is not left with a bare driver error.
			if testCase.sentinel != nil && !errors.Is(err, testCase.sentinel) {
				t.Fatalf("Run() error = %v, want it to wrap %v", err, testCase.sentinel)
			}
			if len(err.Error()) <= len(errUnreachable.Error()) {
				t.Errorf("Run() error = %q, want the failure wrapped with context", err)
			}
		})
	}
}

// notADirectory returns the path of a regular file, so a directory listing of
// it fails for a reason that is not "it is not there".
func notADirectory(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write the stand-in file: %v", err)
	}
	return path
}

// TestAnAbsentLogCorpusStillProducesARefusal covers the degenerate deployment
// where the sample logs are missing entirely. It is a legal, if unhelpful,
// state: the model is told this service has no logs and the list of
// alternatives is simply empty, rather than the tool failing.
func TestAnAbsentLogCorpusStillProducesARefusal(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.store = func(real Store) Store {
			return stubStore{
				Store:    real,
				readLogs: func() ([]string, bool, error) { return nil, false, nil },
				logsDir:  func() string { return filepath.Join(t.TempDir(), "absent") },
			}
		}
	})
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	result := mustRun[SearchServiceLogsResult](t, fixture.tools.SearchServiceLogs(), ctx,
		map[string]any{"service": inventoryService})

	want := `No logs for service "` + inventoryService + `". Available logs: .`
	if result.Error != want {
		t.Errorf("Error = %q, want %q", result.Error, want)
	}
}

// TestANonStringRationaleIsRendered pins the permissiveness Python got from
// str(): a client that answers with a number wrote a strange justification, not
// no justification, and the audit trail records what it was told.
func TestANonStringRationaleIsRendered(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{
		confirmed: true,
		payload:   map[string]any{"rationale": 42},
	}, identity{})

	result := mustRun[RestartServiceResult](t, fixture.tools.RestartService(), ctx,
		map[string]any{"name": inventoryService})

	if result.Audit == nil {
		t.Fatalf("Audit = nil, want the evidence row; error was %q", result.Error)
	}
	if result.Audit.Rationale != "42" {
		t.Errorf("Audit.Rationale = %q, want %q", result.Audit.Rationale, "42")
	}
}

// TestAToolCannotShipWithAnUndocumentedArgument exercises the construction-time
// invariant directly.
//
// It matters more than it looks: the descriptions are assembled at runtime from
// the vocabulary, so a renamed argument or a new field would otherwise reach a
// model as a nameless box to fill in. Failing at construction turns that into a
// startup error nobody can miss.
func TestAToolCannotShipWithAnUndocumentedArgument(t *testing.T) {
	t.Parallel()

	type args struct {
		Documented   string `json:"documented"`
		Undocumented string `json:"undocumented"`
	}
	handler := func(agent.Context, args) (string, error) { return "", nil }

	cases := []struct {
		descriptions map[string]string
		name         string
		want         string
	}{
		{
			name:         "an argument nobody described",
			descriptions: map[string]string{"documented": "The documented one."},
			want:         `argument "undocumented" carries no description`,
		},
		{
			name: "a description for an argument that does not exist",
			descriptions: map[string]string{
				"documented": "The documented one.", "undocumented": "The other one.", "ghost": "Nothing.",
			},
			want: `there is no argument named "ghost"`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			built, err := newTool(
				functiontool.Config{Name: "probe", Description: "A probe."},
				testCase.descriptions, handler,
			)

			if built != nil {
				t.Errorf("newTool() = %v, want nil", built)
			}
			if !errors.Is(err, ErrIncompleteConfig) {
				t.Fatalf("newTool() error = %v, want it to wrap %v", err, ErrIncompleteConfig)
			}
			contains(t, err.Error(), testCase.want, "the error")
			contains(t, err.Error(), "document the probe arguments", "the error")
		})
	}
}
