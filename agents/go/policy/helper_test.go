package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// repositoryDataset is the committed dataset, relative to this package. The
// policy suite only ever reads it: the seed database is byte-reproducible and
// gated by a rebuild comparison, so a test that wrote to it would break the
// repository rather than only itself.
const repositoryDataset = "../../data"

// Identifiers come from the vocabulary rather than from literals, so a pivoted
// domain carries the suite with it — and so the ratchet in
// domain/portability_test.go stays green.
var (
	checkoutService   = domain.Reference().Services.Checkout
	inventoryService  = domain.Reference().Services.Inventory
	databaseService   = domain.Reference().Services.Database
	inventoryIncident = domain.Reference().Incidents.InventoryDown
	authIncident      = domain.Reference().Incidents.AuthErrors
	checkoutIncident  = domain.Reference().Incidents.CheckoutLatency
	cascadeIncident   = domain.Reference().Incidents.CheckoutCascade
	serviceDownBook   = domain.Reference().Runbooks.ServiceDown
)

// newPolicy builds a policy for a test, with a logger that writes into the
// test's own output rather than the process's.
func newPolicy(t *testing.T, cfg Config) *Policy {
	t.Helper()

	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(testWriter{t}, nil))
	}
	built, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return built
}

// testWriter forwards log output to the test log, so a failing case shows what
// the policy said while it ran.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(payload []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(payload), "\n"))
	return len(payload), nil
}

// recordingHandler captures log records so a test can assert that a failure was
// reported, not only that it was handled.
type recordingHandler struct {
	records []slog.Record
	mutex   sync.Mutex
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// rendered returns every captured record as "message key=value …", which is
// enough to assert that a detail did or did not reach the log.
func (h *recordingHandler) rendered() string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	var out strings.Builder
	for _, record := range h.records {
		out.WriteString(record.Message)
		record.Attrs(func(attr slog.Attr) bool {
			out.WriteString(" " + attr.Key + "=" + attr.Value.String())
			return true
		})
		out.WriteString("\n")
	}
	return out.String()
}

// testContext is the callback context the policy actually sees.
//
// It embeds ADK's strict mock, so every method the policy does not use panics
// instead of silently returning a zero value. That is the point: it proves the
// port only reads the accessors that stay live inside a callback wrapper —
// ctx.Session() is one of the many that do not.
type testContext struct {
	agent.StrictContextMock
	state     session.State
	appName   string
	userID    string
	sessionID string
}

func (c *testContext) State() session.State { return c.state }
func (c *testContext) AppName() string      { return c.appName }
func (c *testContext) UserID() string       { return c.userID }
func (c *testContext) SessionID() string    { return c.sessionID }

// newContext returns a callback context over a fresh, empty session state.
func newContext() *testContext { return newContextWithState(newState(nil)) }

// newContextWithState returns a callback context sharing an existing state, so
// two contexts can name the same logical session.
func newContextWithState(state session.State) *testContext {
	return &testContext{
		StrictContextMock: agent.NewStrictContextMock(context.Background()),
		state:             state,
		appName:           AppName,
		userID:            "test-user",
		sessionID:         "test-session",
	}
}

// mapState is a map-backed session.State. It is mutex-guarded because the
// budget's concurrency test drives two goroutines through it, and a data race
// there would fail the run for the wrong reason.
type mapState struct {
	values map[string]any
	mutex  sync.Mutex
}

func newState(values map[string]any) *mapState {
	if values == nil {
		values = map[string]any{}
	}
	return &mapState{values: values}
}

func (s *mapState) Get(key string) (any, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	value, ok := s.values[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return value, nil
}

func (s *mapState) Set(key string, value any) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.values[key] = value
	return nil
}

func (s *mapState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		s.mutex.Lock()
		defer s.mutex.Unlock()
		for key, value := range s.values {
			if !yield(key, value) {
				return
			}
		}
	}
}

// has reports whether a key was ever written.
func (s *mapState) has(key string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_, ok := s.values[key]
	return ok
}

// namedTool is a tool that has a name and nothing else — the shape a remote MCP
// server can produce for any name it likes.
type namedTool struct{ name string }

func (t *namedTool) Name() string        { return t.name }
func (t *namedTool) Description() string { return "" }
func (t *namedTool) IsLongRunning() bool { return false }

// loadSkillTool returns the genuine, locally constructed load_skill tool.
//
// It is built from this repository's own skills directory through ADK's
// skilltoolset, which is the whole point: the trust carve-out is granted to
// that value's identity, so it cannot be forged by a tool that merely calls
// itself load_skill.
func loadSkillTool(t *testing.T) tool.Tool {
	t.Helper()

	source := skill.NewFileSystemSource(os.DirFS(filepath.Join(repositoryDataset, "skills")))
	toolset, err := skilltoolset.New(t.Context(), skilltoolset.Config{Source: source})
	if err != nil {
		t.Fatalf("skilltoolset.New() error = %v, want nil", err)
	}
	// Tools ignores the context and returns the slice cached at construction,
	// which is what makes identity comparison stable across calls.
	built, err := toolset.Tools(nil)
	if err != nil {
		t.Fatalf("Tools() error = %v, want nil", err)
	}
	for _, candidate := range built {
		if candidate.Name() == "load_skill" {
			return candidate
		}
	}
	t.Fatal("the skill toolset did not build a load_skill tool")
	return nil
}

// renderValue flattens a structured value into searchable text.
//
// JSON is used rather than fmt so a map's key order never decides whether a
// test passes. HTML escaping is off because every mask this package writes is
// angle-bracketed, and an escaped "<SECRET>" would make a substring
// assertion fail for a reason that has nothing to do with redaction.
func renderValue(value any) string {
	var encoded strings.Builder
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimRight(encoded.String(), "\n")
}

// textPart builds a one-part message.
func textPart(role, text string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: text}}}
}

// callPart builds a message carrying a tool call.
func callPart(name string, args map[string]any) *genai.Content {
	return &genai.Content{
		Role:  "model",
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}},
	}
}

// resultPart builds a message carrying a tool result.
func resultPart(name string, response map[string]any) *genai.Content {
	return &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: name, Response: response}}},
	}
}

// renderContents flattens a request's contents into one searchable string.
func renderContents(contents []*genai.Content) string {
	var out strings.Builder
	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			out.WriteString(part.Text)
			if part.FunctionCall != nil {
				out.WriteString(renderValue(part.FunctionCall.Args))
			}
			if part.FunctionResponse != nil {
				out.WriteString(renderValue(part.FunctionResponse.Response))
			}
			out.WriteString("\n")
		}
	}
	return out.String()
}
