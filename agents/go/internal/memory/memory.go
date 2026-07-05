// Package memory provides knowledge tools over the runbook library — the Ops Copilot's
// memory/RAG (Chapter 3.4).
//
// The runbooks in agents/data/runbooks are the agent's long-term knowledge. getRunbook
// fetches one by its exact slug (an incident row carries its runbook slug); searchRunbooks
// does a simple, deterministic TF-IDF keyword search. Keyword retrieval keeps the course
// fully offline — swap in a vector store or ADK MemoryService for semantic search.
package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
)

// minTermLength drops very short, low-signal words from a query.
const minTermLength = 3

// defaultLimit is how many runbooks searchRunbooks returns when the caller passes zero.
const defaultLimit = 3

var termPattern = regexp.MustCompile(`[a-z0-9]+`)

// terms splits a query into lowercase search terms, dropping very short ones.
func terms(query string) []string {
	var out []string
	for _, term := range termPattern.FindAllString(strings.ToLower(query), -1) {
		if len(term) >= minTermLength {
			out = append(out, term)
		}
	}
	return out
}

type getRunbookInput struct {
	Slug string `json:"slug"` // runbook identifier, e.g. high-latency or service-down
}

type searchRunbooksInput struct {
	Query string `json:"query"` // what you are trying to resolve, e.g. "database pool exhausted"
	Limit int    `json:"limit"` // max runbooks to return (default 3 when zero)
}

type runbook struct {
	Slug    string `json:"slug"`
	Content string `json:"content"`
}

type searchRunbooksOutput struct {
	Count    int       `json:"count"`
	Runbooks []runbook `json:"runbooks"`
}

// KnowledgeTools builds the memory/RAG tools registered on the agent (Ch. 3.4).
func KnowledgeTools(store *data.Store) ([]tool.Tool, error) {
	all := make([]tool.Tool, 0, 2)
	for _, build := range []func(*data.Store) (tool.Tool, error){getRunbookTool, searchRunbooksTool} {
		tl, err := build(store)
		if err != nil {
			return nil, err
		}
		all = append(all, tl)
	}
	return all, nil
}

func getRunbookTool(store *data.Store) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "get_runbook",
		Description: "Fetch a runbook by its exact slug (e.g. an incident's runbook field).",
	}, func(_ agent.Context, in getRunbookInput) (map[string]any, error) {
		content, ok, err := store.ReadRunbook(in.Slug)
		if err != nil {
			return nil, err
		}
		if !ok {
			slugs, listErr := store.ListRunbookSlugs()
			if listErr != nil {
				return nil, listErr
			}
			return map[string]any{
				"error": fmt.Sprintf("No runbook named %q. Available runbooks: %s.", in.Slug, strings.Join(slugs, ", ")),
			}, nil
		}
		return map[string]any{"slug": in.Slug, "content": content}, nil
	})
}

func searchRunbooksTool(store *data.Store) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "search_runbooks",
		Description: "Search the runbook knowledge base for guidance relevant to a free-text query " +
			"(TF-IDF keyword scoring, most relevant first).",
	}, func(_ agent.Context, in searchRunbooksInput) (searchRunbooksOutput, error) {
		return searchRunbooks(store, in.Query, in.Limit)
	})
}

// searchRunbooks ranks runbooks with TF-IDF-style scoring: rarer terms weigh more (so
// ubiquitous words like "service" don't dominate) and a term matching a runbook's slug
// gets a strong boost.
func searchRunbooks(store *data.Store, query string, limit int) (searchRunbooksOutput, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	slugs, err := store.ListRunbookSlugs()
	if err != nil {
		return searchRunbooksOutput{}, err
	}
	contents := make(map[string]string, len(slugs))
	for _, slug := range slugs {
		content, _, readErr := store.ReadRunbook(slug)
		if readErr != nil {
			return searchRunbooksOutput{}, readErr
		}
		contents[slug] = content
	}

	total := len(contents)
	if total == 0 {
		return searchRunbooksOutput{Count: 0, Runbooks: []runbook{}}, nil
	}
	queryTerms := terms(query)
	docFreq := documentFrequency(queryTerms, contents)

	type ranked struct {
		slug  string
		score float64
	}
	var results []ranked
	for slug, content := range contents {
		haystack := strings.ToLower(content)
		var score float64
		for _, term := range queryTerms {
			if freq := docFreq[term]; freq > 0 {
				score += float64(strings.Count(haystack, term)) * (float64(total) / float64(freq))
			}
			if strings.Contains(slug, term) {
				score += float64(total) * 5 // the slug names the failure mode: a strong signal
			}
		}
		if score > 0 {
			results = append(results, ranked{slug: slug, score: score})
		}
	}
	// Score descending, then slug ascending for deterministic ties.
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].slug < results[j].slug
	})

	out := searchRunbooksOutput{Runbooks: []runbook{}}
	for i, r := range results {
		if i >= limit {
			break
		}
		out.Runbooks = append(out.Runbooks, runbook{Slug: r.slug, Content: contents[r.slug]})
	}
	out.Count = len(out.Runbooks)
	return out, nil
}

// documentFrequency counts, for each distinct term, how many runbooks contain it.
func documentFrequency(queryTerms []string, contents map[string]string) map[string]int {
	docFreq := map[string]int{}
	for _, term := range queryTerms {
		if _, seen := docFreq[term]; seen {
			continue
		}
		count := 0
		for _, content := range contents {
			if strings.Contains(strings.ToLower(content), term) {
				count++
			}
		}
		docFreq[term] = count
	}
	return docFreq
}
