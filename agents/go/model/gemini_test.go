package model

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// geminiModelName is a stand-in tag for the optional provider. It is not the
// pinned course model: nothing here talks to Google, and pinning a real tag in
// a test would invite someone to "update" it.
const geminiModelName = "gemini-test"

// geminiReply is the smallest genai response body the SDK will decode.
func geminiReply(t *testing.T, modelName, text string) string {
	t.Helper()

	return mustJSON(t, map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     7,
			"candidatesTokenCount": 3,
			"totalTokenCount":      10,
		},
		"modelVersion": modelName,
	})
}

// TestBuildSelectsTheGeminiProvider proves the selection behaviorally: the
// request lands on the Gemini surface with the configured key, not on the
// OpenAI-compatible one. Asserting on the concrete type would prove far less,
// because both adapters are unexported provider types.
func TestBuildSelectsTheGeminiProvider(t *testing.T) {
	endpoint := replyWith(t, geminiReply(t, geminiModelName, "ok"))
	// genai exposes no ClientConfig field for the Gemini API host, but it does
	// read this variable. It is the only way to keep the optional provider's
	// round trip offline, and it is set on the test's environment alone.
	t.Setenv("GOOGLE_GEMINI_BASE_URL", endpoint.URL)

	cfg := defaults(t)
	cfg.ModelProvider = config.ProviderGemini
	cfg.Model = geminiModelName
	cfg.GoogleAPIKey = config.Secret("studio-test-key")

	llm, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if llm.Name() != cfg.Model {
		t.Errorf("Name() = %q, want %q", llm.Name(), cfg.Model)
	}
	if got := responseText(single(t, llm)); got != "ok" {
		t.Errorf("response text = %q, want %q", got, "ok")
	}

	received := endpoint.only(t)
	if !strings.Contains(received.path, cfg.Model) || !strings.HasSuffix(received.path, ":generateContent") {
		t.Errorf("request path = %q, want the Gemini generateContent route for %q", received.path, cfg.Model)
	}
	if key := received.header.Get("x-goog-api-key"); key != cfg.GoogleAPIKey.Reveal() {
		t.Errorf("x-goog-api-key = %q, want the configured key", key)
	}
}

// TestGeminiClientConfigChoosesOneAuthenticationMode covers the mapping rather
// than the client, because the client cannot be built here: the enterprise path
// resolves Application Default Credentials during construction, which is a live
// operation an offline test must not perform. The mapping is where the
// decisions live, so the mapping is what is asserted.
func TestGeminiClientConfigChoosesOneAuthenticationMode(t *testing.T) {
	t.Parallel()

	t.Run("api key", func(t *testing.T) {
		t.Parallel()

		cfg := defaults(t)
		cfg.ModelProvider = config.ProviderGemini
		cfg.GoogleAPIKey = config.Secret("  studio-test-key  ")
		// Ambient enterprise coordinates must not turn a key into a project:
		// genai rejects a client configured with both.
		cfg.GoogleCloudProject = "agentops-open-course"
		cfg.GoogleCloudLocation = "global"

		client, err := geminiClientConfig(cfg)
		if err != nil {
			t.Fatalf("geminiClientConfig() error = %v, want nil", err)
		}
		if client.Backend != genai.BackendGeminiAPI {
			t.Errorf("Backend = %v, want the Gemini API backend", client.Backend)
		}
		if client.APIKey != "studio-test-key" {
			t.Errorf("APIKey = %q, want the trimmed configured key", client.APIKey)
		}
		if client.Project != "" || client.Location != "" {
			t.Errorf("Project/Location = %q/%q, want both empty alongside an API key",
				client.Project, client.Location)
		}
	})

	t.Run("enterprise credentials", func(t *testing.T) {
		t.Parallel()

		cfg := defaults(t)
		cfg.ModelProvider = config.ProviderGemini
		cfg.GoogleGenAIUseEnterprise = true
		cfg.GoogleCloudProject = "agentops-open-course"
		cfg.GoogleCloudLocation = "global"

		client, err := geminiClientConfig(cfg)
		if err != nil {
			t.Fatalf("geminiClientConfig() error = %v, want nil", err)
		}
		if client.Backend != genai.BackendEnterprise {
			t.Errorf("Backend = %v, want the enterprise backend", client.Backend)
		}
		if client.APIKey != "" {
			t.Error("APIKey is set, want it empty on the credential-backed path")
		}
		if client.Project != cfg.GoogleCloudProject || client.Location != cfg.GoogleCloudLocation {
			t.Errorf("Project/Location = %q/%q, want %q/%q",
				client.Project, client.Location, cfg.GoogleCloudProject, cfg.GoogleCloudLocation)
		}
	})
}

// TestGeminiCarriesTheModelDeadlineAsAnHTTPOption pins where the deadline lives
// for this provider. genai derives the request's context deadline from
// HTTPOptions.Timeout and documents that an HTTP client's own timeout does not
// affect it, so a deadline set the other way would silently do nothing — and
// supplying an HTTP client would strip ADC off the enterprise path.
func TestGeminiCarriesTheModelDeadlineAsAnHTTPOption(t *testing.T) {
	t.Parallel()

	cfg := defaults(t)
	cfg.ModelProvider = config.ProviderGemini
	cfg.GoogleAPIKey = config.Secret("studio-test-key")
	cfg.ModelTimeout = config.Seconds(12)

	client, err := geminiClientConfig(cfg)
	if err != nil {
		t.Fatalf("geminiClientConfig() error = %v, want nil", err)
	}
	if client.HTTPClient != nil {
		t.Error("HTTPClient is set, want genai to build its own so credentials survive")
	}
	if client.HTTPOptions.Timeout == nil {
		t.Fatal("HTTPOptions.Timeout = nil, want the configured model deadline")
	}
	if got := *client.HTTPOptions.Timeout; got != cfg.ModelTimeout.Duration() {
		t.Errorf("HTTPOptions.Timeout = %v, want %v", got, cfg.ModelTimeout.Duration())
	}
}

// TestGeminiEnterpriseFailsAttributablyWithoutADC covers the failure a learner
// who has not run `gcloud auth application-default login` actually hits.
//
// genai resolves Application Default Credentials while the client is being
// constructed, so the failure lands at startup rather than mid-investigation —
// and the message has to say which model could not be built. Pointing the
// credential variable at a file that does not exist makes the miss local and
// immediate: nothing here reaches a network or a metadata server.
func TestGeminiEnterpriseFailsAttributablyWithoutADC(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "absent.json"))

	cfg := defaults(t)
	cfg.ModelProvider = config.ProviderGemini
	cfg.Model = geminiModelName
	cfg.GoogleGenAIUseEnterprise = true
	cfg.GoogleCloudProject = "agentops-open-course"
	cfg.GoogleCloudLocation = "global"

	llm, err := Build(t.Context(), cfg)
	if err == nil {
		t.Fatalf("Build() = %v, want an error with no credentials available", llm)
	}
	if !strings.Contains(err.Error(), cfg.Model) {
		t.Errorf("Build() error = %q, want it to name the model %q it could not build", err, cfg.Model)
	}
}

// TestBuildRejectsGeminiWithoutCredentials keeps the optional provider honest:
// selecting it without a way to authenticate must fail at startup with a
// message naming every variable that could fix it.
func TestBuildRejectsGeminiWithoutCredentials(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*config.Config){
		"nothing configured": func(*config.Config) {},
		"enterprise without a project": func(cfg *config.Config) {
			cfg.GoogleGenAIUseEnterprise = true
			cfg.GoogleCloudLocation = "global"
		},
		"enterprise without a location": func(cfg *config.Config) {
			cfg.GoogleGenAIUseEnterprise = true
			cfg.GoogleCloudProject = "agentops-open-course"
		},
		"coordinates without the enterprise toggle": func(cfg *config.Config) {
			cfg.GoogleCloudProject = "agentops-open-course"
			cfg.GoogleCloudLocation = "global"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := defaults(t)
			cfg.ModelProvider = config.ProviderGemini
			cfg.Model = geminiModelName
			mutate(&cfg)

			llm, err := Build(t.Context(), cfg)
			if err == nil {
				t.Fatalf("Build() = %v, want an error", llm)
			}
			for _, variable := range []string{
				config.EnvGoogleAPIKey,
				config.EnvGoogleGenAIUseEnterprise,
				config.EnvGoogleCloudProject,
				config.EnvGoogleCloudLocation,
			} {
				if !strings.Contains(err.Error(), variable) {
					t.Errorf("Build() error = %q, want it to name %s", err, variable)
				}
			}
		})
	}
}
