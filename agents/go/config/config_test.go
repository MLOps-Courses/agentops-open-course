package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// validBase is the smallest environment that loads cleanly: the shipped
// defaults. Tests copy it and change one thing, so a failure names exactly one
// cause.
func validBase() map[string]string { return map[string]string{} }

// withEnv copies validBase and applies overrides, so no test can leak a value
// into another.
func withEnv(overrides map[string]string) map[string]string {
	environ := validBase()
	for name, value := range overrides {
		environ[name] = value
	}
	return environ
}

// mustLoad fails the test when a configuration that should be valid is not.
func mustLoad(t *testing.T, environ map[string]string) Config {
	t.Helper()
	cfg, err := LoadFrom(environ)
	if err != nil {
		t.Fatalf("LoadFrom(%v) failed: %v", environ, err)
	}
	return cfg
}

// loadError asserts the configuration is rejected and returns the message.
func loadError(t *testing.T, environ map[string]string) string {
	t.Helper()
	cfg, err := LoadFrom(environ)
	if err == nil {
		t.Fatalf("LoadFrom(%v) unexpectedly succeeded: %+v", environ, cfg)
	}
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("LoadFrom(%v) returned %T, want *ValidationError", environ, err)
	}
	if len(invalid.Problems) == 0 {
		t.Fatalf("LoadFrom(%v) returned a ValidationError with no problems", environ)
	}
	// A rejected configuration must never leak a partially built value.
	if cfg != (Config{}) {
		t.Errorf("LoadFrom(%v) returned a non-zero Config alongside its error", environ)
	}
	return err.Error()
}

// assertContains fails with the full message, which is what an operator sees.
func assertContains(t *testing.T, message string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Errorf("message does not contain %q\nfull message:\n%s", want, message)
		}
	}
}

// cleanEnvironment removes every variable this package reads from the process
// environment, so an ambient developer setup cannot decide a test's outcome.
// The set is derived from the struct, so a new field is isolated automatically.
func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, spec := range specs() {
		previous, ok := os.LookupEnv(spec.variable)
		if !ok {
			continue
		}
		t.Cleanup(func() {
			if err := os.Setenv(spec.variable, previous); err != nil {
				t.Errorf("restoring %s: %v", spec.variable, err)
			}
		})
		if err := os.Unsetenv(spec.variable); err != nil {
			t.Fatalf("unsetting %s: %v", spec.variable, err)
		}
	}
}

func TestDefaultConfigurationIsValid(t *testing.T) {
	cfg := mustLoad(t, validBase())

	if cfg.Entrypoint != EntrypointAgent {
		t.Errorf("Entrypoint = %q, want %q", cfg.Entrypoint, EntrypointAgent)
	}
	if cfg.ModelProvider != ProviderOpenAICompatible {
		t.Errorf("ModelProvider = %q, want %q", cfg.ModelProvider, ProviderOpenAICompatible)
	}
	if cfg.Model != "qwen3:4b-instruct" {
		t.Errorf("Model = %q, want %q", cfg.Model, "qwen3:4b-instruct")
	}
	if cfg.ModelTemperature != nil {
		t.Errorf("ModelTemperature = %v, want unset", *cfg.ModelTemperature)
	}
	if cfg.OpenAIBaseURL != "http://127.0.0.1:11434/v1" {
		t.Errorf("OpenAIBaseURL = %q", cfg.OpenAIBaseURL)
	}
	if cfg.OpenAIAPIKey.Reveal() != "local-ollama" {
		t.Errorf("OpenAIAPIKey = %q, want the non-secret local marker", cfg.OpenAIAPIKey.Reveal())
	}
	if cfg.MCPURL != "" {
		t.Errorf("MCPURL = %q, want unset", cfg.MCPURL)
	}
	if cfg.PIIModel != "qwen3:4b-instruct" || cfg.PIIModelBaseURL != "http://127.0.0.1:11434/v1" || cfg.PIIModelTimeout != 8 {
		t.Errorf("PII model settings = %q/%q/%v, want local bounded defaults",
			cfg.PIIModel, cfg.PIIModelBaseURL, cfg.PIIModelTimeout)
	}
	if !cfg.PIIModelEnabled {
		t.Error("PIIModelEnabled = false, want the OpenAI/Ollama default enabled")
	}
	if cfg.A2ABindHost != "127.0.0.1" {
		t.Errorf("A2ABindHost = %q, want the loopback-only default", cfg.A2ABindHost)
	}
	if cfg.A2AHost != "localhost" {
		t.Errorf("A2AHost = %q", cfg.A2AHost)
	}
	if cfg.EmbeddingTimeout != 120 {
		t.Errorf("EmbeddingTimeout = %v, want 120", cfg.EmbeddingTimeout)
	}
	if !cfg.SanitizeToolOutput {
		t.Error("SanitizeToolOutput = false, want the default-on injection hardening")
	}
	if cfg.DataDir != "../data" || cfg.StateDir != ".state" {
		t.Errorf("DataDir/StateDir = %q/%q", cfg.DataDir, cfg.StateDir)
	}
	if cfg.SessionBackend != SessionBackendSQLite {
		t.Errorf("SessionBackend = %q, want the account-free %q default", cfg.SessionBackend, SessionBackendSQLite)
	}
	if cfg.SessionDSN != "" {
		t.Errorf("SessionDSN = %q, want unset when sessions are a local file", cfg.SessionDSN.Reveal())
	}
	if cfg.SessionMaxConns != 10 {
		t.Errorf("SessionMaxConns = %d, want 10", cfg.SessionMaxConns)
	}
}

func TestSessionBackendIsAValidatedChoice(t *testing.T) {
	// The sqlite member must load on its own; postgres carries a DSN, so it is
	// exercised with one below rather than here.
	cfg := mustLoad(t, withEnv(map[string]string{EnvSessionBackend: string(SessionBackendSQLite)}))
	if cfg.SessionBackend != SessionBackendSQLite {
		t.Errorf("SessionBackend = %q, want %q", cfg.SessionBackend, SessionBackendSQLite)
	}

	message := loadError(t, withEnv(map[string]string{EnvSessionBackend: "mysql"}))
	assertContains(t, message, EnvSessionBackend, "sqlite, postgres", `"mysql"`)
}

func TestPostgresSessionBackendLoadsWithADSN(t *testing.T) {
	cfg := mustLoad(t, withEnv(map[string]string{
		EnvSessionBackend:  string(SessionBackendPostgres),
		EnvSessionDSN:      "postgres://agentops:secret@agentops-sessions:5432/sessions?sslmode=disable",
		EnvSessionMaxConns: "4",
	}))
	if cfg.SessionBackend != SessionBackendPostgres {
		t.Errorf("SessionBackend = %q, want %q", cfg.SessionBackend, SessionBackendPostgres)
	}
	if cfg.SessionMaxConns != 4 {
		t.Errorf("SessionMaxConns = %d, want 4", cfg.SessionMaxConns)
	}
	// The DSN is readable through the deliberate Reveal call and nowhere else.
	if cfg.SessionDSN.String() != SecretMask {
		t.Errorf("SessionDSN renders as %q, want the mask", cfg.SessionDSN.String())
	}
	if !strings.HasPrefix(cfg.SessionDSN.Reveal(), "postgres://agentops:secret@") {
		t.Errorf("SessionDSN.Reveal() = %q, want the configured URL", cfg.SessionDSN.Reveal())
	}
}

func TestPostgresSessionBackendRequiresADSN(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{EnvSessionBackend: string(SessionBackendPostgres)}))
	assertContains(t, message, EnvSessionBackend+"=postgres requires "+EnvSessionDSN, "sslmode=disable")
}

func TestSQLiteSessionBackendRejectsAnIgnoredDSN(t *testing.T) {
	// A DSN beside the sqlite backend is a setting the operator believes is in
	// effect while every session still lands on local disk.
	message := loadError(t, withEnv(map[string]string{
		EnvSessionDSN: "postgres://agentops@127.0.0.1:5432/sessions",
	}))
	assertContains(t, message, EnvSessionDSN+" is only read when "+EnvSessionBackend+"=postgres", EnvStateDir)
}

func TestSessionDSNProblemsNeverEchoTheCredential(t *testing.T) {
	for name, dsn := range map[string]string{
		"wrong scheme":  "mysql://agentops:hunter2@db:3306/sessions",
		"no host":       "postgres:///sessions",
		"empty port":    "postgres://agentops:hunter2@db:/sessions",
		"port zero":     "postgres://agentops:hunter2@db:0/sessions",
		"port too high": "postgres://agentops:hunter2@db:65536/sessions",
		"no database":   "postgres://agentops:hunter2@db:5432/",
		"unparseable":   "postgres://agentops:hunter2@db host:5432/sessions",
	} {
		t.Run(name, func(t *testing.T) {
			message := loadError(t, withEnv(map[string]string{
				EnvSessionBackend: string(SessionBackendPostgres),
				EnvSessionDSN:     dsn,
			}))
			assertContains(t, message, EnvSessionDSN)
			if strings.Contains(message, "hunter2") || strings.Contains(message, dsn) {
				t.Errorf("rejection echoed the credential-bearing DSN:\n%s", message)
			}
		})
	}
}

func TestSessionDSNAcceptsBothLibpqSchemes(t *testing.T) {
	for _, dsn := range []string{
		"postgres://agentops@db:5432/sessions",
		"postgresql://agentops:secret@db/sessions?sslmode=require",
	} {
		mustLoad(t, withEnv(map[string]string{
			EnvSessionBackend: string(SessionBackendPostgres),
			EnvSessionDSN:     dsn,
		}))
	}
}

func TestEntrypointIsAValidatedChoice(t *testing.T) {
	for _, entrypoint := range Entrypoints() {
		cfg := mustLoad(t, withEnv(map[string]string{EnvEntrypoint: string(entrypoint)}))
		if cfg.Entrypoint != entrypoint {
			t.Errorf("Entrypoint = %q, want %q", cfg.Entrypoint, entrypoint)
		}
	}

	message := loadError(t, withEnv(map[string]string{EnvEntrypoint: "unknown"}))
	assertContains(t, message, EnvEntrypoint, "agent, workflow, coordinator", `"unknown"`)
}

func TestModelProviderIsAValidatedChoice(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{EnvModelProvider: "anthropic"}))
	assertContains(t, message, EnvModelProvider, "openai-compatible, gemini", `"anthropic"`)
}

func TestLoadIgnoresALocalDotEnvFile(t *testing.T) {
	// Task runners inject the environment; the agent never reads a .env itself.
	// A file in the working directory must therefore change nothing.
	cleanEnvironment(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("AGENT_MODEL=dotenv-must-not-load\n"), 0o600); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Model != "qwen3:4b-instruct" {
		t.Errorf("Model = %q, want the committed default; a .env must not be loaded", cfg.Model)
	}
}

func TestRemovedGatewayFlagFailsWithMigrationGuidance(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv(EnvGatewayEnabled, "true")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded with the removed AGENT_GATEWAY_ENABLED set")
	}
	assertContains(t, err.Error(),
		"AGENT_GATEWAY_ENABLED was removed",
		"AGENT_MODEL_PROVIDER=openai-compatible",
		"OPENAI_BASE_URL")
}

func TestRemovedGatewayFlagRejectsEveryValue(t *testing.T) {
	// The variable is not an alias of a live field: *any* value means the
	// operator is following stale instructions, so "false" and "" must fail too.
	for _, value := range []string{"true", "false", "1", "0", ""} {
		t.Run(fmt.Sprintf("value=%q", value), func(t *testing.T) {
			message := loadError(t, withEnv(map[string]string{EnvGatewayEnabled: value}))
			assertContains(t, message, "AGENT_GATEWAY_ENABLED was removed")
		})
	}
}

func TestOpenAICompatibleProviderRequiresBaseURL(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{
		EnvModelProvider: string(ProviderOpenAICompatible),
		EnvOpenAIBaseURL: "",
	}))
	assertContains(t, message, "requires OPENAI_BASE_URL", "direct Ollama", "host agentgateway")
}

func TestOpenAICompatibleProviderRequiresAPIKey(t *testing.T) {
	for name, key := range map[string]string{"empty": "", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			message := loadError(t, withEnv(map[string]string{
				EnvModelProvider: string(ProviderOpenAICompatible),
				EnvOpenAIBaseURL: "http://127.0.0.1:4000/v1",
				EnvOpenAIAPIKey:  key,
			}))
			assertContains(t, message, "requires OPENAI_API_KEY", "local-ollama")
		})
	}
}

func TestValidOpenAICompatibleGatewayCombination(t *testing.T) {
	cfg := mustLoad(t, withEnv(map[string]string{
		EnvModelProvider: string(ProviderOpenAICompatible),
		EnvOpenAIBaseURL: "http://127.0.0.1:4000/v1",
		EnvOpenAIAPIKey:  "local-agentgateway",
	}))
	if cfg.OpenAIBaseURL != "http://127.0.0.1:4000/v1" {
		t.Errorf("OpenAIBaseURL = %q", cfg.OpenAIBaseURL)
	}
}

func TestModelTemperatureIsOptionalAndBounded(t *testing.T) {
	cfg := mustLoad(t, withEnv(map[string]string{EnvModelTemperature: "0"}))
	if cfg.ModelTemperature == nil || *cfg.ModelTemperature != 0 {
		t.Fatalf("ModelTemperature = %v, want an explicit 0 (greedy sampling)", cfg.ModelTemperature)
	}

	message := loadError(t, withEnv(map[string]string{EnvModelTemperature: "2.1"}))
	assertContains(t, message, EnvModelTemperature, "between 0 and 2")
}

func TestMCPURLMustBeHTTP(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{EnvMCPURL: "ftp://example.invalid/mcp"}))
	assertContains(t, message, "AGENT_MCP_URL must be an http", "rejected value is omitted")
	if strings.Contains(message, "ftp://example.invalid/mcp") {
		t.Errorf("validation problem echoed the rejected endpoint: %q", message)
	}

	cfg := mustLoad(t, withEnv(map[string]string{EnvMCPURL: "http://127.0.0.1:3000/mcp"}))
	if cfg.MCPURL != "http://127.0.0.1:3000/mcp" {
		t.Errorf("MCPURL = %q", cfg.MCPURL)
	}
}

func TestConfiguredExternalURLsMustBeAbsoluteHTTPURLsWithAHost(t *testing.T) {
	for _, variable := range []string{
		EnvOpenAIBaseURL,
		EnvMCPURL,
		EnvPIIModelBaseURL,
		EnvEmbeddingsURL,
	} {
		for _, value := range []string{"http://%zz", "http:///missing-host", "http://:8080"} {
			t.Run(variable+"/"+value, func(t *testing.T) {
				message := loadError(t, withEnv(map[string]string{variable: value}))
				assertContains(t, message, variable, "absolute", "host")
			})
		}
	}
}

func TestConfiguredExternalURLPortsMustBeDialable(t *testing.T) {
	for _, variable := range []string{
		EnvOpenAIBaseURL,
		EnvMCPURL,
		EnvPIIModelBaseURL,
		EnvEmbeddingsURL,
	} {
		for _, value := range []string{"http://example.test:0", "http://example.test:65536", "http://example.test:"} {
			t.Run(variable+"/"+value, func(t *testing.T) {
				message := loadError(t, withEnv(map[string]string{variable: value}))
				assertContains(t, message, variable, "port", "1", "65535")
			})
		}
	}
}

func TestExternalURLProblemsNeverEchoCredentials(t *testing.T) {
	for _, variable := range []string{
		EnvOpenAIBaseURL,
		EnvMCPURL,
		EnvPIIModelBaseURL,
		EnvEmbeddingsURL,
	} {
		t.Run(variable, func(t *testing.T) {
			const secret = "not-a-real-secret-value"
			message := loadError(t, withEnv(map[string]string{
				variable: "http://operator:" + secret + "@example.test/path?token=" + secret,
			}))
			assertContains(t, message, variable, "credentials")
			if strings.Contains(message, secret) {
				t.Errorf("validation problem retained the URL credential: %q", message)
			}
		})
	}
}

func TestPIIModelURLMustBeHTTP(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{EnvPIIModelBaseURL: "127.0.0.1:11434/v1"}))
	assertContains(t, message, "AGENT_PII_MODEL_BASE_URL must be an http", "rejected value is omitted")

	cfg := mustLoad(t, withEnv(map[string]string{EnvPIIModelBaseURL: "http://ollama:11434/v1"}))
	if cfg.PIIModelBaseURL != "http://ollama:11434/v1" {
		t.Errorf("PIIModelBaseURL = %q", cfg.PIIModelBaseURL)
	}
}

func TestModelFallbackMustDifferFromThePrimaryModel(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{
		EnvModel:         "qwen3:4b-instruct",
		EnvModelFallback: "qwen3:4b-instruct",
	}))
	assertContains(t, message, "AGENT_MODEL_FALLBACK must differ from AGENT_MODEL", "qwen3:4b-instruct")

	cfg := mustLoad(t, withEnv(map[string]string{EnvModelFallback: "qwen3:1.7b"}))
	if cfg.ModelFallback == nil || *cfg.ModelFallback != "qwen3:1.7b" {
		t.Errorf("ModelFallback = %v", cfg.ModelFallback)
	}
}

func TestA2ABindAndAdvertisedHostsAreDistinct(t *testing.T) {
	cfg := mustLoad(t, withEnv(map[string]string{
		// Kubernetes explicitly opts into a container-wide bind; the advertised
		// host stays a name a caller can dial.
		EnvA2ABindHost: "0.0.0.0",
		EnvA2AHost:     "agentops-agent.localhost",
	}))
	if cfg.A2ABindHost != "0.0.0.0" || cfg.A2AHost != "agentops-agent.localhost" {
		t.Errorf("bind/advertise = %q/%q", cfg.A2ABindHost, cfg.A2AHost)
	}

	message := loadError(t, withEnv(map[string]string{EnvA2ABindHost: ""}))
	assertContains(t, message, EnvA2ABindHost)
}

func TestGeminiAPIKeyProviderDoesNotRequireOpenAIConfiguration(t *testing.T) {
	// The OpenAI checks are provider-gated: selecting Gemini must not demand
	// credentials for an adapter that is not in use.
	cfg := mustLoad(t, withEnv(map[string]string{
		EnvModelProvider: string(ProviderGemini),
		EnvGoogleAPIKey:  "gemini-secret",
		EnvOpenAIBaseURL: "",
		EnvOpenAIAPIKey:  "",
	}))
	if cfg.ModelProvider != ProviderGemini {
		t.Errorf("ModelProvider = %q", cfg.ModelProvider)
	}
	if cfg.GoogleAPIKey.Reveal() != "gemini-secret" {
		t.Errorf("GoogleAPIKey = %q", cfg.GoogleAPIKey.Reveal())
	}
}

func TestGeminiProviderRequiresAnExplicitAuthPath(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{EnvModelProvider: string(ProviderGemini)}))
	assertContains(t, message, "requires either GOOGLE_API_KEY",
		"GOOGLE_GENAI_USE_ENTERPRISE=true", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION")
}

func TestGeminiEnterpriseProviderAcceptsTheADCCoursePath(t *testing.T) {
	cfg := mustLoad(t, withEnv(map[string]string{
		EnvModelProvider:            string(ProviderGemini),
		EnvGoogleGenAIUseEnterprise: "true",
		EnvGoogleCloudProject:       "agentops-open-course",
		EnvGoogleCloudLocation:      "global",
	}))
	if !cfg.GoogleGenAIUseEnterprise {
		t.Error("GoogleGenAIUseEnterprise = false, want true")
	}
	if cfg.GoogleCloudProject != "agentops-open-course" || cfg.GoogleCloudLocation != "global" {
		t.Errorf("project/location = %q/%q", cfg.GoogleCloudProject, cfg.GoogleCloudLocation)
	}
}

func TestGeminiEnterpriseProviderRequiresProjectAndLocation(t *testing.T) {
	tests := map[string]struct {
		project  string
		location string
		want     []string
	}{
		"missing project":  {project: "", location: "global", want: []string{EnvGoogleCloudProject}},
		"missing location": {project: "agentops-open-course", location: "", want: []string{EnvGoogleCloudLocation}},
		"blank project":    {project: "   ", location: "global", want: []string{EnvGoogleCloudProject}},
		"missing both": {
			project:  "",
			location: "",
			want:     []string{EnvGoogleCloudProject + " and " + EnvGoogleCloudLocation},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			message := loadError(t, withEnv(map[string]string{
				EnvModelProvider:            string(ProviderGemini),
				EnvGoogleGenAIUseEnterprise: "true",
				EnvGoogleCloudProject:       test.project,
				EnvGoogleCloudLocation:      test.location,
			}))
			assertContains(t, message, test.want...)
		})
	}
}

func TestGeminiProviderRejectsAmbiguousAPIKeyAndEnterpriseAuth(t *testing.T) {
	message := loadError(t, withEnv(map[string]string{
		EnvModelProvider:            string(ProviderGemini),
		EnvGoogleAPIKey:             "gemini-secret",
		EnvGoogleGenAIUseEnterprise: "true",
		EnvGoogleCloudProject:       "agentops-open-course",
		EnvGoogleCloudLocation:      "global",
	}))
	assertContains(t, message, "cannot combine GOOGLE_API_KEY", "GOOGLE_GENAI_USE_ENTERPRISE=true")
}

func TestProviderEnvironmentVariablesAreReadFromTheProcess(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv(EnvModelProvider, string(ProviderOpenAICompatible))
	t.Setenv(EnvOpenAIBaseURL, "http://127.0.0.1:4000/v1")
	t.Setenv(EnvOpenAIAPIKey, "super-sensitive")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.ModelProvider != ProviderOpenAICompatible {
		t.Errorf("ModelProvider = %q", cfg.ModelProvider)
	}
	if cfg.OpenAIAPIKey.Reveal() != "super-sensitive" {
		t.Errorf("OpenAIAPIKey did not round-trip through the environment")
	}
	assertSecretNeverRendered(t, cfg, "super-sensitive")
}

func TestGeminiEnvironmentVariablesAreReadFromTheProcess(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv(EnvModelProvider, string(ProviderGemini))
	t.Setenv(EnvGoogleAPIKey, "gemini-sensitive")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.ModelProvider != ProviderGemini {
		t.Errorf("ModelProvider = %q", cfg.ModelProvider)
	}
	assertSecretNeverRendered(t, cfg, "gemini-sensitive")
}

// assertSecretNeverRendered proves the Secret type, not caller discipline, is
// what keeps a credential out of logs and errors.
func assertSecretNeverRendered(t *testing.T, cfg Config, plaintext string) {
	t.Helper()
	openAIText, err := cfg.OpenAIAPIKey.MarshalText()
	if err != nil {
		t.Fatalf("marshaling the OpenAI key: %v", err)
	}
	googleText, err := cfg.GoogleAPIKey.MarshalText()
	if err != nil {
		t.Fatalf("marshaling the Google key: %v", err)
	}
	renderings := map[string]string{
		"%v on the struct":       fmt.Sprintf("%v", cfg),
		"%+v on the struct":      fmt.Sprintf("%+v", cfg),
		"%q on the OpenAI key":   fmt.Sprintf("%q", cfg.OpenAIAPIKey),
		"%q on the Google key":   fmt.Sprintf("%q", cfg.GoogleAPIKey),
		"String on the keys":     cfg.OpenAIAPIKey.String() + cfg.GoogleAPIKey.String(),
		"MarshalText (JSON etc)": string(openAIText) + string(googleText),
	}
	for rendering, rendered := range renderings {
		if strings.Contains(rendered, plaintext) {
			t.Errorf("%s leaked the secret: %s", rendering, rendered)
		}
	}
	for _, setting := range cfg.Describe() {
		if strings.Contains(setting.Value, plaintext) {
			t.Errorf("Describe leaked the secret in %s", setting.Variable)
		}
	}
}

func TestFieldBoundsMatrix(t *testing.T) {
	// One row per bound the Python track expresses as a pydantic Field
	// constraint. `ok` values must load; `bad` values must be rejected naming
	// their own variable.
	tests := map[string]struct {
		variable string
		ok       []string
		bad      []string
	}{
		"model":                  {EnvModel, []string{"qwen3:1.7b"}, []string{""}},
		"a2a port":               {EnvA2APort, []string{"1", "8080", "65535"}, []string{"0", "65536", "-1", "http"}},
		"a2a protocol":           {EnvA2AProtocol, []string{"http", "https"}, []string{"ftp", "HTTP", ""}},
		"a2a max llm calls":      {EnvA2AMaxLLMCalls, []string{"1", "12", "100"}, []string{"0", "101"}},
		"drain timeout":          {EnvDrainTimeout, []string{"0.5", "10", "300"}, []string{"0", "300.1", "-1"}},
		"model timeout":          {EnvModelTimeout, []string{"0.1", "60", "600", "3600"}, []string{"0", "3600.1"}},
		"pii model timeout":      {EnvPIIModelTimeout, []string{"0.1", "8", "9"}, []string{"0", "9.1"}},
		"tool timeout":           {EnvToolTimeout, []string{"30", "600"}, []string{"0", "601"}},
		"max retries":            {EnvMaxRetries, []string{"0", "2", "10"}, []string{"-1", "11"}},
		"session max conns":      {EnvSessionMaxConns, []string{"1", "10", "100"}, []string{"0", "101", "-1", "many"}},
		"retry backoff":          {EnvRetryBackoff, []string{"0.5", "30"}, []string{"0", "30.1"}},
		"circuit threshold":      {EnvCircuitFailureThreshold, []string{"1", "5", "100"}, []string{"0", "101"}},
		"circuit reset":          {EnvCircuitResetTimeout, []string{"30", "600"}, []string{"0", "601"}},
		"embedding timeout":      {EnvEmbeddingTimeout, []string{"120", "600"}, []string{"0", "601"}},
		"embeddings url":         {EnvEmbeddingsURL, []string{"http://ollama:11434"}, []string{""}},
		"embedding model":        {EnvEmbeddingModel, []string{"nomic-embed-text"}, []string{""}},
		"max history messages":   {EnvMaxHistoryMessages, []string{"2", "40"}, []string{"1", "0", "-1"}},
		"max tokens per session": {EnvMaxTokensPerSession, []string{"1", "50000"}, []string{"0", "-1"}},
		"input price":            {EnvInputPricePer1K, []string{"0", "0.15"}, []string{"-0.1"}},
		"output price":           {EnvOutputPricePer1K, []string{"0", "0.6"}, []string{"-0.1"}},
		"trusted identity header": {
			EnvTrustedIdentityHeader,
			[]string{"x-verified-subject", "X-Verified_Subject"},
			[]string{"", "x verified subject", ":authority", "x\nsubject"},
		},
		"model fallback":          {EnvModelFallback, []string{"qwen3:1.7b"}, []string{""}},
		"a2a bind host":           {EnvA2ABindHost, []string{"0.0.0.0"}, []string{""}},
		"a2a advertised host":     {EnvA2AHost, []string{"agentops-agent.localhost"}, []string{""}},
		"a2a streaming":           {EnvA2AStreaming, []string{"true", "false"}, []string{"yes", ""}},
		"circuit breaker enabled": {EnvCircuitBreakerEnabled, []string{"true", "false"}, []string{"on", ""}},
		"model temperature":       {EnvModelTemperature, []string{"0", "1", "2"}, []string{"-0.1", "2.1", "warm"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for _, value := range test.ok {
				mustLoad(t, withEnv(map[string]string{test.variable: value}))
			}
			for _, value := range test.bad {
				message := loadError(t, withEnv(map[string]string{test.variable: value}))
				assertContains(t, message, test.variable)
			}
		})
	}
}

func TestEveryExternalFloatRejectsNonFiniteEnvironmentValues(t *testing.T) {
	variables := []string{
		EnvModelTemperature,
		EnvDrainTimeout,
		EnvModelTimeout,
		EnvPIIModelTimeout,
		EnvToolTimeout,
		EnvRetryBackoff,
		EnvCircuitResetTimeout,
		EnvEmbeddingTimeout,
		EnvInputPricePer1K,
		EnvOutputPricePer1K,
	}
	for _, variable := range variables {
		for _, value := range []string{"NaN", "+Inf", "-Inf"} {
			t.Run(variable+"/"+value, func(t *testing.T) {
				message := loadError(t, withEnv(map[string]string{variable: value}))
				assertContains(t, message, variable, "finite")
			})
		}
	}
}

func TestEveryExternalFloatRejectsNonFiniteDirectConfiguration(t *testing.T) {
	type fieldCase struct {
		set      func(*Config, float64)
		variable string
	}
	fields := []fieldCase{
		{variable: EnvModelTemperature, set: func(cfg *Config, value float64) { cfg.ModelTemperature = &value }},
		{variable: EnvDrainTimeout, set: func(cfg *Config, value float64) { cfg.DrainTimeout = Seconds(value) }},
		{variable: EnvModelTimeout, set: func(cfg *Config, value float64) { cfg.ModelTimeout = Seconds(value) }},
		{variable: EnvPIIModelTimeout, set: func(cfg *Config, value float64) { cfg.PIIModelTimeout = Seconds(value) }},
		{variable: EnvToolTimeout, set: func(cfg *Config, value float64) { cfg.ToolTimeout = Seconds(value) }},
		{variable: EnvRetryBackoff, set: func(cfg *Config, value float64) { cfg.RetryBackoff = Seconds(value) }},
		{variable: EnvCircuitResetTimeout, set: func(cfg *Config, value float64) { cfg.CircuitResetTimeout = Seconds(value) }},
		{variable: EnvEmbeddingTimeout, set: func(cfg *Config, value float64) { cfg.EmbeddingTimeout = Seconds(value) }},
		{variable: EnvInputPricePer1K, set: func(cfg *Config, value float64) { cfg.InputPricePer1K = value }},
		{variable: EnvOutputPricePer1K, set: func(cfg *Config, value float64) { cfg.OutputPricePer1K = value }},
	}
	for _, field := range fields {
		for name, value := range map[string]float64{
			"nan":               math.NaN(),
			"positive infinity": math.Inf(1),
			"negative infinity": math.Inf(-1),
		} {
			t.Run(field.variable+"/"+name, func(t *testing.T) {
				cfg := mustLoad(t, validBase())
				field.set(&cfg, value)
				problems := cfg.fieldProblems()
				if len(problems) != 1 {
					t.Fatalf("fieldProblems() = %v, want exactly one finite-value problem", problems)
				}
				if problems[0].Variable != field.variable || !strings.Contains(problems[0].Message, "finite") {
					t.Errorf("problem = %+v, want %s finite-value context", problems[0], field.variable)
				}
			})
		}
	}
}

func TestFieldProblemsAccumulateSoEveryMistakeIsReportedAtOnce(t *testing.T) {
	// The whole point of the accumulating shape: a learner with three bad values
	// fixes three values, not one per run.
	_, err := LoadFrom(withEnv(map[string]string{
		EnvA2APort:          "70000",
		EnvMaxRetries:       "99",
		EnvModelTemperature: "3",
	}))
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(invalid.Problems) != 3 {
		t.Fatalf("reported %d problems, want 3: %v", len(invalid.Problems), invalid.Problems)
	}
	assertContains(t, err.Error(), EnvA2APort, EnvMaxRetries, EnvModelTemperature)
}

func TestProviderProblemsAccumulateAndShortCircuitLaterPhases(t *testing.T) {
	// Both provider credentials are missing *and* the MCP URL is wrong. The
	// provider phase is reported whole and on its own, exactly as the Python
	// validator raises its provider list before it looks at anything else.
	_, err := LoadFrom(withEnv(map[string]string{
		EnvOpenAIBaseURL: "",
		EnvOpenAIAPIKey:  "",
		EnvMCPURL:        "ftp://example.invalid/mcp",
	}))
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(invalid.Problems) != 2 {
		t.Fatalf("reported %d problems, want the 2 provider problems: %v", len(invalid.Problems), invalid.Problems)
	}
	assertContains(t, err.Error(), "requires OPENAI_BASE_URL", "requires OPENAI_API_KEY")
	if strings.Contains(err.Error(), "AGENT_MCP_URL") {
		t.Error("provider problems must short-circuit the cross-field phase")
	}
}

func TestOneVariableIsReportedOnce(t *testing.T) {
	// A value that fails to parse also fails the bound check on the resulting
	// zero value; the operator must be told about one mistake once.
	_, err := LoadFrom(withEnv(map[string]string{EnvA2APort: "not-a-number"}))
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(invalid.Problems) != 1 {
		t.Fatalf("reported %d problems, want 1: %v", len(invalid.Problems), invalid.Problems)
	}
	assertContains(t, invalid.Problems[0].String(), EnvA2APort, "not-a-number")
}

func TestExplicitlyEmptyVariablesAreNeverSilentDefaults(t *testing.T) {
	// caarlos0/env resolves an explicitly empty variable to its envDefault. Left
	// alone, `OPENAI_API_KEY=` in a .env would silently become "local-ollama"
	// and `AGENT_MODEL=` would become the shipped model. Every one of these must
	// be rejected instead.
	for _, variable := range []string{
		EnvEntrypoint, EnvModelProvider, EnvModel, EnvOpenAIBaseURL, EnvOpenAIAPIKey,
		EnvA2ABindHost, EnvA2APort, EnvDrainTimeout, EnvEmbeddingModel,
		EnvSanitizeToolOutput, EnvMaxHistoryMessages,
	} {
		t.Run(variable, func(t *testing.T) {
			message := loadError(t, withEnv(map[string]string{variable: ""}))
			assertContains(t, message, variable)
		})
	}

	// An optional credential is the exception: empty means "not configured",
	// which is exactly what unset means, and the Python track accepts it.
	for _, variable := range []string{EnvGoogleAPIKey, EnvMCPToken, EnvMCPURL} {
		t.Run(variable+" tolerates empty", func(t *testing.T) {
			mustLoad(t, withEnv(map[string]string{variable: ""}))
		})
	}
}

func TestSecondsConvertToDurations(t *testing.T) {
	cfg := mustLoad(t, withEnv(map[string]string{
		EnvModelTimeout: "1.5",
		EnvRetryBackoff: "0.25",
	}))
	if got := cfg.ModelTimeout.Duration(); got != 1500*time.Millisecond {
		t.Errorf("ModelTimeout.Duration() = %v, want 1.5s", got)
	}
	if got := cfg.RetryBackoff.Duration(); got != 250*time.Millisecond {
		t.Errorf("RetryBackoff.Duration() = %v, want 250ms", got)
	}
}

func TestVariableConstantsMatchStructTags(t *testing.T) {
	// Go struct tags cannot reference constants, so this is what keeps the
	// exported Env* names and the `env:"..."` tags from drifting apart.
	fromTags := make([]string, 0, len(specs()))
	for _, spec := range specs() {
		fromTags = append(fromTags, spec.variable)
	}
	fromConstants := []string{
		EnvEntrypoint, EnvModelProvider, EnvModel, EnvOpenAIBaseURL, EnvOpenAIAPIKey,
		EnvGoogleAPIKey, EnvGoogleGenAIUseEnterprise, EnvGoogleCloudProject, EnvGoogleCloudLocation,
		EnvGatewayEnabled, EnvMCPURL, EnvMCPToken, EnvDataDir, EnvStateDir,
		EnvSessionBackend, EnvSessionDSN, EnvSessionMaxConns,
		EnvA2ABindHost, EnvA2AHost, EnvA2APort, EnvA2AProtocol, EnvA2AMaxLLMCalls,
		EnvA2AStreaming, EnvDrainTimeout, EnvTrustedIdentityHeader, EnvModelTemperature,
		EnvModelTimeout, EnvToolTimeout, EnvMaxRetries, EnvRetryBackoff,
		EnvCircuitBreakerEnabled, EnvCircuitFailureThreshold, EnvCircuitResetTimeout,
		EnvModelFallback, EnvWritesDisabled, EnvSanitizeToolOutput, EnvPIIModel,
		EnvPIIModelBaseURL, EnvPIIModelEnabled, EnvPIIModelTimeout,
		EnvSemanticRetrieval, EnvEmbeddingsURL, EnvEmbeddingModel, EnvEmbeddingTimeout,
		EnvMaxHistoryMessages, EnvMaxTokensPerSession, EnvInputPricePer1K, EnvOutputPricePer1K,
	}
	slices.Sort(fromTags)
	slices.Sort(fromConstants)
	if !slices.Equal(fromTags, fromConstants) {
		t.Errorf("struct tags and Env* constants disagree:\ntags:      %v\nconstants: %v", fromTags, fromConstants)
	}
}

func TestEveryFieldCarriesAnEnvironmentTag(t *testing.T) {
	// A field without a tag is unreachable configuration: it can never be set,
	// never appears in config:check, and never reaches .env.example.
	configType := reflect.TypeFor[Config]()
	if len(specs()) != configType.NumField() {
		for i := range configType.NumField() {
			if _, ok := configType.Field(i).Tag.Lookup("env"); !ok {
				t.Errorf("Config.%s has no `env` tag", configType.Field(i).Name)
			}
		}
	}
}

// TestWritesDisabledFormsAgree pins the two exported spellings of the kill-switch
// refusal to each other.
//
// Three packages return this sentence — the policy guard, the two guarded write
// tools, and the notes writer — and before they shared it they had drifted into
// two capitalizations and two endings, which left an operator unable to tell
// whether three refusals meant one switch. Sharing the text fixed that; this test
// is what keeps the clause form and the sentence form from drifting apart again,
// and it names the variable, because the person reading the refusal is the person
// who can clear it.
func TestWritesDisabledFormsAgree(t *testing.T) {
	t.Parallel()
	if !strings.Contains(WritesDisabledReason, EnvWritesDisabled) {
		t.Fatalf("the refusal must name %s: %q", EnvWritesDisabled, WritesDisabledReason)
	}
	if strings.HasSuffix(WritesDisabledReason, ".") {
		t.Fatalf("the reason clause must not end in a period, so callers can embed it: %q", WritesDisabledReason)
	}
	want := strings.ToUpper(WritesDisabledReason[:1]) + WritesDisabledReason[1:] + "."
	if WritesDisabledRefusal != want {
		t.Fatalf("WritesDisabledRefusal = %q, want the capitalized reason %q", WritesDisabledRefusal, want)
	}
}
