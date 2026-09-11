package a2aserver_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
)

// The G-4 contract. a2a-go ships no reusable conformance suite — its own store
// tests are internal to the package and typed to the in-memory implementation —
// so every behavior a caller may rely on is re-asserted here against the
// SQLite store, and the ones the in-memory tests pin (immutability, sentinels,
// version bumps) are mirrored deliberately.

// testOwner is the caller every fixture authenticates as.
const testOwner = "alice@example.test"

// newTaskStore opens an isolated store owned by testOwner.
func newTaskStore(t *testing.T, configure ...func(*a2aserver.TaskStoreConfig)) *a2aserver.TaskStore {
	t.Helper()

	return openTaskStoreAt(t, filepath.Join(t.TempDir(), a2aserver.TaskDatabaseName), configure...)
}

// openTaskStoreAt opens a store at an explicit path, so a test can close and
// reopen the same file.
func openTaskStoreAt(t *testing.T, path string, configure ...func(*a2aserver.TaskStoreConfig)) *a2aserver.TaskStore {
	t.Helper()

	cfg := a2aserver.TaskStoreConfig{
		Path:          path,
		Authenticator: func(context.Context) (string, error) { return testOwner, nil },
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	store, err := a2aserver.OpenTaskStore(t.Context(), cfg)
	if err != nil {
		t.Fatalf("OpenTaskStore() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return store
}

// newTask builds a minimal valid task.
func newTask(id, contextID string, state a2a.TaskState) *a2a.Task {
	return &a2a.Task{
		ID:        a2a.TaskID(id),
		ContextID: contextID,
		Status:    a2a.TaskStatus{State: state},
		History:   []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))},
	}
}

// mustCreate creates a task and returns its version.
func mustCreate(t *testing.T, store *a2aserver.TaskStore, task *a2a.Task) taskstore.TaskVersion {
	t.Helper()

	version, err := store.Create(t.Context(), task)
	if err != nil {
		t.Fatalf("Create(%q) error = %v, want nil", task.ID, err)
	}
	return version
}

// mustGet reads a task back.
func mustGet(t *testing.T, store *a2aserver.TaskStore, id a2a.TaskID) *taskstore.StoredTask {
	t.Helper()

	stored, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%q) error = %v, want nil", id, err)
	}
	return stored
}

func TestTaskStoreCreateStartsAtVersionOne(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	if got := mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted)); got != 1 {
		t.Errorf("Create() version = %d, want 1", got)
	}
	stored := mustGet(t, store, "t1")
	if stored.Version != 1 {
		t.Errorf("Get() version = %d, want 1", stored.Version)
	}
	if stored.Task.ContextID != "c1" || stored.Task.Status.State != a2a.TaskStateSubmitted {
		t.Errorf("Get() task = %+v, want the created one", stored.Task)
	}
	if len(stored.Task.History) != 1 {
		t.Errorf("Get() history length = %d, want 1", len(stored.Task.History))
	}
}

func TestTaskStoreCreateRejectsADuplicate(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted))

	version, err := store.Create(t.Context(), newTask("t1", "c2", a2a.TaskStateWorking))
	errIsNot(t, err, taskstore.ErrTaskAlreadyExists, "Create() on a duplicate")
	if version != taskstore.TaskVersionMissing {
		t.Errorf("Create() version = %d, want %d", version, taskstore.TaskVersionMissing)
	}
	// The loser must not have overwritten the winner.
	if got := mustGet(t, store, "t1").Task.ContextID; got != "c1" {
		t.Errorf("ContextID = %q, want the first writer's", got)
	}
}

func TestTaskStoreIsolatesTheCallersTask(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	task := newTask("t1", "c1", a2a.TaskStateSubmitted)
	mustCreate(t, store, task)

	// The in-memory store deep-copies for exactly this reason. A SQLite store
	// gets it from serialization, and the property still has to be asserted:
	// a caller that keeps a reference must not be able to rewrite history.
	task.ContextID = "mutated"
	task.Status.State = a2a.TaskStateFailed
	task.History = append(task.History, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("injected")))

	stored := mustGet(t, store, "t1")
	if stored.Task.ContextID != "c1" || stored.Task.Status.State != a2a.TaskStateSubmitted {
		t.Errorf("stored task = %+v, want the state at Create", stored.Task)
	}
	if len(stored.Task.History) != 1 {
		t.Errorf("stored history length = %d, want 1", len(stored.Task.History))
	}

	// And the same in the other direction: mutating a result must not reach the
	// next reader.
	stored.Task.ContextID = "mutated-again"
	if got := mustGet(t, store, "t1").Task.ContextID; got != "c1" {
		t.Errorf("ContextID after mutating a result = %q, want %q", got, "c1")
	}
}

func TestTaskStoreGetReportsAMissingTask(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	stored, err := store.Get(t.Context(), "absent")
	errIsNot(t, err, a2a.ErrTaskNotFound, "Get() on a missing task")
	if stored != nil {
		t.Errorf("Get() = %+v, want nil", stored)
	}
}

func TestTaskStoreUpdateBumpsTheVersion(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	first := mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted))

	updated := newTask("t1", "c1", a2a.TaskStateWorking)
	second, err := store.Update(t.Context(), &taskstore.UpdateRequest{Task: updated, PrevVersion: first})
	if err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if second != first+1 {
		t.Errorf("Update() version = %d, want %d", second, first+1)
	}
	stored := mustGet(t, store, "t1")
	if stored.Version != second || stored.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("Get() = %+v at version %d, want the updated state at %d", stored.Task, stored.Version, second)
	}
}

func TestTaskStoreUpdateRejectsAStaleVersion(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	first := mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted))
	if _, err := store.Update(t.Context(), &taskstore.UpdateRequest{
		Task: newTask("t1", "c1", a2a.TaskStateWorking), PrevVersion: first,
	}); err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}

	version, err := store.Update(t.Context(), &taskstore.UpdateRequest{
		Task: newTask("t1", "c1", a2a.TaskStateCompleted), PrevVersion: first,
	})
	errIsNot(t, err, taskstore.ErrConcurrentModification, "Update() with a stale version")
	if version != taskstore.TaskVersionMissing {
		t.Errorf("Update() version = %d, want %d", version, taskstore.TaskVersionMissing)
	}
	if got := mustGet(t, store, "t1").Task.Status.State; got != a2a.TaskStateWorking {
		t.Errorf("state = %q, want the winning writer's", got)
	}
}

func TestTaskStoreUpdateWithoutVersionTrackingSkipsTheCheck(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted))

	// TaskVersionMissing means "not tracked", which the interface documents as
	// bypassing the check entirely rather than as a mismatch against zero.
	version, err := store.Update(t.Context(), &taskstore.UpdateRequest{
		Task: newTask("t1", "c1", a2a.TaskStateCompleted), PrevVersion: taskstore.TaskVersionMissing,
	})
	if err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if version != 2 {
		t.Errorf("Update() version = %d, want 2", version)
	}
}

func TestTaskStoreUpdateReportsAMissingTask(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	_, err := store.Update(t.Context(), &taskstore.UpdateRequest{Task: newTask("absent", "c1", a2a.TaskStateWorking)})
	errIsNot(t, err, a2a.ErrTaskNotFound, "Update() on a missing task")
}

func TestTaskStoreGetAndUpdateMaskAnotherOwnersTask(t *testing.T) {
	t.Parallel()

	// A task belongs to whoever created it. Matching the upstream store and A2A
	// spec, a different caller sees "not found" rather than an ownership oracle.
	var caller string
	store := newTaskStore(t, func(cfg *a2aserver.TaskStoreConfig) {
		cfg.Authenticator = func(context.Context) (string, error) { return caller, nil }
	})
	caller = testOwner
	mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted))

	caller = "mallory@example.test"
	stored, err := store.Get(t.Context(), "t1")
	errIsNot(t, err, a2a.ErrTaskNotFound, "Get() by another owner")
	if stored != nil {
		t.Errorf("Get() by another owner = %+v, want nil", stored)
	}
	version, err := store.Update(t.Context(), &taskstore.UpdateRequest{
		Task: newTask("t1", "c1", a2a.TaskStateWorking), PrevVersion: 1,
	})
	errIsNot(t, err, a2a.ErrTaskNotFound, "Update() by another owner")
	if version != taskstore.TaskVersionMissing {
		t.Errorf("Update() by another owner version = %d, want missing", version)
	}
	listed, err := store.List(t.Context(), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(listed.Tasks) != 0 {
		t.Errorf("the second caller listed %d tasks, want 0", len(listed.Tasks))
	}

	caller = testOwner
	listed, err = store.List(t.Context(), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(listed.Tasks) != 1 {
		t.Fatalf("the owner listed %d tasks, want 1", len(listed.Tasks))
	}
	stored = mustGet(t, store, "t1")
	if stored.Task.Status.State != a2a.TaskStateSubmitted || stored.User != testOwner {
		t.Errorf("owner Get() = %+v (user %q), want unchanged task owned by %q", stored.Task, stored.User, testOwner)
	}
}

func TestTaskStoreRejectsUnrepresentableMetadata(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	task := newTask("t1", "c1", a2a.TaskStateSubmitted)
	// uint is deliberately outside the A2A metadata accept-list: the spec has
	// no unsigned number, so a value that round-tripped would come back as
	// something else.
	task.Metadata = map[string]any{"count": uint(1)}

	if _, err := store.Create(t.Context(), task); err == nil {
		t.Fatal("Create() error = nil, want a metadata refusal")
	} else if !strings.Contains(err.Error(), "not permitted in Metadata") {
		t.Errorf("Create() error = %v, want the metadata contract message", err)
	}
	if _, err := store.Get(t.Context(), "t1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Error("a refused Create left a row behind")
	}
}

func TestTaskStoreRejectsCircularMetadata(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	cycle := map[string]any{}
	cycle["self"] = cycle
	task := newTask("t1", "c1", a2a.TaskStateSubmitted)
	task.Metadata = cycle

	// Without the check this is a stack overflow inside encoding/json, which
	// takes the process down rather than the request.
	if _, err := store.Create(t.Context(), task); err == nil {
		t.Fatal("Create() error = nil, want a cycle refusal")
	} else if !strings.Contains(err.Error(), "circular reference") {
		t.Errorf("Create() error = %v, want the cycle message", err)
	}
}

func TestTaskStoreAcceptsRichMetadata(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	task := newTask("t1", "c1", a2a.TaskStateSubmitted)
	task.Metadata = map[string]any{
		"nested": map[string]any{"list": []any{"a", 1.5, true, nil}},
	}
	task.Artifacts = []*a2a.Artifact{{
		ID:    a2a.NewArtifactID(),
		Name:  "answer",
		Parts: a2a.ContentParts{a2a.NewTextPart("the answer")},
	}}
	mustCreate(t, store, task)

	stored := mustGet(t, store, "t1")
	nested, ok := stored.Task.Metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want the nested map to survive", stored.Task.Metadata)
	}
	if list, ok := nested["list"].([]any); !ok || len(list) != 4 {
		t.Errorf("nested list = %#v, want four elements", nested["list"])
	}
	if len(stored.Task.Artifacts) != 1 || stored.Task.Artifacts[0].Name != "answer" {
		t.Errorf("artifacts = %+v, want the created one", stored.Task.Artifacts)
	}
}

// TestTaskStoreSurvivesARestart is the deployment contract's actual
// requirement: a rolling update must not lose in-flight tasks.
func TestTaskStoreSurvivesARestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), a2aserver.TaskDatabaseName)
	store := openTaskStoreAt(t, path)
	mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted))
	if _, err := store.Update(t.Context(), &taskstore.UpdateRequest{
		Task: newTask("t1", "c1", a2a.TaskStateInputRequired), PrevVersion: 1,
	}); err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	// A new process, the same file.
	reopened := openTaskStoreAt(t, path)
	stored := mustGet(t, reopened, "t1")
	if stored.Version != 2 {
		t.Errorf("version after restart = %d, want 2", stored.Version)
	}
	if stored.Task.Status.State != a2a.TaskStateInputRequired {
		t.Errorf("state after restart = %q, want %q", stored.Task.Status.State, a2a.TaskStateInputRequired)
	}
	// The version counter must continue rather than restart, or a client
	// holding version 2 would be able to overwrite a newer generation.
	next, err := reopened.Update(t.Context(), &taskstore.UpdateRequest{
		Task: newTask("t1", "c1", a2a.TaskStateCompleted), PrevVersion: 2,
	})
	if err != nil {
		t.Fatalf("Update() after restart error = %v, want nil", err)
	}
	if next != 3 {
		t.Errorf("version after restart = %d, want 3", next)
	}
	listed, err := reopened.List(t.Context(), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("List() after restart error = %v, want nil", err)
	}
	if listed.TotalSize != 1 {
		t.Errorf("TotalSize after restart = %d, want 1", listed.TotalSize)
	}
}

// TestTaskStoreSerializesConcurrentUpdates is the optimistic concurrency
// guarantee under a real race: two writers read the same version, and exactly
// one of them may win.
func TestTaskStoreSerializesConcurrentUpdates(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	mustCreate(t, store, newTask("t1", "c1", a2a.TaskStateSubmitted))

	const writers = 8
	var (
		wait      sync.WaitGroup
		start     = make(chan struct{})
		guard     sync.Mutex
		succeeded int
		conflicts int
		other     []error
	)
	for index := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			state := a2a.TaskStateWorking
			if index%2 == 0 {
				state = a2a.TaskStateCompleted
			}
			_, err := store.Update(t.Context(), &taskstore.UpdateRequest{
				Task: newTask("t1", "c1", state), PrevVersion: 1,
			})
			guard.Lock()
			defer guard.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, taskstore.ErrConcurrentModification):
				conflicts++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wait.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected failures: %v", other)
	}
	if succeeded != 1 {
		t.Errorf("%d writers succeeded, want exactly 1", succeeded)
	}
	if conflicts != writers-1 {
		t.Errorf("%d writers were rejected, want %d", conflicts, writers-1)
	}
	if got := mustGet(t, store, "t1").Version; got != 2 {
		t.Errorf("version = %d, want exactly one bump", got)
	}
}

func TestTaskStoreListRequiresACaller(t *testing.T) {
	t.Parallel()

	// Task ownership is the store's only access control, so an unnamed caller
	// cannot be shown anything — the same answer the in-memory store gives.
	store := newTaskStore(t, func(cfg *a2aserver.TaskStoreConfig) {
		cfg.Authenticator = func(context.Context) (string, error) { return "", nil }
	})
	_, err := store.List(t.Context(), &a2a.ListTasksRequest{})
	errIsNot(t, err, a2a.ErrUnauthenticated, "List() without a caller")
}

func TestTaskStoreListRejectsAnOutOfRangePageSize(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	for _, pageSize := range []int{-1, 101} {
		_, err := store.List(t.Context(), &a2a.ListTasksRequest{PageSize: pageSize})
		errIsNot(t, err, a2a.ErrInvalidRequest, "List() with an out-of-range page size")
	}
	response, err := store.List(t.Context(), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if response.PageSize != 50 {
		t.Errorf("default PageSize = %d, want 50", response.PageSize)
	}
}

func TestTaskStoreListHonorsEveryFilter(t *testing.T) {
	t.Parallel()

	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	store := newTaskStore(t)

	withTimestamp := newTask("stamped-late", "ctx-a", a2a.TaskStateWorking)
	withTimestamp.Status.Timestamp = &late
	stale := newTask("stamped-early", "ctx-a", a2a.TaskStateWorking)
	stale.Status.Timestamp = &early
	unstamped := newTask("unstamped", "ctx-a", a2a.TaskStateWorking)
	otherContext := newTask("other-context", "ctx-b", a2a.TaskStateWorking)
	otherState := newTask("other-state", "ctx-a", a2a.TaskStateCompleted)
	for _, task := range []*a2a.Task{withTimestamp, stale, unstamped, otherContext, otherState} {
		mustCreate(t, store, task)
	}

	middle := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name    string
		request *a2a.ListTasksRequest
		want    []string
	}{
		{"no filter", &a2a.ListTasksRequest{}, []string{
			"unstamped", "stamped-late", "stamped-early", "other-state", "other-context",
		}},
		{"context", &a2a.ListTasksRequest{ContextID: "ctx-b"}, []string{"other-context"}},
		{"status", &a2a.ListTasksRequest{Status: a2a.TaskStateCompleted}, []string{"other-state"}},
		{
			"context and status",
			&a2a.ListTasksRequest{ContextID: "ctx-a", Status: a2a.TaskStateCompleted},
			[]string{"other-state"},
		},
		{
			// A row with no status timestamp is kept: the filter narrows what it
			// can judge and stays silent about the rest.
			"status timestamp",
			&a2a.ListTasksRequest{StatusTimestampAfter: &middle},
			[]string{"unstamped", "stamped-late", "other-state", "other-context"},
		},
		{"unknown tenant", &a2a.ListTasksRequest{Tenant: "other-tenant"}, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			response, err := store.List(t.Context(), testCase.request)
			if err != nil {
				t.Fatalf("List() error = %v, want nil", err)
			}
			if got := taskIDs(response.Tasks); !equalUnordered(got, testCase.want) {
				t.Errorf("List() = %v, want %v", got, testCase.want)
			}
			if response.TotalSize != len(testCase.want) {
				t.Errorf("TotalSize = %d, want %d", response.TotalSize, len(testCase.want))
			}
		})
	}
}

func TestTaskStoreListOrdersAndPaginates(t *testing.T) {
	t.Parallel()

	// A controlled clock is what makes the ordering assertion exact rather than
	// dependent on how fast the machine runs.
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newTaskStore(t, func(cfg *a2aserver.TaskStoreConfig) {
		cfg.Now = func() time.Time {
			stamp = stamp.Add(time.Second)
			return stamp
		}
	})
	for _, id := range []string{"first", "second", "third", "fourth", "fifth"} {
		mustCreate(t, store, newTask(id, "ctx", a2a.TaskStateWorking))
	}

	// Newest first, which is what a client paging a task list expects.
	first, err := store.List(t.Context(), &a2a.ListTasksRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if got, want := taskIDs(first.Tasks), []string{"fifth", "fourth"}; !equalOrdered(got, want) {
		t.Errorf("first page = %v, want %v", got, want)
	}
	if first.TotalSize != 5 {
		t.Errorf("TotalSize = %d, want 5", first.TotalSize)
	}
	if first.NextPageToken == "" {
		t.Fatal("NextPageToken is empty, want a cursor")
	}

	second, err := store.List(t.Context(), &a2a.ListTasksRequest{PageSize: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if got, want := taskIDs(second.Tasks), []string{"third", "second"}; !equalOrdered(got, want) {
		t.Errorf("second page = %v, want %v", got, want)
	}

	last, err := store.List(t.Context(), &a2a.ListTasksRequest{PageSize: 2, PageToken: second.NextPageToken})
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if got, want := taskIDs(last.Tasks), []string{"first"}; !equalOrdered(got, want) {
		t.Errorf("last page = %v, want %v", got, want)
	}
	if last.NextPageToken != "" {
		t.Errorf("NextPageToken = %q on the last page, want empty", last.NextPageToken)
	}
}

func TestTaskStoreListBreaksTimestampTiesByID(t *testing.T) {
	t.Parallel()

	// A frozen clock makes every row share a last-updated stamp, which is the
	// case where a cursor without a tiebreak would loop or skip.
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newTaskStore(t, func(cfg *a2aserver.TaskStoreConfig) {
		cfg.Now = func() time.Time { return frozen }
	})
	for _, id := range []string{"a", "b", "c", "d"} {
		mustCreate(t, store, newTask(id, "ctx", a2a.TaskStateWorking))
	}

	var seen []string
	token := ""
	for range 4 {
		response, err := store.List(t.Context(), &a2a.ListTasksRequest{PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List() error = %v, want nil", err)
		}
		seen = append(seen, taskIDs(response.Tasks)...)
		token = response.NextPageToken
		if token == "" {
			break
		}
	}
	if want := []string{"d", "c", "b", "a"}; !equalOrdered(seen, want) {
		t.Errorf("paged ids = %v, want %v", seen, want)
	}
}

func TestTaskStoreListRejectsAMalformedPageToken(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	for _, token := range []string{"not-base64!!", "bm90LWEtY3Vyc29y", "MjAyNi0wMS0wMV9pZA"} {
		_, err := store.List(t.Context(), &a2a.ListTasksRequest{PageToken: token})
		errIsNot(t, err, a2a.ErrParseError, "List() with a malformed page token")
	}
}

func TestTaskStoreListTrimsHistoryAndArtifacts(t *testing.T) {
	t.Parallel()

	store := newTaskStore(t)
	task := newTask("t1", "ctx", a2a.TaskStateWorking)
	for range 4 {
		task.History = append(task.History, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("turn")))
	}
	task.Artifacts = []*a2a.Artifact{{ID: a2a.NewArtifactID(), Parts: a2a.ContentParts{a2a.NewTextPart("out")}}}
	mustCreate(t, store, task)

	zero, two := 0, 2
	for _, testCase := range []struct {
		request       *a2a.ListTasksRequest
		name          string
		wantHistory   int
		wantArtifacts int
	}{
		{&a2a.ListTasksRequest{}, "default keeps the history and drops artifacts", 5, 0},
		{&a2a.ListTasksRequest{HistoryLength: &zero}, "zero empties the history", 0, 0},
		{&a2a.ListTasksRequest{HistoryLength: &two}, "a bound keeps the tail", 2, 0},
		{&a2a.ListTasksRequest{IncludeArtifacts: true}, "artifacts on request", 5, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			response, err := store.List(t.Context(), testCase.request)
			if err != nil {
				t.Fatalf("List() error = %v, want nil", err)
			}
			if len(response.Tasks) != 1 {
				t.Fatalf("List() returned %d tasks, want 1", len(response.Tasks))
			}
			listed := response.Tasks[0]
			if len(listed.History) != testCase.wantHistory {
				t.Errorf("history length = %d, want %d", len(listed.History), testCase.wantHistory)
			}
			if len(listed.Artifacts) != testCase.wantArtifacts {
				t.Errorf("artifact count = %d, want %d", len(listed.Artifacts), testCase.wantArtifacts)
			}
		})
	}
}

func TestTaskStoreListScopesToTheCaller(t *testing.T) {
	t.Parallel()

	var caller string
	store := newTaskStore(t, func(cfg *a2aserver.TaskStoreConfig) {
		cfg.Authenticator = func(context.Context) (string, error) { return caller, nil }
	})
	caller = testOwner
	mustCreate(t, store, newTask("mine", "ctx", a2a.TaskStateWorking))
	caller = "bob@example.test"
	mustCreate(t, store, newTask("theirs", "ctx", a2a.TaskStateWorking))

	for _, testCase := range []struct{ caller, want string }{
		{testOwner, "mine"},
		{"bob@example.test", "theirs"},
	} {
		caller = testCase.caller
		response, err := store.List(t.Context(), &a2a.ListTasksRequest{})
		if err != nil {
			t.Fatalf("List() error = %v, want nil", err)
		}
		if got := taskIDs(response.Tasks); !equalOrdered(got, []string{testCase.want}) {
			t.Errorf("%s listed %v, want [%s]", testCase.caller, got, testCase.want)
		}
	}
}

func TestTaskStoreCreateReportsAnAuthenticatorFailure(t *testing.T) {
	t.Parallel()

	refused := errors.New("no credentials")
	store := newTaskStore(t, func(cfg *a2aserver.TaskStoreConfig) {
		cfg.Authenticator = func(context.Context) (string, error) { return "", refused }
	})
	_, err := store.Create(t.Context(), newTask("t1", "ctx", a2a.TaskStateWorking))
	errIsNot(t, err, refused, "Create() with a failing authenticator")
}

func TestTaskStoreOpenRequiresAPath(t *testing.T) {
	t.Parallel()

	_, err := a2aserver.OpenTaskStore(t.Context(), a2aserver.TaskStoreConfig{})
	errIsNot(t, err, a2aserver.ErrTaskStore, "OpenTaskStore() without a path")
}

func TestTaskStoreRefusesAnIncompatibleGeneration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), a2aserver.TaskDatabaseName)
	writeFutureTaskStore(t, path, "99")

	_, err := a2aserver.OpenTaskStore(t.Context(), a2aserver.TaskStoreConfig{Path: path})
	if err == nil {
		t.Fatal("OpenTaskStore() error = nil, want a refusal")
	}
	errIsNot(t, err, a2aserver.ErrTaskStore, "OpenTaskStore() on a future generation")
	if !strings.Contains(err.Error(), "Upgrade the application or select a compatible snapshot") {
		t.Errorf("OpenTaskStore() error = %v, want the snapshot guidance", err)
	}
}

func TestTaskStoreRefusesALegacyDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), a2aserver.TaskDatabaseName)
	execSQL(t, path, `CREATE TABLE tasks (id TEXT PRIMARY KEY)`)

	_, err := a2aserver.OpenTaskStore(t.Context(), a2aserver.TaskStoreConfig{Path: path})
	if err == nil {
		t.Fatal("OpenTaskStore() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "unsupported legacy or malformed schema") {
		t.Errorf("OpenTaskStore() error = %v, want the legacy-schema refusal", err)
	}
}

// taskIDs renders a task slice the way the assertions read it.
func taskIDs(tasks []*a2a.Task) []string {
	found := make([]string, 0, len(tasks))
	for _, task := range tasks {
		found = append(found, string(task.ID))
	}
	return found
}

// equalOrdered compares two id slices exactly.
func equalOrdered(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// equalUnordered compares two id slices as sets, for the filter assertions
// where the order is a separate test's concern.
func equalUnordered(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[string]int, len(want))
	for _, id := range want {
		counts[id]++
	}
	for _, id := range got {
		counts[id]--
		if counts[id] < 0 {
			return false
		}
	}
	return true
}
