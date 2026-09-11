package conventions

import (
	"reflect"
	"testing"
)

// The reviewed source-derived subset, in the order CheckDocs runs it. This is the
// only other place the subset is written down, and it is an assertion rather than
// a second definition: changing which checks the scheduled freshness report runs
// must land here as a deliberate edit, not as a silent consequence of editing
// corpusChecks.
var reviewedCopiedSourceChecks = []string{
	"documented tasks",
	"task expansions",
	"source versions",
	"port contracts",
}

// The reviewed size of the full set. It is a tripwire, not a fact worth knowing:
// a new corpus check trips it, and clearing it forces the author to state whether
// the freshness report runs the new check before the number moves.
const reviewedCorpusCheckCount = 33

func TestCorpusChecksAreNamedOnceEach(t *testing.T) {
	seen := make(map[string]bool, len(corpusChecks))
	for index, check := range corpusChecks {
		if check.name == "" {
			// Nothing prints check.name at runtime; a Problem carries its page and message.
			// The name is the handle these tests and reviewedCopiedSourceChecks match on, so
			// an unnamed entry cannot be held to a subset decision at all.
			t.Fatalf("corpusChecks[%d] has no name; the reviewed subset is matched by name", index)
		}
		if check.run == nil {
			t.Fatalf("corpusChecks[%d] (%q) has no run function", index, check.name)
		}
		if seen[check.name] {
			t.Fatalf("corpusChecks names %q twice; a duplicate name makes a drift failure ambiguous", check.name)
		}
		seen[check.name] = true
	}
}

func TestCopiedSourceSubsetMatchesTheReviewedSet(t *testing.T) {
	if len(corpusChecks) != reviewedCorpusCheckCount {
		t.Fatalf("corpusChecks holds %d checks, reviewed at %d: a new corpus check must declare whether the scheduled "+
			"freshness report runs it — register it with copiedSourceCheck and add its name to reviewedCopiedSourceChecks, "+
			"or with docsCheck and move reviewedCorpusCheckCount", len(corpusChecks), reviewedCorpusCheckCount)
	}
	subset := copiedSourceChecks()
	names := make([]string, len(subset))
	for index, check := range subset {
		names[index] = check.name
	}
	if !reflect.DeepEqual(names, reviewedCopiedSourceChecks) {
		t.Fatalf("copied-source subset = %#v, reviewed %#v", names, reviewedCopiedSourceChecks)
	}
}

// The subset must stay a sub-sequence of the full set that runs the very same
// functions. This is what fails if a future edit replaces the derived subset with
// a hand-written list that has drifted from CheckDocs, in membership or in order.
func TestCopiedSourceSubsetIsASubsequenceOfTheFullSet(t *testing.T) {
	cursor := 0
	for _, wanted := range copiedSourceChecks() {
		found := false
		for cursor < len(corpusChecks) {
			candidate := corpusChecks[cursor]
			cursor++
			if candidate.name != wanted.name {
				continue
			}
			if reflect.ValueOf(candidate.run).Pointer() != reflect.ValueOf(wanted.run).Pointer() {
				t.Fatalf("the freshness subset runs a different function than CheckDocs for %q", wanted.name)
			}
			found = true
			break
		}
		if !found {
			t.Fatalf("the freshness subset names %q, which CheckDocs does not run at or after that point in its order", wanted.name)
		}
	}
}

// Every subset check must supply the words the freshness report prints for it, and
// no docs-only check may supply them: summary is what makes a check a member, so a
// blank one on a member would drop it and a filled one on a non-member would add it.
func TestCopiedSourceSummaryNamesEveryMember(t *testing.T) {
	members := make(map[string]bool, len(reviewedCopiedSourceChecks))
	for _, name := range reviewedCopiedSourceChecks {
		members[name] = true
	}
	for _, check := range corpusChecks {
		if members[check.name] && check.summary == "" {
			t.Fatalf("%q is a copied-source check with no summary for the freshness report", check.name)
		}
		if !members[check.name] && check.summary != "" {
			t.Fatalf("%q is not a reviewed copied-source check but carries the summary %q", check.name, check.summary)
		}
	}
	if want := "documented task names, task expansions, source versions, and stable ports"; CopiedSourceSummary() != want {
		t.Fatalf("CopiedSourceSummary() = %q, want %q", CopiedSourceSummary(), want)
	}
}
