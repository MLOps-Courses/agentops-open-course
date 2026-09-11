package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// The note store is also the agent's persistent ADK memory.Service, and ADK's
// own load_memory and preload_memory tools read through it.

func TestTheNoteStoreIsThePersistentADKMemoryService(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// ADK ships an in-memory service and a Vertex AI one. A course that promises
	// state surviving a restart needs a third, and it is this one.
	service := fixture.memory.Service()
	if service == nil {
		t.Fatal("Service() = nil, want the note store")
	}
	// The compiler is the assertion: Service() is declared to return the ADK
	// interface, so a store that stopped satisfying it would not build.
	if _, ok := service.(*Notes); !ok {
		t.Errorf("Service() returned %T, want the note store itself", service)
	}
}

func TestSearchMemoryReturnsSavedNotesForTheCaller(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := engineerContext(t)
	saveNote(t, fixture, ctx, inventoryIncident, "restarted the pool, still crash-looping")

	response := searchMemory(t, fixture, testApp, testUser, "what did we try")
	if len(response.Memories) != 1 {
		t.Fatalf("memories = %d, want 1", len(response.Memories))
	}
	entry := response.Memories[0]
	contains(t, entryText(t, entry), "crash-looping", "the recalled entry")
	if entry.Author != testUser {
		t.Errorf("author = %q, want %q", entry.Author, testUser)
	}
	if entry.Timestamp.IsZero() {
		t.Error("the entry carries no timestamp, so preload_memory would omit its Time: line")
	}
	// The provenance stamp lets a consumer tell a note an engineer deliberately
	// wrote from a line of transcript that was ingested.
	if source, _ := entry.CustomMetadata["source"].(string); source != incidentNoteSource {
		t.Errorf("source = %v, want %q", entry.CustomMetadata["source"], incidentNoteSource)
	}
	if incident, _ := entry.CustomMetadata["incident_id"].(string); incident != inventoryIncident {
		t.Errorf("incident_id = %v, want %q", entry.CustomMetadata["incident_id"], inventoryIncident)
	}
}

func TestSearchMemoryScopesPerApplicationAndUser(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	saveNote(t, fixture, toolContext(t, testApp, "alice", "s1"), inventoryIncident, "alice private note")

	// The three fields of memory.SearchRequest map straight onto the store's own
	// scoping, which is the whole reason the interface fits.
	for _, scoped := range []struct {
		name    string
		appName string
		userID  string
		want    int
	}{
		{"the same caller", testApp, "alice", 1},
		{"another user", testApp, "bob", 0},
		{"another application", "other-app", "alice", 0},
	} {
		t.Run(scoped.name, func(t *testing.T) {
			t.Parallel()

			response := searchMemory(t, fixture, scoped.appName, scoped.userID, "anything")
			if len(response.Memories) != scoped.want {
				t.Errorf("memories = %d, want %d", len(response.Memories), scoped.want)
			}
		})
	}
}

func TestSearchMemorySelectsTheIncidentNamedInTheQuery(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := engineerContext(t)
	saveNote(t, fixture, ctx, inventoryIncident, "note about the inventory outage")
	saveNote(t, fixture, ctx, checkoutIncident, "note about the checkout latency")

	// This is what maps ADK's single Query field onto recall_incident_context's
	// optional incident filter: a query that names an incident recalls that one.
	filtered := searchMemory(t, fixture, testApp, testUser, "what did we try on "+inventoryIncident+"?")
	if len(filtered.Memories) != 1 {
		t.Fatalf("memories = %d, want only the named incident", len(filtered.Memories))
	}
	contains(t, entryText(t, filtered.Memories[0]), "inventory outage", "the recalled entry")

	// And a query that names none recalls across all of them, exactly as the
	// tool's empty argument does.
	all := searchMemory(t, fixture, testApp, testUser, "where did we get to")
	if len(all.Memories) != 2 {
		t.Errorf("memories = %d, want both incidents", len(all.Memories))
	}
}

func TestAddSessionToMemoryIngestsTextAndIsIdempotent(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	service := fixture.memory.Service()
	transcript := fakeSession{
		id: testSessionID, appName: testApp, userID: testUser,
		events: eventList{
			userEvent("e1", testUser, "the checkout service is timing out"),
			userEvent("e2", "assistant", "I restarted the connection pool"),
			// No content at all: nothing to remember, and nothing to redact.
			{ID: "e3", Author: testUser},
		},
	}
	for range 2 {
		if err := service.AddSessionToMemory(t.Context(), transcript); err != nil {
			t.Fatalf("AddSessionToMemory() error = %v, want nil", err)
		}
	}

	// Two entries, not four: the session's rows are replaced rather than
	// appended to, so re-adding a session is idempotent — matching ADK's own
	// in-memory service, which a host may swap in. And three events produced two
	// entries, because the one with no content had nothing to remember.
	response := searchMemory(t, fixture, testApp, testUser, "the")
	if len(response.Memories) != 2 {
		t.Fatalf("memories = %d, want 2 after two identical ingests of two text events",
			len(response.Memories))
	}
	found := false
	for _, entry := range response.Memories {
		if strings.Contains(entryText(t, entry), "restarted the connection pool") {
			found = true
			if source, _ := entry.CustomMetadata["source"].(string); source != sessionEventSource {
				t.Errorf("source = %v, want %q", entry.CustomMetadata["source"], sessionEventSource)
			}
		}
	}
	if !found {
		t.Error("the ingested transcript does not contain the assistant's line")
	}
}

func TestAddSessionToMemoryMatchesOnWordIntersection(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	transcript := fakeSession{
		id: testSessionID, appName: testApp, userID: testUser,
		events: eventList{userEvent("e1", testUser, "the checkout service is timing out")},
	}
	if err := fixture.memory.Service().AddSessionToMemory(t.Context(), transcript); err != nil {
		t.Fatalf("AddSessionToMemory() error = %v, want nil", err)
	}

	// The tokenizer and the intersection test are ADK's, on purpose: an agent
	// that swaps this service for the framework's in-memory one must not see its
	// recall behavior change as a side effect.
	if hits := searchMemory(t, fixture, testApp, testUser, "timing"); len(hits.Memories) != 1 {
		t.Errorf("memories = %d for an overlapping query, want 1", len(hits.Memories))
	}
	if misses := searchMemory(t, fixture, testApp, testUser, "unrelated"); len(misses.Memories) != 0 {
		t.Errorf("memories = %d for a disjoint query, want 0", len(misses.Memories))
	}
}

func TestIngestedSessionsAreRedactedBeforeBeingPersisted(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.redact = func(text string) string { return strings.ReplaceAll(text, "s3cr3t", "<SECRET>") }
	})
	transcript := fakeSession{
		id: testSessionID, appName: testApp, userID: testUser,
		events: eventList{userEvent("e1", testUser, "the token is s3cr3t and the pool is exhausted")},
	}
	if err := fixture.memory.Service().AddSessionToMemory(t.Context(), transcript); err != nil {
		t.Fatalf("AddSessionToMemory() error = %v, want nil", err)
	}

	// Ingested transcript is durable state like a note, so it crosses the same
	// persistence boundary. Redacting on the way out would leave the original.
	response := searchMemory(t, fixture, testApp, testUser, "pool is exhausted")
	if len(response.Memories) != 1 {
		t.Fatalf("memories = %d, want 1", len(response.Memories))
	}
	text := entryText(t, response.Memories[0])
	if strings.Contains(text, "s3cr3t") {
		t.Errorf("the ingested entry %q still contains the token", text)
	}
	contains(t, text, "<SECRET>", "the ingested entry")
}

func TestErasureAlsoRemovesIngestedSessions(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	saveNote(t, fixture, toolContext(t, testApp, "alice", "s1"), inventoryIncident, "alice wrote this down")
	transcript := fakeSession{
		id: "s1", appName: testApp, userID: "alice",
		events: eventList{userEvent("e1", "alice", "alice said this in passing")},
	}
	if err := fixture.memory.Service().AddSessionToMemory(t.Context(), transcript); err != nil {
		t.Fatalf("AddSessionToMemory() error = %v, want nil", err)
	}

	if _, err := fixture.memory.Notes().ForgetUserMemory(t.Context(), "alice"); err != nil {
		t.Fatalf("ForgetUserMemory() error = %v, want nil", err)
	}
	// A subject-access request covers everything the person contributed, not only
	// the half they typed into a tool.
	response := searchMemory(t, fixture, testApp, "alice", "alice said this in passing")
	if len(response.Memories) != 0 {
		t.Errorf("memories = %d after erasure, want 0", len(response.Memories))
	}
}

func TestSearchMemoryRefusesAnAbsentRequest(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	if _, err := fixture.memory.Service().SearchMemory(t.Context(), nil); err == nil {
		t.Error("SearchMemory(nil) error = nil, want a refusal")
	}
	if err := fixture.memory.Service().AddSessionToMemory(t.Context(), nil); err == nil {
		t.Error("AddSessionToMemory(nil) error = nil, want a refusal")
	}
}

func TestADKMemoryToolsAreRegisteredInTheFrameworkOrder(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	tools := fixture.memory.ADKMemoryTools()
	want := []string{"preload_memory", "load_memory"}

	if len(tools) != len(want) {
		t.Fatalf("ADKMemoryTools() returned %d tools, want %d", len(tools), len(want))
	}
	for index, name := range want {
		if tools[index].Name() != name {
			t.Errorf("ADKMemoryTools()[%d] = %q, want %q", index, tools[index].Name(), name)
		}
	}
	// Built once, so a composition can compare them by identity the way the
	// policy plane compares the load_skill tool.
	if fixture.memory.ADKMemoryTools()[0] != tools[0] {
		t.Error("ADKMemoryTools() builds a new tool on every call, losing identity comparison")
	}
}

func TestLoadMemoryReadsThroughTheNoteStore(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	saveNote(t, fixture, engineerContext(t), inventoryIncident, "restarted the pool, still crash-looping")

	ctx := memoryContext(t, fixture, testApp, testUser, nil)
	loadMemory := fixture.memory.ADKMemoryTools()[1]
	result, err := run(t, loadMemory, ctx, map[string]any{"query": "what did we try"})
	if err != nil {
		t.Fatalf("load_memory.Run() error = %v, want nil", err)
	}
	// The agent-facing surface is standard ADK: the framework's own tool, reading
	// the agent's persistent store rather than a bespoke one.
	entries, ok := result["memories"].([]adkmemory.Entry)
	if !ok {
		t.Fatalf("load_memory returned %T for \"memories\", want []memory.Entry", result["memories"])
	}
	if len(entries) != 1 {
		t.Fatalf("load_memory returned %d entries, want 1", len(entries))
	}
	contains(t, entryText(t, entries[0]), "crash-looping", "the loaded entry")
}

func TestPreloadMemoryInjectsPastConversations(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	saveNote(t, fixture, engineerContext(t), inventoryIncident, "restarted the pool, still crash-looping")

	userContent := genai.NewContentFromText("what did we try", genai.RoleUser)
	ctx := memoryContext(t, fixture, testApp, testUser, userContent)
	preload, ok := fixture.memory.ADKMemoryTools()[0].(requestProcessor)
	if !ok {
		t.Fatal("preload_memory does not process requests, so it can never inject anything")
	}

	request := &model.LLMRequest{}
	if err := preload.ProcessRequest(ctx, request); err != nil {
		t.Fatalf("preload_memory.ProcessRequest() error = %v, want nil", err)
	}
	if request.Config == nil || request.Config.SystemInstruction == nil {
		t.Fatal("preload_memory injected nothing into the system instruction")
	}
	injected := request.Config.SystemInstruction.Parts[0].Text
	contains(t, injected, "PAST_CONVERSATIONS", "the injected instruction")
	contains(t, injected, "crash-looping", "the injected instruction")

	// This is the trade the package documentation calls out: preload_memory
	// stuffs context without a tool call, so nothing about the recall appears in
	// the trace. A composition should bind it or the explicit tools, not both.
	if strings.Contains(injected, RecallIncidentContextToolName) {
		t.Error("the injected block mentions the explicit recall tool, which would double the surface")
	}
}

// requestProcessor is the structural interface a toolset or tool implements to
// mutate the outgoing LLM request. The declaring interface is internal to ADK.
type requestProcessor interface {
	ProcessRequest(ctx agent.Context, request *model.LLMRequest) error
}

// searchMemory runs one ADK memory search against the note store.
func searchMemory(t *testing.T, fixture *fixture, appName, userID, query string) *adkmemory.SearchResponse {
	t.Helper()

	response, err := fixture.memory.Service().SearchMemory(t.Context(), &adkmemory.SearchRequest{
		Query:   query,
		UserID:  userID,
		AppName: appName,
	})
	if err != nil {
		t.Fatalf("SearchMemory() error = %v, want nil", err)
	}
	if response == nil {
		t.Fatal("SearchMemory() = nil, want a response")
	}
	return response
}

// entryText joins the text of one memory entry.
func entryText(t *testing.T, entry adkmemory.Entry) string {
	t.Helper()

	if entry.Content == nil {
		t.Fatalf("entry %q carries no content", entry.ID)
	}
	parts := make([]string, 0, len(entry.Content.Parts))
	for _, part := range entry.Content.Parts {
		parts = append(parts, part.Text)
	}
	return strings.Join(parts, "\n")
}

// userEvent builds one session event carrying a single line of text.
func userEvent(id, author, text string) *session.Event {
	event := &session.Event{ID: id, Author: author, Timestamp: time.Now().UTC()}
	event.Content = genai.NewContentFromText(text, genai.RoleUser)
	return event
}

// memoryContext builds a tool context whose memory service is the note store,
// which is what ADK's launcher wires through launcher.Config.MemoryService.
func memoryContext(t *testing.T, fixture *fixture, appName, userID string, userContent *genai.Content) agent.Context {
	t.Helper()

	invocation := &fakeInvocation{
		Context:     t.Context(),
		session:     fakeSession{id: testSessionID, appName: appName, userID: userID},
		userContent: userContent,
		memory: scopedMemory{
			service: fixture.memory.Service(),
			appName: appName,
			userID:  userID,
		},
	}
	return agent.NewToolContext(invocation, "function-call-1", &session.EventActions{}, nil)
}

// scopedMemory is what ADK's runner puts behind agent.Context.SearchMemory: the
// configured memory service, bound to the invocation's application and user.
type scopedMemory struct {
	service adkmemory.Service
	appName string
	userID  string
}

func (m scopedMemory) AddSessionToMemory(ctx context.Context, current session.Session) error {
	return m.service.AddSessionToMemory(ctx, current)
}

func (m scopedMemory) SearchMemory(ctx context.Context, query string) (*adkmemory.SearchResponse, error) {
	return m.service.SearchMemory(ctx, &adkmemory.SearchRequest{
		Query:   query,
		UserID:  m.userID,
		AppName: m.appName,
	})
}
