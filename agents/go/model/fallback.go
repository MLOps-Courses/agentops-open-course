package model

import (
	"context"
	"fmt"
	"iter"
	"log/slog"

	adkmodel "google.golang.org/adk/v2/model"
)

// Fallback tries a primary model, then a secondary one when the primary fails
// outright.
//
// Retries and deadlines handle a *flaky* call; a fallback handles a *dead* one —
// the endpoint is down, overloaded, or the model was unloaded. The distinction
// is what makes the engagement rule narrow: the fallback runs only if the
// primary failed **before yielding any response**. Once a turn has begun,
// switching models would splice two different completions together, so a
// mid-flight failure is surfaced instead of masked.
type Fallback struct {
	primary  adkmodel.LLM
	fallback adkmodel.LLM
}

// Fallback satisfies ADK's model interface, so it can be handed to
// llmagent.Config.Model exactly like a provider model.
var _ adkmodel.LLM = (*Fallback)(nil)

// NewFallback chains two models. Both are expected to speak the same provider
// and endpoint; a fallback on a different provider would change the request
// semantics halfway through an incident.
func NewFallback(primary, fallback adkmodel.LLM) *Fallback {
	return &Fallback{primary: primary, fallback: fallback}
}

// Name reports the primary model's name.
//
// ADK routes on this string — structured-output handling keys off whether the
// name looks like a Gemini model — so reporting the model that normally answers
// is the only answer that keeps routing consistent with behavior.
func (f *Fallback) Name() string { return f.primary.Name() }

// GenerateContent streams the primary's responses, switching to the fallback
// only when the primary fails before it has said anything.
//
// The sequence is forwarded response by response and never buffered: collecting
// it would defeat streaming and, because ADK only executes function calls on
// non-partial responses, would break the human-in-the-loop pause.
func (f *Fallback) GenerateContent(
	ctx context.Context, req *adkmodel.LLMRequest, stream bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var failure error
		responded := false

		for response, err := range f.primary.GenerateContent(ctx, req, stream) {
			if err != nil {
				if responded {
					// The turn is already underway. Continuing on another model
					// would stitch two answers together, so the caller gets the
					// truth instead of a plausible-looking hybrid.
					yield(nil, fmt.Errorf("model %s failed after it began responding: %w", f.primary.Name(), err))
					return
				}
				failure = err
				break
			}
			responded = true
			if !yield(response, nil) {
				return
			}
		}
		if failure == nil {
			return
		}

		slog.WarnContext(ctx, "primary model failed before responding, falling back",
			"primary", f.primary.Name(), "fallback", f.fallback.Name(), "error", failure)

		for response, err := range f.fallback.GenerateContent(ctx, req, stream) {
			if err != nil {
				// Both models are down. The fallback's error is the one that
				// describes the current state, and the primary's is named in the
				// message so the operator knows two endpoints were tried.
				yield(nil, fmt.Errorf("model %s failed after %s: %w", f.fallback.Name(), f.primary.Name(), err))
				return
			}
			if !yield(response, nil) {
				return
			}
		}
	}
}
