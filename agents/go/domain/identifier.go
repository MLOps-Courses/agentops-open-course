package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// MaxAuditRationaleLength bounds the human justification stored with a
	// guarded action. The write path checks it twice — before and after PII
	// redaction — because redaction can lengthen the text it rewrites.
	MaxAuditRationaleLength = 500

	// CurrentAuditSchemaVersion is the application schema version stamped on
	// every audit row. A row carrying a higher version was written by a newer
	// binary, so this one must refuse to read or extend the trail.
	CurrentAuditSchemaVersion = 1

	// CurrentRuntimeSchemaVersion marks the ADK session database (runtime.db)
	// layout. It is a string, and it is unrelated to CurrentAuditSchemaVersion;
	// conflating the two would let a session-store migration silently pass an
	// audit-schema check.
	CurrentRuntimeSchemaVersion = "1"
)

// ErrInvalid is the sentinel every parse failure in this package wraps, so a
// caller can tell "the boundary rejected this value" from "the store failed".
var ErrInvalid = errors.New("invalid domain value")

var (
	// incidentIDPattern matches the seed's incident identifiers.
	//
	// The digit class is spelled [0-9] rather than \d on purpose. Python's \d
	// is Unicode-aware, so "INC-١٢٣" (Arabic-Indic digits) passed there; Go's
	// \d is already ASCII-only, and writing the class out keeps the ASCII
	// intent visible to the next reader.
	incidentIDPattern = regexp.MustCompile(`^INC-[0-9]+$`)

	// slugPattern matches a lowercase, hyphen-separated identifier. It rejects
	// leading, trailing, and doubled hyphens, uppercase, and — because it
	// admits neither "/" nor "." — every path-traversal shape, which is what
	// lets runbook and log lookups build a file path from a slug safely.
	slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

// IncidentID is an incident identifier that has been proven to match the seed's
// shape. The only ways to obtain one are [ParseIncidentID] and
// [NormalizeIncidentID].
type IncidentID string

// ParseIncidentID accepts an already-canonical incident identifier and performs
// no normalization. This is the dataset boundary: a stored identifier that is
// not canonical is a corrupt row, not something to quietly fix up.
func ParseIncidentID(value string) (IncidentID, error) {
	if !incidentIDPattern.MatchString(value) {
		// The message describes the shape rather than naming a seed incident:
		// this package owns the identifiers, and even here they belong in
		// Reference and nowhere else.
		return "", fmt.Errorf("%w: %q is not an incident identifier of the form INC-<digits>", ErrInvalid, value)
	}
	return IncidentID(value), nil
}

// NormalizeIncidentID trims and upper-cases model-supplied input before
// validating it. This is the tool boundary, where " inc-002 " is a plausible
// thing for a language model to emit and worth accepting.
func NormalizeIncidentID(value string) (IncidentID, error) {
	return ParseIncidentID(strings.ToUpper(strings.TrimSpace(value)))
}

// Slug is a service name or runbook slug that has been proven to match the
// seed's shape. The only ways to obtain one are [ParseSlug] and
// [NormalizeSlug].
type Slug string

// ParseSlug accepts an already-canonical slug and performs no normalization.
// See [ParseIncidentID] for why the dataset boundary is strict.
func ParseSlug(value string) (Slug, error) {
	if !slugPattern.MatchString(value) {
		return "", fmt.Errorf("%w: %q is not a lowercase hyphen-separated slug", ErrInvalid, value)
	}
	return Slug(value), nil
}

// NormalizeSlug trims and lower-cases model-supplied input before validating
// it. See [NormalizeIncidentID] for why the tool boundary is lenient.
func NormalizeSlug(value string) (Slug, error) {
	return ParseSlug(strings.ToLower(strings.TrimSpace(value)))
}

// requireNonEmpty mirrors pydantic's Field(min_length=1). Fields the Python
// models left unconstrained stay unconstrained here: an empty ts or actor is
// ugly data, but rejecting it would be a behavior change, not a port.
func requireNonEmpty(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s: %w: must not be empty", field, ErrInvalid)
	}
	return nil
}
