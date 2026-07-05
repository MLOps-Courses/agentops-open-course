// Package delegation wires multi-agent delegation — a coordinator with a diagnosis sub-agent
// (Chapter 3.6).
//
// Sub-agents are how one agent hands work to another: the coordinator triages, then delegates a
// deep root-cause analysis to a specialist by transferring control (ADK routes to the named
// sub-agent, which shares the session). The same wiring underpins the A2A protocol — expose the
// specialist over A2A (the `a2a` launcher mode) and the coordinator can call it across the
// network instead of in-process.
package delegation

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/memory"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/tools"
)

const (
	diagnosisInstruction = "You are a diagnosis specialist. Given an incident id, use get_incident for its " +
		"details and runbook, get_runbook for the runbook body, and get_service_status for the service. " +
		"Explain the likely root cause in a few sentences and cite the runbook."
	coordinatorInstruction = "You are the on-call coordinator. Triage with list_incidents and " +
		"get_service_status. When a specific incident needs a root-cause analysis, delegate to the " +
		"diagnosis_agent sub-agent, then summarize its findings for the engineer."
)

// New builds the coordinator agent and its diagnosis sub-agent over the shared dataset.
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
	diagnosisTools := make([]tool.Tool, 0, len(readTools)+len(knowledge))
	diagnosisTools = append(diagnosisTools, readTools...)
	diagnosisTools = append(diagnosisTools, knowledge...)

	diagnosis, err := llmagent.New(llmagent.Config{
		Name: "diagnosis_agent", Model: model,
		Description: "Specialist that diagnoses a specific incident using its runbook and service status.",
		Instruction: diagnosisInstruction, Tools: diagnosisTools,
	})
	if err != nil {
		return nil, fmt.Errorf("building diagnosis agent: %w", err)
	}

	return llmagent.New(llmagent.Config{
		Name: "coordinator_agent", Model: model,
		Description: "On-call coordinator that triages incidents and delegates diagnosis.",
		Instruction: coordinatorInstruction,
		Tools:       readTools,
		SubAgents:   []adkagent.Agent{diagnosis},
	})
}
