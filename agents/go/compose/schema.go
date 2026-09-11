package compose

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// The report schema is derived from [domain.TriageReportPayload] rather than
// written out here, and that is deliberate: the payload's struct tags already
// carry the field descriptions the model reads, and duplicating them would let
// the schema the model is shown drift away from the struct that validates its
// answer. It would also drag a seed incident identifier — the example in the
// incident_id description — into a second file, which the domain portability
// ratchet exists to prevent.

// errReportSchema marks a failure to build the report schema. It is a
// programming error rather than a runtime one — the schema is derived from a
// compiled-in struct — so it is reported once, at construction.
var errReportSchema = errors.New("building the triage report schema")

// severityProperty is the one field whose admissible values the Go payload
// cannot state for itself: it is typed string there, where the Python model had
// a three-valued enumeration.
const severityProperty = "severity"

// triageReportSchemaTitle names the schema on the wire. The OpenAI-compatible
// adapter uses a schema's title as the JSON-schema name it sends, so an
// explicit one is what makes a provider-side validation error nameable instead
// of arriving as "adk_response".
const triageReportSchemaTitle = "TriageReport"

// triageReportSchema builds the JSON Schema both representations derive from.
//
// One source, two renderings: [TriageReportSchema] for the agent's output
// schema, and [TriageReportJSONSchema] for the prompt that a bare model call
// carries. Deriving them separately is how the two would come to disagree.
func triageReportSchema() (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[domain.TriageReportPayload](nil)
	if err != nil {
		return nil, fmt.Errorf("%w: inferring it from the payload: %w", errReportSchema, err)
	}
	schema.Title = triageReportSchemaTitle

	severity, ok := schema.Properties[severityProperty]
	if !ok {
		// A rename in the domain payload must fail loudly here rather than
		// silently dropping the constraint: an unconstrained severity is a
		// report field downstream automation cannot branch on.
		return nil, fmt.Errorf(
			"%w: the payload has no %q property to constrain",
			errReportSchema, severityProperty,
		)
	}
	// Enum without a "format" annotation. Gemini's schema dialect documents
	// format:"enum" alongside the values, but that annotation is not part of
	// the JSON Schema subset OpenAI's strict structured outputs accepts, and
	// the account-free OpenAI-compatible path is the one every learner runs.
	// The values alone constrain both providers.
	for _, value := range []domain.Severity{domain.SeveritySev1, domain.SeveritySev2, domain.SeveritySev3} {
		severity.Enum = append(severity.Enum, string(value))
	}
	return schema, nil
}

// TriageReportSchema returns the report schema in the shape
// llmagent.Config.OutputSchema takes.
func TriageReportSchema() (*genai.Schema, error) {
	source, err := triageReportSchema()
	if err != nil {
		return nil, err
	}
	converted, err := genaiSchema(source)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errReportSchema, err)
	}
	return converted, nil
}

// TriageReportJSONSchema returns the report schema as compact JSON, for a
// prompt that has no request configuration to attach a schema to.
func TriageReportJSONSchema() (string, error) {
	source, err := triageReportSchema()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return "", fmt.Errorf("%w: encoding it for the prompt: %w", errReportSchema, err)
	}
	return string(encoded), nil
}

// jsonSchemaTypes maps the JSON Schema type names to the OpenAPI-flavored
// names genai.Schema uses.
var jsonSchemaTypes = map[string]genai.Type{
	"string":  genai.TypeString,
	"number":  genai.TypeNumber,
	"integer": genai.TypeInteger,
	"boolean": genai.TypeBoolean,
	"array":   genai.TypeArray,
	"object":  genai.TypeObject,
}

// genaiSchema converts a JSON Schema into the genai shape.
//
// It is a conversion rather than a JSON round-trip because the two dialects
// disagree on two points that matter. genai.Schema spells its type names in
// upper case, and a lower-case one reaching the Gemini path would be a type the
// API does not recognize. And genai.Schema has a single Type where JSON Schema
// allows a union: the inferrer models every Go slice and pointer as
// ["null", T], because both can be nil, and a schema that admits null for the
// report's evidence list would let a model answer with no evidence at all.
// Collapsing the union to T is what keeps the constraint.
//
// Only the subset the report needs is converted; anything else is refused
// rather than silently dropped, because a dropped constraint is invisible in
// the output and visible only as a model that suddenly answers out of schema.
func genaiSchema(source *jsonschema.Schema) (*genai.Schema, error) {
	if source == nil {
		return nil, nil
	}
	if len(source.AnyOf)+len(source.OneOf)+len(source.AllOf) > 0 || source.Ref != "" {
		return nil, errors.New("schema combinators and references are not supported")
	}

	kind, err := schemaType(source)
	if err != nil {
		return nil, err
	}
	converted := &genai.Schema{
		Title:       source.Title,
		Type:        kind,
		Description: source.Description,
		Required:    slices.Clone(source.Required),
	}
	for _, value := range source.Enum {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("enum value %v is %T, want a string", value, value)
		}
		converted.Enum = append(converted.Enum, text)
	}
	if converted.Items, err = genaiSchema(source.Items); err != nil {
		return nil, err
	}
	if len(source.Properties) == 0 {
		return converted, nil
	}

	converted.Properties = make(map[string]*genai.Schema, len(source.Properties))
	for name, property := range source.Properties {
		if converted.Properties[name], err = genaiSchema(property); err != nil {
			return nil, fmt.Errorf("property %q: %w", name, err)
		}
	}
	converted.PropertyOrdering = propertyOrdering(source)
	return converted, nil
}

// schemaType collapses a JSON Schema type union into the single genai type.
func schemaType(source *jsonschema.Schema) (genai.Type, error) {
	names := source.Types
	if source.Type != "" {
		names = []string{source.Type}
	}
	// A schema with no type at all is legal JSON Schema — it constrains
	// nothing — and genai.Schema renders that as an omitted type.
	var kind genai.Type
	for _, name := range names {
		if name == "null" {
			continue
		}
		mapped, ok := jsonSchemaTypes[name]
		if !ok {
			return "", fmt.Errorf("unknown JSON Schema type %q", name)
		}
		if kind != "" {
			return "", fmt.Errorf("type union %v has more than one non-null member", names)
		}
		kind = mapped
	}
	return kind, nil
}

// propertyOrdering returns the order the model should emit properties in:
// required fields first, in the order the payload declares them, then whatever
// remains, sorted so the result does not depend on Go's randomized map
// iteration.
func propertyOrdering(source *jsonschema.Schema) []string {
	ordering := make([]string, 0, len(source.Properties))
	seen := make(map[string]bool, len(source.Properties))
	for _, name := range source.Required {
		if _, ok := source.Properties[name]; ok && !seen[name] {
			ordering, seen[name] = append(ordering, name), true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(source.Properties)) {
		if !seen[name] {
			ordering = append(ordering, name)
		}
	}
	return ordering
}
