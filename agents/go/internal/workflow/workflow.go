// Package workflow builds a deterministic triage → diagnose → recommend pipeline (Chapter 3.5).
//
// Where the root agent (an llmagent) decides its own steps, a SequentialAgent runs a fixed
// pipeline: each sub-agent runs in order, passing its findings forward through session state.
// In Go this is the idiomatic workflow-agents API (sequentialagent / parallelagent / loopagent);
// note that Python's classic workflow agents are deprecated in favor of its graph Workflow —
// the same pipeline, different SDK surface.
package workflow

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/memory"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/tools"
)

const (
	triageInstruction = "You triage incidents. List the unresolved incidents and pick the single most " +
		"urgent one (lowest SEV number wins). State its id, service, severity, and one-line summary."
	diagnoseInstruction = "You diagnose the incident chosen by triage. Use get_incident for its details " +
		"and its runbook (get_runbook), and get_service_status for the service. Explain the likely cause " +
		"in two or three sentences, citing the runbook."
	recommendInstruction = "You recommend remediation for the diagnosed incident. Using the runbook, give " +
		"a short, ordered list of next steps. Flag any step that needs a guarded action (restart_service, " +
		"resolve_incident) and requires human approval. Cite the runbook you used."
)

// New builds the triage → diagnose → recommend workflow over the shared dataset.
func New(ctx context.Context, cfg config.Config, store *data.Store) (adkagent.Agent, error) {
	model, err := gemini.NewModel(ctx, cfg.Model, &genai.ClientConfig{APIKey: cfg.APIKey})
	if err != nil {
		return nil, fmt.Errorf("creating gemini model %q: %w", cfg.Model, err)
	}

	readTools, err := tools.All(store)
	if err != nil {
		return nil, fmt.Errorf("building tools: %w", err)
	}
	knowledge, err := memory.KnowledgeTools(store)
	if err != nil {
		return nil, fmt.Errorf("building knowledge tools: %w", err)
	}
	diagnoseTools := make([]tool.Tool, 0, len(readTools)+len(knowledge))
	diagnoseTools = append(diagnoseTools, readTools...)
	diagnoseTools = append(diagnoseTools, knowledge...)

	triage, err := llmagent.New(llmagent.Config{
		Name: "triage", Model: model,
		Description: "Finds the most urgent unresolved incident.",
		Instruction: triageInstruction, Tools: readTools,
	})
	if err != nil {
		return nil, fmt.Errorf("building triage step: %w", err)
	}
	diagnose, err := llmagent.New(llmagent.Config{
		Name: "diagnose", Model: model,
		Description: "Explains the likely cause of the triaged incident.",
		Instruction: diagnoseInstruction, Tools: diagnoseTools,
	})
	if err != nil {
		return nil, fmt.Errorf("building diagnose step: %w", err)
	}
	recommend, err := llmagent.New(llmagent.Config{
		Name: "recommend", Model: model,
		Description: "Recommends concrete, runbook-backed remediation.",
		Instruction: recommendInstruction, Tools: knowledge,
	})
	if err != nil {
		return nil, fmt.Errorf("building recommend step: %w", err)
	}

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: adkagent.Config{
			Name:        "triage_workflow",
			Description: "Runs triage → diagnose → recommend over the current incidents.",
			SubAgents:   []adkagent.Agent{triage, diagnose, recommend},
		},
	})
}
