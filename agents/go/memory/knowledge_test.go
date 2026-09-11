package memory

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// This file is the Go port of tests/test_memory.py: the runbook knowledge
// tools, and the deterministic offline scorer behind search_runbooks.

func TestGetRunbookReturnsAKnownRunbook(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	result := mustRun[GetRunbookResult](t, fixture.memory.GetRunbook(),
		engineerContext(t), map[string]any{"slug": highLatencyBook})

	if result.Slug != highLatencyBook {
		t.Errorf("slug = %q, want %q", result.Slug, highLatencyBook)
	}
	// The whole file, not a summary: the model is expected to read the procedure
	// and cite it, which it cannot do from an excerpt someone else chose.
	contains(t, result.Content, "# Runbook:", "content")
	if result.Error != "" {
		t.Errorf("error = %q, want it absent", result.Error)
	}
}

func TestGetRunbookOnAnUnknownSlugListsWhatExists(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	result := mustRun[GetRunbookResult](t, fixture.memory.GetRunbook(),
		engineerContext(t), map[string]any{"slug": "nope"})

	if result.Error == "" {
		t.Fatalf("result = %+v, want an error", result)
	}
	// A dead end that names the alternatives is a next step; one that does not
	// just gets guessed at again.
	contains(t, result.Error, highLatencyBook, "error")
	if result.Content != "" {
		t.Errorf("content = %q, want it absent", result.Content)
	}
}

func TestGetRunbookRefusesPathTraversal(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// The slug is model-controlled, so a traversal payload must be refused at the
	// boundary rather than read off disk.
	for _, payload := range []string{
		"../../../../etc/passwd",
		"/etc/passwd",
		"..",
		"high latency",
		// A real slug wearing a traversal suffix, so the refusal cannot be an
		// accident of the payload never resembling a runbook. Built rather than
		// spelled: the ratchet in domain/portability_test.go counts seed
		// identifiers typed outside the vocabulary, including in tests.
		strings.ToUpper(highLatencyBook) + "/../../etc/passwd",
	} {
		t.Run(payload, func(t *testing.T) {
			t.Parallel()

			result := mustRun[GetRunbookResult](t, fixture.memory.GetRunbook(),
				engineerContext(t), map[string]any{"slug": payload})
			if result.Error == "" {
				t.Fatalf("result = %+v, want a refusal", result)
			}
			if strings.Contains(result.Error, "root:") || result.Content != "" {
				t.Errorf("refusal leaked file content: %+v", result)
			}
		})
	}
}

func TestSearchRunbooksRanksTheRelevantRunbookFirst(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	result := searchRunbooks(t, fixture, "service is completely down and returning 503", nil)

	if *result.Count < 1 {
		t.Fatalf("count = %d, want at least one match", *result.Count)
	}
	if result.Runbooks[0].Slug != serviceDownBook {
		t.Errorf("best match = %q, want %q", result.Runbooks[0].Slug, serviceDownBook)
	}
}

func TestSearchRunbooksReportsTheKeywordMode(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	result := searchRunbooks(t, fixture, "service down", nil)

	// The default offline path must label its retrieval mode, so a downstream
	// reader can tell a keyword result from a semantic one without reading logs.
	if result.Retrieval != keywordMode {
		t.Errorf("retrieval = %q, want %q", result.Retrieval, keywordMode)
	}
}

func TestSearchRunbooksRespectsTheRequestedLimit(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	result := searchRunbooks(t, fixture, "latency errors disk deploy service", pointer(2))

	if *result.Count > 2 {
		t.Errorf("count = %d, want at most 2", *result.Count)
	}
	if len(result.Runbooks) != *result.Count {
		t.Errorf("count = %d but %d runbooks were returned", *result.Count, len(result.Runbooks))
	}
}

func TestSearchRunbooksCapsAModelControlledLimit(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// The limit reaches this tool from the model, so it is a bound to enforce
	// rather than a request to honor: a hundred runbooks would flood the context.
	result := searchRunbooks(t, fixture, "service down latency errors disk deploy", pointer(100))

	if *result.Count != maxRunbookResults {
		t.Errorf("count = %d, want %d", *result.Count, maxRunbookResults)
	}
}

func TestSearchRunbooksWithNoMatchIsAnEmptyResult(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	result := searchRunbooks(t, fixture, "zzzznomatchzzzz", nil)

	if *result.Count != 0 {
		t.Errorf("count = %d, want 0", *result.Count)
	}
	// An empty list, not an omitted key: "nothing matched your query" and "this
	// tool told you nothing" are different answers.
	if result.Runbooks == nil || len(result.Runbooks) != 0 {
		t.Errorf("runbooks = %#v, want an empty list", result.Runbooks)
	}
}

func TestSearchRunbooksTreatsANonPositiveLimitAsTheDefault(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	const query = "service down latency errors disk deploy"
	expected := searchRunbooks(t, fixture, query, nil)

	// Zero results is never what a search meant to ask for, so a non-positive
	// limit takes the default rather than returning nothing.
	for _, limit := range []int{0, -5} {
		if actual := searchRunbooks(t, fixture, query, pointer(limit)); !reflect.DeepEqual(actual, expected) {
			t.Errorf("limit %d returned %d runbooks, want the default %d", limit, *actual.Count, *expected.Count)
		}
	}
}

func TestSearchRunbooksScoresRareTermsAndSlugMatchesHigher(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// "cascade" is rare across the corpus and also names a runbook, so both
	// halves of the scorer point the same way. If either half regressed, a query
	// made only of that term would stop putting its own runbook first.
	result := searchRunbooks(t, fixture, "cascade", nil)

	if *result.Count == 0 {
		t.Fatal("count = 0, want the cascade runbook")
	}
	if result.Runbooks[0].Slug != cascadeBook {
		t.Errorf("best match = %q, want %q", result.Runbooks[0].Slug, cascadeBook)
	}
}

func TestSearchRunbooksIsDeterministicForTiedScores(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	const query = "the a of and in to"
	// Every term here is below the minimum length or ubiquitous, so nothing
	// distinguishes the documents. Reproducible evaluation depends on that case
	// having one answer rather than whichever order the map happened to yield.
	first := searchRunbooks(t, fixture, query, nil)
	for range 5 {
		if next := searchRunbooks(t, fixture, query, nil); !reflect.DeepEqual(next, first) {
			t.Fatalf("repeated search returned %+v, want %+v", next, first)
		}
	}
}

func TestKnowledgeReadsGoThroughTheResilienceGuard(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := engineerContext(t)
	mustRun[GetRunbookResult](t, fixture.memory.GetRunbook(), ctx, map[string]any{"slug": highLatencyBook})
	mustRun[SearchRunbooksResult](t, fixture.memory.SearchRunbooks(), ctx, map[string]any{"query": "latency"})

	// Both runbook reads are idempotent, so both carry the deadline, the bounded
	// retries and the circuit breaker. A read that skipped the guard would keep
	// calling a dead dependency.
	want := []string{GetRunbookToolName, SearchRunbooksToolName}
	if !reflect.DeepEqual(fixture.guard.names.all(), want) {
		t.Errorf("guarded tools = %v, want %v", fixture.guard.names.all(), want)
	}
}

// TestKnowledgeReadsHonorTheGuardsBudget pins the narrow promise
// AGENT_TOOL_TIMEOUT_S makes for the two runbook reads. Their corpus is read
// with os.ReadFile and os.ReadDir, neither of which can be called back once it
// is blocked, so the deadline is enforced at the points where it still decides
// something: before each filesystem read. Without those checks the configured
// deadline would be inert here — both tools would run to completion on a budget
// that is already spent — while the doc comments claimed the Guard bounded them.
func TestKnowledgeReadsHonorTheGuardsBudget(t *testing.T) {
	t.Parallel()

	t.Run("an expired budget stops the runbook read from starting", func(t *testing.T) {
		t.Parallel()

		reads := 0
		fixture := newFixture(t, func(opts *options) {
			opts.store = func(real Store) Store {
				return stubStore{Store: real, readRunbook: func(string) (string, bool, error) {
					reads++
					return "", false, nil
				}}
			}
		})
		fixture.guard.wrap = func(ctx context.Context) context.Context {
			expired, cancel := context.WithCancel(ctx)
			cancel()
			return expired
		}

		_, err := run(t, fixture.memory.GetRunbook(), engineerContext(t),
			map[string]any{"slug": highLatencyBook})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to wrap %v", err, context.Canceled)
		}
		if reads != 0 {
			t.Errorf("the store was read %d times, want the spent budget to stop the read", reads)
		}
	})

	t.Run("a budget spent during the runbook read stops the listing that follows", func(t *testing.T) {
		t.Parallel()

		attempt, spend := context.WithCancel(context.Background())
		t.Cleanup(spend)
		fixture := newFixture(t, func(opts *options) {
			opts.store = func(real Store) Store {
				// The budget runs out while the uncancelable read is in flight, which
				// is what the second checkpoint exists for: composing the "no runbook
				// named this" refusal costs another filesystem read, and nobody is
				// waiting for the answer anymore.
				return stubStore{Store: real, readRunbook: func(string) (string, bool, error) {
					spend()
					return "", false, nil
				}}
			}
		})
		fixture.guard.wrap = func(context.Context) context.Context { return attempt }

		_, err := run(t, fixture.memory.GetRunbook(), engineerContext(t),
			map[string]any{"slug": highLatencyBook})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to wrap %v", err, context.Canceled)
		}
	})

	t.Run("an expired budget stops the corpus listing from starting", func(t *testing.T) {
		t.Parallel()

		listings := 0
		fixture := newFixture(t, func(opts *options) {
			opts.store = func(real Store) Store {
				return stubStore{Store: real, listRunbookSlugs: func() ([]string, error) {
					listings++
					return nil, nil
				}}
			}
		})
		fixture.guard.wrap = func(ctx context.Context) context.Context {
			expired, cancel := context.WithCancel(ctx)
			cancel()
			return expired
		}

		_, err := run(t, fixture.memory.SearchRunbooks(), engineerContext(t),
			map[string]any{"query": "latency"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to wrap %v", err, context.Canceled)
		}
		if listings != 0 {
			t.Errorf("the corpus was listed %d times, want the spent budget to stop the listing", listings)
		}
	})

	t.Run("a budget spent during the corpus listing stops the reads that follow", func(t *testing.T) {
		t.Parallel()

		attempt, spend := context.WithCancel(context.Background())
		t.Cleanup(spend)
		reads := 0
		fixture := newFixture(t, func(opts *options) {
			opts.store = func(real Store) Store {
				// A search reads every runbook, so the per-slug checkpoint is what keeps
				// an exhausted budget from paying for the whole corpus one file at a time.
				return stubStore{
					Store: real,
					listRunbookSlugs: func() ([]string, error) {
						spend()
						return []string{highLatencyBook, serviceDownBook}, nil
					},
					readRunbook: func(string) (string, bool, error) {
						reads++
						return "", false, nil
					},
				}
			}
		})
		fixture.guard.wrap = func(context.Context) context.Context { return attempt }

		_, err := run(t, fixture.memory.SearchRunbooks(), engineerContext(t),
			map[string]any{"query": "latency"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to wrap %v", err, context.Canceled)
		}
		if reads != 0 {
			t.Errorf("%d runbooks were read, want the spent budget to stop the first read", reads)
		}
	})
}

func TestKnowledgeToolsAreRegisteredInOrder(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	tools := fixture.memory.KnowledgeTools()
	want := []string{GetRunbookToolName, SearchRunbooksToolName}

	if len(tools) != len(want) {
		t.Fatalf("KnowledgeTools() returned %d tools, want %d", len(tools), len(want))
	}
	for index, name := range want {
		if tools[index].Name() != name {
			t.Errorf("KnowledgeTools()[%d] = %q, want %q", index, tools[index].Name(), name)
		}
	}
	// A fresh slice on every call: a caller that reorders it must not be able to
	// change what the next caller sees.
	tools[0] = nil
	if fixture.memory.KnowledgeTools()[0] == nil {
		t.Error("KnowledgeTools() handed out its own backing array")
	}
}

func TestEveryToolArgumentIsDescribedToTheModel(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	for _, target := range append(fixture.memory.KnowledgeTools(), fixture.memory.MemoryTools()...) {
		t.Run(target.Name(), func(t *testing.T) {
			t.Parallel()

			describing, ok := target.(declared)
			if !ok {
				t.Fatalf("tool %q does not expose a declaration", target.Name())
			}
			declaration := describing.Declaration()
			if declaration.Name != target.Name() {
				t.Errorf("Declaration().Name = %q, want %q", declaration.Name, target.Name())
			}
			if declaration.Description == "" {
				t.Errorf("tool %q has no description", target.Name())
			}
			schema, ok := declaration.ParametersJsonSchema.(*jsonschema.Schema)
			if !ok {
				t.Fatalf("tool %q carries %T as its argument schema, want a *jsonschema.Schema",
					target.Name(), declaration.ParametersJsonSchema)
			}
			if schema.Type != "object" {
				t.Errorf("the argument schema of %q is %q, want object", target.Name(), schema.Type)
			}
			// A closed schema makes a hallucinated argument a validation failure
			// rather than a silently ignored one.
			if schema.AdditionalProperties == nil {
				t.Errorf("the argument schema of %q accepts additional properties, want it closed", target.Name())
			}
			for name, property := range schema.Properties {
				// An undocumented argument is a silent capability loss: the model sees
				// the field and has no idea what belongs in it.
				if property.Description == "" {
					t.Errorf("argument %q of %q carries no description", name, target.Name())
				}
				// A union type is what the inferrer produces for an optional argument,
				// and some providers reject one outright.
				if len(property.Types) > 0 {
					t.Errorf("argument %q of %q declares the union type %v, want a single type",
						name, target.Name(), property.Types)
				}
			}
		})
	}
}

func TestSearchRefusesAnUnreadableCorpus(t *testing.T) {
	t.Parallel()

	failure := errAssertion
	fixture := newFixture(t, func(o *options) {
		o.store = func(real Store) Store {
			return stubStore{Store: real, listRunbookSlugs: func() ([]string, error) { return nil, failure }}
		}
	})
	// A knowledge base that cannot be listed is a failure, not an empty answer.
	// Reporting zero matches would tell the model the runbooks say nothing.
	if _, err := run(t, fixture.memory.SearchRunbooks(), engineerContext(t),
		map[string]any{"query": "latency"}); err == nil {
		t.Fatal("SearchRunbooks() error = nil, want the store failure")
	}
	if _, err := run(t, fixture.memory.GetRunbook(), engineerContext(t),
		map[string]any{"slug": "nope"}); err == nil {
		t.Fatal("GetRunbook() error = nil, want the store failure")
	}
}

// searchRunbooks runs the search tool and decodes its wire result.
func searchRunbooks(t *testing.T, fixture *fixture, query string, limit *int) SearchRunbooksResult {
	t.Helper()

	args := map[string]any{"query": query}
	if limit != nil {
		args["limit"] = *limit
	}
	return mustRun[SearchRunbooksResult](t, fixture.memory.SearchRunbooks(), engineerContext(t), args)
}
