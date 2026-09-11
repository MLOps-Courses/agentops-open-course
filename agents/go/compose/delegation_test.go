package compose

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// TestCoordinatorDelegatesToBothSpecialists is the Go port of
// test_coordinator_delegates_to_both_specialists.
func TestCoordinatorDelegatesToBothSpecialists(t *testing.T) {
	t.Parallel()

	coordinator, err := newCompose(t).CoordinatorAgent()
	if err != nil {
		t.Fatalf("CoordinatorAgent() error = %v, want nil", err)
	}
	if coordinator.Name() != CoordinatorName {
		t.Errorf("Name() = %q, want %q", coordinator.Name(), CoordinatorName)
	}
	want := []string{DiagnosisName, RemediationName}
	got := make([]string, 0, len(coordinator.SubAgents()))
	for _, subAgent := range coordinator.SubAgents() {
		got = append(got, subAgent.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SubAgents() = %v, want %v", got, want)
	}
}

// TestDelegationRespectsToolBoundaries is the Go port of
// test_delegation_respects_tool_boundaries and
// test_diagnosis_specialist_is_grounded.
//
// Least privilege by construction: each specialist physically lacks the other's
// tools. This is the assertion the whole chapter rests on — a prompt injection
// in a log line cannot make the diagnosis specialist mutate state, because there
// is no write tool in its list to call.
func TestDelegationRespectsToolBoundaries(t *testing.T) {
	t.Parallel()

	composer := newCompose(t)
	readNames := []string{
		tools.ListIncidentsToolName,
		tools.GetIncidentToolName,
		tools.GetServiceStatusToolName,
		tools.SearchServiceLogsToolName,
	}
	knowledgeNames := []string{GetRunbookToolName, SearchRunbooksToolName}
	writeNames := []string{tools.RestartServiceToolName, tools.ResolveIncidentToolName}

	diagnosis := toolNames(composer.diagnosisConfig().Tools)
	remediation := toolNames(composer.remediationConfig().Tools)
	coordinator := toolNames(composer.coordinatorConfig(nil).Tools)

	// The diagnosis specialist is grounded: it holds every read.
	if want := slices.Concat(readNames, knowledgeNames); !reflect.DeepEqual(diagnosis, want) {
		t.Errorf("diagnosis tools = %v, want %v", diagnosis, want)
	}
	// The remediation specialist holds the two guarded writes and nothing else.
	if !reflect.DeepEqual(remediation, writeNames) {
		t.Errorf("remediation tools = %v, want exactly %v", remediation, writeNames)
	}
	// The coordinator itself holds no write: acting requires delegation.
	if !reflect.DeepEqual(coordinator, readNames) {
		t.Errorf("coordinator tools = %v, want %v", coordinator, readNames)
	}

	for _, disjoint := range []struct {
		who    string
		held   []string
		denied []string
	}{
		{who: DiagnosisName, held: diagnosis, denied: writeNames},
		{who: RemediationName, held: remediation, denied: slices.Concat(readNames, knowledgeNames)},
		{who: CoordinatorName, held: coordinator, denied: writeNames},
	} {
		for _, denied := range disjoint.denied {
			if slices.Contains(disjoint.held, denied) {
				t.Errorf("%s holds %q, which its least-privilege contract denies it", disjoint.who, denied)
			}
		}
	}
}

// TestRemediationActionsAreTheGuardedToolValues is the Go port of
// test_remediation_actions_still_require_confirmation.
//
// Go's tool.Tool exposes no confirmation flag, so the assertion is made where
// it is actually stronger: the specialist holds the exact tool values the tools
// package built with RequireConfirmation, compared by identity. A look-alike
// with the right name would fail this, and would have passed the Python test's
// attribute check only because Python could read the flag back.
func TestRemediationActionsAreTheGuardedToolValues(t *testing.T) {
	t.Parallel()

	composer := newCompose(t)
	want := []tool.Tool{composer.tools.RestartService, composer.tools.ResolveIncident}
	got := composer.remediationConfig().Tools
	if len(got) != len(want) {
		t.Fatalf("remediation holds %d tools, want %d", len(got), len(want))
	}
	for i, expected := range want {
		if got[i] != expected {
			t.Errorf("remediation tool %d is not the guarded %s value the tools package built", i, expected.Name())
		}
	}
}

// TestDelegationPromptContracts is the Go port of
// test_delegation_prompt_contracts and
// test_coordinator_eval_dependencies_remain_explicit.
func TestDelegationPromptContracts(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name        string
		instruction string
		phrases     []string
	}{
		{
			name:        DiagnosisName,
			instruction: diagnosisInstruction,
			phrases: []string{
				"use get_incident", "cite the runbook", "post-action verification",
				"label recovery unverified", "You cannot take actions",
			},
		},
		{
			name:        RemediationName,
			instruction: remediationInstruction,
			phrases: []string{
				"exact guarded action", "coordinator handoff is not approval",
				"never claim service recovery from the action response",
				"ADK creates its confirmation request",
				"Never act without a diagnosis",
			},
		},
		{
			name:        CoordinatorName,
			instruction: coordinatorInstruction,
			phrases: []string{
				"delegate to the " + DiagnosisName, "delegate to the " + RemediationName,
				"expected recovery evidence", "audit evidence only", "delegate back to " + DiagnosisName,
				"never claim recovery from the action response alone",
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			for _, phrase := range testCase.phrases {
				if !strings.Contains(testCase.instruction, phrase) {
					t.Errorf("instruction lost the phrase %q the evalsets depend on", phrase)
				}
			}
		})
	}
}

// TestDelegationInstructionsNameTheRealSubAgents pins the routing contract the
// digests cannot: the coordinator's instruction tells the model to transfer to
// two agents by name, and a rename that missed the prose would leave it
// pointing at agents that no longer exist.
func TestDelegationInstructionsNameTheRealSubAgents(t *testing.T) {
	t.Parallel()

	coordinator, err := newCompose(t).CoordinatorAgent()
	if err != nil {
		t.Fatalf("CoordinatorAgent() error = %v, want nil", err)
	}
	for _, subAgent := range coordinator.SubAgents() {
		if !strings.Contains(coordinatorInstruction, subAgent.Name()) {
			t.Errorf("the coordinator instruction never names its %q sub-agent", subAgent.Name())
		}
		if found := coordinator.FindSubAgent(subAgent.Name()); found == nil {
			t.Errorf("FindSubAgent(%q) = nil; ADK cannot route the transfer", subAgent.Name())
		}
	}
}

// TestTransferTopologyStaysOpen pins the default: neither direction is
// disallowed, which is what lets the coordinator hand work down and a specialist
// hand its findings back — the round trip its instruction asks for when it
// verifies an approved action.
func TestTransferTopologyStaysOpen(t *testing.T) {
	t.Parallel()

	composer := newCompose(t)
	for name, cfg := range map[string]bool{
		DiagnosisName + "/parent":   composer.diagnosisConfig().DisallowTransferToParent,
		DiagnosisName + "/peers":    composer.diagnosisConfig().DisallowTransferToPeers,
		RemediationName + "/parent": composer.remediationConfig().DisallowTransferToParent,
		RemediationName + "/peers":  composer.remediationConfig().DisallowTransferToPeers,
		CoordinatorName + "/parent": composer.coordinatorConfig(nil).DisallowTransferToParent,
		CoordinatorName + "/peers":  composer.coordinatorConfig(nil).DisallowTransferToPeers,
	} {
		if cfg {
			t.Errorf("%s disallows the coordinator-specialist round trip", name)
		}
	}
}

// TestSpecialistsResolveThroughTheCoordinatorTree proves the delegation target
// ADK actually uses is reachable.
//
// A transfer is a lookup by name through the agent tree, so two distinct
// specialists that both resolve to themselves from the coordinator is precisely
// what makes the coordinator's "delegate to the diagnosis_agent sub-agent"
// instruction executable rather than aspirational.
func TestSpecialistsResolveThroughTheCoordinatorTree(t *testing.T) {
	t.Parallel()

	coordinator, err := newCompose(t).CoordinatorAgent()
	if err != nil {
		t.Fatalf("CoordinatorAgent() error = %v, want nil", err)
	}
	specialists := coordinator.SubAgents()
	if len(specialists) != 2 || specialists[0] == specialists[1] {
		t.Fatalf("SubAgents() = %v, want two distinct specialists", specialists)
	}
	for _, subAgent := range specialists {
		if found := coordinator.FindSubAgent(subAgent.Name()); found != subAgent {
			t.Errorf("FindSubAgent(%q) did not resolve to the attached specialist", subAgent.Name())
		}
	}
	if found := coordinator.FindSubAgent(ReportAgentName); found != nil {
		t.Errorf("FindSubAgent(%q) = %v, want nil: the report is not a delegation target", ReportAgentName, found)
	}
}
