package domain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file is the Go port of tests/test_domain_portability.py: the proof and
// the ratchet for the seam that lets the course be pivoted onto another domain.
//
// Two of the four Python tests reach across the whole application — they pivot
// the committed dataset on disk and then drive the read tools, the audit write
// path, the PII redactor, the MCP tool list, and the A2A agent card. Those
// belong with the packages they exercise and cannot be honestly reproduced
// here; what this file keeps is everything that is decided inside the domain:
// that both vocabularies are well formed, that both survive the validated
// boundary types unchanged, that the adapter refuses an ambiguous mapping, and
// that no other package spells a seed identifier for itself.

// domains returns every vocabulary the portability suite runs against. A
// property that holds for both is a property that does not depend on the seed.
func domains() []struct {
	name       string
	vocabulary Vocabulary
} {
	return []struct {
		name       string
		vocabulary Vocabulary
	}{
		{"reference", Reference()},
		{"pivot", pivotDomain()},
	}
}

// TestEveryDomainIsWellFormed is the invariant the adapter relies on: a
// vocabulary whose values are not unique cannot be mapped, and one whose
// identifiers do not parse cannot reach the dataset.
func TestEveryDomainIsWellFormed(t *testing.T) {
	t.Parallel()

	for _, domain := range domains() {
		t.Run(domain.name, func(t *testing.T) {
			t.Parallel()

			if duplicates := duplicateValues(domain.vocabulary.Values()); len(duplicates) > 0 {
				t.Errorf("vocabulary contains duplicate values: %v", duplicates)
			}
			for _, id := range domain.vocabulary.Incidents.Values() {
				if _, err := ParseIncidentID(id); err != nil {
					t.Errorf("incident %q: %v", id, err)
				}
			}
			for _, slug := range slices.Concat(
				domain.vocabulary.Services.Values(),
				domain.vocabulary.Runbooks.Values(),
			) {
				if _, err := ParseSlug(slug); err != nil {
					t.Errorf("slug %q: %v", slug, err)
				}
			}
		})
	}
}

// TestEveryDomainCrossesTheValidatedBoundary is the domain-owned half of
// test_pivot_domain_uses_one_adapter_surface_for_reads_writes_and_reports: the
// boundary types must accept a pivoted vocabulary exactly as they accept the
// reference one. A constraint that only fits the shipped seed would make the
// course un-pivotable, and it would fail here rather than three chapters later.
func TestEveryDomainCrossesTheValidatedBoundary(t *testing.T) {
	t.Parallel()

	for _, domain := range domains() {
		t.Run(domain.name, func(t *testing.T) {
			t.Parallel()

			service, err := NewService(serviceRow(domain.vocabulary))
			if err != nil {
				t.Fatalf("NewService() error = %v, want nil", err)
			}
			if string(service.Name()) != domain.vocabulary.Services.Inventory {
				t.Errorf("Name() = %q, want %q", service.Name(), domain.vocabulary.Services.Inventory)
			}

			incident, err := NewIncident(incidentRow(domain.vocabulary))
			if err != nil {
				t.Fatalf("NewIncident() error = %v, want nil", err)
			}
			// The incident links a service to a runbook, and both links must
			// survive the pivot: that link is what the read tools report.
			if string(incident.Service()) != domain.vocabulary.Services.Inventory {
				t.Errorf("Service() = %q, want %q", incident.Service(), domain.vocabulary.Services.Inventory)
			}
			if string(incident.Runbook()) != domain.vocabulary.Runbooks.ServiceDown {
				t.Errorf("Runbook() = %q, want %q", incident.Runbook(), domain.vocabulary.Runbooks.ServiceDown)
			}

			entry, err := NewAuditEntry(auditEntryRow(domain.vocabulary))
			if err != nil {
				t.Fatalf("NewAuditEntry() error = %v, want nil", err)
			}
			if entry.Target() != domain.vocabulary.Services.Inventory {
				t.Errorf("Target() = %q, want %q", entry.Target(), domain.vocabulary.Services.Inventory)
			}

			report, err := NewTriageReport(triageReportPayload(domain.vocabulary))
			if err != nil {
				t.Fatalf("NewTriageReport() error = %v, want nil", err)
			}
			if string(report.IncidentID()) != domain.vocabulary.Incidents.InventoryDown {
				t.Errorf("IncidentID() = %q, want %q",
					report.IncidentID(), domain.vocabulary.Incidents.InventoryDown)
			}
		})
	}
}

// TestEveryDomainDeclaresACoherentDependencyGraph is the domain-owned half of
// test_both_domains_preserve_protocol_policy_and_evaluation_contracts. The
// Python test reads the graph back out of the pivoted dataset through the read
// tools; what it can only assume, and what has to hold before that read is
// meaningful, is that the declared edges name services the vocabulary owns.
func TestEveryDomainDeclaresACoherentDependencyGraph(t *testing.T) {
	t.Parallel()

	for _, domain := range domains() {
		t.Run(domain.name, func(t *testing.T) {
			t.Parallel()

			services := domain.vocabulary.Services.Values()
			if len(domain.vocabulary.DependencyEdges) == 0 {
				t.Fatal("vocabulary declares no dependency edges")
			}
			for _, edge := range domain.vocabulary.DependencyEdges {
				for _, service := range []string{edge.From, edge.To} {
					if !slices.Contains(services, service) {
						t.Errorf("edge %+v names %q, which is not a declared service", edge, service)
					}
				}
			}
		})
	}
}

// TestPivotIsReachableThroughOneAdapterSurface proves the mapping is a
// bijection over the whole vocabulary, which is what lets a pivot be applied
// and reversed without a second application to maintain.
func TestPivotIsReachableThroughOneAdapterSurface(t *testing.T) {
	t.Parallel()

	reference, pivot := Reference(), pivotDomain()
	forward, err := Mapping(reference, pivot)
	if err != nil {
		t.Fatalf("Mapping(reference, pivot) error = %v, want nil", err)
	}
	backward, err := Mapping(pivot, reference)
	if err != nil {
		t.Fatalf("Mapping(pivot, reference) error = %v, want nil", err)
	}
	if len(forward) != len(backward) {
		t.Fatalf("forward has %d replacements, backward has %d", len(forward), len(backward))
	}
	for i, replacement := range forward {
		reversed := backward[i]
		if reversed.From != replacement.To || reversed.To != replacement.From {
			t.Errorf("replacement[%d] = %+v does not reverse %+v", i, reversed, replacement)
		}
	}
}

// ratchetBudgets records how many reference identifiers each file is allowed to
// spell for itself. A file that is absent has a budget of zero, which is the
// standard every package in this module is expected to meet: read the name from
// Reference instead.
//
// One entry exists, and it is the same one the Python track carried in
// models.py: the triage report's schema description shows the model an example
// incident identifier, and an example only teaches if it is concrete.
//
// Raising a budget is a review decision, not a formality. If a package needs a
// seed identifier, the question to answer first is why it cannot read it from
// the vocabulary.
var ratchetBudgets = map[string]int{
	"domain/report.go": 1,
}

// ratchetExempt lists the files that own the vocabulary and therefore must
// spell it.
var ratchetExempt = map[string]bool{
	"domain/vocabulary.go": true,
}

// TestDomainLiteralsCannotSpreadOutsideTheRatchetedContract is the Go port of
// test_domain_literals_cannot_spread_outside_the_ratcheted_contract.
//
// The seam is only real if it is the only place the seed identifiers appear. A
// single "INC-002" typed into another package is enough to make a pivoted
// course fail in a way that is very hard to trace, so growth is reported here,
// once, at the point it is introduced.
func TestDomainLiteralsCannotSpreadOutsideTheRatchetedContract(t *testing.T) {
	t.Parallel()

	reference := Reference()

	// Distinctive terms are safe to count anywhere inside a string: an incident
	// identifier, a runbook slug, and the gateway's name never describe an
	// implementation concept. Generic service names such as the database and
	// the cache do, so those count only when they are the whole literal.
	distinctive := slices.Concat(
		reference.Incidents.Values(),
		reference.Runbooks.Values(),
		[]string{reference.Services.Gateway},
	)
	generic := make([]string, 0, len(reference.Services.Values()))
	for _, service := range reference.Services.Values() {
		if service != reference.Services.Gateway {
			generic = append(generic, service)
		}
	}

	var violations []string
	for _, file := range goFiles(t, moduleRoot(t)) {
		if ratchetExempt[file.path] {
			continue
		}
		observed := 0
		for _, literal := range file.literals {
			for _, term := range distinctive {
				observed += countBounded(literal, term)
			}
			if slices.ContainsFunc(generic, func(term string) bool {
				return strings.EqualFold(literal, term)
			}) {
				observed++
			}
		}
		if budget := ratchetBudgets[file.path]; observed > budget {
			violations = append(violations, fmt.Sprintf(
				"%s: %d domain terms > ratchet %d", file.path, observed, budget,
			))
		}
	}
	if len(violations) > 0 {
		t.Errorf(
			"seed identifiers leaked outside the domain vocabulary:\n%s\n\n"+
				"Read the name from domain.Reference() instead of spelling it, "+
				"or raise the file's budget in ratchetBudgets with the reason.",
			strings.Join(violations, "\n"),
		)
	}
}

// TestRatchetBudgetsAreNotStale keeps the budget table honest: an entry that no
// longer corresponds to a file, or to a file that has since been cleaned up,
// silently widens the ratchet.
func TestRatchetBudgetsAreNotStale(t *testing.T) {
	t.Parallel()

	files := goFiles(t, moduleRoot(t))
	for path := range ratchetBudgets {
		if !slices.ContainsFunc(files, func(file goFile) bool { return file.path == path }) {
			t.Errorf("ratchetBudgets declares %q, which is not a file in this module", path)
		}
	}
	for path := range ratchetExempt {
		if !slices.ContainsFunc(files, func(file goFile) bool { return file.path == path }) {
			t.Errorf("ratchetExempt declares %q, which is not a file in this module", path)
		}
	}
}

// goFile is one parsed source file and every string literal it contains.
type goFile struct {
	path     string
	literals []string
}

// goFiles parses every Go source file in the module. It fails rather than skips
// on a parse error: a file that cannot be scanned is a hole in the ratchet.
func goFiles(t *testing.T, root string) []goFile {
	t.Helper()

	var files []goFile
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// testdata and vendor are invisible to the Go tool chain, and a
			// dot-directory is local state (.state, .git), never source.
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}
		literals, err := stringLiterals(path)
		if err != nil {
			return err
		}
		files = append(files, goFile{path: filepath.ToSlash(relative), literals: literals})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}
	return files
}

// stringLiterals returns the decoded value of every string literal in a file.
// Comments are not literals in Go, so — unlike the Python scan, which had to
// exclude docstrings explicitly — prose is out of scope for free.
func stringLiterals(path string) ([]string, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var literals []string
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			// An unparseable literal cannot be scanned; keep the raw text so a
			// term inside it is still counted.
			value = literal.Value
		}
		literals = append(literals, value)
		return true
	})
	return literals, nil
}

// countBounded counts case-insensitive occurrences of needle in haystack that
// are not part of a longer identifier, the Go equivalent of the Python scan's
// lookaround-guarded regex. RE2 has no lookaround, so the boundaries are
// checked directly.
func countBounded(haystack, needle string) int {
	haystack, needle = strings.ToLower(haystack), strings.ToLower(needle)
	count := 0
	for offset := 0; offset <= len(haystack)-len(needle); {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			break
		}
		start := offset + index
		if !isTermByte(haystack, start-1) && !isTermByte(haystack, start+len(needle)) {
			count++
		}
		offset = start + 1
	}
	return count
}

// isTermByte reports whether the byte at index continues an identifier.
func isTermByte(value string, index int) bool {
	if index < 0 || index >= len(value) {
		return false
	}
	character := value[index]
	return character == '-' ||
		(character >= '0' && character <= '9') ||
		(character >= 'a' && character <= 'z') ||
		(character >= 'A' && character <= 'Z')
}

// moduleRoot walks up from the test's working directory to the go.mod that
// defines this module, so the ratchet covers every package rather than only the
// one it lives in.
func moduleRoot(t *testing.T) string {
	t.Helper()

	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("no go.mod above %s", directory)
		}
		directory = parent
	}
}
