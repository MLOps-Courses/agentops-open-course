package compose

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// Every test in this package is offline and deterministic: no model is called,
// no server is contacted except the in-process ones a test starts itself. A
// composition is a wiring decision, and a wiring decision is provable without a
// model — which is exactly why the Python suite could assert all of this too.

// fakeLLM is a model.LLM that answers nothing.
//
// The compositions never call it: llmagent.New stores the model and the tests
// assert what was stored. Yielding an error rather than a response makes an
// accidental call fail loudly instead of looking like a working turn.
type fakeLLM struct{ name string }

func (f fakeLLM) Name() string { return f.name }

func (f fakeLLM) GenerateContent(
	context.Context, *model.LLMRequest, bool,
) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("fakeLLM must not be called: composition tests are offline"))
	}
}

// namedTool is a stand-in for a tool this module does not own yet.
//
// It carries only a name because that is all a composition observes: which tool
// values an agent holds, and what they are called. The four tools the memory
// package will build are the only ones faked; everything else in the tests is a
// real tool from the tools package, so an identity assertion means what it says.
type namedTool struct{ name string }

func (t namedTool) Name() string        { return t.name }
func (t namedTool) Description() string { return t.name + " (test double)" }
func (t namedTool) IsLongRunning() bool { return false }

// emptyStore satisfies the tools package's Store seam without a database.
//
// Every method fails: the composition tests never run a tool, and a store that
// silently returned empty results would let a test that accidentally did run
// one pass for the wrong reason.
type emptyStore struct{}

var errNoStore = errors.New("emptyStore: composition tests never run a tool")

func (emptyStore) ListIncidents(context.Context, data.IncidentFilter) ([]domain.Incident, error) {
	return nil, errNoStore
}

func (emptyStore) GetIncident(context.Context, domain.IncidentID) (*domain.Incident, error) {
	return nil, errNoStore
}

func (emptyStore) ListServices(context.Context) ([]domain.Service, error) { return nil, errNoStore }

func (emptyStore) GetService(context.Context, domain.Slug) (*domain.Service, error) {
	return nil, errNoStore
}

func (emptyStore) ReadServiceLogs(string) ([]string, bool, error) { return nil, false, errNoStore }

func (emptyStore) LogsDir() string { return "" }

func (emptyStore) RestartServiceWithAudit(
	context.Context, domain.Slug, data.AuditIdentity,
) (*domain.AuditEntry, error) {
	return nil, errNoStore
}

func (emptyStore) ResolveIncidentWithAudit(
	context.Context, domain.IncidentID, data.AuditIdentity,
) (*domain.AuditEntry, error) {
	return nil, errNoStore
}

// fakeToolset is a stand-in for the skill toolset.
type fakeToolset struct {
	name  string
	tools []tool.Tool
}

func (t fakeToolset) Name() string { return t.name }

func (t fakeToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return t.tools, nil }

// testContext is the minimal agent.Context a toolset's request processor needs.
//
// The strict mock panics on every method it was not given, so a toolset that
// started reaching for session state would fail loudly here rather than pick up
// a zero value.
type testContext struct{ agent.StrictContextMock }

func newContext(t *testing.T) agent.Context {
	t.Helper()

	return &testContext{StrictContextMock: agent.NewStrictContextMock(t.Context())}
}

// realTools builds the six tools the tools package owns, over a store that
// refuses every call.
//
// Using the real constructor rather than doubles is what makes the
// least-privilege assertions meaningful: the values the remediation specialist
// holds are the same confirmation-gated tools the tools package proves pause
// for a human, compared by identity.
func realTools(t *testing.T) *tools.Tools {
	t.Helper()

	built, err := tools.New(tools.Config{
		Store:  emptyStore{},
		Guard:  func(ctx context.Context, _ string, call func(context.Context) error) error { return call(ctx) },
		Redact: func(text string) string { return text },
	})
	if err != nil {
		t.Fatalf("tools.New() error = %v, want nil", err)
	}
	return built
}

// testConfig returns a complete, valid composition configuration.
func testConfig(t *testing.T) Config {
	t.Helper()

	agentTools := realTools(t)
	return Config{
		Model: fakeLLM{name: "fake-model"},
		Skills: fakeToolset{name: "skills", tools: []tool.Tool{
			namedTool{name: LoadSkillToolName},
		}},
		Tools: Tools{
			ListIncidents:     agentTools.ListIncidents(),
			GetIncident:       agentTools.GetIncident(),
			GetServiceStatus:  agentTools.GetServiceStatus(),
			SearchServiceLogs: agentTools.SearchServiceLogs(),
			GetRunbook:        namedTool{name: GetRunbookToolName},
			SearchRunbooks:    namedTool{name: SearchRunbooksToolName},
			RestartService:    agentTools.RestartService(),
			ResolveIncident:   agentTools.ResolveIncident(),
		},
		Memory: []tool.Tool{
			namedTool{name: RecallIncidentContextToolName},
			namedTool{name: SaveIncidentNoteToolName},
		},
		Entrypoint: config.EntrypointAgent,
	}
}

// newCompose builds a composer from testConfig with the given adjustments.
func newCompose(t *testing.T, adjust ...func(*Config)) *Compose {
	t.Helper()

	cfg := testConfig(t)
	for _, apply := range adjust {
		apply(&cfg)
	}
	built, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return built
}

// toolNames renders a tool slice the way the assertions read it.
func toolNames(built []tool.Tool) []string {
	names := make([]string, 0, len(built))
	for _, candidate := range built {
		names = append(names, candidate.Name())
	}
	return names
}

// allConfigs returns every llmagent.Config this package produces, named.
//
// It lives in the test rather than in the package because it exists only to let
// one assertion sweep the whole surface; production code has no use for the
// list, and exporting it would be a test seam on [Compose].
func allConfigs(t *testing.T, c *Compose) map[string]llmagent.Config {
	t.Helper()

	report, err := c.reportConfig()
	if err != nil {
		t.Fatalf("reportConfig() error = %v, want nil", err)
	}
	configs := map[string]llmagent.Config{
		AgentName:       c.conversationalConfig(),
		DiagnosisName:   c.diagnosisConfig(),
		RemediationName: c.remediationConfig(),
		CoordinatorName: c.coordinatorConfig(nil),
		ReportAgentName: report,
	}
	for _, stage := range c.workflowStageConfigs() {
		configs[stage.Name] = stage
	}
	return configs
}

// configEntrypoints returns every runnable entrypoint, so a test that must hold
// for all of them cannot silently miss one added later.
func configEntrypoints() []config.Entrypoint { return config.Entrypoints() }
