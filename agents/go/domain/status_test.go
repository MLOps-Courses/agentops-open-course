package domain

import (
	"errors"
	"testing"
)

func TestParseIncidentStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  IncidentStatus
		valid bool
	}{
		{name: "open", value: "open", want: IncidentStatusOpen, valid: true},
		{name: "investigating", value: "investigating", want: IncidentStatusInvestigating, valid: true},
		{name: "resolved", value: "resolved", want: IncidentStatusResolved, valid: true},
		{name: "empty", value: ""},
		{name: "uppercase", value: "OPEN"},
		{name: "unknown", value: "closed"},
		{name: "padded", value: " open"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseIncidentStatus(test.value)
			if test.valid {
				if err != nil {
					t.Fatalf("ParseIncidentStatus(%q) error = %v, want nil", test.value, err)
				}
				if got != test.want {
					t.Errorf("ParseIncidentStatus(%q) = %q, want %q", test.value, got, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseIncidentStatus(%q) error = %v, want ErrInvalid", test.value, err)
			}
		})
	}
}

func TestParseSeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  Severity
		valid bool
	}{
		{name: "sev1", value: "SEV1", want: SeveritySev1, valid: true},
		{name: "sev2", value: "SEV2", want: SeveritySev2, valid: true},
		{name: "sev3", value: "SEV3", want: SeveritySev3, valid: true},
		{name: "empty", value: ""},
		// The severity values are the only upper-case vocabulary in the
		// dataset, so the lower-case spelling is the mistake worth pinning.
		{name: "lowercase", value: "sev1"},
		{name: "out of range", value: "SEV4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSeverity(test.value)
			if test.valid {
				if err != nil {
					t.Fatalf("ParseSeverity(%q) error = %v, want nil", test.value, err)
				}
				if got != test.want {
					t.Errorf("ParseSeverity(%q) = %q, want %q", test.value, got, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseSeverity(%q) error = %v, want ErrInvalid", test.value, err)
			}
		})
	}
}

func TestParseServiceStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  ServiceStatus
		valid bool
	}{
		{name: "operational", value: "operational", want: ServiceStatusOperational, valid: true},
		{name: "degraded", value: "degraded", want: ServiceStatusDegraded, valid: true},
		{name: "down", value: "down", want: ServiceStatusDown, valid: true},
		{name: "empty", value: ""},
		{name: "unknown", value: "unhealthy"},
		{name: "uppercase", value: "DOWN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseServiceStatus(test.value)
			if test.valid {
				if err != nil {
					t.Fatalf("ParseServiceStatus(%q) error = %v, want nil", test.value, err)
				}
				if got != test.want {
					t.Errorf("ParseServiceStatus(%q) = %q, want %q", test.value, got, test.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseServiceStatus(%q) error = %v, want ErrInvalid", test.value, err)
			}
		})
	}
}
