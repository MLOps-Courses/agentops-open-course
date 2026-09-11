package memory

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file is the Go port of tests/test_retrieval.py: the provenance-bound
// semantic index, and the rule that a stale or incomplete generation is never
// queried.
//
// Every test here is offline. The embeddings endpoint is an httptest server
// inside the test process, so the suite behaves identically on a laptop with a
// model pulled and in CI without one.

// The addition that grows one runbook, and therefore the corpus hash.
const corpusEdit = "\n## New check\nInspect inode pressure.\n"

func TestChunkingSplitsOnHeadingsAndKeepsTheSlug(t *testing.T) {
	t.Parallel()

	content := "# Title\nintro\n\n## Symptoms\nslow queries\n\n### Fix\nrestart the pool"
	chunks := ChunkRunbook(highLatencyBook, content)

	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v, want 3", chunks)
	}
	for _, chunk := range chunks {
		// The slug prefix is what carries a chunk's provenance into the vector
		// space: a retrieved passage names its document without a join.
		if !strings.HasPrefix(chunk, highLatencyBook+": ") {
			t.Errorf("chunk %q does not carry its slug", chunk)
		}
	}
	// The heading text stays with the section it introduces — only the marker is
	// consumed — which is what makes a chunk self-describing.
	contains(t, chunks[1], "Symptoms", "the second chunk")
	contains(t, chunks[2], "Fix", "the third chunk")
}

func TestChunkingNeverReturnsNothing(t *testing.T) {
	t.Parallel()

	// A corpus entry that silently vanished would change the chunk count without
	// changing the corpus hash, which is exactly the inconsistency the freshness
	// rule cannot detect.
	if chunks := ChunkRunbook("empty", "   "); len(chunks) != 1 || chunks[0] != "empty: " {
		t.Errorf("ChunkRunbook(\"empty\", \"   \") = %#v, want one empty chunk", chunks)
	}
	if chunks := ChunkRunbook("plain", "no headings here"); len(chunks) != 1 {
		t.Errorf("ChunkRunbook of a heading-less runbook = %#v, want one chunk", chunks)
	}
}

func TestNormalizationMakesTheCorpusHashPlatformIndependent(t *testing.T) {
	t.Parallel()

	// The same document written on three platforms must hash the same, or every
	// checkout on another operating system would rebuild the whole index.
	unix := "# Title\nbody\n"
	windows := "# Title\r\nbody   \r\n"
	classicMac := "# Title\rbody\t\r"
	if normalizeRunbook(unix) != normalizeRunbook(windows) {
		t.Errorf("CRLF normalized to %q, want %q", normalizeRunbook(windows), normalizeRunbook(unix))
	}
	if normalizeRunbook(unix) != normalizeRunbook(classicMac) {
		t.Errorf("CR normalized to %q, want %q", normalizeRunbook(classicMac), normalizeRunbook(unix))
	}
	// Exactly one trailing newline, always.
	if got := normalizeRunbook("body\n\n\n"); got != "body\n" {
		t.Errorf("normalizeRunbook(%q) = %q, want %q", "body\n\n\n", got, "body\n")
	}
	if got := normalizeRunbook(""); got != "\n" {
		t.Errorf("normalizeRunbook(\"\") = %q, want a bare newline", got)
	}
}

func TestCorpusHashReproducesThePythonEncoding(t *testing.T) {
	t.Parallel()

	// The golden value was produced by the Python track's exact expression,
	//
	//	hashlib.sha256(json.dumps(corpus, separators=(",", ":"),
	//	                          ensure_ascii=True).encode()).hexdigest()
	//
	// over the corpus below. Reproducing it byte for byte is what lets the two
	// tracks agree on whether an index is stale for the same runbooks, and the
	// inputs are chosen to cover every escape that differs between Go's
	// encoding/json and Python's: an em dash, a quote, a backslash, a control
	// character, an astral-plane emoji, and U+2028.
	corpus := [][2]string{
		{"alpha", "line — em dash\nsecond \"quoted\" \\ back\n"},
		{"beta", "tab\there\x01ctrl\n"},
		{"gamma", "emoji \U0001F600 and   sep\n"},
	}
	const golden = "c04ebad0993309cf0b0e7c6ebafa45d28000ab3be7bec50b8618902120e8fed8"
	if hash := hashCorpus(corpus); hash != golden {
		t.Errorf("hashCorpus() = %q, want the Python digest %q", hash, golden)
	}
}

func TestIndexAndSearchRankBySimilarity(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	count, err := retriever.IndexRunbooks(t.Context())
	if err != nil {
		t.Fatalf("IndexRunbooks() error = %v, want nil", err)
	}
	if count == 0 {
		t.Fatal("IndexRunbooks() indexed nothing")
	}

	provenance, err := retriever.IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if provenance == nil {
		t.Fatal("IndexProvenance() = nil, want the generation metadata")
	}
	// Every one of these is an input to the freshness rule. A generation that
	// recorded fewer of them could not detect the change it failed to record.
	if provenance.FormatVersion != indexFormatVersion {
		t.Errorf("format version = %d, want %d", provenance.FormatVersion, indexFormatVersion)
	}
	if provenance.EmbeddingModel != embeddingModel {
		t.Errorf("embedding model = %q, want %q", provenance.EmbeddingModel, embeddingModel)
	}
	if provenance.ModelDigest != "sha256:model-a" {
		t.Errorf("model digest = %q, want the resolved artifact", provenance.ModelDigest)
	}
	if provenance.Dimensions != fakeDimensions {
		t.Errorf("dimensions = %d, want %d", provenance.Dimensions, fakeDimensions)
	}
	if provenance.ChunkerVersion != chunkerVersion {
		t.Errorf("chunker version = %q, want %q", provenance.ChunkerVersion, chunkerVersion)
	}
	if provenance.ChunkCount != count {
		t.Errorf("chunk count = %d, want %d", provenance.ChunkCount, count)
	}
	if provenance.CorpusSHA256 == "" || provenance.BuiltAt == "" {
		t.Errorf("provenance = %+v, want a corpus hash and a build time", provenance)
	}

	results, err := retriever.SemanticSearch(t.Context(), "cascade failure upstream dependency chain", 3)
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v, want nil", err)
	}
	if len(results) != 3 {
		t.Fatalf("SemanticSearch() returned %d results, want 3", len(results))
	}
	slugs := make([]string, 0, len(results))
	for _, match := range results {
		slugs = append(slugs, match.Slug)
		if match.Content == "" {
			t.Errorf("match %q carries no content", match.Slug)
		}
	}
	if !slices.Contains(slugs, cascadeBook) {
		t.Errorf("slugs = %v, want them to include %q", slugs, cascadeBook)
	}
	// Nearest first: a ranking that did not order by distance would still return
	// plausible runbooks, which is why this is asserted rather than eyeballed.
	if results[0].Distance > results[len(results)-1].Distance {
		t.Errorf("distances = %v, want them non-decreasing", distancesOf(results))
	}
	// Deduplicated per runbook: several chunks of one document must not crowd out
	// every other document.
	if len(unique(slugs)) != len(slugs) {
		t.Errorf("slugs = %v, want one result per runbook", slugs)
	}
}

func TestProvenanceIsAbsentBeforeTheFirstCompleteGeneration(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	provenance, err := fixture.memory.Retriever().IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if provenance != nil {
		t.Errorf("IndexProvenance() = %+v, want nil before anything was built", provenance)
	}
}

func TestProvenanceIsAbsentWhenAnInterruptedGenerationHasNoMetadataRow(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	if _, err := fixture.memory.Retriever().IndexRunbooks(t.Context()); err != nil {
		t.Fatalf("IndexRunbooks() error = %v, want nil", err)
	}
	// The metadata row is written last, inside the same transaction as the
	// vectors. Its absence is exactly what an interrupted generation looks like,
	// and it must read as "nothing complete was ever built".
	if _, err := vectorsDB(t, fixture).ExecContext(t.Context(), "DELETE FROM "+metadataTable); err != nil {
		t.Fatalf("delete the metadata row: %v", err)
	}

	provenance, err := fixture.memory.Retriever().IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if provenance != nil {
		t.Errorf("IndexProvenance() = %+v, want nil for an interrupted generation", provenance)
	}
}

func TestIndexingRejectsAnEmptyCorpus(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.store = func(real Store) Store {
			return stubStore{Store: real, listRunbookSlugs: func() ([]string, error) { return nil, nil }}
		}
	})
	// An empty knowledge base must fail with a contextual error rather than an
	// index out of range, and it must not degrade to keyword retrieval either:
	// the keyword scorer would answer "nothing found" from the same empty corpus
	// while hiding the reason.
	_, err := fixture.memory.Retriever().IndexRunbooks(t.Context())
	if !errors.Is(err, ErrEmptyCorpus) {
		t.Fatalf("IndexRunbooks() error = %v, want %v", err, ErrEmptyCorpus)
	}
	var unavailableErr *EmbeddingUnavailableError
	if errors.As(err, &unavailableErr) {
		t.Error("an empty corpus was reported as an embedding failure, which would silently fall back")
	}
	contains(t, err.Error(), "Seed the runbook library", "the empty-corpus error")
}

func TestSemanticSearchRejectsANonPositiveLimit(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	for _, limit := range []int{0, -3} {
		_, err := fixture.memory.Retriever().SemanticSearch(t.Context(), "anything", limit)
		if !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("SemanticSearch(limit=%d) error = %v, want %v", limit, err, ErrInvalidLimit)
		}
		// Not an embedding failure: keyword retrieval would not fix a caller that
		// asked for zero results.
		var unavailableErr *EmbeddingUnavailableError
		if errors.As(err, &unavailableErr) {
			t.Errorf("SemanticSearch(limit=%d) reported an embedding failure, which would fall back", limit)
		}
	}
}

func TestSearchBuildsTheIndexOnFirstUse(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	results, err := fixture.memory.Retriever().SemanticSearch(t.Context(), "disk almost full on the node", 2)
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v, want nil", err)
	}
	if len(results) != 2 {
		t.Fatalf("SemanticSearch() returned %d results, want 2", len(results))
	}
	// One corpus batch and one query batch: the index is built lazily, by the
	// first search that needs it, rather than at startup.
	if batches := fixture.ollama.indexBatches(); batches != 1 {
		t.Errorf("corpus embeddings = %d, want 1", batches)
	}
	queries := fixture.ollama.queryBatches()
	if len(queries) != 1 || queries[0][0] != "disk almost full on the node" {
		t.Errorf("query embeddings = %v, want exactly the query", queries)
	}
}

func TestACorpusEditInvalidatesTheGenerationExactlyOnce(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	search(t, retriever, "disk pressure")
	before := fixture.ollama.indexBatches()

	fixture.appendToRunbook(t, diskFullBook, corpusEdit)
	search(t, retriever, "inode pressure")
	search(t, retriever, "inode pressure")

	// Once, not twice: the edit invalidates the generation, the next search
	// rebuilds it, and the search after that reuses what was rebuilt.
	if batches := fixture.ollama.indexBatches(); batches != before+1 {
		t.Errorf("corpus embeddings = %d, want %d", batches, before+1)
	}
}

func TestAModelOrArtifactChangeInvalidatesTheGenerationExactlyOnce(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	search(t, fixture.memory.Retriever(), "disk pressure")

	// A different model name is a different vector space even if the endpoint
	// happens to serve it from the same weights.
	fixture.reconfigure(t, func(o *options) { o.embeddingModel = replacementModel })
	search(t, fixture.memory.Retriever(), "disk pressure")

	// And the same name can point at a new build. The immutable artifact digest
	// is what catches that; the name alone would not.
	fixture.ollama.setDigests("sha256:model-b")
	search(t, fixture.memory.Retriever(), "disk pressure")
	search(t, fixture.memory.Retriever(), "disk pressure")

	if batches := fixture.ollama.indexBatches(); batches != 3 {
		t.Errorf("corpus embeddings = %d, want 3 — one per provenance change", batches)
	}
}

func TestADimensionOrChunkerChangeInvalidatesTheGenerationExactlyOnce(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	search(t, retriever, "disk pressure")

	// A model that returns wider vectors is a different vector space too, and the
	// stored dimensionality is what proves it.
	fixture.ollama.update(func(o *ollama) { o.dimensions = fakeDimensions + 1 })
	search(t, retriever, "disk pressure")

	// So is a change to how documents are split, even with the same corpus and
	// the same model.
	retriever.chunkerVersion = "markdown-h2-h3-v2"
	search(t, retriever, "disk pressure")
	search(t, retriever, "disk pressure")

	if batches := fixture.ollama.indexBatches(); batches != 3 {
		t.Errorf("corpus embeddings = %d, want 3 — one per provenance change", batches)
	}
}

func TestSearchRejectsAModelSwapDuringQueryEmbedding(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// The digest is sampled on both sides of the request. A pull that lands
	// between the two produces vectors from an artifact nothing else was built
	// with, and averaging over the two spaces would be a silent correctness bug.
	fixture.ollama.setDigests("sha256:model-a", "sha256:model-b")

	_, err := fixture.memory.Retriever().SemanticSearch(t.Context(), "disk pressure", 2)
	contains(t, unavailable(t, err), "artifact changed", "the refusal")

	provenance, err := fixture.memory.Retriever().IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if provenance != nil {
		t.Errorf("IndexProvenance() = %+v, want nil — nothing was built", provenance)
	}
}

func TestSearchRejectsAModelSwapDuringCorpusEmbedding(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	search(t, retriever, "disk pressure")
	previous, err := retriever.IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}

	fixture.appendToRunbook(t, diskFullBook, corpusEdit)
	// Stable across the query embedding, then swapped underneath the corpus
	// rebuild: the four samples are the query's pair and the corpus batch's pair.
	fixture.ollama.setDigests("sha256:model-a", "sha256:model-a", "sha256:model-a", "sha256:model-b")

	_, err = retriever.SemanticSearch(t.Context(), "inode pressure", 2)
	contains(t, unavailable(t, err), "artifact changed", "the refusal")

	// The failed rebuild rolled back, so the previous generation is still on disk
	// intact — and still correctly stale for the corpus that failed to build.
	after, err := retriever.IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if after == nil || *after != *previous {
		t.Errorf("provenance = %+v, want the previous generation %+v", after, previous)
	}
}

func TestAFailedRebuildKeepsTheOldGenerationWithoutPresentingItAsCurrent(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	search(t, fixture.memory.Retriever(), "disk pressure")
	previous, err := fixture.memory.Retriever().IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}

	// A new model whose corpus embedding fails. The query still embeds, so the
	// search gets as far as the rebuild and no further.
	fixture.reconfigure(t, func(o *options) {
		o.embeddingModel = brokenModel
		o.configure = func(fake *ollama) {
			fake.embed = func(texts []string) ([][]float64, error) {
				if len(texts) > 1 {
					return nil, errAssertion
				}
				return [][]float64{fakeVector(texts[0], fakeDimensions)}, nil
			}
		}
	})
	retriever := fixture.memory.Retriever()
	_, err = retriever.SemanticSearch(t.Context(), "disk pressure", 2)
	unavailable(t, err)

	// The bytes survive: a failed rebuild must not destroy a working index.
	after, err := retriever.IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if after == nil || *after != *previous {
		t.Errorf("provenance = %+v, want the previous generation %+v", after, previous)
	}
	// And they are simultaneously not current for the configuration now in force.
	// Surviving and being served are different things, and this is the rule the
	// whole mechanism exists for.
	if ready := isReady(t, fixture, retriever, fakeDimensions); ready {
		t.Error("the surviving generation is reported ready for the new provenance")
	}
}

func TestConcurrentFirstUseBuildsOneGeneration(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()

	const workers = 4
	var group sync.WaitGroup
	results := make([][]SemanticMatch, workers)
	failures := make([]error, workers)
	group.Add(workers)
	for worker := range workers {
		go func() {
			defer group.Done()
			results[worker], failures[worker] = retriever.SemanticSearch(context.Background(), "disk pressure", 2)
		}()
	}
	group.Wait()

	for worker, err := range failures {
		if err != nil {
			t.Errorf("worker %d: SemanticSearch() error = %v, want nil", worker, err)
		}
		if len(results[worker]) == 0 {
			t.Errorf("worker %d returned nothing", worker)
		}
	}
	// The write lock is taken at BEGIN and held across the embedding call, so
	// four workers starting at once produce one generation rather than four.
	if batches := fixture.ollama.indexBatches(); batches != 1 {
		t.Errorf("corpus embeddings = %d, want 1", batches)
	}
}

func TestVectorsArePersistedAsBlobsInPlainSQLite(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	if _, err := fixture.memory.Retriever().IndexRunbooks(t.Context()); err != nil {
		t.Fatalf("IndexRunbooks() error = %v, want nil", err)
	}
	db := vectorsDB(t, fixture)

	var kind string
	var length int
	if err := db.QueryRowContext(t.Context(),
		"SELECT typeof(embedding), length(embedding) FROM "+chunksTable+" LIMIT 1",
	).Scan(&kind, &length); err != nil {
		t.Fatalf("inspect a stored embedding: %v", err)
	}
	// Little-endian float32, one value after another: 32 dimensions is 128 bytes,
	// and a 768-dimension production vector is 3 KB rather than 6.
	if kind != "blob" {
		t.Errorf("embedding storage class = %q, want blob", kind)
	}
	if want := 4 * fakeDimensions; length != want {
		t.Errorf("embedding length = %d bytes, want %d", length, want)
	}

	// There is no vector extension, and there must not be: the available Go
	// binding is cgo, which the static distroless binary rules out.
	var definition string
	if err := db.QueryRowContext(t.Context(),
		"SELECT sql FROM sqlite_master WHERE name = ?", chunksTable,
	).Scan(&definition); err != nil {
		t.Fatalf("read the chunk table definition: %v", err)
	}
	if strings.Contains(strings.ToLower(definition), "virtual") ||
		strings.Contains(strings.ToLower(definition), "vec0") {
		t.Errorf("the chunk table is %q, want a plain table", definition)
	}
}

func TestCosineDistanceOrdersNearerFirst(t *testing.T) {
	t.Parallel()

	query := []float64{1, 0, 0}
	cases := []struct {
		name   string
		vector []float64
		want   float64
	}{
		{"identical", []float64{1, 0, 0}, 0},
		{"scaled", []float64{5, 0, 0}, 0},
		{"orthogonal", []float64{0, 1, 0}, 1},
		{"opposite", []float64{-1, 0, 0}, 2},
		{"zero", []float64{0, 0, 0}, 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := cosineDistance(query, testCase.vector); math.Abs(got-testCase.want) > 1e-9 {
				t.Errorf("cosineDistance(%v, %v) = %v, want %v", query, testCase.vector, got, testCase.want)
			}
		})
	}
	// A mismatched width cannot happen while the provenance check holds; if it
	// ever did, the honest answer is "maximally far", never "closest".
	if got := cosineDistance(query, []float64{1, 0}); !math.IsInf(got, 1) {
		t.Errorf("cosineDistance over mismatched widths = %v, want +Inf", got)
	}
}

func TestVectorSerializationRoundTrips(t *testing.T) {
	t.Parallel()

	original := []float64{0, 1, -1, 0.5, 1e-7}
	restored, err := deserializeVector(serializeVector(original))
	if err != nil {
		t.Fatalf("deserializeVector() error = %v, want nil", err)
	}
	if len(restored) != len(original) {
		t.Fatalf("restored %d values, want %d", len(restored), len(original))
	}
	for index, value := range original {
		// float32 storage is a deliberate halving of the on-disk size, so the
		// tolerance is the format's, not a fudge factor.
		if math.Abs(restored[index]-value) > 1e-6 {
			t.Errorf("value %d = %v, want %v", index, restored[index], value)
		}
	}
	if _, err := deserializeVector([]byte{1, 2, 3}); err == nil {
		t.Error("deserializeVector() accepted a truncated blob, want a refusal")
	}
}

func TestIndexVectorValidationRejectsIncompleteGenerations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		want     string
		vectors  [][]float64
		expected int
	}{
		{"short batch", "mismatched index batch", [][]float64{{1}}, 2},
		{"no vectors", "empty vector", nil, 0},
		{"empty vector", "empty vector", [][]float64{{}}, 1},
		{"ragged", "inconsistent vector dimensions", [][]float64{{1}, {1, 2}}, 2},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			// A half-valid generation is worse than none: it would satisfy the
			// freshness rule by chunk count and then rank against the wrong shapes.
			_, err := validateVectors(testCase.vectors, testCase.expected)
			contains(t, unavailable(t, err), testCase.want, "the refusal")
		})
	}
}

func TestSearchRejectsQueryAndCorpusDimensionMismatch(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.configure = func(fake *ollama) {
			fake.embed = func(texts []string) ([][]float64, error) {
				// A query embedded at a different width than the corpus. Ranking
				// across the two would compare numbers that mean nothing to each other.
				if len(texts) == 1 {
					return [][]float64{{1, 0}}, nil
				}
				vectors := make([][]float64, 0, len(texts))
				for _, text := range texts {
					vectors = append(vectors, fakeVector(text, fakeDimensions))
				}
				return vectors, nil
			}
		}
	})
	_, err := fixture.memory.Retriever().SemanticSearch(t.Context(), "disk pressure", 3)
	contains(t, unavailable(t, err), "different dimensions", "the refusal")

	provenance, err := fixture.memory.Retriever().IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if provenance != nil {
		t.Errorf("IndexProvenance() = %+v, want nil — nothing complete was built", provenance)
	}
}

func TestAnEmptyQueryVectorIsRefused(t *testing.T) {
	t.Parallel()

	// The embeddings client already rejects an empty vector, so this is the
	// second gate rather than the only one. It is asserted because everything
	// downstream indexes into the slice.
	for _, batch := range []embeddingBatch{{}, {vectors: [][]float64{{}}}} {
		_, err := queryVector(batch)
		contains(t, unavailable(t, err), "empty query vector", "the refusal")
	}
	if _, err := queryVector(embeddingBatch{vectors: [][]float64{{1}}}); err != nil {
		t.Errorf("queryVector() error = %v, want nil for a usable vector", err)
	}
}

func TestSearchMapsAVectorStoreRebuildFailureToTheKeywordFallback(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	retriever.ensureIndex = func(context.Context, indexSpec, int) (int, error) {
		return 0, errors.Join(errVectorStore, errAssertion)
	}
	// A broken derived cache degrades to keyword retrieval; it does not break the
	// tool. That is the difference between a cache and a source of truth.
	_, err := retriever.SemanticSearch(t.Context(), "disk pressure", 2)
	contains(t, unavailable(t, err), "rebuild failed safely", "the refusal")
}

func TestSearchMapsAVectorStoreReadFailureToTheKeywordFallback(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	if _, err := retriever.IndexRunbooks(t.Context()); err != nil {
		t.Fatalf("IndexRunbooks() error = %v, want nil", err)
	}
	retriever.ensureIndex = func(_ context.Context, _ indexSpec, dimensions int) (int, error) {
		return dimensions, nil
	}
	retriever.indexReady = func(context.Context, *sql.Tx, indexSpec, int) (bool, error) {
		return false, errors.Join(errVectorStore, errAssertion)
	}
	_, err := retriever.SemanticSearch(t.Context(), "disk pressure", 2)
	contains(t, unavailable(t, err), "read failed safely", "the refusal")
}

func TestSearchRebuildsOnceWhenProvenanceChangesBetweenRebuildAndRead(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	if _, err := retriever.IndexRunbooks(t.Context()); err != nil {
		t.Fatalf("IndexRunbooks() error = %v, want nil", err)
	}

	// Another process switching configurations between this process's rebuild
	// and its read. The provenance is re-proved under the read transaction, so
	// the swap is caught rather than served.
	original := retriever.indexReady
	checks := 0
	retriever.indexReady = func(ctx context.Context, tx *sql.Tx, spec indexSpec, dimensions int) (bool, error) {
		checks++
		if checks == 2 {
			return false, nil
		}
		return original(ctx, tx, spec, dimensions)
	}
	results, err := retriever.SemanticSearch(t.Context(), "disk pressure", 2)
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v, want nil", err)
	}
	if len(results) == 0 {
		t.Fatal("SemanticSearch() returned nothing after recovering")
	}
	// Rebuild-check, read-check that fails, rebuild-check, read-check that holds.
	if checks != 4 {
		t.Errorf("provenance checks = %d, want 4", checks)
	}
	// And nothing was re-embedded: the generation on disk was already correct.
	if batches := fixture.ollama.indexBatches(); batches != 1 {
		t.Errorf("corpus embeddings = %d, want 1", batches)
	}
}

func TestSearchFailsClosedWhenProvenanceChangesTwice(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	retriever := fixture.memory.Retriever()
	if _, err := retriever.IndexRunbooks(t.Context()); err != nil {
		t.Fatalf("IndexRunbooks() error = %v, want nil", err)
	}
	rebuilds := 0
	retriever.ensureIndex = func(context.Context, indexSpec, int) (int, error) {
		rebuilds++
		return 0, nil
	}
	retriever.indexReady = func(context.Context, *sql.Tx, indexSpec, int) (bool, error) {
		return false, nil
	}

	// Configuration changing faster than it can be served. Answering from an
	// index whose provenance could not be confirmed is exactly what this whole
	// mechanism exists to prevent, so it fails closed.
	_, err := retriever.SemanticSearch(t.Context(), "disk pressure", 2)
	contains(t, unavailable(t, err), "provenance changed concurrently", "the refusal")
	if rebuilds != 3 {
		t.Errorf("rebuild attempts = %d, want 3 — one up front and one per read attempt", rebuilds)
	}
}

func TestSearchRunbooksUsesSemanticRetrievalWhenEnabled(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) { o.semanticRetrieval = true })
	result := searchRunbooks(t, fixture, "cache eviction storm memory pressure", nil)

	if result.Retrieval != semanticMode {
		t.Errorf("retrieval = %q, want %q", result.Retrieval, semanticMode)
	}
	if *result.Count == 0 {
		t.Error("count = 0, want the semantic index to return something")
	}
	// The distance is deliberately absent from the tool result: it is an artifact
	// of which mode happened to run, and means nothing in the keyword mode.
	for _, runbook := range result.Runbooks {
		if runbook.Slug == "" || runbook.Content == "" {
			t.Errorf("runbook = %+v, want a slug and its content", runbook)
		}
	}
}

func TestSearchRunbooksFallsBackToKeywordsWithALog(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.semanticRetrieval = true
		o.configure = func(fake *ollama) { fake.embedStatus = 503 }
	})
	result := searchRunbooks(t, fixture, "database connection pool exhausted", nil)

	// The fallback is visible in the result, not only in the log: a downstream
	// reader must be able to tell which retrieval answered without correlating.
	if result.Retrieval != keywordMode {
		t.Errorf("retrieval = %q, want %q", result.Retrieval, keywordMode)
	}
	if *result.Count == 0 {
		t.Error("count = 0, want the keyword scorer to answer")
	}
	contains(t, fixture.logs.String(), "falling back to keywords", "the log")
}

func TestTheOfflineDefaultNeverTouchesTheVectorStack(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.configure = func(fake *ollama) {
			fake.embed = func([]string) ([][]float64, error) {
				return nil, errors.New("the embedder must not be called when the flag is off")
			}
		}
	})
	result := searchRunbooks(t, fixture, "high latency after deploy", nil)

	if *result.Count == 0 {
		t.Error("count = 0, want the keyword scorer to answer")
	}
	// Not one request: with the toggle off the account-free path is genuinely
	// offline, which is what keeps the test gate deterministic and model-free.
	if batches := len(fixture.ollama.batches); batches != 0 {
		t.Errorf("embedding requests = %d, want none", batches)
	}
}

// search runs one semantic search that is expected to succeed.
func search(t *testing.T, retriever *Retriever, query string) []SemanticMatch {
	t.Helper()

	results, err := retriever.SemanticSearch(t.Context(), query, 2)
	if err != nil {
		t.Fatalf("SemanticSearch(%q) error = %v, want nil", query, err)
	}
	return results
}

// isReady asks the retriever whether the stored generation is current for the
// configuration now in force, through the same transaction the read path uses.
func isReady(t *testing.T, fixture *fixture, retriever *Retriever, dimensions int) bool {
	t.Helper()

	spec, err := retriever.spec(t.Context(), nil)
	if err != nil {
		t.Fatalf("spec() error = %v, want nil", err)
	}
	transaction, err := vectorsDB(t, fixture).BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin a read transaction: %v", err)
	}
	defer func() { _ = transaction.Rollback() }()

	ready, err := retriever.readyForSpec(t.Context(), transaction, spec, dimensions)
	if err != nil {
		t.Fatalf("readyForSpec() error = %v, want nil", err)
	}
	return ready
}

// unique returns the distinct values in order.
func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	distinct := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			distinct = append(distinct, value)
		}
	}
	return distinct
}

// distancesOf projects the ranked distances, for a readable failure message.
func distancesOf(matches []SemanticMatch) []float64 {
	distances := make([]float64, 0, len(matches))
	for _, match := range matches {
		distances = append(distances, match.Distance)
	}
	return distances
}

// embeddingsFixture builds a client against the fake endpoint, for the tests
// that exercise the transport rather than the index.
func embeddingsFixture(t *testing.T, model string, configure ...func(*ollama)) *embeddingsClient {
	t.Helper()

	fake := newOllama(t)
	for _, apply := range configure {
		apply(fake)
	}
	return newEmbeddingsClient(fake.server.Client(), fake.server.URL, model, 0)
}

func TestTheEmbeddingFailureIsActionable(t *testing.T) {
	t.Parallel()

	// Nothing is listening, and nothing ever leaves the test process.
	client := newEmbeddingsClient(nil, closedServer(t), embeddingModel, time.Second)
	_, err := client.Embed(t.Context(), []string{"hello"})

	message := unavailable(t, err)
	// "Embeddings unavailable" on its own sends a learner to read the source.
	contains(t, message, "ollama pull "+embeddingModel, "the failure")
	contains(t, message, "AGENT_SEMANTIC_RETRIEVAL", "the failure")
}

func TestTheEmbeddingRequestCarriesAColdStartDeadline(t *testing.T) {
	t.Parallel()

	fake := newOllama(t)
	fake.update(func(o *ollama) { o.embedDelay = 500 * time.Millisecond })
	client := newEmbeddingsClient(fake.server.Client(), fake.server.URL, embeddingModel, 20*time.Millisecond)

	_, err := client.Embed(t.Context(), []string{"hello"})
	unavailable(t, err)
	// A cold model can take minutes to load, so the deadline is separate from the
	// tool deadline — but it is a deadline, and an endpoint that never answers
	// must not hold a tool call open forever.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Embed() error = %v, want it to wrap the deadline", err)
	}
}

func TestEmbeddingClientRefusesCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var captured atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				captured.Add(1)
				http.Error(writer, "redirect target", http.StatusBadGateway)
			}))
			t.Cleanup(target.Close)
			source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Location", target.URL+"/api/embed")
				writer.WriteHeader(status)
			}))
			t.Cleanup(source.Close)

			client := newEmbeddingsClient(source.Client(), source.URL, embeddingModel, time.Second)
			if _, err := client.Embed(t.Context(), []string{"private runbook text"}); err == nil {
				t.Fatal("Embed() error = nil, want redirect refusal")
			}
			if got := captured.Load(); got != 0 {
				t.Fatalf("redirect target received %d request(s), want zero retrieval-text replays", got)
			}
		})
	}
}

func TestEmbeddingResponseValidationRejectsUnsafeVectors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"short batch", `{"embeddings":[]}`, "mismatched batch"},
		{"null batch", `{"embeddings":null}`, "mismatched batch"},
		{"empty vector", `{"embeddings":[[]]}`, "empty or invalid vector"},
		{"not a vector", `{"embeddings":["not-a-vector"]}`, "empty or invalid vector"},
		{"boolean value", `{"embeddings":[[true]]}`, "non-finite or non-numeric"},
		{"string value", `{"embeddings":[["one"]]}`, "non-finite or non-numeric"},
		{"missing field", `{}`, "ollama pull"},
		{"not an object", `[1,2,3]`, "ollama pull"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := embeddingsFixture(t, embeddingModel, func(o *ollama) { o.embedBody = testCase.body })
			// A vector store is durable state: a NaN or a string that reaches it
			// makes the generation unusable in a way that only surfaces at query time.
			_, err := client.Embed(t.Context(), []string{"hello"})
			contains(t, unavailable(t, err), testCase.want, "the refusal")
		})
	}

	// JSON has no infinity literal, so the non-finite branch cannot be reached
	// through the wire at all — the decoder rejects the number first. It is
	// asserted directly rather than left unproven.
	_, err := parseVector([]any{math.Inf(1)})
	contains(t, unavailable(t, err), "non-finite or non-numeric", "the refusal")
	_, err = parseVector([]any{math.NaN()})
	contains(t, unavailable(t, err), "non-finite or non-numeric", "the refusal")
}

func TestTheModelDigestComesFromTheResolvedOllamaArtifact(t *testing.T) {
	t.Parallel()

	client := embeddingsFixture(t, embeddingModel)
	if digest := client.ModelDigest(t.Context()); digest != "sha256:model-a" {
		t.Errorf("ModelDigest() = %q, want the configured model's digest", digest)
	}

	// An untagged name has to match Ollama's ":latest" spelling as well, or every
	// default configuration would report an unresolved digest.
	tagged := embeddingsFixture(t, embeddingModel+":latest")
	if digest := tagged.ModelDigest(t.Context()); digest != "sha256:model-a" {
		t.Errorf("ModelDigest() for an explicit tag = %q, want the same digest", digest)
	}
}

func TestTheModelDigestIsOptional(t *testing.T) {
	t.Parallel()

	cases := []struct {
		configure func(*ollama)
		name      string
		model     string
	}{
		{func(o *ollama) { o.tagsBody = `{"models":"invalid"}` }, "models is not a list", embeddingModel},
		{func(o *ollama) {
			o.tagsBody = `{"models":[null,{"name":"another-model:latest","digest":"sha256:other"}]}`
		}, "the model is not listed", embeddingModel},
		{func(o *ollama) { o.tagsBody = `{}` }, "no models field", embeddingModel},
		{func(o *ollama) {
			o.tagsBody = `{"models":[{"name":"` + embeddingModel + `:latest","digest":""}]}`
		}, "an exact tag with an empty digest", embeddingModel + ":latest"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			// An endpoint that is not Ollama still serves vectors. Refusing to work
			// without provenance metadata would make the feature unusable behind a
			// proxy; "" is a real provenance value meaning "unresolved", and a
			// generation built while unresolved is invalidated as soon as one resolves.
			client := embeddingsFixture(t, testCase.model, testCase.configure)
			if digest := client.ModelDigest(t.Context()); digest != "" {
				t.Errorf("ModelDigest() = %q, want it unresolved", digest)
			}
		})
	}

	t.Run("the endpoint is unreachable", func(t *testing.T) {
		t.Parallel()

		client := newEmbeddingsClient(nil, closedServer(t), embeddingModel, time.Second)
		if digest := client.ModelDigest(t.Context()); digest != "" {
			t.Errorf("ModelDigest() = %q, want it unresolved", digest)
		}
	})
}

func TestAnUnresolvedDigestIsStillProvenance(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) {
		o.configure = func(fake *ollama) { fake.tagsBody = `{}` }
	})
	retriever := fixture.memory.Retriever()
	search(t, retriever, "disk pressure")

	provenance, err := retriever.IndexProvenance(t.Context())
	if err != nil {
		t.Fatalf("IndexProvenance() error = %v, want nil", err)
	}
	if provenance == nil || provenance.ModelDigest != "" {
		t.Fatalf("provenance = %+v, want an empty digest recorded", provenance)
	}

	// And the moment a digest does resolve, the generation built without one is
	// stale — the unresolved marker is a value, not an exemption.
	fixture.ollama.update(func(o *ollama) { o.tagsBody = "" })
	search(t, retriever, "disk pressure")
	if batches := fixture.ollama.indexBatches(); batches != 2 {
		t.Errorf("corpus embeddings = %d, want 2 — resolving a digest is a provenance change", batches)
	}
}

func TestTheDigestChangeMessageNamesBothArtifacts(t *testing.T) {
	t.Parallel()

	// "( -> )" reads like a bug in the message rather than a fact about the
	// endpoint, so the empty marker is rendered as a word.
	contains(t, digestChanged("", "sha256:b"), "unresolved -> sha256:b", "the message")
	contains(t, digestChanged("sha256:a", ""), "sha256:a -> unresolved", "the message")
}
