package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// ModelProvider names the ADK client contract, not the deployment topology.
// It is a defined type rather than a string so a bare literal can never reach a
// call site: the compiler rejects `if cfg.ModelProvider == "gemini"` typos that
// the Python StrEnum only catches at runtime.
type ModelProvider string

const (
	// ProviderOpenAICompatible is the account-free default: ADK's OpenAI client
	// pointed at Ollama directly, or at agentgateway once Chapter 5 introduces
	// the governed data plane. Only OPENAI_BASE_URL changes between the two.
	ProviderOpenAICompatible ModelProvider = "openai-compatible"
	// ProviderGemini is the optional native path — AI Studio by API key, or
	// Vertex by Application Default Credentials.
	ProviderGemini ModelProvider = "gemini"
)

// ModelProviders lists every accepted provider in the order error messages
// present them. It is the single source of truth for both validation and the
// operator-facing message, so the two can never disagree.
func ModelProviders() []ModelProvider {
	return []ModelProvider{ProviderOpenAICompatible, ProviderGemini}
}

// Valid reports whether p is one of the supported providers.
func (p ModelProvider) Valid() bool { return slices.Contains(ModelProviders(), p) }

// UnmarshalText parses a provider at the environment boundary, so an unknown
// value fails during Load rather than selecting a silently wrong adapter later.
func (p *ModelProvider) UnmarshalText(text []byte) error {
	candidate := ModelProvider(text)
	if !candidate.Valid() {
		return fmt.Errorf("must be one of %s; got %q", joinValues(ModelProviders()), text)
	}
	*p = candidate
	return nil
}

// SessionBackend names the database engine behind ADK's session store.
//
// It is a defined type for the same reason ModelProvider is: the value selects
// which store a replica opens, and a typo that reached the switch would pick a
// silently wrong one. The two members are not interchangeable — they differ in
// how many processes may write at once, which is the whole subject of Chapter
// 6.9.
type SessionBackend string

const (
	// SessionBackendSQLite keeps sessions in AGENT_STATE_DIR beside the other
	// runtime databases. It is the account-free default and the single-process
	// contract: one file, one writer, nothing to install.
	SessionBackendSQLite SessionBackend = "sqlite"
	// SessionBackendPostgres moves sessions onto a shared server, so several
	// replicas can serve turns in the same conversation. It costs one more
	// component to run and back up.
	SessionBackendPostgres SessionBackend = "postgres"
)

// SessionBackends lists every accepted backend in the order error messages
// present them, so validation and the operator-facing message cannot disagree.
func SessionBackends() []SessionBackend {
	return []SessionBackend{SessionBackendSQLite, SessionBackendPostgres}
}

// Valid reports whether b is one of the supported session backends.
func (b SessionBackend) Valid() bool { return slices.Contains(SessionBackends(), b) }

// UnmarshalText parses a session backend at the environment boundary, so an
// unknown value fails during Load rather than at the first turn that needs a
// session.
func (b *SessionBackend) UnmarshalText(text []byte) error {
	candidate := SessionBackend(text)
	if !candidate.Valid() {
		return fmt.Errorf("must be one of %s; got %q", joinValues(SessionBackends()), text)
	}
	*b = candidate
	return nil
}

// Entrypoint selects which composition the server and CLI serve.
type Entrypoint string

const (
	// EntrypointAgent is the default conversational agent.
	EntrypointAgent Entrypoint = "agent"
	// EntrypointWorkflow is the bounded read-only triage graph.
	EntrypointWorkflow Entrypoint = "workflow"
	// EntrypointCoordinator is the least-privilege delegation composition.
	EntrypointCoordinator Entrypoint = "coordinator"
)

// Entrypoints lists every runnable composition in the order error messages
// present them.
func Entrypoints() []Entrypoint {
	return []Entrypoint{EntrypointAgent, EntrypointWorkflow, EntrypointCoordinator}
}

// Valid reports whether e is one of the runnable compositions.
func (e Entrypoint) Valid() bool { return slices.Contains(Entrypoints(), e) }

// UnmarshalText parses an entrypoint at the environment boundary.
func (e *Entrypoint) UnmarshalText(text []byte) error {
	candidate := Entrypoint(text)
	if !candidate.Valid() {
		return fmt.Errorf("must be one of %s; got %q", joinValues(Entrypoints()), text)
	}
	*e = candidate
	return nil
}

// SecretMask is what a Secret renders as everywhere except Reveal.
const SecretMask = "**********"

// Secret carries a credential. String, Format and MarshalText all render the
// mask, so a key cannot reach a log line, an error, a JSON encoder, or
// config:check by accident — reading the plaintext takes the deliberate,
// greppable Reveal call. This replaces pydantic's SecretStr.
type Secret string

// String masks the credential. An empty Secret stays empty so config:check can
// distinguish "not configured" from "configured and hidden".
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return SecretMask
}

// MarshalText masks the credential for every encoding/json and slog JSON
// handler path, which ignore fmt.Stringer. Nothing in the course round-trips a
// Secret, so losing the plaintext on the way out is the correct trade.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Reveal returns the plaintext credential. Every call site is a deliberate
// disclosure; there is no other way out of the type.
func (s Secret) Reveal() string { return string(s) }

// Seconds is a duration expressed the way the environment expresses it: a bare
// number of seconds, matching the `_S` variable suffix the Python track already
// documents. Keeping the wire format unitless lets one .env serve both tracks;
// Duration converts it once, at the point of use.
type Seconds float64

// Duration converts the configured seconds into a time.Duration.
func (s Seconds) Duration() time.Duration {
	return time.Duration(float64(s) * float64(time.Second))
}

// joinValues renders a set of string-backed constants for an error message.
func joinValues[T ~string](values []T) string {
	rendered := make([]string, len(values))
	for i, value := range values {
		rendered[i] = string(value)
	}
	return strings.Join(rendered, ", ")
}
