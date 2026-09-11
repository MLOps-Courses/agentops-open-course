package compose

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// TestTriageReportSchemaDescribesEveryPayloadField pins the derivation: the
// schema the model is shown and the struct that validates its answer are the
// same seven fields, in the payload's own declaration order, each carrying the
// description a learner reads in the domain package.
func TestTriageReportSchemaDescribesEveryPayloadField(t *testing.T) {
	t.Parallel()

	schema, err := TriageReportSchema()
	if err != nil {
		t.Fatalf("TriageReportSchema() error = %v, want nil", err)
	}
	if schema.Type != genai.TypeObject {
		t.Errorf("Type = %q, want %q", schema.Type, genai.TypeObject)
	}
	want := []string{
		"incident_id", "severity", "affected_services",
		"hypothesis", "evidence", "recommended_runbook", "proposed_actions",
	}
	if !reflect.DeepEqual(schema.Required, want) {
		t.Errorf("Required = %v, want %v", schema.Required, want)
	}
	if !reflect.DeepEqual(schema.PropertyOrdering, want) {
		t.Errorf("PropertyOrdering = %v, want %v", schema.PropertyOrdering, want)
	}
	if len(schema.Properties) != len(want) {
		t.Errorf("Properties = %d, want %d", len(schema.Properties), len(want))
	}
	for _, name := range want {
		property, ok := schema.Properties[name]
		if !ok {
			t.Errorf("the schema has no %q property", name)
			continue
		}
		if property.Description == "" {
			t.Errorf("%q carries no description; the model is told nothing about the field", name)
		}
	}
}

// TestSchemaCollapsesTheNullableListUnions is the property the conversion
// exists for.
//
// The inferrer models every Go slice as ["null", "array"] because a slice can
// be nil. genai.Schema cannot express a union at all, and a schema that admitted
// null for the evidence list would let a model answer with no evidence — which
// the domain constructor then rejects, turning a schema hint into a hard failure
// one layer later.
func TestSchemaCollapsesTheNullableListUnions(t *testing.T) {
	t.Parallel()

	source, err := triageReportSchema()
	if err != nil {
		t.Fatalf("triageReportSchema() error = %v, want nil", err)
	}
	unions := 0
	for name, property := range source.Properties {
		if slices.Contains(property.Types, "null") {
			unions++
			if len(property.Types) != 2 {
				t.Errorf("%q has types %v; the fixture assumes a two-member union", name, property.Types)
			}
		}
	}
	if unions == 0 {
		t.Fatal("the inferred schema has no nullable union left to collapse; this test proves nothing")
	}

	schema, err := TriageReportSchema()
	if err != nil {
		t.Fatalf("TriageReportSchema() error = %v, want nil", err)
	}
	for _, name := range []string{"affected_services", "evidence", "proposed_actions"} {
		property := schema.Properties[name]
		if property.Type != genai.TypeArray {
			t.Errorf("%q Type = %q, want %q", name, property.Type, genai.TypeArray)
		}
		if property.Items == nil || property.Items.Type != genai.TypeString {
			t.Errorf("%q Items = %+v, want a string element", name, property.Items)
		}
	}
}

// TestSeverityIsConstrainedToTheDomainValues covers the one constraint the Go
// payload cannot state for itself, where the Python model had an enumeration.
func TestSeverityIsConstrainedToTheDomainValues(t *testing.T) {
	t.Parallel()

	schema, err := TriageReportSchema()
	if err != nil {
		t.Fatalf("TriageReportSchema() error = %v, want nil", err)
	}
	severity := schema.Properties[severityProperty]
	want := []string{string(domain.SeveritySev1), string(domain.SeveritySev2), string(domain.SeveritySev3)}
	if !reflect.DeepEqual(severity.Enum, want) {
		t.Errorf("Enum = %v, want %v", severity.Enum, want)
	}
	// A "format" annotation would be rejected by the strict JSON-Schema subset
	// the OpenAI-compatible path uses, which is the path every learner runs.
	if severity.Format != "" {
		t.Errorf("Format = %q, want it unset", severity.Format)
	}
	for _, value := range severity.Enum {
		if _, err := domain.ParseSeverity(value); err != nil {
			t.Errorf("the schema admits %q, which the domain rejects: %v", value, err)
		}
	}
}

// TestReportPromptCarriesTheSameSchema pins the single source: the prompt a
// bare model call carries and the schema the agent attaches are two renderings
// of one document, so a model driven either way is told the same thing.
func TestReportPromptCarriesTheSameSchema(t *testing.T) {
	t.Parallel()

	encoded, err := TriageReportJSONSchema()
	if err != nil {
		t.Fatalf("TriageReportJSONSchema() error = %v, want nil", err)
	}
	if strings.ContainsAny(encoded, "\n\t") {
		t.Error("the prompt schema is not compact; it wastes context on whitespace")
	}
	var decoded map[string]any
	if err = json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, want nil", err)
	}
	if decoded["title"] != triageReportSchemaTitle {
		t.Errorf("title = %v, want %q", decoded["title"], triageReportSchemaTitle)
	}

	prompt, err := ReportPrompt(domain.Reference().Incidents.InventoryDown)
	if err != nil {
		t.Fatalf("ReportPrompt() error = %v, want nil", err)
	}
	if !strings.Contains(prompt, encoded) {
		t.Error("the prompt does not carry the schema it claims to")
	}
}

// TestGenaiSchemaRefusesWhatItCannotCarry pins the refusals.
//
// A dropped constraint is invisible in the converted schema and visible only
// much later, as a model that suddenly answers out of schema, so the conversion
// fails instead of silently losing one.
func TestGenaiSchemaRefusesWhatItCannotCarry(t *testing.T) {
	t.Parallel()

	for name, source := range map[string]*jsonschema.Schema{
		"anyOf":            {AnyOf: []*jsonschema.Schema{{Type: "string"}}},
		"oneOf":            {OneOf: []*jsonschema.Schema{{Type: "string"}}},
		"allOf":            {AllOf: []*jsonschema.Schema{{Type: "string"}}},
		"ref":              {Ref: "#/$defs/other"},
		"unknown type":     {Type: "tuple"},
		"two-member union": {Types: []string{"string", "number"}},
		"non-string enum":  {Type: "string", Enum: []any{1}},
		"bad property":     {Type: "object", Properties: map[string]*jsonschema.Schema{"x": {Type: "tuple"}}},
		"bad items":        {Type: "array", Items: &jsonschema.Schema{Type: "tuple"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := genaiSchema(source); err == nil {
				t.Error("genaiSchema() error = nil, want a refusal")
			}
		})
	}
}

// TestGenaiSchemaCarriesTheScalarTypes covers the type table itself, which is
// the part a JSON round-trip would have got wrong: genai spells its type names
// in upper case and the Gemini path would not recognize a lower-case one.
func TestGenaiSchemaCarriesTheScalarTypes(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]genai.Type{
		"string":  genai.TypeString,
		"number":  genai.TypeNumber,
		"integer": genai.TypeInteger,
		"boolean": genai.TypeBoolean,
		"array":   genai.TypeArray,
		"object":  genai.TypeObject,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			converted, err := genaiSchema(&jsonschema.Schema{Type: name})
			if err != nil {
				t.Fatalf("genaiSchema() error = %v, want nil", err)
			}
			if converted.Type != want {
				t.Errorf("Type = %q, want %q", converted.Type, want)
			}
		})
	}

	// A schema with no type constrains nothing, and genai renders that as an
	// omitted type rather than as an error.
	converted, err := genaiSchema(&jsonschema.Schema{})
	if err != nil {
		t.Fatalf("genaiSchema() on an untyped schema error = %v, want nil", err)
	}
	if converted.Type != "" {
		t.Errorf("Type = %q, want it unset", converted.Type)
	}
	if converted, err := genaiSchema(nil); err != nil || converted != nil {
		t.Errorf("genaiSchema(nil) = %v, %v; want nil, nil", converted, err)
	}
}

// TestPropertyOrderingIsDeterministic pins the ordering rule against Go's
// randomized map iteration: an unrequired property still has to land in the same
// place on every run, or two builds of the same agent would show the model two
// different schemas.
func TestPropertyOrderingIsDeterministic(t *testing.T) {
	t.Parallel()

	source := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"zulu": {Type: "string"}, "alpha": {Type: "string"},
			"kilo": {Type: "string"}, "bravo": {Type: "string"},
		},
		// "quebec" is required but has no property; it must not appear.
		Required: []string{"kilo", "zulu", "quebec"},
	}
	want := []string{"kilo", "zulu", "alpha", "bravo"}
	for range 8 {
		if got := propertyOrdering(source); !reflect.DeepEqual(got, want) {
			t.Fatalf("propertyOrdering() = %v, want %v", got, want)
		}
	}
}
