package model

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3/option"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/httpguard"
)

// openAIProvider builds the account-free default: ADK's OpenAI-compatible
// client, pointed wherever OPENAI_BASE_URL says.
//
// "openai-compatible" names the client contract, not the deployment topology.
// The same factory serves a local Ollama and an agentgateway route; only the
// base URL differs, which is the whole reason Chapter 5 can introduce a governed
// data plane without touching application code.
func openAIProvider(ctx context.Context, cfg config.Config) (namedModel, error) {
	// config.Load already rejects both of these. Checking again is not
	// redundancy for its own sake: an empty field here would be filled from the
	// ambient OPENAI_BASE_URL / OPENAI_API_KEY by openai-go's own defaults, so
	// an unvalidated Config would quietly send the course's traffic to whatever
	// the shell exported. Refusing is the only way to keep that from happening.
	apiKey := strings.TrimSpace(cfg.OpenAIAPIKey.Reveal())
	switch {
	case cfg.OpenAIBaseURL == "":
		return nil, fmt.Errorf(
			"%s=%s requires %s; run `mise run config:check` for the resolved configuration",
			config.EnvModelProvider, config.ProviderOpenAICompatible, config.EnvOpenAIBaseURL,
		)
	case apiKey == "":
		return nil, fmt.Errorf(
			"%s=%s requires %s; run `mise run config:check` for the resolved configuration",
			config.EnvModelProvider, config.ProviderOpenAICompatible, config.EnvOpenAIAPIKey,
		)
	}

	client := &openaimodel.ClientConfig{
		APIKey:  apiKey,
		BaseURL: cfg.OpenAIBaseURL,
		// openai-go has no client-wide deadline of its own, so AGENT_MODEL_TIMEOUT_S
		// rides on the HTTP client. http.Client.Timeout is per attempt and covers
		// the response body, which is the same shape the Python track's httpx
		// timeout had. The transport is left nil deliberately: an *http.Client is
		// a stateless handle over the standard library's shared, self-managing
		// connection pool, so there is no per-model resource to own or release.
		HTTPClient: httpguard.NoRedirects(&http.Client{Timeout: cfg.ModelTimeout.Duration()}),
		// Retries for a flaky endpoint, bounded by the same AGENT_MAX_RETRIES the
		// read tools use. This is not [Fallback]: retries handle a *slow or
		// momentarily unavailable* endpoint, a fallback handles a *dead* one.
		// Passing the configured value matters even though it matches openai-go's
		// own default, because AGENT_MAX_RETRIES=0 must actually mean no retry.
		Options: []option.RequestOption{option.WithMaxRetries(cfg.MaxRetries)},
	}

	return func(name string) (adkmodel.LLM, error) {
		// One ClientConfig serves every model this factory builds; openaimodel
		// only reads it, so the primary and its fallback share the endpoint,
		// deadline and retry budget rather than drifting apart.
		llm, err := openaimodel.NewModel(ctx, name, client)
		if err != nil {
			return nil, fmt.Errorf("building the OpenAI-compatible model %q: %w", name, err)
		}
		return llm, nil
	}, nil
}
