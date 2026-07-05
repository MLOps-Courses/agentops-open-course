// Package actions holds the Ops Copilot's guarded mock actions — its write side (Chapter 4.5).
//
// These tools change state (a service's status, an incident's resolution) and append to the
// audit log. Each is built with functiontool RequireConfirmation:true, so ADK pauses for
// human approval (HITL) before the function runs. Everything is mock and local.
package actions

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
)

// actor is who the audit log records for agent-initiated actions.
const actor = "ops-copilot"

type restartServiceInput struct {
	Name string `json:"name"` // the service to restart, e.g. inventory
}

type resolveIncidentInput struct {
	IncidentID string `json:"incident_id"` // the incident to resolve, e.g. INC-002
}

// All builds the guarded action tools, each closing over the shared data store.
func All(store *data.Store) ([]tool.Tool, error) {
	all := make([]tool.Tool, 0, 2)
	for _, build := range []func(*data.Store) (tool.Tool, error){restartServiceTool, resolveIncidentTool} {
		tl, err := build(store)
		if err != nil {
			return nil, err
		}
		all = append(all, tl)
	}
	return all, nil
}

func restartServiceTool(store *data.Store) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:                "restart_service",
		Description:         "Restart a service (mock): flip it back to operational and write an audit entry.",
		RequireConfirmation: true, // HITL: ADK asks for approval before this runs
	}, func(_ agent.Context, in restartServiceInput) (map[string]any, error) {
		_, ok, err := store.GetService(in.Name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return map[string]any{"error": fmt.Sprintf("No service named %q; nothing to restart.", in.Name)}, nil
		}
		if _, err = store.SetServiceStatus(in.Name, "operational"); err != nil {
			return nil, err
		}
		entry, err := store.AppendAudit(actor, "restart_service", in.Name, "service restarted (mock)")
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"result": fmt.Sprintf("Service %q restarted and marked operational.", in.Name),
			"audit":  entry,
		}, nil
	})
}

func resolveIncidentTool(store *data.Store) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:                "resolve_incident",
		Description:         "Resolve an incident (mock): mark it resolved and write an audit entry.",
		RequireConfirmation: true, // HITL: ADK asks for approval before this runs
	}, func(_ agent.Context, in resolveIncidentInput) (map[string]any, error) {
		_, ok, err := store.GetIncident(in.IncidentID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return map[string]any{"error": fmt.Sprintf("No incident with id %q.", in.IncidentID)}, nil
		}
		resolved, err := store.ResolveIncident(in.IncidentID)
		if err != nil {
			return nil, err
		}
		if !resolved {
			return map[string]any{"error": fmt.Sprintf("Incident %q is already resolved.", in.IncidentID)}, nil
		}
		entry, err := store.AppendAudit(actor, "resolve_incident", in.IncidentID, "incident resolved (mock)")
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"result": fmt.Sprintf("Incident %q marked resolved.", in.IncidentID),
			"audit":  entry,
		}, nil
	})
}
