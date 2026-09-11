// Package config carries the typed, fail-fast settings every agent runtime
// parses once at startup.
//
// Cross-field dependencies are validated during Load — parse, don't validate
// scattered — so a bad combination fails at startup with a message that names
// the fix, instead of surfacing as a stack trace deep in a turn.
//
// Unlike the Python track, there is no package-level settings singleton. Load
// returns a value that callers thread into the packages that need it, which is
// what makes every downstream package testable without mutating the process
// environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/caarlos0/env/v11"
)

// Environment variable names, grouped the way .env.example presents them.
//
// Go struct tags must be literals, so these constants cannot be interpolated
// into the tags themselves. TestVariableConstantsMatchStructTags asserts the
// two sets are identical, which is what keeps them from drifting apart.
const (
	EnvEntrypoint               = "AGENT_ENTRYPOINT"
	EnvModelProvider            = "AGENT_MODEL_PROVIDER"
	EnvModel                    = "AGENT_MODEL"
	EnvOpenAIBaseURL            = "OPENAI_BASE_URL"
	EnvOpenAIAPIKey             = "OPENAI_API_KEY"
	EnvGoogleAPIKey             = "GOOGLE_API_KEY"
	EnvGoogleGenAIUseEnterprise = "GOOGLE_GENAI_USE_ENTERPRISE"
	EnvGoogleCloudProject       = "GOOGLE_CLOUD_PROJECT"
	EnvGoogleCloudLocation      = "GOOGLE_CLOUD_LOCATION"
	EnvGatewayEnabled           = "AGENT_GATEWAY_ENABLED"
	EnvMCPURL                   = "AGENT_MCP_URL"
	EnvMCPToken                 = "AGENT_MCP_TOKEN"
	EnvDataDir                  = "AGENT_DATA_DIR"
	EnvStateDir                 = "AGENT_STATE_DIR"
	EnvSessionBackend           = "AGENT_SESSION_BACKEND"
	EnvSessionDSN               = "AGENT_SESSION_DSN"
	EnvSessionMaxConns          = "AGENT_SESSION_MAX_CONNS"
	EnvA2ABindHost              = "AGENT_A2A_BIND_HOST"
	EnvA2AHost                  = "AGENT_A2A_HOST"
	EnvA2APort                  = "AGENT_A2A_PORT"
	EnvA2AProtocol              = "AGENT_A2A_PROTOCOL"
	EnvA2AMaxLLMCalls           = "AGENT_A2A_MAX_LLM_CALLS"
	EnvA2AStreaming             = "AGENT_A2A_STREAMING"
	EnvDrainTimeout             = "AGENT_DRAIN_TIMEOUT_S"
	EnvTrustedIdentityHeader    = "AGENT_TRUSTED_IDENTITY_HEADER"
	EnvModelTemperature         = "AGENT_MODEL_TEMPERATURE"
	EnvModelTimeout             = "AGENT_MODEL_TIMEOUT_S"
	EnvToolTimeout              = "AGENT_TOOL_TIMEOUT_S"
	EnvMaxRetries               = "AGENT_MAX_RETRIES"
	EnvRetryBackoff             = "AGENT_RETRY_BACKOFF_S"
	EnvCircuitBreakerEnabled    = "AGENT_CIRCUIT_BREAKER_ENABLED"
	EnvCircuitFailureThreshold  = "AGENT_CIRCUIT_FAILURE_THRESHOLD"
	EnvCircuitResetTimeout      = "AGENT_CIRCUIT_RESET_TIMEOUT_S"
	EnvModelFallback            = "AGENT_MODEL_FALLBACK"
	EnvWritesDisabled           = "AGENT_WRITES_DISABLED"
	EnvSanitizeToolOutput       = "AGENT_SANITIZE_TOOL_OUTPUT"
	EnvPIIModel                 = "AGENT_PII_MODEL"
	EnvPIIModelBaseURL          = "AGENT_PII_MODEL_BASE_URL"
	EnvPIIModelEnabled          = "AGENT_PII_MODEL_ENABLED"
	EnvPIIModelTimeout          = "AGENT_PII_MODEL_TIMEOUT_S"
	EnvSemanticRetrieval        = "AGENT_SEMANTIC_RETRIEVAL"
	EnvEmbeddingsURL            = "AGENT_EMBEDDINGS_URL"
	EnvEmbeddingModel           = "AGENT_EMBEDDING_MODEL"
	EnvEmbeddingTimeout         = "AGENT_EMBEDDING_TIMEOUT_S"
	EnvMaxHistoryMessages       = "AGENT_MAX_HISTORY_MESSAGES"
	EnvMaxTokensPerSession      = "AGENT_MAX_TOKENS_PER_SESSION"
	EnvInputPricePer1K          = "AGENT_INPUT_PRICE_PER_1K"
	EnvOutputPricePer1K         = "AGENT_OUTPUT_PRICE_PER_1K"
)

// WritesDisabledReason is the one wording of the kill-switch refusal, phrased as
// a reason so a caller can interpolate it: no leading capital, no trailing
// period. It says what still works, because freezing writes must not read as
// "the agent is broken", and it names the variable to clear, because the on-call
// engineer reading the transcript is the person who can clear it.
//
// Three packages refuse a write while this switch is set — the policy guard
// before ADK builds a confirmation, the two guarded write tools, and the notes
// writer — and each used to spell the sentence for itself. They drifted into two
// capitalizations and two endings, which left an operator unable to tell whether
// the three refusals meant the same switch.
//
// The text lives in config rather than in domain, which the work order suggested,
// for two reasons. Domain is the pivotable vocabulary and owns no runtime
// configuration, so hosting it there would force domain either to import config
// or to re-spell AGENT_WRITES_DISABLED — reintroducing the duplication this
// removes. And config already declares the variable the sentence names, imports
// nothing else in this module, and is therefore impossible to import into a
// cycle; policy already depended on it for exactly this sentence.
const WritesDisabledReason = "w" + writesDisabledReasonTail

// WritesDisabledRefusal is the same reason as a standalone sentence, for callers
// that return it on its own.
//
// It is a const rather than a value derived with strings.ToUpper, because a
// package-level var is writable at runtime and a refusal an attacker-adjacent
// code path could rewrite is not a refusal. TestWritesDisabledFormsAgree pins it
// to the reason it capitalizes, so the two cannot drift apart in a diff either.
const WritesDisabledRefusal = "W" + writesDisabledReasonTail + "."

// writesDisabledReasonTail is WritesDisabledReason without its leading letter, so
// both exported forms are spelled once and the test that compares them is
// comparing construction rather than two literals someone typed twice.
const writesDisabledReasonTail = "rites are frozen by the " + EnvWritesDisabled +
	" kill-switch; reads still work. Clear the flag once the incident is contained to resume approvals"

// Config is the resolved agent configuration.
//
// Fields are laid out in four regions — optional overrides, then names and
// endpoints, then limits, then toggles — because Go lays a struct out in
// declaration order and `go vet`'s fieldalignment check rejects a shape that
// interleaves pointers, words and bytes. The concern-based grouping a learner
// reads lives in .env.example; the grouping here is what the compiler wants.
// Each field keeps the rationale it carries in the Python track.
//
// Optional settings whose "unset" state differs from their zero value are
// pointers: AGENT_MODEL_TEMPERATURE=0 is greedy sampling while unset means
// "keep the provider default", and AGENT_MAX_HISTORY_MESSAGES=1 must be
// rejected while unset means "send the full history". Collapsing either onto a
// zero value would turn a rejected value into a silently accepted one.
type Config struct {
	// ---- Optional overrides, where unset differs from zero ----------------

	// Name of the request header carrying a gateway-verified caller identity
	// (Ch. 5.5 / 7.6). Unset keeps a synthetic A2A id for session and memory
	// scoping but grants no guarded-action authority. Only set this when a
	// trusted gateway validates the JWT and *sets* this header itself,
	// overwriting any client-supplied copy — a raw client could otherwise forge
	// it. A verified value becomes the audit approved_by.
	TrustedIdentityHeader *string `env:"AGENT_TRUSTED_IDENTITY_HEADER"`

	// Unset preserves each provider's sampling default; evaluations opt into
	// lower-variance greedy sampling with 0 (Ch. 4.4).
	ModelTemperature *float64 `env:"AGENT_MODEL_TEMPERATURE"`

	// Optional secondary model tried when the primary fails every retry
	// (Ch. 5.4). Unset keeps the single-model path. Uses the same provider and
	// endpoint as the primary — a smaller local model is the intended
	// account-free fallback.
	ModelFallback *string `env:"AGENT_MODEL_FALLBACK"`

	// Bounded conversation history / context compaction (Ch. 3.4). Unset sends
	// ADK's full session history to the model every turn; a positive value keeps
	// only that many most-recent messages and replaces the older ones with one
	// compact synthetic note. Deterministic and model-free, so the offline test
	// gate stays exact. A single user turn can expand into several messages (a
	// model tool call, a tool response, a model answer), so set it with headroom.
	MaxHistoryMessages *int `env:"AGENT_MAX_HISTORY_MESSAGES"`

	// Hard per-session token budget (Ch. 7.3). Unset disables enforcement.
	MaxTokensPerSession *int `env:"AGENT_MAX_TOKENS_PER_SESSION"`

	// Removed in favor of OPENAI_BASE_URL. A pointer, not a string, because
	// *any* value — including an empty one — must produce the migration error;
	// only a genuinely absent variable is silence.
	DeprecatedGatewayEnabled *string `env:"AGENT_GATEWAY_ENABLED"`

	// ---- Names, endpoints and paths --------------------------------------

	// --8<-- [start:settings-provider-fields]
	Entrypoint    Entrypoint    `env:"AGENT_ENTRYPOINT"     envDefault:"agent"`
	ModelProvider ModelProvider `env:"AGENT_MODEL_PROVIDER" envDefault:"openai-compatible"`
	Model         string        `env:"AGENT_MODEL"          envDefault:"qwen3:4b-instruct"`

	// "openai-compatible" describes the ADK client contract, not the deployment
	// topology. Point this URL directly at Ollama for the account-free first
	// run, or at agentgateway when the governed data plane is introduced.
	OpenAIBaseURL string `env:"OPENAI_BASE_URL" envDefault:"http://127.0.0.1:11434/v1"`
	OpenAIAPIKey  Secret `env:"OPENAI_API_KEY"  envDefault:"local-ollama"`

	// Optional Gemini paths: an AI Studio API key, or Enterprise/Vertex via ADC
	// with an explicit project and location.
	GoogleAPIKey        Secret `env:"GOOGLE_API_KEY"`
	GoogleCloudProject  string `env:"GOOGLE_CLOUD_PROJECT"`
	GoogleCloudLocation string `env:"GOOGLE_CLOUD_LOCATION"`
	// --8<-- [end:settings-provider-fields]

	// Governed MCP route (Ch. 5.5). Unset calls the six read tools directly, in
	// process; the token is only needed for a JWT/API-key-secured route.
	MCPURL   string `env:"AGENT_MCP_URL"`
	MCPToken Secret `env:"AGENT_MCP_TOKEN"`

	// The committed dataset is immutable input; runtime SQLite state is
	// disposable. Both defaults are relative to the working directory, which
	// every mise task in agents/go pins to the module root, so `../data` is the
	// shared agents/data seed and `.state` never dirties git.
	DataDir  string `env:"AGENT_DATA_DIR"  envDefault:"../data"`
	StateDir string `env:"AGENT_STATE_DIR" envDefault:".state"`

	// Which engine holds ADK's sessions (Ch. 6.9). The default keeps them in
	// AGENT_STATE_DIR as one SQLite file with one writer, which is why the
	// account-free path needs nothing installed. "postgres" moves them to a
	// shared server so more than one replica can serve the same conversation.
	SessionBackend SessionBackend `env:"AGENT_SESSION_BACKEND" envDefault:"sqlite"`

	// Connection URL for the postgres backend, unset for sqlite. A Secret rather
	// than a string because this URL normally carries a password: it is masked in
	// config:check, in logs, and in every error this module raises.
	SessionDSN Secret `env:"AGENT_SESSION_DSN"`

	// Bind and advertised A2A addresses are deliberately separate. The host
	// runtime stays loopback-only; Kubernetes explicitly opts into 0.0.0.0.
	// Never advertise 0.0.0.0: it is a listener, not a callable endpoint.
	A2ABindHost string `env:"AGENT_A2A_BIND_HOST" envDefault:"127.0.0.1"`
	A2AHost     string `env:"AGENT_A2A_HOST"      envDefault:"localhost"`
	A2AProtocol string `env:"AGENT_A2A_PROTOCOL"  envDefault:"http"`

	// Layer-2 NER calls Ollama directly rather than recursively entering the
	// gateway whose promptGuard invoked it.
	PIIModel        string `env:"AGENT_PII_MODEL"          envDefault:"qwen3:4b-instruct"`
	PIIModelBaseURL string `env:"AGENT_PII_MODEL_BASE_URL" envDefault:"http://127.0.0.1:11434/v1"`

	// Endpoint and model for the opt-in semantic runbook retrieval below.
	EmbeddingsURL  string `env:"AGENT_EMBEDDINGS_URL"  envDefault:"http://127.0.0.1:11434"`
	EmbeddingModel string `env:"AGENT_EMBEDDING_MODEL" envDefault:"nomic-embed-text"`

	// ---- Limits, deadlines and budgets -----------------------------------

	A2APort        int `env:"AGENT_A2A_PORT"          envDefault:"8080"`
	A2AMaxLLMCalls int `env:"AGENT_A2A_MAX_LLM_CALLS" envDefault:"12"`

	// Connections one replica may hold open against the postgres session store.
	// The ceiling that matters is shared, not local: replicas multiplied by this
	// value must stay under the server's own max_connections, or a routine scale-up
	// turns into "too many clients already" for every replica at once. Ignored by
	// the sqlite backend, which holds exactly one connection by construction.
	SessionMaxConns int `env:"AGENT_SESSION_MAX_CONNS" envDefault:"10"`

	// Graceful-shutdown drain: how long in-flight A2A requests may finish after
	// SIGTERM. Kubernetes' terminationGracePeriodSeconds must exceed this.
	DrainTimeout Seconds `env:"AGENT_DRAIN_TIMEOUT_S" envDefault:"10"`

	// Bounded retries with exponential backoff for idempotent reads and model
	// calls; guarded write actions are never retried (Ch. 4.5).
	ModelTimeout    Seconds `env:"AGENT_MODEL_TIMEOUT_S" envDefault:"60"`
	PIIModelTimeout Seconds `env:"AGENT_PII_MODEL_TIMEOUT_S" envDefault:"8"`
	ToolTimeout     Seconds `env:"AGENT_TOOL_TIMEOUT_S"  envDefault:"30"`
	MaxRetries      int     `env:"AGENT_MAX_RETRIES"     envDefault:"2"`
	RetryBackoff    Seconds `env:"AGENT_RETRY_BACKOFF_S" envDefault:"0.5"`

	// Consecutive failures that open the circuit, and how long it stays open
	// before one trial call tests recovery. Only meaningful when
	// CircuitBreakerEnabled is on.
	CircuitFailureThreshold int     `env:"AGENT_CIRCUIT_FAILURE_THRESHOLD" envDefault:"5"`
	CircuitResetTimeout     Seconds `env:"AGENT_CIRCUIT_RESET_TIMEOUT_S"   envDefault:"30"`

	// A cold local embedding model can take substantially longer to load than an
	// ordinary tool call. Keep that startup bounded without forcing every read
	// tool to inherit the same generous deadline.
	EmbeddingTimeout Seconds `env:"AGENT_EMBEDDING_TIMEOUT_S" envDefault:"120"`

	// Cost attribution (Ch. 7.3). Prices are per 1000 tokens and default to 0
	// because the reference path is local Ollama, which bills nothing.
	InputPricePer1K  float64 `env:"AGENT_INPUT_PRICE_PER_1K"  envDefault:"0"`
	OutputPricePer1K float64 `env:"AGENT_OUTPUT_PRICE_PER_1K" envDefault:"0"`

	// ---- Toggles ----------------------------------------------------------

	// Enterprise/Vertex authentication by Application Default Credentials
	// instead of an AI Studio key. Requires a project and a location.
	GoogleGenAIUseEnterprise bool `env:"GOOGLE_GENAI_USE_ENTERPRISE" envDefault:"false"`

	// Opt-in per-token model streaming for A2A requests (Ch. 3.6). Off by
	// default: chunked output weakens PII redaction (entities spanning chunk
	// boundaries are already sent) and the gateway path reports no usage on
	// streamed responses.
	A2AStreaming bool `env:"AGENT_A2A_STREAMING" envDefault:"false"`

	// Circuit breaker for idempotent read tools (Ch. 4.5). Off by default so the
	// shipped behavior stays "retry only". When on, a read tool that fails
	// CircuitFailureThreshold times in a row opens: further calls fail fast
	// (shedding load off a dead dependency) until CircuitResetTimeout elapses
	// and one trial call tests recovery. Never wraps writes.
	CircuitBreakerEnabled bool `env:"AGENT_CIRCUIT_BREAKER_ENABLED" envDefault:"false"`

	// Emergency kill-switch (Ch. 4.5 / 7.7). When true at process startup, every
	// guarded write action refuses before approval. Roll out the setting and
	// restart the process to freeze mutations while reads keep working.
	WritesDisabled bool `env:"AGENT_WRITES_DISABLED" envDefault:"false"`

	// Default-on prompt-injection hardening for tool/retrieval content (Ch. 4.6):
	// spotlight untrusted text and neutralize known injection markers.
	SanitizeToolOutput bool `env:"AGENT_SANITIZE_TOOL_OUTPUT" envDefault:"true"`

	// The three OpenAI/Ollama gateway profiles enable the NER webhook. The GKE
	// Vertex profile explicitly disables it because no local Ollama is present.
	PIIModelEnabled bool `env:"AGENT_PII_MODEL_ENABLED" envDefault:"true"`

	// Opt-in semantic runbook retrieval (Ch. 3.4). Off by default so the test
	// gate stays deterministic and model-free; requires a local Ollama with the
	// embedding model pulled. Falls back to the keyword scorer on failure.
	SemanticRetrieval bool `env:"AGENT_SEMANTIC_RETRIEVAL" envDefault:"false"`
}

// Load parses and validates the process environment.
//
// It is the only entrypoint a runtime needs; every other package receives the
// returned value rather than reaching for the environment itself.
func Load() (Config, error) {
	return LoadFrom(env.ToMap(os.Environ()))
}

// LoadFrom parses and validates an explicit environment.
//
// It exists so tests and tools can exercise a configuration without mutating
// the process environment, which is what lets the validation matrix be
// table-driven instead of a sequence of global mutations.
func LoadFrom(environ map[string]string) (Config, error) {
	cfg, parseErr := env.ParseAsWithOptions[Config](env.Options{Environment: environ})

	// Field-shaped problems are reported together, the way pydantic collects
	// every field error before it runs the model validator: a learner with three
	// bad values sees three lines.
	found := parseProblems(parseErr)
	found = append(found, restoreExplicitEmptyValues(&cfg, environ)...)
	found = append(found, cfg.fieldProblems()...)
	if found = firstPerVariable(found); len(found) > 0 {
		return Config{}, &ValidationError{Problems: found}
	}

	// Provider problems short-circuit the rest, exactly as the Python validator
	// raises its provider list before it looks at anything else: with the wrong
	// credentials there is nothing to say about MCP or fallback models yet.
	if found := cfg.providerProblems(); len(found) > 0 {
		return Config{}, &ValidationError{Problems: found}
	}
	if found := cfg.crossFieldProblems(); len(found) > 0 {
		return Config{}, &ValidationError{Problems: found}
	}
	return cfg, nil
}

// fieldSpec is one Config field as the environment sees it.
type fieldSpec struct {
	goName   string
	variable string
	index    int
	// kind is the field's kind after dereferencing a pointer, so a *string and a
	// string are handled the same way.
	kind    reflect.Kind
	pointer bool
}

// specs describes Config once, in declaration order.
//
// Deriving the variable list from the struct instead of repeating it means a
// new field is automatically covered by the empty-value repair below, by
// config:check's output, and by the .env.example gate — none of which can drift.
var specs = sync.OnceValue(func() []fieldSpec {
	configType := reflect.TypeFor[Config]()
	found := make([]fieldSpec, 0, configType.NumField())
	for i := range configType.NumField() {
		field := configType.Field(i)
		variable, ok := field.Tag.Lookup("env")
		if !ok {
			continue
		}
		kind, pointer := field.Type.Kind(), false
		if kind == reflect.Pointer {
			kind, pointer = field.Type.Elem().Kind(), true
		}
		found = append(found, fieldSpec{
			goName:   field.Name,
			variable: variable,
			index:    i,
			kind:     kind,
			pointer:  pointer,
		})
	}
	return found
})

// restoreExplicitEmptyValues repairs the one place caarlos0/env cannot express
// what this configuration means.
//
// The library treats an explicitly empty variable as absent: getOr substitutes
// the envDefault when the value is empty, and setField skips a field whose
// resolved value is empty. So `OPENAI_API_KEY=` in a .env would silently resolve
// to "local-ollama", and `AGENT_GATEWAY_ENABLED=` would resolve to "not set" —
// both of which the Python track rejects. Write the operator's literal empty
// string back onto string-shaped fields so validation judges what was actually
// set, and report the non-string ones, where an empty value is a parse failure.
func restoreExplicitEmptyValues(cfg *Config, environ map[string]string) []Problem {
	value := reflect.ValueOf(cfg).Elem()
	var found []Problem
	for _, spec := range specs() {
		if raw, ok := environ[spec.variable]; !ok || raw != "" {
			continue
		}
		if spec.kind != reflect.String {
			found = append(found, Problem{
				Variable: spec.variable,
				Message:  "must not be empty; unset the variable to use its default",
			})
			continue
		}
		field := value.Field(spec.index)
		if spec.pointer {
			// A pointer to the empty string, not nil: the variable *was* set, and
			// the difference is the whole point of the pointer.
			field.Set(reflect.New(field.Type().Elem()))
			continue
		}
		field.SetString("")
	}
	return found
}

// parseProblems turns caarlos0/env's aggregate into problems named by the
// environment variable the operator set, not by the Go field they landed in.
func parseProblems(err error) []Problem {
	if err == nil {
		return nil
	}
	byGoName := make(map[string]string, len(specs()))
	for _, spec := range specs() {
		byGoName[spec.goName] = spec.variable
	}

	var aggregate env.AggregateError
	if !errors.As(err, &aggregate) {
		return []Problem{{Message: fmt.Sprintf("could not read the environment: %v", err)}}
	}
	found := make([]Problem, 0, len(aggregate.Errors))
	for _, cause := range aggregate.Errors {
		var parseErr env.ParseError
		if !errors.As(cause, &parseErr) {
			found = append(found, Problem{Message: cause.Error()})
			continue
		}
		message := parseErr.Err.Error()
		// Strip strconv's function-name preamble ("strconv.ParseInt: "): the
		// environment variable is already named on the Problem, and the remainder
		// is the part that tells the operator what was wrong with their value.
		if _, rest, ok := strings.Cut(message, ": "); ok && strings.HasPrefix(message, "strconv.") {
			message = rest
		}
		found = append(found, Problem{Variable: byGoName[parseErr.Name], Message: message})
	}
	return found
}

// firstPerVariable keeps one problem per variable, preserving order.
//
// A variable that failed to parse also fails its bound check on the resulting
// zero value; reporting both would tell the operator to fix one mistake twice.
func firstPerVariable(found []Problem) []Problem {
	seen := make(map[string]bool, len(found))
	kept := found[:0]
	for _, problem := range found {
		if problem.Variable != "" && seen[problem.Variable] {
			continue
		}
		seen[problem.Variable] = true
		kept = append(kept, problem)
	}
	return kept
}
