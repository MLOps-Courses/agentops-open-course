package domain

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// fieldNames returns the exported field names of a struct in declaration order.
func fieldNames[T any]() []string {
	fields := reflect.VisibleFields(reflect.TypeFor[T]())
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.Name)
	}
	return names
}

// labelledFields returns a vocabulary group whose every field holds its own
// field name, so Values can be compared against the declaration order directly.
func labelledFields[T any]() T {
	var group T
	value := reflect.ValueOf(&group).Elem()
	for i := range value.NumField() {
		value.Field(i).SetString(value.Type().Field(i).Name)
	}
	return group
}

// TestVocabularyValuesFollowFieldOrder pins Values to the struct's declaration
// order. Python got this for free from dataclasses.astuple; a hand-written
// Values can drift from its struct silently, and a drifted Values remaps a
// pivoted dataset onto the wrong identifiers without any error.
func TestVocabularyValuesFollowFieldOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []string
		order  []string
	}{
		{"incidents", labelledFields[IncidentVocabulary]().Values(), fieldNames[IncidentVocabulary]()},
		{"services", labelledFields[ServiceVocabulary]().Values(), fieldNames[ServiceVocabulary]()},
		{"runbooks", labelledFields[RunbookVocabulary]().Values(), fieldNames[RunbookVocabulary]()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if !slices.Equal(test.values, test.order) {
				t.Errorf("Values() = %v, want declaration order %v", test.values, test.order)
			}
		})
	}
}

// TestVocabularyValuesConcatenatesEveryGroup pins the order Mapping zips by.
func TestVocabularyValuesConcatenatesEveryGroup(t *testing.T) {
	t.Parallel()

	reference := Reference()
	want := slices.Concat(
		reference.Incidents.Values(),
		reference.Services.Values(),
		reference.Runbooks.Values(),
	)
	if got := reference.Values(); !slices.Equal(got, want) {
		t.Errorf("Values() = %v, want %v", got, want)
	}
}

// TestReferenceIsIndependentPerCall proves the single owner actually owns the
// vocabulary: a caller that mutates what it received must not be able to change
// what the next caller sees.
func TestReferenceIsIndependentPerCall(t *testing.T) {
	t.Parallel()

	first := Reference()
	first.DependencyEdges[0] = DependencyEdge{From: "tampered", To: "tampered"}
	first.Services.Checkout = "tampered"

	second := Reference()
	if second.DependencyEdges[0].From == "tampered" {
		t.Error("mutating the returned dependency edges changed the reference vocabulary")
	}
	if second.Services.Checkout == "tampered" {
		t.Error("mutating the returned services changed the reference vocabulary")
	}
}

func TestMappingPairsVocabulariesPositionally(t *testing.T) {
	t.Parallel()

	source := Reference()
	target := pivotDomain()
	replacements, err := Mapping(source, target)
	if err != nil {
		t.Fatalf("Mapping() error = %v, want nil", err)
	}

	sourceValues := source.Values()
	targetValues := target.Values()
	if len(replacements) != len(sourceValues) {
		t.Fatalf("Mapping() returned %d replacements, want %d", len(replacements), len(sourceValues))
	}
	for i, replacement := range replacements {
		if replacement.From != sourceValues[i] || replacement.To != targetValues[i] {
			t.Errorf(
				"replacement[%d] = %+v, want {From:%s To:%s}",
				i, replacement, sourceValues[i], targetValues[i],
			)
		}
	}
}

// TestMappingRejectsDuplicateValues is the Go port of
// test_domain_adapter_rejects_source_and_target_vocabulary_collisions: an
// adapter must fail before duplicate vocabulary can overwrite a mapping.
func TestMappingRejectsDuplicateValues(t *testing.T) {
	t.Parallel()

	collidingReference := Reference()
	collidingReference.Services.Payments = collidingReference.Services.Checkout
	collidingPivot := pivotDomain()
	collidingPivot.Services.Payments = collidingPivot.Services.Checkout

	tests := []struct {
		name   string
		source Vocabulary
		target Vocabulary
		role   VocabularyRole
		values []string
	}{
		{
			name:   "source collision",
			source: collidingReference,
			target: pivotDomain(),
			role:   SourceVocabulary,
			values: []string{Reference().Services.Checkout},
		},
		{
			name:   "target collision",
			source: Reference(),
			target: collidingPivot,
			role:   TargetVocabulary,
			values: []string{pivotDomain().Services.Checkout},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			replacements, err := Mapping(test.source, test.target)
			if replacements != nil {
				t.Errorf("Mapping() replacements = %v, want nil", replacements)
			}
			var duplicates *DuplicateValuesError
			if !errors.As(err, &duplicates) {
				t.Fatalf("Mapping() error = %v, want a *DuplicateValuesError", err)
			}
			if duplicates.Role != test.role {
				t.Errorf("Role = %q, want %q", duplicates.Role, test.role)
			}
			if !slices.Equal(duplicates.Values, test.values) {
				t.Errorf("Values = %v, want %v", duplicates.Values, test.values)
			}
		})
	}
}

// TestDuplicateValuesErrorMessage keeps the sentence identical to the Python
// adapter's, so the two tracks fail the same way for the same reason.
func TestDuplicateValuesErrorMessage(t *testing.T) {
	t.Parallel()

	err := &DuplicateValuesError{Role: SourceVocabulary, Values: []string{"beta", "alpha"}}
	const want = "source domain vocabulary contains duplicate values: beta, alpha"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
