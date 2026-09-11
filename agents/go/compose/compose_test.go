package compose

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// TestRootAgentIsDefined is the Go port of test_root_agent_defined: the default
// entrypoint builds, is named the way every evalset and agent card expects, and
// carries a model.
func TestRootAgentIsDefined(t *testing.T) {
	t.Parallel()

	root, err := newCompose(t).RootAgent()
	if err != nil {
		t.Fatalf("RootAgent() error = %v, want nil", err)
	}
	if root.Name() != AgentName {
		t.Errorf("Name() = %q, want %q", root.Name(), AgentName)
	}
	if root.Description() != AgentDescription {
		t.Errorf("Description() = %q, want %q", root.Description(), AgentDescription)
	}
	if got := newCompose(t).conversationalConfig().Model; got == nil {
		t.Error("conversational agent carries no model")
	}
}

// TestEntrypointSelectsExactlyOneComposition is the Go port of
// test_composition_selector_exposes_each_validated_entrypoint.
//
// The count assertions are the point, not the identity ones: the Python track
// imported each sub-module inside its branch so the composition that was not
// selected was never constructed, and losing that would mean every start paid
// for three compositions — three sets of agents, and on the coordinator path
// two extra specialists — to serve one.
func TestEntrypointSelectsExactlyOneComposition(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		entrypoint config.Entrypoint
		want       string
	}{
		{config.EntrypointAgent, "conversational"},
		{config.EntrypointWorkflow, "workflow"},
		{config.EntrypointCoordinator, "coordinator"},
	} {
		t.Run(string(testCase.entrypoint), func(t *testing.T) {
			t.Parallel()

			calls := map[string]int{}
			builder := func(name string) func() (agent.Agent, error) {
				return func() (agent.Agent, error) {
					calls[name]++
					return namedAgent(t, name), nil
				}
			}
			selected, err := selectRoot(testCase.entrypoint, rootBuilders{
				agent:       builder("conversational"),
				workflow:    builder("workflow"),
				coordinator: builder("coordinator"),
			})
			if err != nil {
				t.Fatalf("selectRoot() error = %v, want nil", err)
			}
			if selected.Name() != testCase.want {
				t.Errorf("selectRoot() built %q, want %q", selected.Name(), testCase.want)
			}
			if want := map[string]int{testCase.want: 1}; !reflect.DeepEqual(calls, want) {
				t.Errorf("builders called %v, want %v", calls, want)
			}
		})
	}
}

// TestUnknownEntrypointIsRefused pins the default branch: an entrypoint added
// to the enum without a composition must fail loudly rather than silently
// serving the conversational agent.
func TestUnknownEntrypointIsRefused(t *testing.T) {
	t.Parallel()

	_, err := selectRoot(config.Entrypoint("reflection-loop"), rootBuilders{})
	if !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("selectRoot() error = %v, want ErrIncompleteConfig", err)
	}
}

// TestEveryEntrypointBuilds proves each composition actually constructs, which
// selectRoot's fake builders deliberately do not cover.
func TestEveryEntrypointBuilds(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		entrypoint config.Entrypoint
		want       string
	}{
		{config.EntrypointAgent, AgentName},
		{config.EntrypointWorkflow, WorkflowName},
		{config.EntrypointCoordinator, CoordinatorName},
	} {
		t.Run(string(testCase.entrypoint), func(t *testing.T) {
			t.Parallel()

			composer := newCompose(t, func(cfg *Config) { cfg.Entrypoint = testCase.entrypoint })
			root, err := composer.RootAgent()
			if err != nil {
				t.Fatalf("RootAgent() error = %v, want nil", err)
			}
			if root.Name() != testCase.want {
				t.Errorf("RootAgent() = %q, want %q", root.Name(), testCase.want)
			}
			if composer.Entrypoint() != testCase.entrypoint {
				t.Errorf("Entrypoint() = %q, want %q", composer.Entrypoint(), testCase.entrypoint)
			}
		})
	}
}

// TestIncompleteConfigIsRefusedByName covers the wiring contract. Each case
// removes exactly one requirement and expects the failure to name it, because a
// startup error that says only "invalid configuration" sends the reader to the
// wrong file.
func TestIncompleteConfigIsRefusedByName(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		break_ func(*Config)
		name   string
		want   string
	}{
		{name: "model", break_: func(cfg *Config) { cfg.Model = nil }, want: "Model is required"},
		{name: "skills", break_: func(cfg *Config) { cfg.Skills = nil }, want: "Skills is required"},
		{
			name:   "entrypoint",
			break_: func(cfg *Config) { cfg.Entrypoint = "reflection-loop" },
			want:   `Entrypoint "reflection-loop"`,
		},
		{
			name:   "list-incidents",
			break_: func(cfg *Config) { cfg.Tools.ListIncidents = nil },
			want:   "Tools." + tools.ListIncidentsToolName,
		},
		{
			name:   "get-incident",
			break_: func(cfg *Config) { cfg.Tools.GetIncident = nil },
			want:   "Tools." + tools.GetIncidentToolName,
		},
		{
			name:   "get-service-status",
			break_: func(cfg *Config) { cfg.Tools.GetServiceStatus = nil },
			want:   "Tools." + tools.GetServiceStatusToolName,
		},
		{
			name:   "search-service-logs",
			break_: func(cfg *Config) { cfg.Tools.SearchServiceLogs = nil },
			want:   "Tools." + tools.SearchServiceLogsToolName,
		},
		{
			name:   "get-runbook",
			break_: func(cfg *Config) { cfg.Tools.GetRunbook = nil },
			want:   "Tools." + GetRunbookToolName,
		},
		{
			name:   "search-runbooks",
			break_: func(cfg *Config) { cfg.Tools.SearchRunbooks = nil },
			want:   "Tools." + SearchRunbooksToolName,
		},
		{
			name:   "restart-service",
			break_: func(cfg *Config) { cfg.Tools.RestartService = nil },
			want:   "Tools." + tools.RestartServiceToolName,
		},
		{
			name:   "resolve-incident",
			break_: func(cfg *Config) { cfg.Tools.ResolveIncident = nil },
			want:   "Tools." + tools.ResolveIncidentToolName,
		},
		{
			name:   "memory-missing",
			break_: func(cfg *Config) { cfg.Memory = nil },
			want:   RecallIncidentContextToolName,
		},
		{
			name:   "memory-nil-entry",
			break_: func(cfg *Config) { cfg.Memory = []tool.Tool{nil} },
			want:   "Memory[0] is nil",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig(t)
			testCase.break_(&cfg)
			_, err := New(cfg)
			if !errors.Is(err, ErrIncompleteConfig) {
				t.Fatalf("New() error = %v, want ErrIncompleteConfig", err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("New() error = %q, want it to name %q", err, testCase.want)
			}
		})
	}
}

// TestEveryWiringHoleIsReportedTogether pins the accumulating report: a
// composition is assembled once at startup, and telling an operator about one
// hole at a time turns one fix into three restarts.
func TestEveryWiringHoleIsReportedTogether(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.Model, cfg.Skills, cfg.Tools.GetRunbook = nil, nil, nil
	_, err := New(cfg)
	if err == nil {
		t.Fatal("New() error = nil, want a validation failure")
	}
	for _, want := range []string{"Model is required", "Skills is required", "Tools." + GetRunbookToolName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %q, want it to name %q", err, want)
		}
	}
}

// TestInstructionDefaultsToTheCommittedText is the Go port of
// test_instruction_defaults_to_the_committed_text.
func TestInstructionDefaultsToTheCommittedText(t *testing.T) {
	t.Parallel()

	if got := newCompose(t).Instruction(); got != Instruction() {
		t.Error("Instruction() did not default to the committed text")
	}
}

// TestNoCompositionWiresItsOwnPolicyCallbacks is the Go port of
// test_no_step_wires_its_own_policy_callbacks, widened from the four workflow
// stages to every agent this package builds.
//
// Asserting the absence is what keeps the guarantee honest. If any composition
// ever re-attached its own callbacks, the application-level policy plugin would
// run twice for it — double-counting tokens, double-redacting — and nothing
// else in the system would notice.
func TestNoCompositionWiresItsOwnPolicyCallbacks(t *testing.T) {
	t.Parallel()

	for name, cfg := range allConfigs(t, newCompose(t)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if cfg.Model == nil {
				t.Error("carries no model")
			}
			slots := map[string]int{
				"BeforeModelCallbacks":  len(cfg.BeforeModelCallbacks),
				"AfterModelCallbacks":   len(cfg.AfterModelCallbacks),
				"OnModelErrorCallbacks": len(cfg.OnModelErrorCallbacks),
				"BeforeToolCallbacks":   len(cfg.BeforeToolCallbacks),
				"AfterToolCallbacks":    len(cfg.AfterToolCallbacks),
				"OnToolErrorCallbacks":  len(cfg.OnToolErrorCallbacks),
				"BeforeAgentCallbacks":  len(cfg.BeforeAgentCallbacks),
				"AfterAgentCallbacks":   len(cfg.AfterAgentCallbacks),
			}
			for slot, count := range slots {
				if count != 0 {
					t.Errorf("%s holds %d callbacks, want 0: policy is attached once, at the app boundary", slot, count)
				}
			}
		})
	}
}

// TestEveryCompositionSharesOneModel pins the single-instance rule: the model
// package validates credentials once and returns a client with one endpoint,
// one deadline, one retry budget and one connection pool, and building one per
// agent would multiply all four.
func TestEveryCompositionSharesOneModel(t *testing.T) {
	t.Parallel()

	composer := newCompose(t)
	for name, cfg := range allConfigs(t, composer) {
		if cfg.Model != composer.model {
			t.Errorf("%s carries a different model instance", name)
		}
	}
}

// TestToolGroupsAreFreshOnEveryCall pins the defensive copy: a caller that
// reorders one group must not be able to change what the next caller sees.
func TestToolGroupsAreFreshOnEveryCall(t *testing.T) {
	t.Parallel()

	surface := newCompose(t).tools
	for name, group := range map[string]func() []tool.Tool{
		"ReadTools":      surface.ReadTools,
		"KnowledgeTools": surface.KnowledgeTools,
		"ActionTools":    surface.ActionTools,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			first := group()
			want := toolNames(first)
			first[0] = namedTool{name: "swapped"}
			if got := toolNames(group()); !reflect.DeepEqual(got, want) {
				t.Errorf("second call = %v, want %v", got, want)
			}
		})
	}
}

// namedAgent builds a trivial agent for the selector's fake builders.
func namedAgent(t *testing.T, name string) agent.Agent {
	t.Helper()

	built, err := llmagent.New(llmagent.Config{Name: name, Model: fakeLLM{name: "fake-model"}})
	if err != nil {
		t.Fatalf("llmagent.New(%q) error = %v, want nil", name, err)
	}
	return built
}
