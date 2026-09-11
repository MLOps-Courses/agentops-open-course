package tools

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// The declaration is the only thing the model ever reads about a tool. These
// tests are the Go home of what the Python suite could assert about a
// docstring: if the prose does not reach the schema, the tool is invisible in
// exactly the way that is hardest to debug.

// TestEveryToolDeclaresItselfToTheModel is the blanket contract: a name, a
// description, and a documented, closed argument schema.
func TestEveryToolDeclaresItselfToTheModel(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	for _, target := range allTools(fixture) {
		t.Run(target.Name(), func(t *testing.T) {
			declaration := declarationOf(t, target)

			if declaration.Name != target.Name() {
				t.Errorf("Declaration().Name = %q, want %q", declaration.Name, target.Name())
			}
			if declaration.Description == "" {
				t.Error("Declaration().Description is empty, so the model is told nothing about this tool")
			}
			schema := parametersOf(t, declaration)
			if schema.Type != "object" {
				t.Errorf("the argument schema type is %q, want object", schema.Type)
			}
			// A closed schema is what makes a hallucinated argument a validation
			// failure rather than a silently ignored one.
			if schema.AdditionalProperties == nil {
				t.Error("the argument schema accepts additional properties, want it closed")
			}
			for name, property := range schema.Properties {
				if property.Description == "" {
					t.Errorf("argument %q carries no description, so the model must guess what it means", name)
				}
				// A union type is what the inferrer produces for an optional
				// argument, and some providers reject one outright.
				if len(property.Types) > 0 {
					t.Errorf("argument %q declares the union type %v, want a single type", name, property.Types)
				}
			}
		})
	}
}

// TestArgumentSchemasMatchTheToolContract pins each tool's arguments and which
// of them are required, because "required" is what decides whether an omitted
// argument reaches the handler's default or fails validation first.
func TestArgumentSchemasMatchTheToolContract(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	cases := []struct {
		tool       string
		properties []string
		required   []string
	}{
		{ListIncidentsToolName, []string{"service", "status"}, nil},
		{GetIncidentToolName, []string{"incident_id"}, []string{"incident_id"}},
		{GetServiceStatusToolName, []string{"name"}, []string{"name"}},
		{SearchServiceLogsToolName, []string{"limit", "query", "service"}, []string{"service"}},
		{RestartServiceToolName, []string{"name"}, []string{"name"}},
		{ResolveIncidentToolName, []string{"incident_id"}, []string{"incident_id"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.tool, func(t *testing.T) {
			schema := parametersOf(t, declarationOf(t, toolNamed(t, fixture, testCase.tool)))

			properties := slices.Sorted(maps.Keys(schema.Properties))
			if !slices.Equal(properties, testCase.properties) {
				t.Errorf("arguments = %v, want %v", properties, testCase.properties)
			}
			required := slices.Clone(schema.Required)
			slices.Sort(required)
			if !slices.Equal(required, testCase.required) {
				t.Errorf("required = %v, want %v", required, testCase.required)
			}
		})
	}
}

// TestTheLogSearchLimitIsAPlainOptionalInteger pins the one argument whose
// three states — omitted, in range, out of range — all mean different things.
// A union type here would make the whole default-and-refuse contract depend on
// whether the provider happened to accept it.
func TestTheLogSearchLimitIsAPlainOptionalInteger(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	schema := parametersOf(t, declarationOf(t, fixture.tools.SearchServiceLogs()))

	limit, ok := schema.Properties["limit"]
	if !ok {
		t.Fatal("the log search declares no limit argument")
	}
	if limit.Type != "integer" || len(limit.Types) != 0 {
		t.Errorf("limit type = %q / %v, want a plain integer", limit.Type, limit.Types)
	}
	if slices.Contains(schema.Required, "limit") {
		t.Error("limit is required, want it optional so an omitted limit reaches the default")
	}
	// The bound is documented but deliberately not enforced by the schema: the
	// handler's own refusal names the range in a sentence the model can act on,
	// where a schema violation would surface as framework noise.
	contains(t, limit.Description, "from 1 to 100", "the limit description")
	if limit.Minimum != nil || limit.Maximum != nil {
		t.Error("the limit schema enforces the bound, want the handler's refusal to be what the model reads")
	}
}

// TestArgumentDescriptionsComeFromTheVocabulary proves the domain seam reaches
// all the way to the model. A learner who pivots the course onto another domain
// edits one file; if these examples were literals, the model would keep being
// shown identifiers that no longer exist.
func TestArgumentDescriptionsComeFromTheVocabulary(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	vocabulary := domain.Reference()

	cases := []struct {
		tool     string
		argument string
		want     string
	}{
		{ListIncidentsToolName, "service", vocabulary.Services.Checkout},
		{GetIncidentToolName, "incident_id", vocabulary.Incidents.CheckoutLatency},
		{GetServiceStatusToolName, "name", vocabulary.Services.Inventory},
		{SearchServiceLogsToolName, "service", vocabulary.Services.Checkout},
		{RestartServiceToolName, "name", vocabulary.Services.Inventory},
		{ResolveIncidentToolName, "incident_id", vocabulary.Incidents.InventoryDown},
	}
	for _, testCase := range cases {
		t.Run(testCase.tool+"."+testCase.argument, func(t *testing.T) {
			schema := parametersOf(t, declarationOf(t, toolNamed(t, fixture, testCase.tool)))
			property, ok := schema.Properties[testCase.argument]
			if !ok {
				t.Fatalf("there is no argument named %q", testCase.argument)
			}
			contains(t, property.Description, testCase.want, testCase.argument+" description")
		})
	}
}

// TestTheStatusFilterDocumentsEveryAcceptedValue keeps the description and the
// refusal in step: both are rendered from the same list, so a new status cannot
// appear in one without the other.
func TestTheStatusFilterDocumentsEveryAcceptedValue(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	schema := parametersOf(t, declarationOf(t, fixture.tools.ListIncidents()))

	description := schema.Properties["status"].Description
	for _, status := range []domain.IncidentStatus{
		domain.IncidentStatusOpen,
		domain.IncidentStatusInvestigating,
		domain.IncidentStatusResolved,
	} {
		contains(t, description, string(status), "the status description")
	}
}

// TestActionToolsDescribeTheConfirmationContract is the Python suite's
// description assertion, and every phrase in it answers an observed failure: a
// model that promises the restart happened, or that describes an approval in
// prose instead of creating the confirmation request.
func TestActionToolsDescribeTheConfirmationContract(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	names := make([]string, 0, len(fixture.tools.ActionTools()))
	for _, action := range fixture.tools.ActionTools() {
		names = append(names, action.Name())
	}
	if !slices.Equal(names, []string{RestartServiceToolName, ResolveIncidentToolName}) {
		t.Errorf("ActionTools() = %v, want the two guarded writes in registration order", names)
	}

	for _, action := range fixture.tools.ActionTools() {
		t.Run(action.Name(), func(t *testing.T) {
			description := declarationOf(t, action).Description
			for _, phrase := range []string{
				"Create an ADK confirmation request",
				"Call this tool before approval",
				"call does not",
				"A prose promise cannot create the confirmation request",
			} {
				contains(t, description, phrase, "the description")
			}
		})
	}

	// A caller that mutates the returned slice must not change the registry.
	fixture.tools.ActionTools()[0] = nil
	if fixture.tools.ActionTools()[0] == nil {
		t.Error("ActionTools() returns a shared slice; a caller mutated the registry")
	}
}

// TestOnlyTheGuardedWritesRequireConfirmation is the least-privilege half of
// the HITL contract, asserted behaviorally because ADK exposes no accessor for
// the flag: a read answers, a write pauses.
func TestOnlyTheGuardedWritesRequireConfirmation(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx, _ := toolContext(t, confirmation{absent: true}, identity{})

	for _, read := range fixture.tools.ReadTools() {
		if _, err := run(t, read, ctx, argumentsFor(read.Name())); err != nil {
			t.Errorf("%s Run() error = %v, want a read to answer without approval", read.Name(), err)
		}
	}
	for _, action := range fixture.tools.ActionTools() {
		_, err := run(t, action, ctx, argumentsFor(action.Name()))
		if err == nil {
			t.Errorf("%s Run() succeeded without approval, want it to pause", action.Name())
		}
	}
}

// argumentsFor returns a valid argument map for one tool, so a test can drive
// every tool through the same loop.
func argumentsFor(name string) map[string]any {
	switch name {
	case GetIncidentToolName, ResolveIncidentToolName:
		return map[string]any{"incident_id": inventoryIncident}
	case GetServiceStatusToolName, RestartServiceToolName:
		return map[string]any{"name": inventoryService}
	case SearchServiceLogsToolName:
		return map[string]any{"service": inventoryService}
	default:
		return map[string]any{}
	}
}

// allTools returns the six tools the agent exposes.
func allTools(fixture *fixture) []tool.Tool {
	return append(fixture.tools.ReadTools(), fixture.tools.ActionTools()...)
}

// toolNamed returns the tool with the given name.
func toolNamed(t *testing.T, fixture *fixture, name string) tool.Tool {
	t.Helper()

	for _, target := range allTools(fixture) {
		if target.Name() == name {
			return target
		}
	}
	t.Fatalf("no tool named %q", name)
	return nil
}

// declarationOf returns what ADK sends to the model for one tool.
func declarationOf(t *testing.T, target tool.Tool) *genai.FunctionDeclaration {
	t.Helper()

	describing, ok := target.(declared)
	if !ok {
		t.Fatalf("tool %q does not expose Declaration", target.Name())
	}
	declaration := describing.Declaration()
	if declaration == nil {
		t.Fatalf("tool %q declares nothing", target.Name())
	}
	return declaration
}

// parametersOf returns the argument schema out of a declaration.
func parametersOf(t *testing.T, declaration *genai.FunctionDeclaration) *jsonschema.Schema {
	t.Helper()

	schema, ok := declaration.ParametersJsonSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("the declaration carries %T as its argument schema, want a *jsonschema.Schema",
			declaration.ParametersJsonSchema)
	}
	return schema
}

// TestDescriptionsAreProseRatherThanFragments guards the one thing a schema
// cannot check: that the text a model reads is a sentence. A truncated or
// placeholder description passes every structural assertion above.
func TestDescriptionsAreProseRatherThanFragments(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)

	for _, target := range allTools(fixture) {
		declaration := declarationOf(t, target)
		first, _, _ := strings.Cut(declaration.Description, "\n")
		if !strings.HasSuffix(strings.TrimSpace(first), ".") {
			t.Errorf("%s: the description opens with %q, want a complete sentence", target.Name(), first)
		}
		schema := parametersOf(t, declaration)
		for name, property := range schema.Properties {
			if !strings.HasSuffix(strings.TrimSpace(property.Description), ".") {
				t.Errorf("%s.%s: the description is %q, want a complete sentence",
					target.Name(), name, property.Description)
			}
		}
	}
}
