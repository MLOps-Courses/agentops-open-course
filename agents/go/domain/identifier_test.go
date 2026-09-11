package domain

import (
	"errors"
	"testing"
)

func TestParseIncidentID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  IncidentID
		valid bool
	}{
		{name: "single digit", value: "INC-1", want: "INC-1", valid: true},
		{name: "zero padded", value: "INC-042", want: "INC-042", valid: true},
		{name: "empty", value: ""},
		{name: "lowercase prefix", value: "inc-1"},
		{name: "no digits", value: "INC-"},
		{name: "trailing letters", value: "INC-1a"},
		{name: "leading text", value: "XINC-1"},
		{name: "surrounding spaces", value: " INC-1 "},
		// Python's re.search-backed pydantic pattern accepted a trailing
		// newline, because "$" also matches just before one. Go's MatchString
		// with an anchored pattern does not, and the stricter reading is the
		// one the dataset actually wants.
		{name: "trailing newline", value: "INC-1\n"},
		// Python's \d is Unicode-aware, so Arabic-Indic digits parsed there.
		// The Go pattern is ASCII-only by construction.
		{name: "non ascii digits", value: "INC-١٢٣"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseIncidentID(test.value)
			if test.valid {
				if err != nil {
					t.Fatalf("ParseIncidentID(%q) error = %v, want nil", test.value, err)
				}
				if got != test.want {
					t.Errorf("ParseIncidentID(%q) = %q, want %q", test.value, got, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseIncidentID(%q) error = %v, want ErrInvalid", test.value, err)
			}
			if got != "" {
				t.Errorf("ParseIncidentID(%q) = %q, want the zero value on failure", test.value, got)
			}
		})
	}
}

func TestNormalizeIncidentID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  IncidentID
		valid bool
	}{
		{name: "already canonical", value: "INC-042", want: "INC-042", valid: true},
		{name: "lowercase", value: "inc-042", want: "INC-042", valid: true},
		{name: "surrounding whitespace", value: "\t INC-042 \n", want: "INC-042", valid: true},
		{name: "mixed case and whitespace", value: " Inc-042 ", want: "INC-042", valid: true},
		{name: "inner whitespace", value: "INC - 042"},
		{name: "empty", value: ""},
		{name: "whitespace only", value: "   "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeIncidentID(test.value)
			if test.valid {
				if err != nil {
					t.Fatalf("NormalizeIncidentID(%q) error = %v, want nil", test.value, err)
				}
				if got != test.want {
					t.Errorf("NormalizeIncidentID(%q) = %q, want %q", test.value, got, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NormalizeIncidentID(%q) error = %v, want ErrInvalid", test.value, err)
			}
		})
	}
}

func TestParseSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  Slug
		valid bool
	}{
		{name: "single segment", value: "orders", want: "orders", valid: true},
		{name: "digits only", value: "0", want: "0", valid: true},
		{name: "alphanumeric", value: "web2", want: "web2", valid: true},
		{name: "two segments", value: "web-tier", want: "web-tier", valid: true},
		{name: "three segments", value: "web-tier-2", want: "web-tier-2", valid: true},
		{name: "empty", value: ""},
		{name: "leading hyphen", value: "-web"},
		{name: "trailing hyphen", value: "web-"},
		{name: "doubled hyphen", value: "web--tier"},
		{name: "uppercase", value: "Web"},
		{name: "underscore", value: "web_tier"},
		{name: "inner space", value: "web tier"},
		{name: "surrounding space", value: " web "},
		{name: "trailing newline", value: "web\n"},
		// The slug pattern is also the path-traversal guard: runbook and log
		// lookups build a file path out of a slug, so "/" and "." must never
		// survive parsing.
		{name: "parent directory", value: "../web"},
		{name: "path separator", value: "logs/web"},
		{name: "dot", value: "web.tier"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSlug(test.value)
			if test.valid {
				if err != nil {
					t.Fatalf("ParseSlug(%q) error = %v, want nil", test.value, err)
				}
				if got != test.want {
					t.Errorf("ParseSlug(%q) = %q, want %q", test.value, got, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseSlug(%q) error = %v, want ErrInvalid", test.value, err)
			}
			if got != "" {
				t.Errorf("ParseSlug(%q) = %q, want the zero value on failure", test.value, got)
			}
		})
	}
}

func TestNormalizeSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  Slug
		valid bool
	}{
		{name: "already canonical", value: "web-tier", want: "web-tier", valid: true},
		{name: "uppercase", value: "WEB-TIER", want: "web-tier", valid: true},
		{name: "surrounding whitespace", value: " \tWeb-Tier\n ", want: "web-tier", valid: true},
		{name: "inner space", value: "web tier"},
		{name: "empty", value: ""},
		{name: "whitespace only", value: " \t\n"},
		{name: "traversal survives lowercasing", value: "../WEB"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeSlug(test.value)
			if test.valid {
				if err != nil {
					t.Fatalf("NormalizeSlug(%q) error = %v, want nil", test.value, err)
				}
				if got != test.want {
					t.Errorf("NormalizeSlug(%q) = %q, want %q", test.value, got, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NormalizeSlug(%q) error = %v, want ErrInvalid", test.value, err)
			}
		})
	}
}

// TestSchemaVersionsAreDistinct guards the one constant pair a reader is likely
// to conflate: the audit schema version is an integer for audit_log rows, and
// the runtime schema version is a string marking the ADK session database.
func TestSchemaVersionsAreDistinct(t *testing.T) {
	t.Parallel()

	if CurrentAuditSchemaVersion != 1 {
		t.Errorf("CurrentAuditSchemaVersion = %d, want 1", CurrentAuditSchemaVersion)
	}
	if CurrentRuntimeSchemaVersion != "1" {
		t.Errorf("CurrentRuntimeSchemaVersion = %q, want \"1\"", CurrentRuntimeSchemaVersion)
	}
	if MaxAuditRationaleLength != 500 {
		t.Errorf("MaxAuditRationaleLength = %d, want 500", MaxAuditRationaleLength)
	}
}
