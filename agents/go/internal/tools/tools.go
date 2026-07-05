// Package tools defines the Ops Copilot's function tools (Chapter 3.1).
//
// Each tool wraps a data-layer call in a typed handler. ADK infers the parameter and
// result JSON schema the model sees from the handler's input/output struct types, so the
// struct field names and json tags matter. Guarded actions (restart/resolve) join in Ch. 4.5.
package tools

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
)

// listIncidentsInput filters the incident list; empty fields mean "no filter".
type listIncidentsInput struct {
	Status  string `json:"status"`  // one of open, investigating, resolved; empty for all
	Service string `json:"service"` // service name, e.g. checkout; empty for all
}

type listIncidentsOutput struct {
	Count     int             `json:"count"`
	Incidents []data.Incident `json:"incidents"`
}

type getIncidentInput struct {
	IncidentID string `json:"incident_id"` // incident identifier, e.g. INC-001
}

type getServiceStatusInput struct {
	Name string `json:"name"` // service name, e.g. checkout or inventory
}

// All builds the tools registered on the agent, each closing over the shared data store.
func All(store *data.Store) ([]tool.Tool, error) {
	all := make([]tool.Tool, 0, 3)
	for _, build := range []func(*data.Store) (tool.Tool, error){
		listIncidentsTool, getIncidentTool, getServiceStatusTool,
	} {
		tl, err := build(store)
		if err != nil {
			return nil, err
		}
		all = append(all, tl)
	}
	return all, nil
}

func listIncidentsTool(store *data.Store) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "list_incidents",
		Description: "List incidents on the platform, most recent first. Optionally filter by status and/or service.",
	}, func(_ agent.Context, in listIncidentsInput) (listIncidentsOutput, error) {
		incidents, err := store.ListIncidents(in.Status, in.Service)
		if err != nil {
			return listIncidentsOutput{}, err
		}
		return listIncidentsOutput{Count: len(incidents), Incidents: incidents}, nil
	})
}

func getIncidentTool(store *data.Store) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "get_incident",
		Description: "Get the full details of one incident by its id (e.g. INC-001), including its runbook.",
	}, func(_ agent.Context, in getIncidentInput) (map[string]any, error) {
		incident, ok, err := store.GetIncident(in.IncidentID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return map[string]any{"error": fmt.Sprintf("No incident found with id %q.", in.IncidentID)}, nil
		}
		return map[string]any{"incident": incident}, nil
	})
}

func getServiceStatusTool(store *data.Store) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "get_service_status",
		Description: "Get the current status of a service and its open incidents.",
	}, func(_ agent.Context, in getServiceStatusInput) (map[string]any, error) {
		service, ok, err := store.GetService(in.Name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return map[string]any{"error": fmt.Sprintf("No service named %q.", in.Name)}, nil
		}
		incidents, err := store.ListIncidents("", in.Name)
		if err != nil {
			return nil, err
		}
		open := []data.Incident{}
		for _, inc := range incidents {
			if inc.Status != "resolved" {
				open = append(open, inc)
			}
		}
		return map[string]any{"service": service, "open_incidents": open}, nil
	})
}
