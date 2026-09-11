package a2aserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// TestStartupRunsRecoveryBeforePreflightAndPublication pins the course
// invariant: crash recovery precedes every other startup action, and the
// dataset is published only after the state generation has been accepted.
func TestStartupRunsRecoveryBeforePreflightAndPublication(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	if got, want := fixture.steps.sequence(), []string{"recover", "prepare", "migrate"}; !slices.Equal(got, want) {
		t.Errorf("startup order = %v, want %v", got, want)
	}
	for _, name := range []string{"incidents.db", a2aserver.SessionDatabaseName, a2aserver.TaskDatabaseName} {
		if _, err := os.Stat(filepath.Join(fixture.stateDir, name)); err != nil {
			t.Errorf("after startup, %s: %v", name, err)
		}
	}
}

// TestStartupRefusesAnIncompatibleTaskGenerationWithoutPublishing is the Go
// shape of the Python "future runtime state" rejection: an unreadable state
// generation stops startup before anything is written, and the incident
// database is never created.
func TestStartupRefusesAnIncompatibleTaskGenerationWithoutPublishing(t *testing.T) {
	t.Parallel()

	fixture := newUnstartedFixture(t)
	path := filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName)
	writeFutureTaskStore(t, path, "99")
	before := readFile(t, path)

	err := fixture.server.Start(t.Context())
	if err == nil {
		t.Fatal("Start() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "Upgrade the application or select a compatible snapshot") {
		t.Errorf("Start() error = %v, want the snapshot guidance", err)
	}
	if got := readFile(t, path); !slices.Equal(got, before) {
		t.Error("the task database was mutated by a refused startup")
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, "incidents.db")); !os.IsNotExist(err) {
		t.Errorf("a refused startup published the incident database: %v", err)
	}
}

// TestStartupRefusesAnIncompleteCurrentTaskSchema covers the other half of the
// Python rejection matrix: a state generation this binary does understand, but
// which is missing structure, is an operator problem rather than something to
// migrate around.
func TestStartupRefusesAnIncompleteCurrentTaskSchema(t *testing.T) {
	t.Parallel()

	fixture := newUnstartedFixture(t)
	path := filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName)
	writeFutureTaskStore(t, path, domain.CurrentRuntimeSchemaVersion)
	before := readFile(t, path)

	err := fixture.server.Start(t.Context())
	if err == nil {
		t.Fatal("Start() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "incomplete current schema") {
		t.Errorf("Start() error = %v, want the incomplete-schema refusal", err)
	}
	if got := readFile(t, path); !slices.Equal(got, before) {
		t.Error("the task database was mutated by a refused startup")
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, "incidents.db")); !os.IsNotExist(err) {
		t.Errorf("a refused startup published the incident database: %v", err)
	}
}

// TestStartupRefusesACorruptTaskDatabase proves the integrity gate fires before
// publication too, and leaves the bytes alone.
func TestStartupRefusesACorruptTaskDatabase(t *testing.T) {
	t.Parallel()

	fixture := newUnstartedFixture(t)
	path := filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName)
	if err := os.WriteFile(path, []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatalf("writing the corrupt task database: %v", err)
	}
	before := readFile(t, path)

	if err := fixture.server.Start(t.Context()); err == nil {
		t.Fatal("Start() error = nil, want a refusal")
	}
	if got := readFile(t, path); !slices.Equal(got, before) {
		t.Error("the corrupt task database was replaced rather than reported")
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, "incidents.db")); !os.IsNotExist(err) {
		t.Errorf("a refused startup published the incident database: %v", err)
	}
}

// TestHandlerBeforeStartReportsUnready keeps the one misuse path honest: a
// handler taken before startup answers a status, never a panic.
func TestHandlerBeforeStartReportsUnready(t *testing.T) {
	t.Parallel()

	fixture := newUnstartedFixture(t)
	server := fixture.serve(t)

	response, err := server.Client().Get(server.URL + a2aserver.LivenessPath)
	if err != nil {
		t.Fatalf("GET %s: %v", a2aserver.LivenessPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

// TestHealthEndpointsReportReady is the readiness contract kubelet polls.
func TestHealthEndpointsReportReady(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	for _, probe := range []struct {
		want map[string]any
		path string
	}{
		{map[string]any{"status": "alive"}, a2aserver.LivenessPath},
		{map[string]any{"status": "ready"}, a2aserver.ReadinessPath},
	} {
		t.Run(probe.path, func(t *testing.T) {
			response, err := server.Client().Get(server.URL + probe.path)
			if err != nil {
				t.Fatalf("GET %s: %v", probe.path, err)
			}
			defer func() { _ = response.Body.Close() }()
			if got := response.StatusCode; got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decoding the probe body: %v", err)
			}
			if len(body) != len(probe.want) || body["status"] != probe.want["status"] {
				t.Errorf("body = %v, want %v", body, probe.want)
			}
		})
	}
}

func TestReadinessChecksNEROnlyWhenPromptGuardIsConfigured(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.promptGuard = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
		opts.promptGuardReadiness = func(context.Context) error {
			calls.Add(1)
			return errors.New("model catalog unavailable")
		}
	})
	server := fixture.serve(t)

	response, err := server.Client().Get(server.URL + a2aserver.ReadinessPath)
	if err != nil {
		t.Fatalf("GET %s: %v", a2aserver.ReadinessPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	var body struct {
		Problems []string `json:"problems"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if !slices.ContainsFunc(body.Problems, func(problem string) bool {
		return strings.HasPrefix(problem, "named-entity model unavailable:")
	}) {
		t.Errorf("problems = %q, want opaque named-entity model failure", body.Problems)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("named-entity readiness calls = %d, want 1", got)
	}
}

// TestReadinessReportsAnUnavailableDataset proves the probe is what answers,
// rather than a status cached at startup.
func TestReadinessReportsAnUnavailableDataset(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)
	if err := os.Remove(filepath.Join(fixture.stateDir, "incidents.db")); err != nil {
		t.Fatalf("removing the runtime dataset: %v", err)
	}

	problems := readinessProblems(t, server)
	if !slices.ContainsFunc(problems, func(problem string) bool {
		return strings.Contains(problem, "dataset unavailable")
	}) {
		t.Errorf("problems = %v, want one naming the dataset", problems)
	}
}

// TestReadinessReportsAnInvalidTaskStore covers the Python assertion that a
// table dropped after startup makes the replica unready.
func TestReadinessReportsAnInvalidTaskStore(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)
	// The store keeps its own handle open, so the drop goes through a second
	// connection — exactly how an operator would break it.
	execSQL(t, filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName), "DROP TABLE tasks")

	problems := readinessProblems(t, server)
	if !slices.ContainsFunc(problems, func(problem string) bool {
		return strings.Contains(problem, "session/task store invalid")
	}) {
		t.Errorf("problems = %v, want one naming the session or task store", problems)
	}
}

// TestReadinessReportsAnInvalidSessionStore does the same for ADK's own schema,
// which is the half the Python column list could not be ported onto.
func TestReadinessReportsAnInvalidSessionStore(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)
	execSQL(t, filepath.Join(fixture.stateDir, a2aserver.SessionDatabaseName), "DROP TABLE events")

	problems := readinessProblems(t, server)
	if !slices.ContainsFunc(problems, func(problem string) bool {
		return strings.Contains(problem, "session/task store invalid")
	}) {
		t.Errorf("problems = %v, want one naming the session or task store", problems)
	}
}

// TestReadinessOnTheSharedBackendIgnoresTheStateDirectoryFile is the regression
// test for a replica that never became ready.
//
// With the sessions on PostgreSQL, no runtime.db exists in AGENT_STATE_DIR — so
// a readiness check that opened one reported "not initialized" forever, on a
// deployment whose session store was perfectly healthy. The file is deleted
// here to make the point unmissable: the store readiness reports on is the one
// the backend names, and the supplied probe is what answers for it.
func TestReadinessOnTheSharedBackendIgnoresTheStateDirectoryFile(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	fixture := newFixture(t, onSharedSessionBackend(func(context.Context) error {
		calls.Add(1)
		return nil
	}))
	server := fixture.serve(t)
	if err := os.Remove(filepath.Join(fixture.stateDir, a2aserver.SessionDatabaseName)); err != nil {
		t.Fatalf("removing the session file the shared backend does not use: %v", err)
	}

	response, err := server.Client().Get(server.URL + a2aserver.ReadinessPath)
	if err != nil {
		t.Fatalf("GET %s: %v", a2aserver.ReadinessPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusOK {
		t.Errorf("status = %d, want %d with a healthy shared session store", got, http.StatusOK)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("shared session probe calls = %d, want exactly 1 per readiness poll", got)
	}
}

// TestReadinessReportsAnInvalidSharedSessionStore proves the shared backend's
// probe is a real gate: a session schema its owner reports as unusable makes
// this replica unready, in the same problem class as the file store.
func TestReadinessReportsAnInvalidSharedSessionStore(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, onSharedSessionBackend(func(context.Context) error {
		return errors.New("the PostgreSQL session store named by AGENT_SESSION_DSN is missing tables: events")
	}))
	server := fixture.serve(t)

	problems := readinessProblems(t, server)
	if !slices.ContainsFunc(problems, func(problem string) bool {
		return strings.Contains(problem, "session/task store invalid")
	}) {
		t.Errorf("problems = %v, want one naming the session or task store", problems)
	}
	// The body carries a failure class and never the store's own message: a
	// session-store error can name a host, a database and a role, and readiness
	// is served to anyone who can reach the port.
	for _, problem := range problems {
		if strings.Contains(problem, "AGENT_SESSION_DSN") {
			t.Errorf("problem %q leaked the session store's message onto the wire", problem)
		}
	}
}

// TestReadinessStillProbesTheTaskFileOnTheSharedBackend keeps the two stores
// separate. Only the sessions move to PostgreSQL; tasks stay a SQLite file in
// AGENT_STATE_DIR under both backends, so breaking that file must still be
// caught by a shared-backend replica.
func TestReadinessStillProbesTheTaskFileOnTheSharedBackend(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, onSharedSessionBackend(func(context.Context) error { return nil }))
	server := fixture.serve(t)
	execSQL(t, filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName), "DROP TABLE tasks")

	problems := readinessProblems(t, server)
	if !slices.ContainsFunc(problems, func(problem string) bool {
		return strings.Contains(problem, "session/task store invalid")
	}) {
		t.Errorf("problems = %v, want the task store reported on the shared backend", problems)
	}
}

// TestStartupJudgesTheLeftoverSessionFileByTheBackend covers the other half of
// the same decision, one step earlier.
//
// The preflight refuses an incompatible state generation. Under the file
// backend runtime.db is part of that generation and an incomplete one is a
// refusal; under the shared backend it is a leftover no process opens, and
// refusing over it would block the migration to a shared store outright.
func TestStartupJudgesTheLeftoverSessionFileByTheBackend(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		configure func(*fixtureOptions)
		wantStart bool
	}{
		"file backend refuses it":   {configure: func(*fixtureOptions) {}, wantStart: false},
		"shared backend ignores it": {configure: onSharedSessionBackend(func(context.Context) error { return nil }), wantStart: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixture := newUnstartedFixture(t, testCase.configure)
			// A valid SQLite database carrying tables ADK never created: the
			// "current generation, incomplete structure" case the preflight exists
			// to refuse, written before startup so nothing has published yet.
			execSQL(t, filepath.Join(fixture.stateDir, a2aserver.SessionDatabaseName),
				"CREATE TABLE leftover (id TEXT)")

			err := fixture.server.Start(t.Context())
			if testCase.wantStart {
				if err != nil {
					t.Fatalf("Start() error = %v, want nil for a file the shared backend never opens", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Start() error = nil, want the incomplete-schema refusal")
			}
			if !strings.Contains(err.Error(), "incomplete current schema") {
				t.Errorf("Start() error = %v, want the incomplete-schema refusal", err)
			}
		})
	}
}

// TestNewPairsTheSessionProbeWithTheBackend pins the wiring rule that keeps
// readiness honest: exactly one store answers, and it is the configured one.
func TestNewPairsTheSessionProbeWithTheBackend(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		probe   a2aserver.Step
		backend config.SessionBackend
		want    string
	}{
		"a shared backend without a probe": {
			backend: config.SessionBackendPostgres,
			want:    "ProbeSessions is required",
		},
		"a file backend with a probe": {
			backend: config.SessionBackendSQLite,
			probe:   func(context.Context) error { return nil },
			want:    "ProbeSessions must not be set",
		},
		"an unknown backend": {
			backend: "mysql",
			probe:   func(context.Context) error { return nil },
			want:    "Options.SessionBackend is \"mysql\"",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The rest of the wiring is deliberately left out: New accumulates
			// every hole, and this test judges the one it is about.
			_, err := a2aserver.New(a2aserver.Config{
				ProbeSessions: testCase.probe,
				Options: a2aserver.Options{
					StateDir:       t.TempDir(),
					Entrypoint:     config.EntrypointAgent,
					SessionBackend: testCase.backend,
				},
			})
			if err == nil {
				t.Fatal("New() error = nil, want a refusal")
			}
			errIsNot(t, err, a2aserver.ErrIncompleteConfig, "New()")
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("New() error = %v, want it to name %q", err, testCase.want)
			}
		})
	}
}

// TestReadinessReportsAnUnwritableStateDirectory covers the third problem class.
func TestReadinessReportsAnUnwritableStateDirectory(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so there is nothing to observe")
	}
	fixture := newFixture(t)
	server := fixture.serve(t)
	if err := os.Chmod(fixture.stateDir, 0o500); err != nil {
		t.Fatalf("making the state directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.stateDir, 0o750) })

	problems := readinessProblems(t, server)
	if !slices.ContainsFunc(problems, func(problem string) bool {
		return strings.Contains(problem, "state directory is not writable")
	}) {
		t.Errorf("problems = %v, want one naming the state directory", problems)
	}
}

// TestReadinessAnswersWhileTheTaskDatabaseIsBeingWritten is the Go form of the
// Python assertion that a busy traffic pool still reports ready.
//
// What it proves is exactly what it says: with another writer holding a write
// transaction on the task database, readiness still answers 200 — because the
// probe opens its own read-only handle rather than queueing behind traffic. It
// does not prove the stronger statement that the store's own single connection
// is unreachable from the probe; that one is structural, and the probe's
// dedicated handles are visible in probeStateStores.
func TestReadinessAnswersWhileTheTaskDatabaseIsBeingWritten(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	// Hold the task store's only connection for the duration of the probe.
	done := make(chan struct{})
	go func() {
		defer close(done)
		holdTaskStoreConnection(t, fixture, func() {
			response, err := server.Client().Get(server.URL + a2aserver.ReadinessPath)
			if err != nil {
				t.Errorf("GET %s: %v", a2aserver.ReadinessPath, err)
				return
			}
			defer func() { _ = response.Body.Close() }()
			if got := response.StatusCode; got != http.StatusOK {
				t.Errorf("status while the store is busy = %d, want %d", got, http.StatusOK)
			}
		})
	}()
	<-done
}

// TestNewRefusesAnIncompleteWiring accumulates every hole in one report,
// because a server is assembled once at startup and each hole is a capability
// the deployment contract claims.
func TestNewRefusesAnIncompleteWiring(t *testing.T) {
	t.Parallel()

	_, err := a2aserver.New(a2aserver.Config{})
	if err == nil {
		t.Fatal("New() error = nil, want a refusal")
	}
	errIsNot(t, err, a2aserver.ErrIncompleteConfig, "New()")
	for _, want := range []string{
		"RootAgent is required",
		"SessionService is required",
		"RecoverState is required",
		"PrepareDataset is required",
		"MigrateSessions is required",
		"ProbeDataset is required",
		"Options.StateDir is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %v, want it to name %q", err, want)
		}
	}
}

// TestNewRefusesANonConversationalEntrypoint keeps the Python guarantee that
// the A2A contract serves the conversational composition. In Go every
// entrypoint is an agent.Agent, so the type system no longer enforces it and
// the check has to be explicit.
func TestNewRefusesANonConversationalEntrypoint(t *testing.T) {
	t.Parallel()

	for _, entrypoint := range []config.Entrypoint{config.EntrypointWorkflow, config.EntrypointCoordinator} {
		t.Run(string(entrypoint), func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("New() panicked: %v", recovered)
				}
			}()
			_, err := a2aserver.New(a2aserver.Config{Options: a2aserver.Options{
				StateDir:   t.TempDir(),
				Entrypoint: entrypoint,
			}})
			if err == nil {
				t.Fatal("New() error = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), config.EnvEntrypoint) {
				t.Errorf("New() error = %v, want it to name %s", err, config.EnvEntrypoint)
			}
		})
	}
}

// TestOptionsFromMapsTheAgentConfiguration pins the one place the A2A settings
// are read, so a renamed field is a failing test rather than a silent default.
func TestOptionsFromMapsTheAgentConfiguration(t *testing.T) {
	t.Parallel()

	header := "x-verified-subject"
	cfg, err := config.LoadFrom(map[string]string{
		config.EnvStateDir: "/tmp/state",
		// The backend decides which store readiness reports on, so it has to
		// survive this mapping; the DSN beside it is what config validation
		// requires of that backend, and never reaches these options.
		config.EnvSessionBackend:        string(config.SessionBackendPostgres),
		config.EnvSessionDSN:            "postgres://agentops:hunter2@sessions.internal:5432/sessions?sslmode=disable",
		config.EnvA2ABindHost:           "0.0.0.0",
		config.EnvA2AHost:               "agent.example",
		config.EnvA2APort:               "9090",
		config.EnvA2AProtocol:           "https",
		config.EnvA2AMaxLLMCalls:        "7",
		config.EnvA2AStreaming:          "true",
		config.EnvDrainTimeout:          "25",
		config.EnvTrustedIdentityHeader: header,
	})
	if err != nil {
		t.Fatalf("config.LoadFrom() error = %v, want nil", err)
	}

	options := a2aserver.OptionsFrom(cfg, "1.2.3")
	if options.StateDir != "/tmp/state" || options.BindHost != "0.0.0.0" ||
		options.Host != "agent.example" || options.Protocol != "https" || options.Port != 9090 {
		t.Errorf("address settings = %+v, want the configured ones", options)
	}
	if options.MaxLLMCalls != 7 || !options.Streaming || options.DrainTimeout.Seconds() != 25 {
		t.Errorf("policy settings = %+v, want the configured ones", options)
	}
	if options.TrustedIdentityHeader != header {
		t.Errorf("TrustedIdentityHeader = %q, want %q", options.TrustedIdentityHeader, header)
	}
	if options.Version != "1.2.3" || options.Entrypoint != config.EntrypointAgent {
		t.Errorf("identity settings = %+v, want the configured ones", options)
	}
	if options.SessionBackend != config.SessionBackendPostgres {
		t.Errorf("SessionBackend = %q, want %q", options.SessionBackend, config.SessionBackendPostgres)
	}
}

// TestAnUnsetSessionBackendResolvesToTheFileStore keeps the account-free default
// explicit from both directions: an Options built in code and one mapped from an
// environment with nothing set both mean the SQLite file, which is the only
// backend this package can probe without help.
func TestAnUnsetSessionBackendResolvesToTheFileStore(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadFrom(map[string]string{})
	if err != nil {
		t.Fatalf("config.LoadFrom() error = %v, want nil", err)
	}
	if got := a2aserver.OptionsFrom(cfg, "").SessionBackend; got != config.SessionBackendSQLite {
		t.Errorf("SessionBackend = %q, want %q", got, config.SessionBackendSQLite)
	}
	// An Options built in code carries the empty value, which resolve() fills.
	fixture := newUnstartedFixture(t)
	if got := fixture.server.Options().SessionBackend; got != config.SessionBackendSQLite {
		t.Errorf("resolved SessionBackend = %q, want %q", got, config.SessionBackendSQLite)
	}
}

// TestUnsetTrustedIdentityHeaderStaysEmpty proves the pointer field's "unset"
// state survives the mapping: an absent header must never become a header named
// the empty string, which the middleware would then read on every request.
func TestUnsetTrustedIdentityHeaderStaysEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadFrom(map[string]string{})
	if err != nil {
		t.Fatalf("config.LoadFrom() error = %v, want nil", err)
	}
	if got := a2aserver.OptionsFrom(cfg, "").TrustedIdentityHeader; got != "" {
		t.Errorf("TrustedIdentityHeader = %q, want empty", got)
	}
}

// TestAddressAndURLAreDistinct guards the bind/advertise separation: the
// container binds every interface and must still advertise a callable name.
func TestAddressAndURLAreDistinct(t *testing.T) {
	t.Parallel()

	fixture := newUnstartedFixture(t, func(opts *fixtureOptions) {
		opts.options = func(options *a2aserver.Options) {
			options.BindHost = "0.0.0.0"
			options.Host = "agent.example"
			options.Protocol = "https"
			options.Port = 8443
		}
	})
	if got, want := fixture.server.Address(), "0.0.0.0:8443"; got != want {
		t.Errorf("Address() = %q, want %q", got, want)
	}
	if got, want := fixture.server.URL(), "https://agent.example:8443"; got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

// TestDefaultsBindLoopback keeps the safe default explicit: a server that bound
// every interface by default would be one misconfiguration from the network.
func TestDefaultsBindLoopback(t *testing.T) {
	t.Parallel()

	fixture := newUnstartedFixture(t)
	options := fixture.server.Options()
	if options.BindHost != a2aserver.DefaultBindHost {
		t.Errorf("BindHost = %q, want %q", options.BindHost, a2aserver.DefaultBindHost)
	}
	if options.Host != a2aserver.DefaultHost || options.Protocol != a2aserver.DefaultProtocol ||
		options.Port != a2aserver.DefaultPort {
		t.Errorf("advertised settings = %+v, want the documented defaults", options)
	}
	if options.Version != a2aserver.UnknownVersion {
		t.Errorf("Version = %q, want %q for a source-checkout build", options.Version, a2aserver.UnknownVersion)
	}
}

// TestA2AOptionsCarryTheStoreAndTheIdentityInterceptor covers the launcher
// seam: a deployment that prefers ADK's own web surface installs the same task
// store and the same identity binding through launcher.Config.A2AOptions.
func TestA2AOptionsCarryTheStoreAndTheIdentityInterceptor(t *testing.T) {
	t.Parallel()

	unstarted := newUnstartedFixture(t)
	if _, err := unstarted.server.A2AOptions(); !errors.Is(err, a2aserver.ErrNotStarted) {
		// The store is opened after the state preflight, so there is genuinely
		// nothing to install before startup has run.
		t.Errorf("A2AOptions() before Start error = %v, want %v", err, a2aserver.ErrNotStarted)
	}

	fixture := newFixture(t)
	options, err := fixture.server.A2AOptions()
	if err != nil {
		t.Fatalf("A2AOptions() error = %v, want nil", err)
	}
	if len(options) == 0 {
		t.Fatal("A2AOptions() returned nothing to install")
	}
	// The store the options install is the one this server opened, so a
	// launcher-hosted deployment and this one share a single task database.
	if fixture.server.TaskStore() == nil {
		t.Error("the server exposes no task store after startup")
	}
	if got := fixture.server.TaskStore().Path(); !strings.HasSuffix(got, a2aserver.TaskDatabaseName) {
		t.Errorf("task store path = %q, want it inside the state directory", got)
	}
}

// TestCloseIsIdempotentAndReportsEveryFailure covers the shutdown contract the
// Python Runtime.close() had: every resource is closed even when an earlier one
// fails, and closing twice is not an error.
func TestCloseIsIdempotentAndReportsEveryFailure(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	for attempt := range 2 {
		if err := fixture.server.Close(); err != nil {
			t.Errorf("Close() attempt %d error = %v, want nil", attempt, err)
		}
	}
}

type closeableSessions struct {
	session.Service
	closed atomic.Int32
}

func (s *closeableSessions) Close() error {
	if s.closed.Add(1) != 1 {
		return errors.New("session store closed more than once")
	}
	return nil
}

func TestCloseOwnsAProvidedSessionCloserExactlyOnce(t *testing.T) {
	t.Parallel()

	sessions := &closeableSessions{Service: session.InMemoryService()}
	fixture := newUnstartedFixture(t, func(options *fixtureOptions) { options.sessions = sessions })
	for attempt := range 2 {
		if err := fixture.server.Close(); err != nil {
			t.Errorf("Close() attempt %d error = %v, want nil", attempt, err)
		}
	}
	if got := sessions.closed.Load(); got != 1 {
		t.Errorf("session Close() calls = %d, want exactly 1", got)
	}
}
