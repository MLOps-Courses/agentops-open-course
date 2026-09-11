package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

const (
	// minTermLength drops query terms that carry little retrieval signal. It is a
	// length filter, not a stop-list: "the" and "cpu" are treated alike, which is
	// what keeps the scorer domain-independent.
	minTermLength = 3
	// maxRunbookResults caps what a model-controlled limit can ask for.
	maxRunbookResults = 3
)

// The retrieval modes reported in the result. Naming the mode is what makes a
// semantic-to-keyword fallback visible to whoever reads the transcript rather
// than only to whoever reads the logs.
const (
	keywordMode  = "keyword"
	semanticMode = "semantic"
)

// slugBoostFactor multiplies the corpus size when a query term appears inside a
// runbook's slug. The slug names the failure mode, so a match there is a far
// stronger signal than a match anywhere in the body.
const slugBoostFactor = 5

// termPattern splits a query into alphanumeric terms. It runs over the
// lowercased query, so the character class needs no upper-case range.
var termPattern = regexp.MustCompile(`[a-z0-9]+`)

// GetRunbookArgs are the arguments of the get_runbook tool.
type GetRunbookArgs struct {
	Slug string `json:"slug"`
}

// GetRunbookResult is what get_runbook returns: the runbook, or a refusal.
//
// Every field is `omitzero`, so exactly one shape reaches the model: either
// Error alone, or Slug with Content.
type GetRunbookResult struct {
	Slug    string `json:"slug,omitzero"`
	Content string `json:"content,omitzero"`
	Error   string `json:"error,omitzero"`
}

// SearchRunbooksArgs are the arguments of the search_runbooks tool.
//
// Limit is a pointer because "absent" and "zero" mean different things and both
// occur: an omitted limit takes the default, and an explicit non-positive one
// is a model mistake that also takes the default rather than returning nothing.
type SearchRunbooksArgs struct {
	Limit *int   `json:"limit,omitempty"`
	Query string `json:"query"`
}

// RunbookMatch is one retrieved runbook. The similarity distance a semantic
// search computes is deliberately not carried here: it is an artifact of which
// retrieval mode happened to run, and a model that saw it would start reasoning
// about a number that has no meaning in the keyword mode.
type RunbookMatch struct {
	Slug    string `json:"slug"`
	Content string `json:"content"`
}

// SearchRunbooksResult is what search_runbooks returns.
//
// There is no error field: a query that matches nothing is a successful search
// with a count of zero, and a genuine failure — an unreadable corpus, a broken
// vector store — is a Go error the policy plane classifies.
type SearchRunbooksResult struct {
	Count     *int           `json:"count,omitzero"`
	Retrieval string         `json:"retrieval,omitzero"`
	Runbooks  []RunbookMatch `json:"runbooks,omitzero"`
}

// runGetRunbook is the get_runbook handler.
//
// Each read follows the same two-layer shape as the tools package: the handler
// ADK calls puts the work through the [Guard], and the inner method does that
// work against a context the Guard controls.
// --8<-- [start:get-runbook]
func (m *Memory) runGetRunbook(ctx agent.Context, args GetRunbookArgs) (GetRunbookResult, error) {
	var result GetRunbookResult
	err := m.guard(callContext(ctx), GetRunbookToolName, func(ctx context.Context) error {
		var err error
		result, err = m.readRunbook(ctx, args.Slug)
		return err
	})
	return result, err
}

// --8<-- [end:get-runbook]

// readRunbook fetches one runbook by slug.
//
// The slug is model-controlled, so it is parsed at the boundary and only the
// trusted value is used afterwards — the read, the error message and the result
// all speak the normalized slug. A traversal payload cannot be expressed as a
// [domain.Slug], so it never reaches the filesystem.
//
// It honors the Guard's per-attempt context at every point it can, which is a
// narrower promise than a database read makes — and the difference is worth
// stating rather than glossing over. The knowledge base lives on the filesystem,
// and [Store.ReadRunbook] wraps os.ReadFile, which takes no context: the
// deadline therefore decides whether each filesystem read is *started*, but
// cannot preempt one already blocked on a stalled mount. So the checkpoints
// below are the real bound, and the limit is documented rather than implied
// away.
func (m *Memory) readRunbook(ctx context.Context, slug string) (GetRunbookResult, error) {
	normalized, err := domain.NormalizeSlug(slug)
	if err != nil {
		known, listErr := m.knownRunbooks(ctx)
		if listErr != nil {
			return GetRunbookResult{}, listErr
		}
		// The raw slug, quoted: echoing what the model actually sent is what makes
		// the refusal legible, and quoting keeps a payload full of spaces readable.
		return GetRunbookResult{Error: fmt.Sprintf(
			"Invalid runbook slug %q. Available runbooks: %s.", slug, known,
		)}, nil
	}
	// The last point at which an exhausted budget can still stop the work rather
	// than pay for it: once the read below is under way, nothing can call it back.
	if expired := ctx.Err(); expired != nil {
		return GetRunbookResult{}, fmt.Errorf("read runbook %s: %w", normalized, expired)
	}
	content, found, err := m.store.ReadRunbook(string(normalized))
	if err != nil {
		return GetRunbookResult{}, fmt.Errorf("read runbook %s: %w", normalized, err)
	}
	if !found {
		known, listErr := m.knownRunbooks(ctx)
		if listErr != nil {
			return GetRunbookResult{}, listErr
		}
		// The normalized slug this time: the input parsed, so the honest report is
		// the name that was actually looked up.
		return GetRunbookResult{Error: fmt.Sprintf(
			"No runbook named %q. Available runbooks: %s.", normalized, known,
		)}, nil
	}
	return GetRunbookResult{Slug: string(normalized), Content: content}, nil
}

// knownRunbooks renders the available slugs for a refusal, sorted.
//
// Naming what does exist turns a dead end into a next step: the model guessed a
// slug, and the answer tells it which ones are real.
//
// It carries the same checkpoint as the read it composes a refusal for:
// [Store.ListRunbookSlugs] wraps os.ReadDir, which cannot be canceled either, so
// a budget that expired while the read ran has to be caught before the listing
// starts rather than during it.
func (m *Memory) knownRunbooks(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("list the runbook knowledge base: %w", err)
	}
	slugs, err := m.store.ListRunbookSlugs()
	if err != nil {
		return "", fmt.Errorf("list the runbook knowledge base: %w", err)
	}
	return strings.Join(slugs, ", "), nil
}

// runSearchRunbooks is the search_runbooks handler.
func (m *Memory) runSearchRunbooks(ctx agent.Context, args SearchRunbooksArgs) (SearchRunbooksResult, error) {
	var result SearchRunbooksResult
	err := m.guard(callContext(ctx), SearchRunbooksToolName, func(guarded context.Context) error {
		var err error
		result, err = m.rankRunbooks(guarded, args)
		return err
	})
	return result, err
}

// rankRunbooks ranks the knowledge base for a free-text query.
func (m *Memory) rankRunbooks(ctx context.Context, args SearchRunbooksArgs) (SearchRunbooksResult, error) {
	limit := maxRunbookResults
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit <= 0 {
		// A non-positive limit is treated as the default, not as "return nothing":
		// zero results is never what a search meant to ask for.
		limit = maxRunbookResults
	}
	limit = min(limit, maxRunbookResults)

	if m.semanticRetrieval {
		matches, err := m.retriever.SemanticSearch(ctx, args.Query, limit)
		var unavailable *EmbeddingUnavailableError
		switch {
		case err == nil:
			runbooks := make([]RunbookMatch, 0, len(matches))
			for _, match := range matches {
				runbooks = append(runbooks, RunbookMatch{Slug: match.Slug, Content: match.Content})
			}
			count := len(runbooks)
			return SearchRunbooksResult{Count: &count, Retrieval: semanticMode, Runbooks: runbooks}, nil
		case errors.As(err, &unavailable):
			// An explicit, logged fallback — never a silent degradation. The result
			// says "keyword" as well, so the transcript carries it too.
			m.logger.WarnContext(ctx, "semantic retrieval unavailable, falling back to keywords", "error", err)
		default:
			// Anything else — an empty corpus, a non-positive limit — is a real
			// failure. Falling back would hide it, and the keyword scorer would fail
			// on the same corpus anyway.
			return SearchRunbooksResult{}, err
		}
	}
	return m.keywordSearch(ctx, args.Query, limit)
}

// runbookDocument is one runbook as the keyword scorer sees it: its slug, its
// markdown, and the lowercased copy every substring test runs against.
type runbookDocument struct {
	slug     string
	content  string
	haystack string
}

// scoredRunbook is one runbook that matched, with the score that ranked it.
type scoredRunbook struct {
	slug    string
	content string
	score   float64
}

// keywordSearch is the deterministic, offline TF-IDF-style scorer.
//
// A term counts more when it is rare across the corpus, so ubiquitous words do
// not dominate, and a term that appears in a runbook's slug earns a large flat
// boost. Nothing here consults a model or a network, which is what keeps the
// default path reproducible for evaluation.
//
// It takes the Guard's per-attempt context for the corpus read alone: the
// scoring below is pure CPU work over data already in memory, and the reads that
// feed it are the only part a deadline can still act on — see [Memory.readRunbook]
// for why that bound is a narrow one.
func (m *Memory) keywordSearch(ctx context.Context, query string, limit int) (SearchRunbooksResult, error) {
	terms := queryTerms(query)
	documents, err := m.runbookCorpus(ctx)
	if err != nil {
		return SearchRunbooksResult{}, err
	}
	// An empty corpus would divide by zero below. One is the neutral denominator:
	// with no documents nothing scores anyway.
	total := len(documents)
	if total == 0 {
		total = 1
	}

	// Document frequency: how many runbooks contain each distinct term. Computed
	// once per distinct term, then reused for every repeat of that term in the
	// query — a repeated term is deliberately scored twice, because saying
	// "latency latency" is a stronger signal than saying it once.
	documentFrequency := make(map[string]int, len(terms))
	for _, term := range terms {
		if _, seen := documentFrequency[term]; seen {
			continue
		}
		frequency := 0
		for _, document := range documents {
			if strings.Contains(document.haystack, term) {
				frequency++
			}
		}
		documentFrequency[term] = frequency
	}

	ranked := make([]scoredRunbook, 0, len(documents))
	for _, document := range documents {
		score := 0.0
		for _, term := range terms {
			if frequency := documentFrequency[term]; frequency > 0 {
				// Rarer term, higher weight. The count is of non-overlapping
				// substrings, not of words: "latency" inside "high-latency" counts.
				score += float64(strings.Count(document.haystack, term)) * (float64(total) / float64(frequency))
			}
			if strings.Contains(document.slug, term) {
				score += float64(total * slugBoostFactor)
			}
		}
		if score > 0 {
			ranked = append(ranked, scoredRunbook{slug: document.slug, content: document.content, score: score})
		}
	}
	// Score descending, then slug ascending. The slug tie-break is what keeps the
	// ordering deterministic for equal scores, which reproducible evaluations
	// depend on.
	slices.SortFunc(ranked, func(a, b scoredRunbook) int {
		if order := cmp.Compare(b.score, a.score); order != 0 {
			return order
		}
		return cmp.Compare(a.slug, b.slug)
	})

	top := ranked[:min(limit, len(ranked))]
	// Allocated non-nil so an empty result serializes as [] rather than being
	// omitted: "nothing matched" and "this tool told you nothing" differ.
	runbooks := make([]RunbookMatch, 0, len(top))
	for _, entry := range top {
		runbooks = append(runbooks, RunbookMatch{Slug: entry.slug, Content: entry.content})
	}
	count := len(runbooks)
	return SearchRunbooksResult{Count: &count, Retrieval: keywordMode, Runbooks: runbooks}, nil
}

// runbookCorpus reads every runbook once, in slug order.
//
// A slug the store cannot resolve contributes an empty document rather than an
// error: the listing and the read are two filesystem operations, and a file
// that vanished between them is not a reason to refuse the whole search.
//
// This is the most expensive read the package performs — one listing plus one
// os.ReadFile per runbook — so the budget is checked before each of them. None
// can be preempted once started, but a corpus of any size is a sequence of
// starts, and an exhausted budget stops the next one.
func (m *Memory) runbookCorpus(ctx context.Context) ([]runbookDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list the runbook knowledge base: %w", err)
	}
	slugs, err := m.store.ListRunbookSlugs()
	if err != nil {
		return nil, fmt.Errorf("list the runbook knowledge base: %w", err)
	}
	documents := make([]runbookDocument, 0, len(slugs))
	for _, slug := range slugs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("read runbook %s: %w", slug, err)
		}
		content, _, err := m.store.ReadRunbook(slug)
		if err != nil {
			return nil, fmt.Errorf("read runbook %s: %w", slug, err)
		}
		documents = append(documents, runbookDocument{
			slug:     slug,
			content:  content,
			haystack: strings.ToLower(content),
		})
	}
	return documents, nil
}

// queryTerms splits a query into lowercase alphanumeric search terms, dropping
// very short ones. Duplicates are kept on purpose — see [Memory.keywordSearch].
func queryTerms(query string) []string {
	matches := termPattern.FindAllString(strings.ToLower(query), -1)
	terms := make([]string, 0, len(matches))
	for _, term := range matches {
		if len(term) >= minTermLength {
			terms = append(terms, term)
		}
	}
	return terms
}

// callContext yields a context the work can use even when the tool was called
// outside ADK, where there is no agent context at all.
func callContext(ctx agent.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
