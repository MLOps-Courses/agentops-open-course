package model

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// TestModelNamesDoNotResolveThroughTheADKRegistry pins the finding that decides
// this package's shape.
//
// ADK ships a model registry, but no provider package registers a name pattern
// and nothing in ADK ever calls NewLLM — llmagent.Config.Model takes an
// instance. A configuration that only carried "qwen3:4b-instruct" would resolve
// to nothing, which is why Build constructs the client itself. If ADK ever
// starts registering patterns this test goes red, and that is the moment to
// revisit the decision, not before.
func TestModelNamesDoNotResolveThroughTheADKRegistry(t *testing.T) {
	t.Parallel()

	cfg := defaults(t)
	llm, err := adkmodel.NewLLM(t.Context(), cfg.Model)
	if err == nil {
		t.Fatalf("model.NewLLM(%q) = %v, want an error; Build returns an instance because nothing resolves a name",
			cfg.Model, llm)
	}
}

// TestBuildWithoutAFallbackReturnsTheProviderModelUnwrapped keeps the default
// path free of the chaining wrapper. A single-model configuration must carry
// none of the fallback's behavior.
func TestBuildWithoutAFallbackReturnsTheProviderModelUnwrapped(t *testing.T) {
	t.Parallel()

	cfg := defaults(t)
	if cfg.ModelFallback != nil {
		t.Fatalf("default %s = %q, want it unset", config.EnvModelFallback, *cfg.ModelFallback)
	}

	llm, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if chain, wrapped := llm.(*Fallback); wrapped {
		t.Errorf("Build() = %T wrapping %q, want the provider model itself", chain, chain.Name())
	}
	if llm.Name() != cfg.Model {
		t.Errorf("Name() = %q, want %q", llm.Name(), cfg.Model)
	}
}

// TestBuildWithAFallbackChainsTwoModelsOnOneEndpoint proves the wiring end to
// end: both models speak to the configured endpoint, they differ only by model
// tag, and a dead primary really does hand the turn to the secondary.
func TestBuildWithAFallbackChainsTwoModelsOnOneEndpoint(t *testing.T) {
	t.Parallel()

	cfg := defaults(t)
	fallbackModel := cfg.Model + "-small"
	cfg.ModelFallback = &fallbackModel
	// One attempt per model keeps the recorded requests a faithful account of
	// what the chain did.
	cfg.MaxRetries = 0

	endpoint := newStubEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding the request body: %v", err)
			return
		}
		if payload.Model == cfg.Model {
			// The primary endpoint is down, which is exactly the failure a
			// fallback exists for.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, responsesReply(t, payload.Model, "from the fallback")); err != nil {
			t.Errorf("writing the stub reply: %v", err)
		}
	})
	cfg.OpenAIBaseURL = endpoint.URL + "/v1"

	llm, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	chain, wrapped := llm.(*Fallback)
	if !wrapped {
		t.Fatalf("Build() = %T, want *Fallback when %s is set", llm, config.EnvModelFallback)
	}
	if chain.Name() != cfg.Model {
		t.Errorf("Name() = %q, want the primary %q", chain.Name(), cfg.Model)
	}
	if chain.fallback.Name() != fallbackModel {
		t.Errorf("fallback name = %q, want %q", chain.fallback.Name(), fallbackModel)
	}

	spoken, failure := drive(t, chain)
	if failure != nil {
		t.Fatalf("GenerateContent() error = %v, want the fallback to answer", failure)
	}
	if !slices.Equal(spoken, []string{"from the fallback"}) {
		t.Errorf("responses = %q, want the fallback's answer", spoken)
	}

	sent := endpoint.requests()
	asked := make([]string, 0, len(sent))
	for _, received := range sent {
		if received.path != "/v1/responses" {
			t.Errorf("request path = %q, want %q for every model in the chain", received.path, "/v1/responses")
		}
		name, _ := received.body["model"].(string)
		asked = append(asked, name)
	}
	if !slices.Equal(asked, []string{cfg.Model, fallbackModel}) {
		t.Errorf("models asked = %q, want the primary then the fallback", asked)
	}
}

// TestBuildNamesTheModelItCouldNotConstruct keeps a construction failure
// attributable. With two model names in play, "the model could not be built" is
// not an answer an operator can act on — which of the two, and which variable
// set it, is.
func TestBuildNamesTheModelItCouldNotConstruct(t *testing.T) {
	t.Parallel()

	// An empty model name is what ADK's adapter refuses. config.Load rejects it
	// too, so reaching here means a Config was assembled in code.
	blank := ""

	t.Run("primary", func(t *testing.T) {
		t.Parallel()

		cfg := defaults(t)
		cfg.Model = blank

		llm, err := Build(t.Context(), cfg)
		if err == nil {
			t.Fatalf("Build() = %v, want an error", llm)
		}
		if strings.Contains(err.Error(), config.EnvModelFallback) {
			t.Errorf("Build() error = %q, want it to blame the primary, not the fallback", err)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		t.Parallel()

		cfg := defaults(t)
		cfg.ModelFallback = &blank

		llm, err := Build(t.Context(), cfg)
		if err == nil {
			t.Fatalf("Build() = %v, want an error", llm)
		}
		if !strings.Contains(err.Error(), config.EnvModelFallback) {
			t.Errorf("Build() error = %q, want it to name %s", err, config.EnvModelFallback)
		}
	})
}

// TestBuildRejectsAnUnknownProvider covers the branch config.Load cannot reach.
// A Config assembled in code must not silently select an adapter nobody asked
// for.
func TestBuildRejectsAnUnknownProvider(t *testing.T) {
	t.Parallel()

	cfg := defaults(t)
	cfg.ModelProvider = config.ModelProvider("something-else")

	llm, err := Build(t.Context(), cfg)
	if err == nil {
		t.Fatalf("Build() = %v, want an error", llm)
	}
	if !strings.Contains(err.Error(), config.EnvModelProvider) {
		t.Errorf("Build() error = %q, want it to name %s", err, config.EnvModelProvider)
	}
	for _, supported := range config.ModelProviders() {
		if !strings.Contains(err.Error(), string(supported)) {
			t.Errorf("Build() error = %q, want it to offer %q", err, supported)
		}
	}
}

// TestGenerationConfigIsExplicitOnlyWhenRequested keeps "unset" distinct from
// zero. An unset temperature must leave each provider's own sampling default
// alone, while an explicit 0 is greedy sampling that evaluations rely on.
func TestGenerationConfigIsExplicitOnlyWhenRequested(t *testing.T) {
	t.Parallel()

	cfg := defaults(t)
	if got := GenerationConfig(cfg); got != nil {
		t.Errorf("GenerationConfig() = %+v with no temperature configured, want nil", got)
	}

	for _, temperature := range []float64{0, 0.5, 2} {
		cfg.ModelTemperature = &temperature

		got := GenerationConfig(cfg)
		if got == nil {
			t.Fatalf("GenerationConfig() = nil for temperature %v, want an explicit config", temperature)
		}
		if got.Temperature == nil {
			t.Fatalf("Temperature = nil for %v, want it set", temperature)
		}
		if *got.Temperature != float32(temperature) {
			t.Errorf("Temperature = %v, want %v", *got.Temperature, float32(temperature))
		}
	}
}
