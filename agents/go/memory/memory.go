// Package memory is the AgentOps Agent's knowledge and recall surface: the
// runbook library it reads, and the durable notes it keeps about an incident
// across conversations.
//
// # Three stores, and the separation is the lesson
//
//   - The runbook library under AGENT_DATA_DIR is committed, immutable
//     knowledge. get_runbook fetches one by its exact slug; search_runbooks
//     finds one by symptom.
//   - The note store (memory.db, in AGENT_STATE_DIR) is disposable runtime
//     state, scoped per application and per user. recall_incident_context reads
//     it, save_incident_note writes it, and [Notes.ForgetUserMemory] erases one
//     user's half of it.
//   - The vector index (vectors.db, also in AGENT_STATE_DIR) is a derived cache
//     of the runbook library. It is bound to the provenance of the corpus that
//     produced it and is never queried while stale — see [Retriever].
//
// # Recall is a tool call, never silent context stuffing
//
// Both directions of long-term memory are explicit tools the model calls, so
// every read and every write is visible in the trace, auditable, and testable.
// The alternative — prepending remembered text to the prompt — is invisible
// exactly when an incident review needs to see it. ADK's own preload_memory
// tool does inject silently; [Memory.ADKMemoryTools] exposes it because the
// framework surface is worth teaching, but a composition should pick one style
// rather than run both.
//
// # Writes are redacted, reads are guarded
//
// A note passes the PII redactor before it is written, not after it is read:
// the store is a persistence boundary like the audit trail, and a credential
// that lands in it is already leaked. The three idempotent reads run through a
// [Guard] — a deadline, bounded retries, a circuit breaker; save_incident_note
// deliberately does not, because a retried write can cross an unknown commit
// boundary.
//
// # Configuration
//
// The package does not import the configuration loader: [Config] is a value the
// composition builds from a config.Config, exactly as the tools and policy
// packages are wired.
package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/preloadmemorytool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// The names the model calls, and the names everything else keys on: the MCP
// allowlist, the evalsets, the root instruction and the course prose. The
// compose package repeats them because it has to name them before this package
// exists in its import graph; they are the same four strings, and renaming one
// means renaming it in both places.
const (
	// GetRunbookToolName fetches one runbook by its exact slug.
	GetRunbookToolName = "get_runbook"
	// SearchRunbooksToolName finds runbook guidance by symptom.
	SearchRunbooksToolName = "search_runbooks"
	// RecallIncidentContextToolName reads the caller's durable incident notes.
	RecallIncidentContextToolName = "recall_incident_context"
	// SaveIncidentNoteToolName writes one durable incident note.
	SaveIncidentNoteToolName = "save_incident_note"
)

// getIncidentToolName is the read tool whose result carries a runbook slug, and
// the only tool outside this package that get_runbook's description names.
//
// It is spelled here rather than imported from the tools package: importing
// tools for one string would make the knowledge surface depend on the incident
// surface, and the two are deliberately independent — a specialist can hold one
// without the other (Chapter 3.7).
const getIncidentToolName = "get_incident"

// The tool descriptions the model reads. They carry the prose of the Python
// Google-style docstrings, minus the per-argument text, which lives on the
// schema properties instead — see [describeProperties].
const (
	getRunbookDescription = "Fetch a runbook by its exact slug (e.g. an incident's `runbook` field).\n\n" +
		"Returns a dict with the `slug` and its markdown `content`, or an `error` if unknown."

	searchRunbooksDescription = "Search the runbook knowledge base for guidance relevant to a free-text query.\n\n" +
		"Uses TF-IDF-style scoring by default: a term counts more when it is rare across runbooks, " +
		"and a term matching a runbook's slug gets a strong boost. Deterministic and offline. " +
		"With AGENT_SEMANTIC_RETRIEVAL=true it ranks by local-embedding similarity instead, " +
		"falling back to this keyword scorer when the embedding model is unavailable.\n\n" +
		"Returns a dict with `count`, a `retrieval` mode (`semantic` or `keyword`), and a " +
		"`runbooks` list (slug + markdown content), best match first. The mode makes a " +
		"semantic-to-keyword fallback visible in the result, not only in the log."

	recallIncidentContextDescription = "Recall your saved notes from previous conversations, newest first.\n\n" +
		"Call this at the start of an investigation to pick up where the last conversation left off.\n\n" +
		"Returns a dict with `count` and `notes` (ts, incident_id, note) for the current user."

	saveIncidentNoteDescription = "Save a durable note about an incident for future conversations.\n\n" +
		"Use this when you learn something worth remembering across sessions: a fix that was " +
		"attempted, its outcome, or context the next on-call engineer needs.\n\n" +
		"Returns the saved note (with PII redacted), or an `error` for invalid input."
)

// ErrIncompleteConfig marks a [Config] that cannot produce working tools.
//
// It is a sentinel so a caller can tell "this composition is wired wrong" —
// which is a startup bug — apart from a runtime failure.
var ErrIncompleteConfig = errors.New("incomplete memory configuration")

// Store is the dataset access this package needs.
//
// It is declared here rather than reused from the data package because the
// consumer is the one that knows which operations it depends on: a reader sees
// the whole surface memory can reach, and a test can substitute a store that
// fails on demand. *data.Store satisfies it.
type Store interface {
	// ListRunbookSlugs returns every runbook slug, sorted.
	ListRunbookSlugs() ([]string, error)
	// ReadRunbook returns a runbook's markdown, and whether it exists. The slug
	// is model-controlled, so the implementation normalizes it before it builds
	// a path: a traversal payload is reported as "not found", never read.
	ReadRunbook(slug string) (string, bool, error)
	// RunbooksDir names the knowledge base, for the message an empty corpus gets.
	RunbooksDir() string
	// GetIncident returns one incident, or nil when no row carries that id. A
	// note may only be attached to an incident that exists.
	GetIncident(ctx context.Context, id domain.IncidentID) (*domain.Incident, error)
}

// Guard runs one idempotent read tool call.
//
// It is the seam the resilience package fills: the deadline, the bounded
// retries and the circuit breaker all live behind it, exactly as Python's
// with_resilience decorator wrapped each read function. call must therefore be
// safe to run more than once — which is precisely why save_incident_note never
// goes through a Guard.
//
// The tool name is passed through because the circuit breaker is per tool: one
// failing dependency must not shed load from the others.
type Guard func(ctx context.Context, toolName string, call func(context.Context) error) error

// Redactor removes PII and credentials from text that is about to be persisted.
//
// It is the seam the policy package fills. Every note travels through it before
// it reaches the note store, because redacting on the way out would leave the
// original on disk.
type Redactor func(text string) string

// Config is everything this package needs from the rest of the runtime.
//
// Field order follows what the compiler wants (govet's fieldalignment check),
// not what a reader would group.
type Config struct {
	// Store reads the runbook library and validates an incident id. Required.
	Store Store

	// Guard wraps the three idempotent reads. Required rather than optional
	// because a nil one would be a silent downgrade of a Chapter 4.5 guarantee.
	Guard Guard

	// Redact cleans a note before it is persisted. Required for the same reason:
	// an unredacted note is a credential in durable state.
	Redact Redactor

	// HTTPClient reaches the embeddings endpoint. Nil builds one bounded by
	// EmbeddingTimeout, which is what every runtime path does; a test supplies
	// its own so nothing leaves the process.
	HTTPClient *http.Client

	// Logger records the semantic-to-keyword fallback. Nil uses slog.Default().
	// The fallback is the one event here worth a log line: the result already
	// says which mode answered, and this says why the other one could not.
	Logger *slog.Logger

	// StateDir holds memory.db and vectors.db. Required: both are disposable
	// runtime state and must never be written into the committed dataset.
	StateDir string

	// EmbeddingsURL and EmbeddingModel address the local Ollama (or a
	// compatible stand-in) that produces vectors. Only read when
	// SemanticRetrieval is on.
	EmbeddingsURL  string
	EmbeddingModel string

	// EmbeddingTimeout bounds one embedding request. A cold local model can take
	// substantially longer to load than an ordinary tool call, which is why it
	// is separate from the tool deadline.
	EmbeddingTimeout time.Duration

	// SemanticRetrieval switches search_runbooks from the deterministic keyword
	// scorer to embedding similarity (AGENT_SEMANTIC_RETRIEVAL). Off keeps the
	// offline path, and is the default the test gate runs on.
	SemanticRetrieval bool

	// WritesDisabled is the AGENT_WRITES_DISABLED kill-switch, read once at
	// startup exactly as the configuration documents it. When set,
	// save_incident_note refuses before it touches the filesystem.
	WritesDisabled bool
}

// Memory is the agent's knowledge and recall surface.
//
// The four tools are built once, during [New], because schema inference can
// fail and a tool that cannot describe itself must not reach a model. The zero
// value is not usable.
type Memory struct {
	store     Store
	guard     Guard
	logger    *slog.Logger
	notes     *Notes
	retriever *Retriever

	getRunbook            tool.Tool
	searchRunbooks        tool.Tool
	recallIncidentContext tool.Tool
	saveIncidentNote      tool.Tool
	preloadMemory         tool.Tool
	loadMemory            tool.Tool

	semanticRetrieval bool
}

// New builds the four tools, the note store and the retriever over cfg.
//
// Validation is fail-fast and accumulating: a wiring with three holes reports
// three lines, because this is assembled once at startup and every hole is a
// capability the agent's own instruction claims it has.
func New(cfg Config) (*Memory, error) {
	var problems []error
	if cfg.Store == nil {
		problems = append(problems, fmt.Errorf("%w: Store is required", ErrIncompleteConfig))
	}
	if cfg.Guard == nil {
		problems = append(problems, fmt.Errorf(
			"%w: Guard is required; an unguarded read has no deadline, no retries and no circuit breaker",
			ErrIncompleteConfig,
		))
	}
	if cfg.Redact == nil {
		problems = append(problems, fmt.Errorf(
			"%w: Redact is required; an unredacted note would be persisted verbatim",
			ErrIncompleteConfig,
		))
	}
	if cfg.StateDir == "" {
		problems = append(problems, fmt.Errorf(
			"%w: StateDir is required; long-term memory is disposable state and never lives in the dataset",
			ErrIncompleteConfig,
		))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	built := &Memory{
		store:             cfg.Store,
		guard:             cfg.Guard,
		logger:            logger,
		semanticRetrieval: cfg.SemanticRetrieval,
		notes: newNotes(notesConfig{
			store:          cfg.Store,
			redact:         cfg.Redact,
			stateDir:       cfg.StateDir,
			writesDisabled: cfg.WritesDisabled,
		}),
		retriever: newRetriever(retrieverConfig{
			store:      cfg.Store,
			httpClient: cfg.HTTPClient,
			stateDir:   cfg.StateDir,
			url:        cfg.EmbeddingsURL,
			model:      cfg.EmbeddingModel,
			timeout:    cfg.EmbeddingTimeout,
		}),
		// Neither ADK tool takes a configuration, and both are built once so a
		// composition can compare them by identity the way the policy plane
		// compares the load_skill tool.
		preloadMemory: preloadmemorytool.New(),
		loadMemory:    loadmemorytool.New(),
	}

	// Every example identifier the model is shown comes from the vocabulary, not
	// from a literal here. A learner who pivots the course onto another domain
	// edits domain/vocabulary.go and these descriptions follow; the ratchet in
	// domain/portability_test.go is what keeps it that way.
	vocabulary := domain.Reference()

	// A table rather than four `if err != nil` blocks: the constructions differ
	// only in their arguments, and collecting them here gives them one error path
	// and puts the registration order in one readable place.
	build := []struct {
		target      *tool.Tool
		constructor func() (tool.Tool, error)
	}{
		{&built.getRunbook, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        GetRunbookToolName,
				Description: getRunbookDescription,
			}, map[string]string{
				"slug": "The exact runbook identifier returned by " + getIncidentToolName + ", e.g. " +
					vocabulary.Runbooks.HighLatency + " or " + vocabulary.Runbooks.ServiceDown +
					". Never derive it from the service name.",
			}, built.runGetRunbook)
		}},
		{&built.searchRunbooks, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        SearchRunbooksToolName,
				Description: searchRunbooksDescription,
			}, map[string]string{
				"query": "What you are trying to resolve, e.g. `database connection pool exhausted`.",
				"limit": fmt.Sprintf("The requested result count. Values above %d are capped at %d.",
					maxRunbookResults, maxRunbookResults),
			}, built.runSearchRunbooks)
		}},
		{&built.recallIncidentContext, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        RecallIncidentContextToolName,
				Description: recallIncidentContextDescription,
			}, map[string]string{
				"incident_id": "Optional incident to filter by, e.g. " + vocabulary.Incidents.InventoryDown +
					". Leave empty to recall recent notes across all incidents.",
			}, built.runRecallIncidentContext)
		}},
		{&built.saveIncidentNote, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        SaveIncidentNoteToolName,
				Description: saveIncidentNoteDescription,
			}, map[string]string{
				"incident_id": "The incident the note belongs to, e.g. " + vocabulary.Incidents.InventoryDown + ".",
				"note":        "One short, factual sentence (attempted action, outcome, decision).",
			}, built.runSaveIncidentNote)
		}},
	}
	for _, entry := range build {
		constructed, err := entry.constructor()
		if err != nil {
			return nil, err
		}
		*entry.target = constructed
	}
	return built, nil
}

// The two runbook reads, individually. Naming each one lets a specialist agent
// expose only what its contract needs.
func (m *Memory) GetRunbook() tool.Tool     { return m.getRunbook }
func (m *Memory) SearchRunbooks() tool.Tool { return m.searchRunbooks }

// The two long-term memory tools, individually.
func (m *Memory) RecallIncidentContext() tool.Tool { return m.recallIncidentContext }
func (m *Memory) SaveIncidentNote() tool.Tool      { return m.saveIncidentNote }

// KnowledgeTools returns the two runbook reads in registration order. It is the
// Go counterpart of Python's KNOWLEDGE_TOOLS.
//
// The slice is fresh on every call: a caller that appends to it, or reorders
// it, must not be able to change what the next caller sees.
func (m *Memory) KnowledgeTools() []tool.Tool {
	return []tool.Tool{m.getRunbook, m.searchRunbooks}
}

// MemoryTools returns the two long-term memory tools in registration order. It
// is the Go counterpart of Python's MEMORY_TOOLS.
//
// Erasure is deliberately absent: [Notes.ForgetUserMemory] is an operator
// action invoked out of band, so the model can never erase memory on its own.
func (m *Memory) MemoryTools() []tool.Tool {
	return []tool.Tool{m.recallIncidentContext, m.saveIncidentNote}
}

// ADKMemoryTools returns ADK's own memory surface — preload_memory then
// load_memory — in the order the framework's example registers them.
//
// They read through [Memory.Service], so an agent that binds them sees the same
// per-user, per-application store the bespoke tools use. preload_memory injects
// what it finds into the system instruction without a tool call, which is the
// opposite of this package's explicit-recall stance; bind it only when that
// trade is the one you want.
func (m *Memory) ADKMemoryTools() []tool.Tool {
	return []tool.Tool{m.preloadMemory, m.loadMemory}
}

// Service returns the persistent ADK memory service backing load_memory and
// preload_memory. Wire it as launcher.Config.MemoryService.
func (m *Memory) Service() memory.Service { return m.notes }

// Notes returns the long-term note store, for the operator actions that are
// deliberately not agent tools.
func (m *Memory) Notes() *Notes { return m.notes }

// Retriever returns the provenance-bound semantic index, for the operator
// commands that build or inspect a generation.
func (m *Memory) Retriever() *Retriever { return m.retriever }

// newTool builds one function tool with an explicit, fully documented schema.
//
// The schema is inferred from the argument and result types and then edited,
// rather than left to inference alone, for two reasons: ADK reads descriptions
// only from struct tags, and a description that must name a domain identifier
// cannot be a tag because a tag has to be a literal; and the inferrer models an
// optional field as the union ["null", T], which some providers reject outright.
//
// The tools package carries the same helper. Sharing it would mean exporting a
// construction detail from one leaf package to another, and neither package
// owns the other's schemas.
func newTool[TArgs, TResults any](
	cfg functiontool.Config, descriptions map[string]string, handler functiontool.Func[TArgs, TResults],
) (tool.Tool, error) {
	input, err := jsonschema.For[TArgs](nil)
	if err != nil {
		return nil, fmt.Errorf("infer the %s input schema: %w", cfg.Name, err)
	}
	output, err := jsonschema.For[TResults](nil)
	if err != nil {
		return nil, fmt.Errorf("infer the %s output schema: %w", cfg.Name, err)
	}
	if err = describeProperties(input, descriptions); err != nil {
		return nil, fmt.Errorf("document the %s arguments: %w", cfg.Name, err)
	}
	collapseNullable(input)
	collapseNullable(output)
	cfg.InputSchema, cfg.OutputSchema = input, output

	constructed, err := functiontool.New(cfg, handler)
	if err != nil {
		return nil, fmt.Errorf("build the %s tool: %w", cfg.Name, err)
	}
	return constructed, nil
}

// describeProperties writes the model-facing description of every argument, and
// refuses a schema where one is missing.
//
// That refusal is the point: an undocumented argument is a silent capability
// loss — the model still sees the field and has no idea what to put in it.
func describeProperties(schema *jsonschema.Schema, descriptions map[string]string) error {
	for name, description := range descriptions {
		property, ok := schema.Properties[name]
		if !ok {
			return fmt.Errorf("%w: there is no argument named %q to document", ErrIncompleteConfig, name)
		}
		property.Description = description
	}
	// Sorted rather than ranged: Go randomizes map iteration, and an error that
	// names a different argument on every run is not reproducible.
	for _, name := range slices.Sorted(maps.Keys(schema.Properties)) {
		if schema.Properties[name].Description == "" {
			return fmt.Errorf("%w: argument %q carries no description for the model", ErrIncompleteConfig, name)
		}
	}
	return nil
}

// collapseNullable rewrites {"type": ["null", T]} back to {"type": T} for every
// optional property, recursively.
//
// The inferrer produces that union for every Go pointer and slice, because both
// can be nil, and some model providers reject a union type outright. An
// optional property never travels as null — an argument the model omits is
// absent, and a result field that does not apply is dropped by `omitzero` — so
// for those the union describes a value that cannot occur.
func collapseNullable(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = true
	}
	for name, property := range schema.Properties {
		if !required[name] && len(property.Types) == 2 && slices.Contains(property.Types, "null") {
			for _, kind := range property.Types {
				if kind != "null" {
					// Type and Types are mutually exclusive in this schema type, so
					// the slice has to be cleared as the scalar is set.
					property.Type, property.Types = kind, nil
				}
			}
		}
		collapseNullable(property)
	}
	collapseNullable(schema.Items)
	// Map-typed fields land in AdditionalProperties rather than Properties, so the
	// recursion is incomplete without this line. No struct here declares one today;
	// this completes the walk rather than fixing an observed break.
	collapseNullable(schema.AdditionalProperties)
}
