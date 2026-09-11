// Package model turns the resolved configuration into the LLM the agent runs on.
//
// Three facts shape everything here:
//
//   - ADK takes a model *instance*, not a name. llmagent.Config.Model is typed
//     model.LLM, and nothing in ADK registers a name pattern or calls
//     model.NewLLM, so a bare "qwen3:4b-instruct" resolves to nothing at all.
//     [Build] therefore constructs the client and hands back the value.
//   - Endpoints and credentials come from the validated configuration, never
//     from the ambient environment. Both SDKs silently fall back to their own
//     environment variables when a field is left empty, which would make the
//     effective endpoint depend on whatever the shell happened to export. Every
//     field this package needs is passed explicitly, and the process
//     environment is never written.
//   - The account-free path is the default, not a fallback. An
//     OpenAI-compatible client pointed at a local Ollama is what a learner gets
//     with no configuration at all; Gemini is opt-in and needs credentials it
//     will never invent.
//
// The request deadline lands in a different place per provider, because the two
// SDKs disagree about where a deadline lives. See [openAIProvider] and
// [geminiClientConfig] for the reasoning at each site.
package model

import (
	"context"
	"fmt"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// namedModel builds one provider-backed model for a specific model name.
//
// Provider credentials are validated once, when the factory is created, and the
// resulting client configuration is shared by every model the factory builds.
// That is what keeps the primary and its fallback on one endpoint, one deadline
// and one connection pool instead of two independently configured clients.
type namedModel func(name string) (adkmodel.LLM, error)

// Build returns the configured ADK model.
//
// Gemini mode uses ADK's native integration. The default account-free mode uses
// ADK's OpenAI-compatible client, where OPENAI_BASE_URL alone chooses direct
// Ollama or an agentgateway route without changing application code. When
// AGENT_MODEL_FALLBACK names a second model, the primary is wrapped so that a
// dead primary fails over to it on the same provider (Ch. 5.4).
// --8<-- [start:build-model]
func Build(ctx context.Context, cfg config.Config) (adkmodel.LLM, error) {
	build, err := provider(ctx, cfg)
	if err != nil {
		return nil, err
	}
	primary, err := build(cfg.Model)
	if err != nil {
		return nil, err
	}
	if cfg.ModelFallback == nil {
		// No fallback configured: hand back the provider model untouched, so the
		// single-model path carries none of the wrapper's behavior.
		return primary, nil
	}
	secondary, err := build(*cfg.ModelFallback)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvModelFallback, err)
	}
	return NewFallback(primary, secondary), nil
}

// --8<-- [end:build-model]

// GenerationConfig returns explicit sampling controls only when the operator
// configured them.
//
// Returning nil is meaningful: it keeps each provider's own sampling defaults.
// Evaluations opt into lower-variance greedy sampling with
// AGENT_MODEL_TEMPERATURE=0, which is why the configuration models the setting
// as a pointer — zero is a real temperature, not "unset".
func GenerationConfig(cfg config.Config) *genai.GenerateContentConfig {
	if cfg.ModelTemperature == nil {
		return nil
	}
	// genai carries temperature as a *float32 while the environment parses into
	// a float64, so the narrowing happens once, here, at the boundary.
	temperature := float32(*cfg.ModelTemperature)
	return &genai.GenerateContentConfig{Temperature: &temperature}
}

// provider selects the model factory for the configured provider and validates
// that it can authenticate before any model is built.
//
// Failing here rather than on the first turn is the point: a missing endpoint
// or credential is a startup problem, and an operator who learns about it three
// tool calls into an investigation has to guess at the cause.
func provider(ctx context.Context, cfg config.Config) (namedModel, error) {
	switch cfg.ModelProvider {
	case config.ProviderOpenAICompatible:
		return openAIProvider(ctx, cfg)
	case config.ProviderGemini:
		return geminiProvider(ctx, cfg)
	default:
		// Unreachable through config.Load, which parses the provider into a typed
		// constant. A Config assembled in code can still carry an unknown value,
		// and silently defaulting it would pick an adapter nobody asked for.
		return nil, fmt.Errorf("%s: must be one of %s; got %q",
			config.EnvModelProvider, providerNames(), cfg.ModelProvider)
	}
}

// providerNames renders the supported providers for an error message, reading
// them from the configuration package so the two lists cannot drift apart.
func providerNames() string {
	supported := config.ModelProviders()
	names := make([]string, len(supported))
	for index, candidate := range supported {
		names[index] = string(candidate)
	}
	return strings.Join(names, ", ")
}
