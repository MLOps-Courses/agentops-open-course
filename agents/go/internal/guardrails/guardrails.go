// Package guardrails provides safety callbacks that run before a tool executes (Chapter 4.5).
//
// A BeforeToolCallback can inspect the arguments and short-circuit the call by returning a
// result map (which the model sees instead of running the tool), or return (nil, nil) to let
// the call proceed. ValidateActions fails fast on malformed inputs to the mutating actions —
// a boundary check kept separate from the actions' own business logic.
package guardrails

import (
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

var incidentID = regexp.MustCompile(`^INC-\d+$`)

// ValidateActions is a BeforeToolCallback: it rejects malformed inputs to mutating actions.
func ValidateActions(_ agent.Context, tl tool.Tool, args map[string]any) (map[string]any, error) {
	switch tl.Name() {
	case "resolve_incident":
		id, _ := args["incident_id"].(string)
		if !incidentID.MatchString(id) {
			return map[string]any{"error": fmt.Sprintf("Refusing to resolve %q: expected an id like INC-002.", id)}, nil
		}
	case "restart_service":
		name, _ := args["name"].(string)
		if strings.TrimSpace(name) == "" {
			return map[string]any{"error": "Refusing to restart: no service name was provided."}, nil
		}
	}
	return nil, nil // not a guarded action, or inputs are valid: proceed
}
