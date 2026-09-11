package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadRetrievalCasesDerivesQueriesFromImmutableSeed(t *testing.T) {
	t.Parallel()

	cases, err := LoadRetrievalCases(filepath.Join("..", "agents", "data"))
	if err != nil {
		t.Fatalf("LoadRetrievalCases() error = %v", err)
	}
	if len(cases) != 10 {
		t.Fatalf("len(cases) = %d, want 10", len(cases))
	}
	if got, want := cases[0], (RetrievalCase{
		ID:              "INC-001",
		Query:           "Checkout latency spike. p99 checkout latency rose from 200ms to 3.5s right after the 08:00 deploy.",
		ExpectedRunbook: "high-latency",
	}); got != want {
		t.Fatalf("cases[0] = %#v, want %#v", got, want)
	}
	if got, want := cases[len(cases)-1].ID, "INC-010"; got != want {
		t.Fatalf("last case id = %q, want %q", got, want)
	}
}

func TestRunRetrievalEvaluationUsesDistinctModesAndWritesOnlyAggregateEvidence(t *testing.T) {
	t.Parallel()

	dataDir, cases := writeRetrievalFixture(t)
	answers := map[RetrievalMode]map[string][]string{
		RetrievalKeyword: {
			cases[0].Query: {"latency", "other-one", "other-two"},
			cases[1].Query: {"other-one", "down", "other-two"},
			cases[2].Query: {"other-one", "other-two", "other-three"},
			cases[3].Query: {"other-one", "other-two", "other-three"},
		},
		RetrievalSemantic: {
			cases[0].Query: {"latency", "other-one", "other-two"},
			cases[1].Query: {"down", "other-one", "other-two"},
			cases[2].Query: {"other-one", "errors", "other-two"},
			cases[3].Query: {"other-one", "other-two", "other-three"},
		},
	}
	factory := &fakeRetrievalFactory{answers: answers}

	artifact, err := RunRetrievalEvaluation(t.Context(), RetrievalRunConfig{
		DataDir: dataDir, Source: testSourceEvidence(), EmbeddingModel: "embed-fixture",
		EmbeddingModelDigest: "sha256:model-fixture",
		Factory:              factory.New,
	})
	if err != nil {
		t.Fatalf("RunRetrievalEvaluation() error = %v", err)
	}
	if got, want := factory.modes, []RetrievalMode{RetrievalKeyword, RetrievalSemantic}; !slices.Equal(got, want) {
		t.Fatalf("factory modes = %v, want %v", got, want)
	}
	if got, want := factory.closed, []RetrievalMode{RetrievalKeyword, RetrievalSemantic}; !slices.Equal(got, want) {
		t.Fatalf("closed modes = %v, want %v", got, want)
	}
	for mode, client := range factory.clients {
		if got, want := client.calls, len(cases); got != want {
			t.Errorf("%s calls = %d, want %d", mode, got, want)
		}
	}
	if artifact.SchemaVersion != RetrievalArtifactSchemaVersion || artifact.CaseCount != 4 {
		t.Fatalf("artifact identity = %#v", artifact)
	}
	if len(artifact.CorpusDigest) != 64 {
		t.Errorf("corpus digest length = %d, want 64", len(artifact.CorpusDigest))
	}
	if artifact.EmbeddingModelDigest != "sha256:model-fixture" {
		t.Errorf("embedding model digest = %q", artifact.EmbeddingModelDigest)
	}
	if got, want := artifact.Keyword, (RetrievalRates{HitRateAt1: 0.25, HitRateAt3: 0.5}); got != want {
		t.Errorf("keyword rates = %#v, want %#v", got, want)
	}
	if got, want := artifact.Semantic, (RetrievalRates{HitRateAt1: 0.5, HitRateAt3: 0.75}); got != want {
		t.Errorf("semantic rates = %#v, want %#v", got, want)
	}

	rendered, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, secret := range []string{
		cases[0].Query,
		cases[0].ExpectedRunbook,
		"http://127.0.0.1",
		"tool payload",
	} {
		if strings.Contains(string(rendered), secret) {
			t.Errorf("artifact contains retrieval content %q: %s", secret, rendered)
		}
	}
}

func TestRetrievalCorpusDigestBindsRunbookContents(t *testing.T) {
	t.Parallel()

	dataDir, cases := writeRetrievalFixture(t)
	before, err := retrievalCorpusDigest(dataDir, cases)
	if err != nil {
		t.Fatalf("retrievalCorpusDigest(before) error = %v", err)
	}
	path := filepath.Join(dataDir, "runbooks", "latency.md")
	if writeErr := os.WriteFile(path, []byte("# latency\n\nChanged body only.\n"), 0o600); writeErr != nil {
		t.Fatalf("WriteFile(runbook) error = %v", writeErr)
	}
	after, err := retrievalCorpusDigest(dataDir, cases)
	if err != nil {
		t.Fatalf("retrievalCorpusDigest(after) error = %v", err)
	}
	if before == after {
		t.Fatalf("digest stayed %q after a runbook-content-only change", before)
	}
}

func TestLoadRetrievalCasesRejectsAnUnparsedIncidentRow(t *testing.T) {
	t.Parallel()

	dataDir, _ := writeRetrievalFixture(t)
	path := filepath.Join(dataDir, "sql", "seed.sql")
	seed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(seed) error = %v", err)
	}
	malformed := strings.Replace(string(seed), "'Disk summary.'", "NULL", 1)
	if writeErr := os.WriteFile(path, []byte(malformed), 0o600); writeErr != nil {
		t.Fatalf("WriteFile(seed) error = %v", writeErr)
	}
	if _, err := LoadRetrievalCases(dataDir); err == nil || !strings.Contains(err.Error(), "parsed 3 of 4") {
		t.Fatalf("LoadRetrievalCases() error = %v, want incomplete parse", err)
	}
}

func TestRunRetrievalEvaluationRejectsSemanticFallback(t *testing.T) {
	t.Parallel()

	dataDir, cases := writeRetrievalFixture(t)
	answers := answersForEveryCase(cases)
	factory := &fakeRetrievalFactory{
		answers:      answers,
		observedMode: map[RetrievalMode]RetrievalMode{RetrievalSemantic: RetrievalKeyword},
	}

	_, err := RunRetrievalEvaluation(t.Context(), RetrievalRunConfig{
		DataDir: dataDir, Source: testSourceEvidence(), EmbeddingModel: "embed", Factory: factory.New,
	})
	if err == nil || !strings.Contains(err.Error(), "semantic runtime reported keyword retrieval") {
		t.Fatalf("RunRetrievalEvaluation() error = %v, want semantic fallback rejection", err)
	}
	if got, want := factory.closed, []RetrievalMode{RetrievalKeyword, RetrievalSemantic}; !slices.Equal(got, want) {
		t.Fatalf("closed modes = %v, want %v", got, want)
	}
}

func TestRunRetrievalEvaluationRejectsReusedRuntime(t *testing.T) {
	t.Parallel()

	dataDir, cases := writeRetrievalFixture(t)
	factory := &fakeRetrievalFactory{answers: answersForEveryCase(cases), sharedID: "same-runtime"}

	_, err := RunRetrievalEvaluation(t.Context(), RetrievalRunConfig{
		DataDir: dataDir, Source: testSourceEvidence(), EmbeddingModel: "embed", Factory: factory.New,
	})
	if err == nil || !strings.Contains(err.Error(), "must use a distinct isolated runtime") {
		t.Fatalf("RunRetrievalEvaluation() error = %v, want runtime isolation rejection", err)
	}
	if got, want := factory.closed, []RetrievalMode{RetrievalKeyword, RetrievalSemantic}; !slices.Equal(got, want) {
		t.Fatalf("closed modes = %v, want %v", got, want)
	}
}

func TestRunRetrievalEvaluationJoinsCleanupFailure(t *testing.T) {
	t.Parallel()

	dataDir, cases := writeRetrievalFixture(t)
	factory := &fakeRetrievalFactory{
		answers: answersForEveryCase(cases),
		closeErrors: map[RetrievalMode]error{
			RetrievalKeyword: errors.New("cleanup failed"),
		},
	}

	_, err := RunRetrievalEvaluation(t.Context(), RetrievalRunConfig{
		DataDir: dataDir, Source: testSourceEvidence(), EmbeddingModel: "embed", Factory: factory.New,
	})
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("RunRetrievalEvaluation() error = %v, want cleanup failure", err)
	}
	if len(factory.modes) != 1 {
		t.Fatalf("factory modes = %v, want evaluation to stop after invalid cleanup", factory.modes)
	}
}

func TestLoadRetrievalCasesRefusesASeedItCannotTrust(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		build func(*testing.T) string
		want  string
	}{
		"no data directory": {
			build: func(*testing.T) string { return "  " },
			want:  "immutable data directory",
		},
		"missing seed": {
			build: func(t *testing.T) string { t.Helper(); return t.TempDir() },
			want:  "read retrieval seed",
		},
		"no incidents insert": {
			build: func(t *testing.T) string {
				t.Helper()
				return rewriteRetrievalSeed(t, "INSERT INTO incidents", "INSERT INTO services")
			},
			want: "no incidents insert",
		},
		"unparseable rows": {
			build: func(t *testing.T) string {
				t.Helper()
				directory, _ := writeRetrievalFixture(t)
				writeFixtureFile(t, filepath.Join(directory, "sql", "seed.sql"),
					"INSERT INTO incidents (id) VALUES\n    (1);\n")
				return directory
			},
			want: "no parseable incident rows",
		},
		"duplicate incident": {
			build: func(t *testing.T) string { t.Helper(); return rewriteRetrievalSeed(t, "'INC-002'", "'INC-001'") },
			want:  `repeats incident "INC-001"`,
		},
		"blank summary": {
			build: func(t *testing.T) string { t.Helper(); return rewriteRetrievalSeed(t, "'Latency summary.'", "'   '") },
			want:  `incident "INC-001" needs a title and summary`,
		},
		"unknown runbook": {
			build: func(t *testing.T) string { t.Helper(); return rewriteRetrievalSeed(t, "'latency'", "'absent-runbook'") },
			want:  `names unknown runbook "absent-runbook"`,
		},
		"missing runbooks": {
			build: func(t *testing.T) string {
				t.Helper()
				directory, _ := writeRetrievalFixture(t)
				removeFixturePath(t, filepath.Join(directory, "runbooks"))
				return directory
			},
			want: "read domain directory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadRetrievalCases(test.build(t)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadRetrievalCases() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestRunRetrievalEvaluationRefusesAnUnidentifiedConfiguration(t *testing.T) {
	t.Parallel()

	dataDir, cases := writeRetrievalFixture(t)
	valid := RetrievalRunConfig{
		DataDir: dataDir, Source: testSourceEvidence(), EmbeddingModel: "embed",
		Factory: (&fakeRetrievalFactory{answers: answersForEveryCase(cases)}).New,
	}
	for name, test := range map[string]struct {
		mutate func(*RetrievalRunConfig)
		want   string
	}{
		"data directory": {
			mutate: func(c *RetrievalRunConfig) { c.DataDir = "" },
			want:   "needs an immutable data directory",
		},
		"source": {
			mutate: func(c *RetrievalRunConfig) { c.Source = SourceEvidence{Revision: "abc"} },
			want:   "one full lowercase revision",
		},
		"embedding model": {
			mutate: func(c *RetrievalRunConfig) { c.EmbeddingModel = " embed " },
			want:   "trimmed, single-line embedding model",
		},
		"embedding digest": {
			mutate: func(c *RetrievalRunConfig) { c.EmbeddingModelDigest = "sha256:one\nsha256:two" },
			want:   "digest must be trimmed and single-line",
		},
		"factory": {
			mutate: func(c *RetrievalRunConfig) { c.Factory = nil },
			want:   "needs a runtime factory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config := valid
			test.mutate(&config)
			if _, err := RunRetrievalEvaluation(t.Context(), config); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("RunRetrievalEvaluation() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestRunRetrievalEvaluationRefusesAnUnrankableSearchResult(t *testing.T) {
	t.Parallel()

	dataDir, cases := writeRetrievalFixture(t)
	// The hit rates only mean something if the runtime answered with a bounded,
	// de-duplicated ranking of real runbook slugs. Anything else is not a ranking,
	// so it fails the run instead of quietly scoring as a miss.
	for name, test := range map[string]struct {
		want  string
		slugs []string
	}{
		"over the limit": {
			slugs: []string{"latency", "down", "errors", "disk"},
			want:  "returned 4 results, limit is 3",
		},
		"invalid slug":   {slugs: []string{"Service Down"}, want: "invalid runbook slug"},
		"repeated slug":  {slugs: []string{"latency", "latency"}, want: "repeated runbook slug"},
		"empty top slug": {slugs: []string{""}, want: "invalid runbook slug"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answers := answersForEveryCase(cases)
			answers[RetrievalKeyword][cases[0].Query] = test.slugs
			_, err := RunRetrievalEvaluation(t.Context(), RetrievalRunConfig{
				DataDir: dataDir, Source: testSourceEvidence(), EmbeddingModel: "embed",
				Factory: (&fakeRetrievalFactory{answers: answers}).New,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RunRetrievalEvaluation() error = %v, want one mentioning %q", err, test.want)
			}
			if strings.Contains(err.Error(), cases[0].Query) {
				t.Fatalf("RunRetrievalEvaluation() error leaked the query: %v", err)
			}
		})
	}
}

func TestWriteRetrievalArtifactPublishesOnlyValidatedEvidence(t *testing.T) {
	t.Parallel()

	valid := RetrievalArtifact{
		Source: testSourceEvidence(), CorpusDigest: strings.Repeat("a", 64), EmbeddingModel: "embed",
		Keyword: RetrievalRates{HitRateAt1: 0.5, HitRateAt3: 1}, Semantic: RetrievalRates{HitRateAt1: 0.75, HitRateAt3: 1},
		SchemaVersion: RetrievalArtifactSchemaVersion, CaseCount: 4,
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "retrieval.json")
	if err := WriteRetrievalArtifact(path, valid); err != nil {
		t.Fatalf("WriteRetrievalArtifact() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(artifact) error = %v", err)
	}
	var published RetrievalArtifact
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatalf("json.Unmarshal(artifact) error = %v", err)
	}
	// TreeDigest is not part of the artifact schema, so the round trip drops it.
	want := valid
	want.Source.TreeDigest = ""
	if published != want {
		t.Fatalf("published artifact = %+v, want %+v", published, want)
	}

	// An artifact that cannot pass its own validation is never written at all:
	// a file on disk is read later as evidence that a measurement happened.
	invalid := valid
	invalid.CaseCount = 0
	rejected := filepath.Join(directory, "rejected.json")
	if err := WriteRetrievalArtifact(rejected, invalid); err == nil ||
		!strings.Contains(err.Error(), "case_count") {
		t.Fatalf("WriteRetrievalArtifact(invalid) error = %v, want a validation refusal", err)
	}
	if _, err := os.Stat(rejected); !os.IsNotExist(err) {
		t.Fatalf("Stat(rejected artifact) = %v, want the file never to have been created", err)
	}
}

func rewriteRetrievalSeed(t *testing.T, old, replacement string) string {
	t.Helper()
	directory, _ := writeRetrievalFixture(t)
	path := filepath.Join(directory, "sql", "seed.sql")
	seed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(seed) error = %v", err)
	}
	if !strings.Contains(string(seed), old) {
		t.Fatalf("seed fixture no longer contains %q", old)
	}
	writeFixtureFile(t, path, strings.Replace(string(seed), old, replacement, 1))
	return directory
}

func TestValidateRetrievalArtifactRejectsIncompleteIdentityAndInvalidRates(t *testing.T) {
	t.Parallel()

	valid := RetrievalArtifact{
		Source: testSourceEvidence(), CorpusDigest: strings.Repeat("a", 64), EmbeddingModel: "embed",
		EmbeddingModelDigest: "sha256:model",
		Keyword:              RetrievalRates{HitRateAt1: 0.5, HitRateAt3: 1},
		Semantic:             RetrievalRates{HitRateAt1: 0.75, HitRateAt3: 1},
		SchemaVersion:        RetrievalArtifactSchemaVersion, CaseCount: 4,
	}
	if err := ValidateRetrievalArtifact(valid); err != nil {
		t.Fatalf("ValidateRetrievalArtifact(valid) error = %v", err)
	}

	tests := map[string]RetrievalArtifact{
		"source":       func() RetrievalArtifact { value := valid; value.Source.Revision = "abc"; return value }(),
		"digest":       func() RetrievalArtifact { value := valid; value.CorpusDigest = "short"; return value }(),
		"embedding":    func() RetrievalArtifact { value := valid; value.EmbeddingModel = ""; return value }(),
		"model digest": func() RetrievalArtifact { value := valid; value.EmbeddingModelDigest = "bad\ndigest"; return value }(),
		"case count":   func() RetrievalArtifact { value := valid; value.CaseCount = 0; return value }(),
		"rate range":   func() RetrievalArtifact { value := valid; value.Semantic.HitRateAt3 = 1.1; return value }(),
		"monotonic": func() RetrievalArtifact {
			value := valid
			value.Keyword.HitRateAt1 = 0.75
			value.Keyword.HitRateAt3 = 0.5
			return value
		}(),
	}
	for name, artifact := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateRetrievalArtifact(artifact); err == nil {
				t.Fatal("ValidateRetrievalArtifact() error = nil")
			}
		})
	}
}

type fakeRetrievalClient struct {
	answers      map[string][]string
	observedMode RetrievalMode
	calls        int
}

func (c *fakeRetrievalClient) SearchRunbooks(_ context.Context, query string, limit int) (RetrievalSearchResult, error) {
	c.calls++
	if limit != 3 {
		return RetrievalSearchResult{}, fmt.Errorf("limit = %d, want 3", limit)
	}
	return RetrievalSearchResult{Mode: c.observedMode, Slugs: slices.Clone(c.answers[query])}, nil
}

type fakeRetrievalFactory struct {
	answers      map[RetrievalMode]map[string][]string
	observedMode map[RetrievalMode]RetrievalMode
	closeErrors  map[RetrievalMode]error
	clients      map[RetrievalMode]*fakeRetrievalClient
	sharedID     string
	modes        []RetrievalMode
	closed       []RetrievalMode
}

func (f *fakeRetrievalFactory) New(_ context.Context, mode RetrievalMode) (RetrievalRuntime, error) {
	f.modes = append(f.modes, mode)
	observed := mode
	if override := f.observedMode[mode]; override != "" {
		observed = override
	}
	client := &fakeRetrievalClient{answers: f.answers[mode], observedMode: observed}
	if f.clients == nil {
		f.clients = make(map[RetrievalMode]*fakeRetrievalClient)
	}
	f.clients[mode] = client
	id := string(mode) + "-runtime"
	if f.sharedID != "" {
		id = f.sharedID
	}
	return RetrievalRuntime{
		ID: id, Client: client,
		Close: func() error {
			f.closed = append(f.closed, mode)
			return f.closeErrors[mode]
		},
	}, nil
}

func answersForEveryCase(cases []RetrievalCase) map[RetrievalMode]map[string][]string {
	answers := map[RetrievalMode]map[string][]string{
		RetrievalKeyword:  {},
		RetrievalSemantic: {},
	}
	for _, evalCase := range cases {
		answers[RetrievalKeyword][evalCase.Query] = []string{evalCase.ExpectedRunbook}
		answers[RetrievalSemantic][evalCase.Query] = []string{evalCase.ExpectedRunbook}
	}
	return answers
}

func writeRetrievalFixture(t *testing.T) (string, []RetrievalCase) {
	t.Helper()
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "sql"), 0o755); err != nil {
		t.Fatalf("MkdirAll(sql) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "runbooks"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runbooks) error = %v", err)
	}
	seed := `INSERT INTO incidents (id, service, title, severity, status, runbook, opened_at, resolved_at, summary) VALUES
    ('INC-001', 'checkout', 'Latency title', 'SEV2', 'open', 'latency', '2026-01-01T00:00:00Z', NULL, 'Latency summary.'),
    ('INC-002', 'inventory', 'Down title', 'SEV1', 'open', 'down', '2026-01-01T00:00:00Z', NULL, 'Down summary.'),
    ('INC-003', 'payments', 'Errors title', 'SEV3', 'open', 'errors', '2026-01-01T00:00:00Z', NULL, 'Errors summary.'),
    ('INC-004', 'checkout', 'Disk title', 'SEV2', 'open', 'disk', '2026-01-01T00:00:00Z', NULL, 'Disk summary.');
`
	if err := os.WriteFile(filepath.Join(directory, "sql", "seed.sql"), []byte(seed), 0o600); err != nil {
		t.Fatalf("WriteFile(seed) error = %v", err)
	}
	for _, slug := range []string{"latency", "down", "errors", "disk"} {
		if err := os.WriteFile(filepath.Join(directory, "runbooks", slug+".md"), []byte("# "+slug+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(runbook) error = %v", err)
		}
	}
	cases, err := LoadRetrievalCases(directory)
	if err != nil {
		t.Fatalf("LoadRetrievalCases() error = %v", err)
	}
	return directory, cases
}
