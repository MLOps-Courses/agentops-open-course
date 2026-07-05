// Package agent defines the Ops Copilot — the AgentOps Open Course reference agent (Go track).
//
// An on-call assistant that helps engineers triage and resolve incidents for a fictional
// platform, using a 100% local, bundled dataset. It grows chapter by chapter: tools (Ch. 3.1),
// skills (3.2), MCP (3.3), memory/RAG (3.4), workflows (3.5), and A2A delegation (3.6).
package agent

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/actions"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/guardrails"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/memory"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/tools"
)

// Agent identity, shared with other agents (A2A) and the developer UI.
const (
	Name        = "agentops_agent"
	Description = "An on-call Ops Copilot that triages and resolves incidents from a local dataset."
)

// Instruction is the persona and operating rules — explicit so behavior is reproducible and evaluable.
const Instruction = `You are the Ops Copilot, an on-call assistant for a fictional online platform.
You help engineers triage and resolve incidents quickly and safely.

Operating rules:
- Always ground your answers in the tools. Never invent incidents, services, or statuses.
- When asked about incidents or a service, call the matching tool and report exactly what it returns.
- To recommend a fix, consult the runbooks: an incident carries a runbook slug — fetch it with
  get_runbook, or use search_runbooks to find guidance by symptom. Cite the runbook you used.
- Taking an action (restart_service, resolve_incident) changes state and needs human approval —
  propose it, and only call the tool when the engineer asks you to. Report the audit result.
- Refer to incidents by id (e.g. INC-001) and services by name (e.g. checkout).
- Be concise and actionable: lead with the answer, then the key details.
- If a tool returns an error or no data, say so plainly instead of guessing.`

// New builds the reference agent on a native Gemini model, wired to the bundled dataset.
// The caller owns the returned data.Store and should Close it on shutdown.
func New(ctx context.Context, cfg config.Config) (adkagent.Agent, *data.Store, error) {
	model, err := gemini.NewModel(ctx, cfg.Model, &genai.ClientConfig{APIKey: cfg.APIKey})
	if err != nil {
		return nil, nil, fmt.Errorf("creating gemini model %q: %w", cfg.Model, err)
	}

	store, err := data.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("opening dataset: %w", err)
	}

	toolset, err := tools.All(store)
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("building tools: %w", err)
	}

	knowledge, err := memory.KnowledgeTools(store)
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("building knowledge tools: %w", err)
	}

	actionTools, err := actions.All(store)
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("building action tools: %w", err)
	}

	allTools := make([]tool.Tool, 0, len(toolset)+len(knowledge)+len(actionTools))
	allTools = append(allTools, toolset...)
	allTools = append(allTools, knowledge...)
	allTools = append(allTools, actionTools...)

	a, err := llmagent.New(llmagent.Config{
		Name:                Name,
		Model:               model,
		Description:         Description,
		Instruction:         Instruction,
		Tools:               allTools,
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{guardrails.ValidateActions},
	})
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("building agent: %w", err)
	}
	return a, store, nil
}
