package model

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// responsesReply is the smallest OpenAI Responses body ADK's adapter accepts:
// one assistant message, plus the usage block the token budget reads.
func responsesReply(t *testing.T, modelName, text string) string {
	t.Helper()

	return mustJSON(t, map[string]any{
		"id":     "resp_test",
		"object": "response",
		"model":  modelName,
		"status": "completed",
		"output": []any{map[string]any{
			"type":    "message",
			"id":      "msg_test",
			"role":    "assistant",
			"status":  "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}},
		"usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
	})
}

// TestOpenAICompatibleIsTheAccountFreeDefault is the course invariant: an empty
// environment must produce a working local-model client, with no account, no
// key and no fee. If this ever fails, Chapter 2's first interaction fails with
// it.
func TestOpenAICompatibleIsTheAccountFreeDefault(t *testing.T) {
	cfg := defaults(t)
	if cfg.ModelProvider != config.ProviderOpenAICompatible {
		t.Fatalf("default provider = %q, want %q", cfg.ModelProvider, config.ProviderOpenAICompatible)
	}

	endpoint := replyWith(t, responsesReply(t, cfg.Model, "ok"))
	// Only the base URL moves. Everything else — the model tag, the non-secret
	// marker standing in for an API key — is what a learner runs with.
	cfg.OpenAIBaseURL = endpoint.URL + "/v1"

	llm, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if llm.Name() != cfg.Model {
		t.Errorf("Name() = %q, want %q", llm.Name(), cfg.Model)
	}

	response := single(t, llm)
	if got := responseText(response); got != "ok" {
		t.Errorf("response text = %q, want %q", got, "ok")
	}

	received := endpoint.only(t)
	// The Responses API, not chat completions: ADK's adapter speaks only this
	// surface, and the gateway route in Chapter 5.4 is configured to match.
	if received.path != "/v1/responses" {
		t.Errorf("request path = %q, want %q", received.path, "/v1/responses")
	}
	if authorization := received.header.Get("Authorization"); authorization != "Bearer "+cfg.OpenAIAPIKey.Reveal() {
		t.Errorf("Authorization = %q, want the configured marker", authorization)
	}
	if received.body["model"] != cfg.Model {
		t.Errorf("request model = %v, want %q", received.body["model"], cfg.Model)
	}
}

// TestOpenAICompatibleResponsesCarryTokenUsage pins the finding the token
// budget and the cost evaluations depend on: a non-streaming response arrives
// with usage metadata populated. Without it, Chapters 4.4 and 7.3 have nothing
// to meter.
func TestOpenAICompatibleResponsesCarryTokenUsage(t *testing.T) {
	cfg := defaults(t)
	endpoint := replyWith(t, responsesReply(t, cfg.Model, "ok"))
	cfg.OpenAIBaseURL = endpoint.URL + "/v1"

	llm, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	usage := single(t, llm).UsageMetadata
	if usage == nil {
		t.Fatal("UsageMetadata = nil, want the provider's token counts")
	}
	for _, count := range []struct {
		name string
		got  int32
		want int32
	}{
		{"PromptTokenCount", usage.PromptTokenCount, 7},
		{"CandidatesTokenCount", usage.CandidatesTokenCount, 3},
		{"TotalTokenCount", usage.TotalTokenCount, 10},
	} {
		if count.got != count.want {
			t.Errorf("%s = %d, want %d", count.name, count.got, count.want)
		}
	}
}

// TestOpenAICompatibleIgnoresTheAmbientEnvironment is the property the Python
// track called "uses validated settings without mutating the environment".
// openai-go falls back to OPENAI_BASE_URL and OPENAI_API_KEY for any field left
// empty, so passing them explicitly is the only thing standing between the
// course and an endpoint chosen by whatever the shell exported.
func TestOpenAICompatibleIgnoresTheAmbientEnvironment(t *testing.T) {
	const ambientURL = "http://ambient.invalid/v1"
	const ambientKey = "ambient-marker"
	t.Setenv(config.EnvOpenAIBaseURL, ambientURL)
	t.Setenv(config.EnvOpenAIAPIKey, ambientKey)

	cfg := defaults(t)
	endpoint := replyWith(t, responsesReply(t, cfg.Model, "ok"))
	cfg.OpenAIBaseURL = endpoint.URL + "/v1"
	cfg.OpenAIAPIKey = config.Secret("configured-marker")

	llm, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got := responseText(single(t, llm)); got != "ok" {
		// Reaching ambient.invalid would be a DNS failure, not a wrong answer,
		// so a successful turn is itself proof the configured URL won.
		t.Fatalf("response text = %q, want %q", got, "ok")
	}

	received := endpoint.only(t)
	if authorization := received.header.Get("Authorization"); authorization != "Bearer "+cfg.OpenAIAPIKey.Reveal() {
		t.Errorf("Authorization = %q, want the configured marker, not the ambient one", authorization)
	}
	// Building a model must not rewrite the process environment either: the
	// Python track's shim existed precisely to avoid that.
	if got := os.Getenv(config.EnvOpenAIBaseURL); got != ambientURL {
		t.Errorf("%s = %q after Build, want it untouched at %q", config.EnvOpenAIBaseURL, got, ambientURL)
	}
	if got := os.Getenv(config.EnvOpenAIAPIKey); got != ambientKey {
		t.Errorf("%s = %q after Build, want it untouched", config.EnvOpenAIAPIKey, got)
	}
}

// TestOpenAICompatibleCarriesTheModelDeadline proves AGENT_MODEL_TIMEOUT_S
// reaches the wire. A hung model server must fail the turn on the agent's
// schedule, not the provider's.
func TestOpenAICompatibleCarriesTheModelDeadline(t *testing.T) {
	// The handler outlives the deadline by a wide margin but still returns, so a
	// regression fails the assertion instead of hanging the suite.
	endpoint := newStubEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	})

	cfg := defaults(t)
	cfg.OpenAIBaseURL = endpoint.URL + "/v1"
	cfg.ModelTimeout = config.Seconds(0.05)
	// Retries would multiply the deadline and blur what is being measured.
	cfg.MaxRetries = 0

	llm, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}

	started := time.Now()
	_, failure := drive(t, llm)
	elapsed := time.Since(started)

	if failure == nil {
		t.Fatal("GenerateContent() error = nil, want the configured deadline to fire")
	}
	var timeout net.Error
	if !errors.As(failure, &timeout) || !timeout.Timeout() {
		t.Errorf("GenerateContent() error = %v, want a timeout", failure)
	}
	if elapsed >= 5*time.Second {
		t.Errorf("GenerateContent() took %v, want it bounded by the %v deadline", elapsed, cfg.ModelTimeout.Duration())
	}
	if received := endpoint.requests(); len(received) != 1 {
		t.Errorf("endpoint received %d requests, want 1 with %s=0", len(received), config.EnvMaxRetries)
	}
}

func TestOpenAICompatibleRefusesCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var captured atomic.Int64
			target := newStubEndpoint(t, func(writer http.ResponseWriter, _ *http.Request) {
				captured.Add(1)
				http.Error(writer, "redirect target", http.StatusBadGateway)
			})
			source := newStubEndpoint(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Location", target.URL+"/v1/responses")
				writer.WriteHeader(status)
			})

			cfg := defaults(t)
			cfg.OpenAIBaseURL = source.URL + "/v1"
			cfg.OpenAIAPIKey = config.Secret("redirect-sensitive-model-token")
			cfg.MaxRetries = 0
			llm, err := Build(t.Context(), cfg)
			if err != nil {
				t.Fatalf("Build() error = %v, want nil", err)
			}
			if _, err := drive(t, llm); err == nil {
				t.Fatal("GenerateContent() error = nil, want redirect refusal")
			}
			if got := captured.Load(); got != 0 {
				t.Fatalf("redirect target received %d request(s), want zero credential or prompt replays", got)
			}
		})
	}
}

// TestBuildRejectsAnOpenAICompatibleConfigItCannotAuthenticate covers the
// defense-in-depth check. config.Load rejects these already; a Config assembled
// in code must not be able to slip past it and inherit an ambient endpoint.
func TestBuildRejectsAnOpenAICompatibleConfigItCannotAuthenticate(t *testing.T) {
	for name, testCase := range map[string]struct {
		mutate func(*config.Config)
		want   string
	}{
		"no endpoint": {
			mutate: func(cfg *config.Config) { cfg.OpenAIBaseURL = "" },
			want:   config.EnvOpenAIBaseURL,
		},
		"no credential": {
			mutate: func(cfg *config.Config) { cfg.OpenAIAPIKey = "" },
			want:   config.EnvOpenAIAPIKey,
		},
		"blank credential": {
			mutate: func(cfg *config.Config) { cfg.OpenAIAPIKey = "   " },
			want:   config.EnvOpenAIAPIKey,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaults(t)
			testCase.mutate(&cfg)

			llm, err := Build(t.Context(), cfg)
			if err == nil {
				t.Fatalf("Build() = %v, want an error naming %s", llm, testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("Build() error = %q, want it to name %s", err, testCase.want)
			}
		})
	}
}
