package model

import (
	"context"
	"fmt"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// geminiProvider builds the optional native Gemini path.
//
// It is opt-in for a reason the course states plainly: Gemini is a proprietary
// service behind an account, and the open-source path must keep working without
// one. Nothing here is reachable unless AGENT_MODEL_PROVIDER says so.
func geminiProvider(ctx context.Context, cfg config.Config) (namedModel, error) {
	client, err := geminiClientConfig(cfg)
	if err != nil {
		return nil, err
	}
	return func(name string) (adkmodel.LLM, error) {
		// gemini.NewModel copies the configuration before genai mutates it, so
		// the same value can back both the primary and its fallback.
		llm, err := gemini.NewModel(ctx, name, client)
		if err != nil {
			return nil, fmt.Errorf("building the Gemini model %q: %w", name, err)
		}
		return llm, nil
	}, nil
}

// geminiClientConfig maps the validated Gemini settings onto genai's client
// configuration, choosing between AI Studio's API key and the ADC-backed
// enterprise path.
//
// It is a separate function because building the client is not something an
// offline test may do: the enterprise path detects Application Default
// Credentials during construction, which is a live-environment operation. The
// mapping is the part that carries the decisions, so the mapping is the part
// that is tested directly.
func geminiClientConfig(cfg config.Config) (*genai.ClientConfig, error) {
	timeout := cfg.ModelTimeout.Duration()
	// genai derives the request's context deadline from HTTPOptions.Timeout and
	// documents that HTTPClient.Timeout does *not* affect it. Supplying our own
	// HTTP client would also be actively wrong on the enterprise path, where
	// genai builds a credential-injecting client itself and a replacement would
	// strip the authentication off every request.
	httpOptions := genai.HTTPOptions{Timeout: &timeout}

	if apiKey := strings.TrimSpace(cfg.GoogleAPIKey.Reveal()); apiKey != "" {
		return &genai.ClientConfig{
			// Naming the backend explicitly keeps an ambient
			// GOOGLE_GENAI_USE_VERTEXAI from redirecting a key-authenticated
			// client at Vertex, where the key would not be accepted. Project and
			// location stay unset: genai rejects either one combined with a key.
			Backend:     genai.BackendGeminiAPI,
			APIKey:      apiKey,
			HTTPOptions: httpOptions,
		}, nil
	}

	if !cfg.GoogleGenAIUseEnterprise || cfg.GoogleCloudProject == "" || cfg.GoogleCloudLocation == "" {
		return nil, fmt.Errorf(
			"%s=%s requires a validated %s, or %s=true with %s and %s; run `mise run config:check`",
			config.EnvModelProvider, config.ProviderGemini, config.EnvGoogleAPIKey,
			config.EnvGoogleGenAIUseEnterprise, config.EnvGoogleCloudProject, config.EnvGoogleCloudLocation,
		)
	}
	return &genai.ClientConfig{
		// BackendEnterprise names the course's GOOGLE_GENAI_USE_ENTERPRISE
		// setting; genai folds it onto its Vertex backend, which is the one that
		// resolves Application Default Credentials. No API key is set here — the
		// two authentication modes are mutually exclusive.
		Backend:     genai.BackendEnterprise,
		Project:     cfg.GoogleCloudProject,
		Location:    cfg.GoogleCloudLocation,
		HTTPOptions: httpOptions,
	}, nil
}
