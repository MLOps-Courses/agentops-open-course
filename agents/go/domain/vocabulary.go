// Package domain owns the reference operations vocabulary and the validated
// types that cross the SQLite, tool, and model boundaries.
//
// It is the single owner of the seed identifiers: every other package reads a
// service name, incident identifier, or runbook slug from [Reference] instead
// of spelling it as a string literal. That seam is what lets a learner pivot
// the course onto their own domain by editing one file, and the ratchet in
// portability_test.go keeps it shut.
//
// The package deliberately depends on nothing outside the standard library:
// every other package in the module imports it, so a dependency here is a
// dependency everywhere.
package domain

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// IncidentVocabulary names the incident identifiers shipped in the immutable
// reference seed.
//
// Field order is load-bearing. [IncidentVocabulary.Values] reports the fields
// positionally and [Mapping] zips two vocabularies by position, so reordering a
// field silently remaps the dataset. TestVocabularyValuesFollowFieldOrder
// pins the two together.
type IncidentVocabulary struct {
	CheckoutLatency string
	InventoryDown   string
	PaymentsErrors  string
	AuthErrors      string
	SearchLatency   string
	CheckoutDisk    string
	CacheMemory     string
	DatabaseCascade string
	CheckoutCascade string
	GatewayMemory   string
}

// Values returns the incident identifiers in stable field order.
func (v IncidentVocabulary) Values() []string {
	return []string{
		v.CheckoutLatency,
		v.InventoryDown,
		v.PaymentsErrors,
		v.AuthErrors,
		v.SearchLatency,
		v.CheckoutDisk,
		v.CacheMemory,
		v.DatabaseCascade,
		v.CheckoutCascade,
		v.GatewayMemory,
	}
}

// ServiceVocabulary names the services shipped in the immutable reference seed.
// Field order is load-bearing; see [IncidentVocabulary].
type ServiceVocabulary struct {
	Checkout  string
	Payments  string
	Auth      string
	Search    string
	Inventory string
	Database  string
	Cache     string
	Gateway   string
}

// Values returns the service names in stable field order.
func (v ServiceVocabulary) Values() []string {
	return []string{
		v.Checkout,
		v.Payments,
		v.Auth,
		v.Search,
		v.Inventory,
		v.Database,
		v.Cache,
		v.Gateway,
	}
}

// RunbookVocabulary names the runbook slugs shipped with the reference agent.
// Field order is load-bearing; see [IncidentVocabulary].
type RunbookVocabulary struct {
	CascadeFailure     string
	DeploymentRollback string
	DiskFull           string
	ElevatedErrors     string
	HighLatency        string
	MemoryLeak         string
	ServiceDown        string
}

// Values returns the runbook slugs in stable field order.
func (v RunbookVocabulary) Values() []string {
	return []string{
		v.CascadeFailure,
		v.DeploymentRollback,
		v.DiskFull,
		v.ElevatedErrors,
		v.HighLatency,
		v.MemoryLeak,
		v.ServiceDown,
	}
}

// DependencyEdge is one directed edge of the service dependency graph: a
// failure in From propagates to To. The reference seed encodes the same graph
// as a chain of linked incidents, and a pivoted domain must keep it intact.
type DependencyEdge struct {
	From string
	To   string
}

// Vocabulary is the complete set of domain-owned identifiers plus the service
// dependency graph that relates them.
type Vocabulary struct {
	Incidents       IncidentVocabulary
	Services        ServiceVocabulary
	Runbooks        RunbookVocabulary
	DependencyEdges []DependencyEdge
}

// Values returns every identifier the vocabulary owns, incidents then services
// then runbooks, each group in field order. [Mapping] pairs two vocabularies
// through exactly this ordering, so it is the definition of "positional".
func (v Vocabulary) Values() []string {
	return slices.Concat(v.Incidents.Values(), v.Services.Values(), v.Runbooks.Values())
}

// Reference returns the vocabulary of the committed reference seed
// (agents/data/incidents.db and its runbooks).
//
// It is a function rather than an exported variable because Go has no frozen
// value: a package-level var could be reassigned, and its DependencyEdges slice
// mutated in place, by any importer. Returning a fresh value with a fresh slice
// on every call makes the single owner actually own it. The allocation is
// irrelevant next to a SQLite query or a model call.
func Reference() Vocabulary {
	return Vocabulary{
		Incidents: IncidentVocabulary{
			CheckoutLatency: "INC-001",
			InventoryDown:   "INC-002",
			PaymentsErrors:  "INC-003",
			AuthErrors:      "INC-004",
			SearchLatency:   "INC-005",
			CheckoutDisk:    "INC-006",
			CacheMemory:     "INC-007",
			DatabaseCascade: "INC-008",
			CheckoutCascade: "INC-009",
			GatewayMemory:   "INC-010",
		},
		Services: ServiceVocabulary{
			Checkout:  "checkout",
			Payments:  "payments",
			Auth:      "auth",
			Search:    "search",
			Inventory: "inventory",
			Database:  "database",
			Cache:     "cache",
			// The gateway service is the one name that is not its field name;
			// the seed calls it api-gateway.
			Gateway: "api-gateway",
		},
		Runbooks: RunbookVocabulary{
			CascadeFailure:     "cascade-failure",
			DeploymentRollback: "deployment-rollback",
			DiskFull:           "disk-full",
			ElevatedErrors:     "elevated-errors",
			HighLatency:        "high-latency",
			MemoryLeak:         "memory-leak",
			ServiceDown:        "service-down",
		},
		DependencyEdges: []DependencyEdge{
			{From: "cache", To: "database"},
			{From: "database", To: "checkout"},
			{From: "inventory", To: "checkout"},
		},
	}
}

// VocabularyRole labels which side of a [Mapping] a problem was found on.
type VocabularyRole string

const (
	// SourceVocabulary is the vocabulary a dataset is being pivoted away from.
	SourceVocabulary VocabularyRole = "source"
	// TargetVocabulary is the vocabulary a dataset is being pivoted onto.
	TargetVocabulary VocabularyRole = "target"
)

// DuplicateValuesError reports a vocabulary whose identifiers are not unique.
//
// This is fatal rather than cosmetic: a pivot replaces identifiers by value, so
// two fields sharing one value would collapse into a single replacement and
// silently rewrite the dataset the wrong way. [Mapping] refuses to build a
// mapping it cannot invert.
type DuplicateValuesError struct {
	Role   VocabularyRole
	Values []string
}

func (e *DuplicateValuesError) Error() string {
	// The wording matches the Python adapter so a learner comparing the two
	// tracks reads the same sentence.
	return fmt.Sprintf(
		"%s domain vocabulary contains duplicate values: %s",
		e.Role, strings.Join(e.Values, ", "),
	)
}

// Replacement is one identifier rewrite: every occurrence of From becomes To.
type Replacement struct {
	From string
	To   string
}

// Mapping pairs the source vocabulary with the target vocabulary positionally
// and returns the replacements that pivot a dataset from one to the other.
//
// It returns an ordered slice rather than a map because replacement order is
// observable — a later replacement can rewrite text an earlier one produced —
// and Go map iteration is randomized. Python's dict preserved insertion order
// for free; here it has to be a slice to stay deterministic.
//
// The two vocabularies always have the same length: [Vocabulary] is a struct,
// so the "zip strict" check Python needed is enforced by the compiler.
func Mapping(source, target Vocabulary) ([]Replacement, error) {
	sourceValues := source.Values()
	targetValues := target.Values()
	for _, side := range []struct {
		role   VocabularyRole
		values []string
	}{
		{SourceVocabulary, sourceValues},
		{TargetVocabulary, targetValues},
	} {
		if duplicates := duplicateValues(side.values); len(duplicates) > 0 {
			return nil, &DuplicateValuesError{Role: side.role, Values: duplicates}
		}
	}

	replacements := make([]Replacement, 0, len(sourceValues))
	for i, from := range sourceValues {
		replacements = append(replacements, Replacement{From: from, To: targetValues[i]})
	}
	return replacements, nil
}

// duplicateValues returns the sorted set of values that appear more than once.
func duplicateValues(values []string) []string {
	counts := make(map[string]int, len(values))
	for _, value := range values {
		counts[value]++
	}
	duplicates := make([]string, 0, len(counts))
	for value, count := range counts {
		if count > 1 {
			duplicates = append(duplicates, value)
		}
	}
	slices.SortFunc(duplicates, cmp.Compare)
	return duplicates
}
