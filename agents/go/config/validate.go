package config

import (
	"cmp"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Problem is one actionable configuration defect: which variable is at fault
// and what to do about it.
//
// Variable is empty for problems that no single variable owns — a provider
// selected without its credentials, a fallback model equal to the primary.
type Problem struct {
	Variable string
	Message  string
}

// String renders one problem the way an operator reads it.
func (p Problem) String() string {
	if p.Variable == "" {
		return p.Message
	}
	return p.Variable + ": " + p.Message
}

// ValidationError reports every problem found in one phase of Load.
//
// Problems accumulate within a phase and are reported whole: a learner with
// three bad values sees all three, not the first one three times over three
// edit-and-rerun cycles.
type ValidationError struct{ Problems []Problem }

// Error renders every problem, one per line.
func (e *ValidationError) Error() string {
	rendered := make([]string, 0, len(e.Problems)+1)
	rendered = append(rendered, fmt.Sprintf("agent configuration is invalid (%d problems)", len(e.Problems)))
	for _, problem := range e.Problems {
		rendered = append(rendered, "  - "+problem.String())
	}
	return strings.Join(rendered, "\n")
}

// problems accumulates defects within one validation phase.
type problems []Problem

func (p *problems) add(variable, format string, args ...any) {
	*p = append(*p, Problem{Variable: variable, Message: fmt.Sprintf(format, args...)})
}

// addCrossField records a defect that no single variable owns.
func (p *problems) addCrossField(format string, args ...any) {
	p.add("", format, args...)
}

// closedRange records a problem when value falls outside the inclusive bounds,
// mirroring pydantic's ge/le.
func closedRange[T cmp.Ordered](found *problems, variable string, value, low, high T) {
	if value < low || value > high {
		found.add(variable, "must be between %v and %v, got %v", low, high, value)
	}
}

// positiveAtMost records a problem when value is not strictly above zero or
// exceeds high, mirroring pydantic's gt=0/le. Every deadline uses this rather
// than closedRange: a zero-second deadline can never be met.
func positiveAtMost(found *problems, variable string, value, high Seconds) {
	if !finite(found, variable, float64(value)) {
		return
	}
	if value <= 0 || value > high {
		found.add(variable, "must be greater than 0 and at most %v, got %v", high, value)
	}
}

// finite rejects IEEE-754 sentinel values before ordinary comparisons. NaN
// compares false in every direction, while infinity can otherwise pass an
// open-ended lower bound such as a non-negative token price.
func finite(found *problems, variable string, value float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		found.add(variable, "must be finite, got %v", value)
		return false
	}
	return true
}

// notEmpty records a problem when a required string is blank.
func notEmpty(found *problems, variable, value string) {
	if value == "" {
		found.add(variable, "must not be empty")
	}
}

// validHTTPFieldName implements the RFC 9110 token grammar used for HTTP
// field names. Checking the configured trust seam at startup is safer than
// silently looking up a header that an HTTP server can never accept.
func validHTTPFieldName(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

// fieldProblems checks every per-field constraint the Python track expresses as
// a pydantic Field bound. These run before the cross-field phases, so a bad
// bound never reaches logic that assumes a sane value.
func (c Config) fieldProblems() []Problem {
	var found problems

	// The enum types reject unknown values while parsing, so these two only fire
	// for a Config built in code or for a variable set to the empty string —
	// neither of which may be allowed to select a silently wrong adapter.
	if !c.Entrypoint.Valid() {
		found.add(EnvEntrypoint, "must be one of %s; got %q", joinValues(Entrypoints()), c.Entrypoint)
	}
	if !c.ModelProvider.Valid() {
		found.add(EnvModelProvider, "must be one of %s; got %q", joinValues(ModelProviders()), c.ModelProvider)
	}
	if !c.SessionBackend.Valid() {
		found.add(EnvSessionBackend, "must be one of %s; got %q", joinValues(SessionBackends()), c.SessionBackend)
	}

	notEmpty(&found, EnvModel, c.Model)
	// Python resolves an empty path to the working directory; Go rejects it.
	// An empty seed or state directory is never what an operator meant, and
	// failing here beats a confusing "seed database is missing: /incidents.db"
	// from the data layer three calls later.
	notEmpty(&found, EnvDataDir, c.DataDir)
	notEmpty(&found, EnvStateDir, c.StateDir)
	notEmpty(&found, EnvA2ABindHost, c.A2ABindHost)
	notEmpty(&found, EnvA2AHost, c.A2AHost)
	notEmpty(&found, EnvEmbeddingsURL, c.EmbeddingsURL)
	notEmpty(&found, EnvEmbeddingModel, c.EmbeddingModel)

	closedRange(&found, EnvA2APort, c.A2APort, 1, 65535)
	closedRange(&found, EnvA2AMaxLLMCalls, c.A2AMaxLLMCalls, 1, 100)
	closedRange(&found, EnvMaxRetries, c.MaxRetries, 0, 10)
	closedRange(&found, EnvSessionMaxConns, c.SessionMaxConns, 1, 100)
	closedRange(&found, EnvCircuitFailureThreshold, c.CircuitFailureThreshold, 1, 100)

	positiveAtMost(&found, EnvDrainTimeout, c.DrainTimeout, 300)
	// 3600, not 600. A grounded turn makes at least two model calls and each one
	// re-reads the whole context, so on a CPU-only host the second call is the
	// expensive one: measured here, the first took ~130s and the second exceeded a
	// 600s budget. The default stays at 60 — correct on hardware that can meet it,
	// and a long default would hide a genuinely dead provider behind a long hang —
	// but the ceiling has to leave room for the hardware the course requires.
	positiveAtMost(&found, EnvModelTimeout, c.ModelTimeout, 3600)
	// agentgateway 1.4.1 fixes webhook calls at ten seconds. Finishing first is
	// what lets the reachable service return its conservative mask action.
	positiveAtMost(&found, EnvPIIModelTimeout, c.PIIModelTimeout, 9)
	positiveAtMost(&found, EnvToolTimeout, c.ToolTimeout, 600)
	positiveAtMost(&found, EnvRetryBackoff, c.RetryBackoff, 30)
	positiveAtMost(&found, EnvCircuitResetTimeout, c.CircuitResetTimeout, 600)
	positiveAtMost(&found, EnvEmbeddingTimeout, c.EmbeddingTimeout, 600)

	if c.A2AProtocol != "http" && c.A2AProtocol != "https" {
		found.add(EnvA2AProtocol, "must be http or https, got %q", c.A2AProtocol)
	}
	if finite(&found, EnvInputPricePer1K, c.InputPricePer1K) && c.InputPricePer1K < 0 {
		found.add(EnvInputPricePer1K, "must not be negative, got %v", c.InputPricePer1K)
	}
	if finite(&found, EnvOutputPricePer1K, c.OutputPricePer1K) && c.OutputPricePer1K < 0 {
		found.add(EnvOutputPricePer1K, "must not be negative, got %v", c.OutputPricePer1K)
	}

	// Optional settings are only checked when they are actually set; unset is a
	// valid, documented state for every pointer field.
	if c.ModelTemperature != nil {
		if finite(&found, EnvModelTemperature, *c.ModelTemperature) {
			closedRange(&found, EnvModelTemperature, *c.ModelTemperature, 0, 2)
		}
	}
	if c.TrustedIdentityHeader != nil {
		if !validHTTPFieldName(*c.TrustedIdentityHeader) {
			found.add(EnvTrustedIdentityHeader, "must be a valid HTTP field name")
		}
	}
	if c.ModelFallback != nil {
		notEmpty(&found, EnvModelFallback, *c.ModelFallback)
	}
	if c.MaxHistoryMessages != nil && *c.MaxHistoryMessages < 2 {
		found.add(EnvMaxHistoryMessages, "must be at least 2, got %d", *c.MaxHistoryMessages)
	}
	if c.MaxTokensPerSession != nil && *c.MaxTokensPerSession < 1 {
		found.add(EnvMaxTokensPerSession, "must be at least 1, got %d", *c.MaxTokensPerSession)
	}
	return found
}

// providerProblems rejects model-provider combinations that cannot authenticate.
//
// This phase is reported on its own and short-circuits the rest: with no usable
// credentials there is nothing worth saying about MCP routes or fallback models.
// --8<-- [start:settings-provider-validation]
func (c Config) providerProblems() []Problem {
	var found problems

	// Any value at all — including "false" and the empty string — is a migration
	// signal. Someone who wrote this variable is following stale instructions and
	// must be told so, never handed a silent default.
	if c.DeprecatedGatewayEnabled != nil {
		found.addCrossField(
			"AGENT_GATEWAY_ENABLED was removed. Keep AGENT_MODEL_PROVIDER=openai-compatible " +
				"and select direct Ollama or agentgateway with OPENAI_BASE_URL " +
				"(http://127.0.0.1:11434/v1 or http://127.0.0.1:4000/v1).",
		)
	}
	if c.ModelProvider == ProviderOpenAICompatible && c.OpenAIBaseURL == "" {
		found.addCrossField(
			"AGENT_MODEL_PROVIDER=openai-compatible requires OPENAI_BASE_URL. Use " +
				"http://127.0.0.1:11434/v1 for direct Ollama or http://127.0.0.1:4000/v1 " +
				"for the host agentgateway model route.",
		)
	}
	if c.ModelProvider == ProviderOpenAICompatible && strings.TrimSpace(c.OpenAIAPIKey.Reveal()) == "" {
		found.addCrossField(
			"AGENT_MODEL_PROVIDER=openai-compatible requires OPENAI_API_KEY. Ollama and the open " +
				"local gateway accept a non-secret marker such as local-ollama.",
		)
	}

	googleAPIKey := strings.TrimSpace(c.GoogleAPIKey.Reveal())
	if c.ModelProvider == ProviderGemini {
		switch {
		case googleAPIKey != "" && c.GoogleGenAIUseEnterprise:
			found.addCrossField(
				"AGENT_MODEL_PROVIDER=gemini cannot combine GOOGLE_API_KEY with " +
					"GOOGLE_GENAI_USE_ENTERPRISE=true in this course. Choose AI Studio API-key auth " +
					"or the ADC-backed enterprise path.",
			)
		case c.GoogleGenAIUseEnterprise:
			var missing []string
			for _, required := range []struct{ variable, value string }{
				{EnvGoogleCloudProject, c.GoogleCloudProject},
				{EnvGoogleCloudLocation, c.GoogleCloudLocation},
			} {
				if strings.TrimSpace(required.value) == "" {
					missing = append(missing, required.variable)
				}
			}
			if len(missing) > 0 {
				found.addCrossField(
					"AGENT_MODEL_PROVIDER=gemini with GOOGLE_GENAI_USE_ENTERPRISE=true requires %s "+
						"for the ADC-backed course path.", strings.Join(missing, " and "),
				)
			}
		case googleAPIKey == "":
			found.addCrossField(
				"AGENT_MODEL_PROVIDER=gemini requires either GOOGLE_API_KEY for AI Studio, or " +
					"GOOGLE_GENAI_USE_ENTERPRISE=true with GOOGLE_CLOUD_PROJECT and " +
					"GOOGLE_CLOUD_LOCATION for ADC.",
			)
		}
	}
	return found
}

// --8<-- [end:settings-provider-validation]

// crossFieldProblems rejects combinations that parse but cannot work.
func (c Config) crossFieldProblems() []Problem {
	// The session store comes first: a replica that cannot open the database its
	// conversations live in never reaches the question of which model endpoint
	// it would have called.
	found := problems(c.sessionStoreProblems())

	endpoints := []struct {
		variable string
		value    string
		example  string
		required bool
	}{
		{EnvOpenAIBaseURL, c.OpenAIBaseURL, "http://127.0.0.1:11434/v1", false},
		{EnvMCPURL, c.MCPURL, "http://127.0.0.1:3000/mcp", false},
		{EnvPIIModelBaseURL, c.PIIModelBaseURL, "http://127.0.0.1:11434/v1", true},
		{EnvEmbeddingsURL, c.EmbeddingsURL, "http://127.0.0.1:11434", true},
	}
	for _, endpoint := range endpoints {
		// OpenAI is unused under Gemini, and empty MCP selects the in-process read
		// tools. The PII-model and embeddings URLs retain their required defaults.
		if endpoint.value == "" && !endpoint.required {
			continue
		}
		if reason := httpURLProblem(endpoint.value); reason != "" {
			found.addCrossField(
				"%s %s, such as %s; the rejected value is omitted from diagnostics.",
				endpoint.variable, reason, endpoint.example,
			)
		}
	}
	if c.ModelFallback != nil && *c.ModelFallback == c.Model {
		found.addCrossField(
			"AGENT_MODEL_FALLBACK must differ from AGENT_MODEL (both are %q); "+
				"a fallback identical to the primary adds no resilience. "+
				"Unset it or pick a distinct model.", c.Model,
		)
	}
	return found
}

// sessionStoreProblems rejects session-store settings that parse but cannot open
// a store.
//
// Both directions are errors, not warnings. A postgres backend with no DSN has
// nowhere to connect, and a DSN set beside the sqlite backend is a setting the
// operator believes is in effect while every session still lands on local disk —
// the kind of quiet no-op that only surfaces when a second replica loses a
// conversation.
func (c Config) sessionStoreProblems() []Problem {
	var found problems
	dsn := strings.TrimSpace(c.SessionDSN.Reveal())
	switch {
	case c.SessionBackend == SessionBackendPostgres && dsn == "":
		found.addCrossField(
			"AGENT_SESSION_BACKEND=postgres requires AGENT_SESSION_DSN, in the form " +
				"postgres://user:password@host:5432/database?sslmode=disable. Keep the value in a " +
				"secret store; the shipped example line stays empty on purpose.",
		)
	case c.SessionBackend == SessionBackendSQLite && dsn != "":
		found.addCrossField(
			"AGENT_SESSION_DSN is only read when AGENT_SESSION_BACKEND=postgres. Select that " +
				"backend or unset the DSN; the sqlite store always lives in AGENT_STATE_DIR.",
		)
	case c.SessionBackend == SessionBackendPostgres:
		if reason := postgresDSNProblem(dsn); reason != "" {
			found.addCrossField(
				"AGENT_SESSION_DSN %s; the rejected value is omitted from diagnostics "+
					"because it carries a password.", reason,
			)
		}
	}
	return found
}

// postgresDSNProblem validates the session DSN without returning it.
//
// The same omission rule as httpURLProblem, for a stronger reason: a provider
// endpoint may carry a credential by accident, while this URL is expected to
// carry one. Everything below judges shape only.
func postgresDSNProblem(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "must be a postgres:// or postgresql:// URL"
	}
	if parsed.Hostname() == "" {
		return "must name a host"
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "must use a port between 1 and 65535 when a port is present"
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "must use a port between 1 and 65535 when a port is present"
		}
	}
	// A libpq URL without a path connects to a database named after the user,
	// which is never what a deployment means to say. Naming it is one character
	// of typing and removes a whole class of "why is this table empty".
	if strings.Trim(parsed.Path, "/") == "" {
		return "must name a database as the URL path, such as /sessions"
	}
	return ""
}

// httpURLProblem validates a provider endpoint without returning the rejected
// value: userinfo or a query can hold credentials that diagnostics must not
// echo. Parsing matters because a scheme prefix alone accepts malformed escapes
// and values such as http:///missing-host that no client can dial.
func httpURLProblem(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "must be an http(s) URL that is absolute and has a host"
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "must not contain credentials, a query, or a fragment"
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "must use a port between 1 and 65535 when a port is present"
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "must use a port between 1 and 65535 when a port is present"
		}
	}
	return ""
}
