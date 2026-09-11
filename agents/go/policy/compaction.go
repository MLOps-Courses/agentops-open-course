package policy

import (
	"slices"
	"strconv"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Bounded conversation history — deterministic context compaction (Ch. 3.4).
//
// An incident investigation can run long. Each user turn may expand into a
// model tool call, a tool response and a model answer, and the session service
// keeps every message forever. Left unbounded the prompt grows every turn until
// it strains the model's context window, inflates token cost and slows each
// call — the same failure the token budget bounds from the cost side, seen from
// the context side.
//
// [Policy.CompactHistory] keeps the most recent messages and replaces the older
// ones with a single synthetic note recording how many were elided and which
// tools they touched. It is deterministic and model-free — no summarization
// call — so the offline test gate stays exact, and it is off by default.
// Summarizing the elided span with the model is the natural extension, traded
// against determinism and one extra model call per compaction.
//
// ADK v2.3.0 ships that extension as session compaction: a summarizer call whose
// result is appended to the session as a compaction event. This guard stays,
// because it rewrites only the outgoing request and never the stored history.

// minHistoryMessages is the smallest window that can hold a tool call and its
// result. Anything less would elide one half of a pair on every turn.
const minHistoryMessages = 2

// markerRole keeps the synthetic note in the conversational frame the model
// reads. It is a first-party note, never model- or tool-authored, so it carries
// no injection risk of its own.
const markerRole = "user"

// CompactHistory is the before-model guard that keeps only the most recent
// messages in the prompt.
//
// It always returns nil — it never short-circuits the model call — and only
// rewrites the outgoing request. The rewrite is ephemeral: ADK rebuilds the
// request's contents from the stored session events every turn, so nothing is
// deleted from the session, no marker ever accumulates, and each call compacts
// the full history afresh.
// --8<-- [start:compact-history]
func (p *Policy) CompactHistory(_ agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	// Compaction depends only on the outgoing request, never on the context.
	if p.maxHistoryMessages == nil || request == nil {
		return nil, nil
	}
	keep := *p.maxHistoryMessages
	contents := request.Contents
	if len(contents) <= keep {
		return nil, nil
	}

	cut := len(contents) - keep
	// Prefer a standalone message boundary, but a request can end mid-tool-loop
	// with only function responses in the retained tail. Stopping one short of
	// the end keeps at least the newest result: replacing the entire request
	// with a marker would discard the evidence the model just asked for.
	for cut < len(contents)-1 && hasFunctionResponse(contents[cut]) {
		cut++
	}
	request.Contents = slices.Concat([]*genai.Content{compactionMarker(contents[:cut])}, contents[cut:])
	return nil, nil
}

// --8<-- [end:compact-history]

// hasFunctionResponse reports whether a message carries a tool result.
func hasFunctionResponse(content *genai.Content) bool {
	if content == nil {
		return false
	}
	return slices.ContainsFunc(content.Parts, func(part *genai.Part) bool {
		return part != nil && part.FunctionResponse != nil
	})
}

// earlierToolNames returns the distinct tool names called in the elided span,
// in first-seen order. Order matters: the note is read by a model, and a stable
// note keeps a prompt reproducible across runs.
func earlierToolNames(contents []*genai.Content) []string {
	var names []string
	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionCall == nil || part.FunctionCall.Name == "" {
				continue
			}
			if !slices.Contains(names, part.FunctionCall.Name) {
				names = append(names, part.FunctionCall.Name)
			}
		}
	}
	return names
}

// compactionMarker is the single message that stands in for the elided span.
func compactionMarker(elided []*genai.Content) *genai.Content {
	var note strings.Builder
	note.WriteString("[history compacted: ")
	note.WriteString(strconv.Itoa(len(elided)))
	note.WriteString(" earlier message(s) elided to bound context")
	if names := earlierToolNames(elided); len(names) > 0 {
		note.WriteString("; tools used earlier: ")
		note.WriteString(strings.Join(names, ", "))
	}
	note.WriteString("]")
	return &genai.Content{Role: markerRole, Parts: []*genai.Part{{Text: note.String()}}}
}
