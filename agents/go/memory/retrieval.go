package memory

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

// The provenance constants. Every one of them is an input to the freshness
// rule: change any and the whole generation is invalid.
const (
	// indexFormatVersion is the shape of the two tables below. Bump it when the
	// schema changes so an old generation is rebuilt rather than misread.
	//
	// It is 2 rather than 1 because version 1 stored chunks in a sqlite-vec
	// virtual table. Its metadata row is otherwise identical, so a state
	// directory still carrying a version 1 index would pass every provenance
	// check and then fail to read a table this binary cannot open — which is
	// precisely the misread this field exists to prevent.
	indexFormatVersion = 2
	// chunkerVersion names the chunking algorithm. Two corpora that hash the same
	// but were split differently are different generations.
	chunkerVersion = "markdown-h2-h3-v1"

	metadataTable = "runbook_index_metadata"
	chunksTable   = "runbook_chunks"
)

const (
	// readAttempts bounds the retry when another process swaps the index out
	// between the rebuild and the read. Two is enough to survive one swap; a
	// third would mean the configuration is changing faster than it can be
	// served, and failing closed is the honest answer to that.
	readAttempts = 2

	// overFetchFactor widens the nearest-neighbor scan before per-slug
	// deduplication, so a runbook whose chunks take several of the top slots
	// cannot starve the result of other runbooks.
	overFetchFactor = 3

	// The busy timeout for the vector store is the embedding deadline plus a
	// margin, floored: the writer holds the lock across an embedding call, so a
	// reader that gave up sooner would fail exactly when a cold model is loading.
	minVectorBusyTimeout    = 5 * time.Second
	vectorBusyTimeoutMargin = 5 * time.Second
)

// headingPattern splits a runbook at its H2 and H3 markers, and only the marker
// plus one whitespace character is consumed — the heading text stays with the
// section it introduces, which is what makes a chunk self-describing.
var headingPattern = regexp.MustCompile(`(?m)^#{2,3}\s`)

// ErrEmptyCorpus refuses to index a knowledge base with nothing in it.
//
// It is not an [EmbeddingUnavailableError]: falling back to the keyword scorer
// would produce an empty result from the same empty corpus while hiding the
// reason. An operator has to see this one.
var ErrEmptyCorpus = errors.New("no runbook chunks to index")

// ErrInvalidLimit refuses a non-positive result count. It is deliberately not
// an embedding failure: a caller asking for zero results has a bug, and keyword
// retrieval would not fix it.
var ErrInvalidLimit = errors.New("invalid semantic search limit")

// errVectorStore marks every failure of the vector store itself.
//
// It is unexported because it never reaches a caller: [Retriever.SemanticSearch]
// converts it into an [EmbeddingUnavailableError] whose message says the index
// failed *safely*, which is the whole point — a broken derived cache degrades to
// keyword retrieval instead of breaking the tool.
var errVectorStore = errors.New("vector store operation failed")

// Provenance is the reproducibility metadata of one complete index generation.
//
// It is what a stale generation is detected by, and what an operator reads to
// answer "which corpus, which model, which chunker produced these vectors".
type Provenance struct {
	CorpusSHA256   string
	EmbeddingModel string
	ModelDigest    string
	ChunkerVersion string
	BuiltAt        string
	FormatVersion  int
	Dimensions     int
	ChunkCount     int
}

// SemanticMatch is one ranked runbook, with the cosine distance that ranked it.
type SemanticMatch struct {
	Slug     string
	Content  string
	Distance float64
}

// runbookChunk is one indexed passage: the runbook it came from, and the text
// that was embedded.
type runbookChunk struct {
	slug string
	text string
}

// indexSpec is the complete provenance of the generation the current
// configuration and corpus call for. An index whose stored metadata does not
// match one of these fields exactly is stale by definition.
type indexSpec struct {
	corpusSHA256   string
	embeddingModel string
	modelDigest    string
	chunkerVersion string
	chunks         []runbookChunk
}

// embeddingBatch is a set of vectors bound to the model artifact that was
// observed, unchanged, on both sides of the request that produced them.
type embeddingBatch struct {
	modelDigest string
	vectors     [][]float64
}

// retrieverConfig is what [newRetriever] needs. It is unexported because
// [Config] is the only supported way to build one.
type retrieverConfig struct {
	store      Store
	httpClient *http.Client
	stateDir   string
	url        string
	model      string
	timeout    time.Duration
}

// Retriever is the provenance-bound semantic index over the runbook library.
//
// # The index is a cache, and its provenance is the cache key
//
// Vectors are only ever queried when the stored metadata matches the current
// corpus hash, embedding model name, model artifact digest, chunker version,
// chunk count and dimensionality — all six. Anything else means the vectors
// were produced by a different configuration, and answering from them would be
// a silent correctness failure that looks exactly like a working search.
//
// # There is no vector extension
//
// The corpus is well under a hundred vectors at a few hundred dimensions, so a
// brute-force cosine scan over the whole table is microseconds and the entire
// index is a couple of hundred kilobytes. A vector extension at this scale
// would buy nothing, and the available Go binding is cgo, which the course's
// static, distroless binary rules out. Knowing the scale at which a vector
// database earns its keep is the lesson.
type Retriever struct {
	store      Store
	embeddings *embeddingsClient

	// ensureIndex and indexReady are the two seams the concurrency and failure
	// tests drive. They are fields because the behavior they stand in for —
	// another process swapping the index out between this process's rebuild and
	// its read, or the store failing mid-read — cannot be provoked from outside.
	// Production wires them to the methods below and never reassigns them.
	ensureIndex func(ctx context.Context, spec indexSpec, dimensions int) (int, error)
	indexReady  func(ctx context.Context, tx *sql.Tx, spec indexSpec, dimensions int) (bool, error)

	stateDir string
	// chunkerVersion is a field rather than a constant so a test can prove that
	// changing the chunker invalidates the generation exactly once.
	chunkerVersion string

	busyTimeout time.Duration
}

func newRetriever(cfg retrieverConfig) *Retriever {
	busyTimeout := max(minVectorBusyTimeout, cfg.timeout+vectorBusyTimeoutMargin)
	built := &Retriever{
		store:          cfg.store,
		embeddings:     newEmbeddingsClient(cfg.httpClient, cfg.url, cfg.model, cfg.timeout),
		stateDir:       cfg.stateDir,
		chunkerVersion: chunkerVersion,
		busyTimeout:    busyTimeout,
	}
	built.ensureIndex = built.buildIndex
	built.indexReady = built.readyForSpec
	return built
}

// IndexRunbooks builds, or reuses, the complete vector generation for the
// current provenance and returns how many chunks it covers.
func (r *Retriever) IndexRunbooks(ctx context.Context) (int, error) {
	digest := r.embeddings.ModelDigest(ctx)
	spec, err := r.spec(ctx, &digest)
	if err != nil {
		return 0, err
	}
	return r.ensureIndex(ctx, spec, 0)
}

// IndexProvenance returns the metadata of the last complete generation, or nil.
//
// Nil is the honest answer both before the first build and after an interrupted
// one: the metadata row is written last, inside the same transaction as the
// vectors, so its absence means no generation was ever completed.
func (r *Retriever) IndexProvenance(ctx context.Context) (provenance *Provenance, err error) {
	db, err := r.open(ctx, false)
	if err != nil {
		return nil, err
	}
	defer func() { err = closeWith(db, err) }()
	return readMetadata(ctx, db), nil
}

// SemanticSearch ranks runbooks by embedding similarity, and only after proving
// the stored vectors match the current provenance.
//
// The sequence is deliberate. The query is embedded first, which pins the model
// artifact digest that the corpus generation must also have been built with; a
// model swapped underneath either half is caught rather than averaged over.
func (r *Retriever) SemanticSearch(ctx context.Context, query string, limit int) ([]SemanticMatch, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: must be positive, got %d", ErrInvalidLimit, limit)
	}
	queryBatch, err := r.embedVerified(ctx, []string{query}, nil)
	if err != nil {
		return nil, err
	}
	vector, err := queryVector(queryBatch)
	if err != nil {
		return nil, err
	}
	// The corpus generation is pinned to the digest observed around the *query*
	// embedding, so both halves of the comparison provably come from one artifact.
	spec, err := r.spec(ctx, &queryBatch.modelDigest)
	if err != nil {
		return nil, err
	}
	if _, err := r.ensureIndex(ctx, spec, len(vector)); err != nil {
		return nil, rebuildFailure(err)
	}

	var ranked []scoredChunk
	served := false
	for range readAttempts {
		rows, ready, err := r.readRanked(ctx, spec, vector, limit*overFetchFactor)
		if err != nil {
			return nil, readFailure(err)
		}
		if ready {
			ranked, served = rows, true
			break
		}
		// A different process switched configurations between the rebuild above and
		// this read. Rebuild for the spec we are serving and look again.
		if _, err := r.ensureIndex(ctx, spec, len(vector)); err != nil {
			return nil, readFailure(err)
		}
	}
	if !served {
		// Fail closed. Answering from an index whose provenance we could not
		// confirm is exactly the failure this whole mechanism exists to prevent.
		return nil, embeddingUnavailable(
			"Semantic index provenance changed concurrently; retry or use keyword retrieval.", nil,
		)
	}
	return r.matches(ranked, limit)
}

// rebuildFailure and readFailure convert a vector-store failure into the
// keyword fallback, and let every other failure through untouched.
//
// The distinction matters: a broken derived cache is recoverable — the keyword
// scorer answers the same question offline — while a broken corpus or a model
// swap is a condition an operator has to see.
func rebuildFailure(err error) error {
	if errors.Is(err, errVectorStore) {
		return embeddingUnavailable(
			"Semantic index rebuild failed safely; using keyword retrieval until a complete generation is available.",
			err,
		)
	}
	return err
}

func readFailure(err error) error {
	if errors.Is(err, errVectorStore) {
		return embeddingUnavailable(
			"Semantic index read failed safely; using keyword retrieval until the index can be rebuilt.", err,
		)
	}
	return err
}

// queryVector takes the single vector a query embedding must have produced.
//
// [embeddingsClient.Embed] already rejects an empty or short batch, so this is a
// second gate rather than the only one; it is here because everything below
// indexes into the slice, and a future embedder that loosened that contract
// would otherwise panic instead of degrading.
func queryVector(batch embeddingBatch) ([]float64, error) {
	if len(batch.vectors) == 0 || len(batch.vectors[0]) == 0 {
		return nil, embeddingUnavailable("Embeddings endpoint returned an empty query vector.", nil)
	}
	return batch.vectors[0], nil
}

// matches turns the ranked chunks into one result per runbook.
//
// A runbook is represented by its best-matching chunk, and the content returned
// is the raw file rather than the normalized text that was embedded: the
// normalization exists to make the hash stable, not to change what a human or a
// model reads.
func (r *Retriever) matches(ranked []scoredChunk, limit int) ([]SemanticMatch, error) {
	best := make(map[string]float64, len(ranked))
	for _, chunk := range ranked {
		if current, seen := best[chunk.slug]; !seen || chunk.distance < current {
			best[chunk.slug] = chunk.distance
		}
	}
	slugs := make([]string, 0, len(best))
	for slug := range best {
		slugs = append(slugs, slug)
	}
	// Ascending distance, then ascending slug. The slug tie-break is what keeps
	// two equally close runbooks in a reproducible order across runs.
	slices.SortFunc(slugs, func(a, b string) int {
		if order := cmp.Compare(best[a], best[b]); order != 0 {
			return order
		}
		return cmp.Compare(a, b)
	})

	results := make([]SemanticMatch, 0, min(limit, len(slugs)))
	for _, slug := range slugs[:min(limit, len(slugs))] {
		content, _, err := r.store.ReadRunbook(slug)
		if err != nil {
			return nil, fmt.Errorf("read runbook %s: %w", slug, err)
		}
		results = append(results, SemanticMatch{
			Slug:    slug,
			Content: content,
			// Rounded because a distance is reported to a human, and sixteen
			// significant digits of float noise makes two identical rankings
			// look different.
			Distance: math.Round(best[slug]*1e4) / 1e4,
		})
	}
	return results, nil
}

// spec computes the provenance the current corpus and configuration call for.
//
// modelDigest is a pointer because "resolve it now" and "use exactly this one"
// are different requests: a search pins the digest observed around its query
// embedding, while a standalone index build resolves its own.
func (r *Retriever) spec(ctx context.Context, modelDigest *string) (indexSpec, error) {
	slugs, err := r.store.ListRunbookSlugs()
	if err != nil {
		return indexSpec{}, fmt.Errorf("list the runbook knowledge base: %w", err)
	}
	corpus := make([][2]string, 0, len(slugs))
	var chunks []runbookChunk
	for _, slug := range slugs {
		raw, _, err := r.store.ReadRunbook(slug)
		if err != nil {
			return indexSpec{}, fmt.Errorf("read runbook %s: %w", slug, err)
		}
		content := normalizeRunbook(raw)
		corpus = append(corpus, [2]string{slug, content})
		for _, text := range ChunkRunbook(slug, content) {
			chunks = append(chunks, runbookChunk{slug: slug, text: text})
		}
	}
	if len(chunks) == 0 {
		return indexSpec{}, fmt.Errorf(
			"%w: %s is empty. Seed the runbook library before enabling semantic retrieval",
			ErrEmptyCorpus, r.store.RunbooksDir(),
		)
	}
	digest := ""
	if modelDigest != nil {
		digest = *modelDigest
	} else {
		digest = r.embeddings.ModelDigest(ctx)
	}
	return indexSpec{
		corpusSHA256:   hashCorpus(corpus),
		embeddingModel: r.embeddings.model,
		modelDigest:    digest,
		chunkerVersion: r.chunkerVersion,
		chunks:         chunks,
	}, nil
}

// embedVerified embeds once and binds the vectors to a stable model artifact.
//
// The digest is sampled before and after the request. Ollama can be told to
// pull a new build of a tag while a process is running, and a batch that
// straddled that swap would mix two vector spaces in one index — the kind of
// corruption that produces plausible, wrong answers forever after.
func (r *Retriever) embedVerified(
	ctx context.Context, texts []string, expectedDigest *string,
) (embeddingBatch, error) {
	before := r.embeddings.ModelDigest(ctx)
	if expectedDigest != nil && before != *expectedDigest {
		return embeddingBatch{}, embeddingUnavailable(digestChanged(*expectedDigest, before), nil)
	}
	vectors, err := r.embeddings.Embed(ctx, texts)
	if err != nil {
		return embeddingBatch{}, err
	}
	after := r.embeddings.ModelDigest(ctx)
	if after != before {
		return embeddingBatch{}, embeddingUnavailable(digestChanged(before, after), nil)
	}
	return embeddingBatch{vectors: vectors, modelDigest: after}, nil
}

// digestChanged is the one sentence both artifact-stability checks report.
func digestChanged(from, to string) string {
	return fmt.Sprintf(
		"The embedding model artifact changed while vectors were generated (%s -> %s); retry with a stable model.",
		orUnresolved(from), orUnresolved(to),
	)
}

// orUnresolved renders the empty digest marker as a word, because "( -> )"
// reads like a bug in the message rather than a fact about the endpoint.
func orUnresolved(digest string) string {
	if digest == "" {
		return "unresolved"
	}
	return digest
}

// buildIndex builds exactly one complete generation, or reuses the one already
// on disk.
//
// The write lock is taken at BEGIN and held across the embedding request. That
// is the cross-process serialization point: four workers starting at once
// produce one generation, not four, and the three that lose the race find the
// index ready and embed nothing.
func (r *Retriever) buildIndex(ctx context.Context, spec indexSpec, dimensions int) (count int, err error) {
	db, err := r.open(ctx, true)
	if err != nil {
		return 0, err
	}
	defer func() { err = closeWith(db, err) }()

	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: begin the index transaction: %w", errVectorStore, err)
	}
	ready, err := r.indexReady(ctx, transaction, spec, dimensions)
	if err != nil {
		return 0, rollback(transaction, err)
	}
	if ready {
		if commitErr := transaction.Commit(); commitErr != nil {
			return 0, fmt.Errorf("%w: release the index transaction: %w", errVectorStore, commitErr)
		}
		return len(spec.chunks), nil
	}

	texts := make([]string, 0, len(spec.chunks))
	for _, chunk := range spec.chunks {
		texts = append(texts, chunk.text)
	}
	batch, err := r.embedVerified(ctx, texts, &spec.modelDigest)
	if err != nil {
		return 0, rollback(transaction, err)
	}
	built, err := validateVectors(batch.vectors, len(spec.chunks))
	if err != nil {
		return 0, rollback(transaction, err)
	}
	if dimensions != 0 && built != dimensions {
		return 0, rollback(transaction, embeddingUnavailable(
			"Query and corpus embeddings have different dimensions; refresh the embedding model and retry.", nil,
		))
	}
	if writeErr := writeGeneration(ctx, transaction, spec, batch.vectors, built); writeErr != nil {
		return 0, rollback(transaction, writeErr)
	}
	// The drop, the rebuild and the metadata row commit together, so a failure
	// anywhere above leaves the *previous* generation intact on disk — intact,
	// and still correctly reported as stale for the spec that failed to build.
	if commitErr := transaction.Commit(); commitErr != nil {
		return 0, fmt.Errorf("%w: commit the index generation: %w", errVectorStore, commitErr)
	}
	return len(spec.chunks), nil
}

// writeGeneration replaces the whole index inside one transaction.
func writeGeneration(
	ctx context.Context, transaction *sql.Tx, spec indexSpec, vectors [][]float64, dimensions int,
) error {
	schema := []string{
		"DROP TABLE IF EXISTS " + chunksTable,
		"DROP TABLE IF EXISTS " + metadataTable,
		`CREATE TABLE ` + metadataTable + ` (
			singleton       INTEGER PRIMARY KEY CHECK (singleton = 1),
			format_version  INTEGER NOT NULL,
			corpus_sha256   TEXT NOT NULL,
			embedding_model TEXT NOT NULL,
			model_digest    TEXT NOT NULL,
			dimensions      INTEGER NOT NULL CHECK (dimensions > 0),
			chunker_version TEXT NOT NULL,
			built_at        TEXT NOT NULL,
			chunk_count     INTEGER NOT NULL CHECK (chunk_count > 0)
		)`,
		`CREATE TABLE ` + chunksTable + ` (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			slug      TEXT NOT NULL,
			chunk     TEXT NOT NULL,
			embedding BLOB NOT NULL
		)`,
	}
	for _, statement := range schema {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("%w: rebuild the index tables: %w", errVectorStore, err)
		}
	}
	insert, err := transaction.PrepareContext(ctx,
		"INSERT INTO "+chunksTable+" (slug, chunk, embedding) VALUES (?, ?, ?)")
	if err != nil {
		return fmt.Errorf("%w: prepare the chunk insert: %w", errVectorStore, err)
	}
	defer func() { _ = insert.Close() }()
	for index, chunk := range spec.chunks {
		if _, err := insert.ExecContext(ctx, chunk.slug, chunk.text, serializeVector(vectors[index])); err != nil {
			return fmt.Errorf("%w: insert chunk %d: %w", errVectorStore, index, err)
		}
	}
	if _, err := transaction.ExecContext(ctx,
		`INSERT INTO `+metadataTable+`
			(singleton, format_version, corpus_sha256, embedding_model,
			 model_digest, dimensions, chunker_version, built_at, chunk_count)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		indexFormatVersion, spec.corpusSHA256, spec.embeddingModel, spec.modelDigest,
		dimensions, spec.chunkerVersion, time.Now().UTC().Format(timestampLayout), len(spec.chunks),
	); err != nil {
		return fmt.Errorf("%w: record the index provenance: %w", errVectorStore, err)
	}
	return nil
}

// scoredChunk is one chunk with its distance from the query vector.
type scoredChunk struct {
	slug     string
	distance float64
}

// readRanked scans the index for the k nearest chunks, inside a transaction
// that first re-proves the generation is the one being asked for.
//
// The re-check is not paranoia about this process: the provenance is verified
// under the read transaction because another process can switch configurations
// between the rebuild and the read, and the whole guarantee is that a stale
// generation is never queried — not that it is rarely queried.
func (r *Retriever) readRanked(
	ctx context.Context, spec indexSpec, vector []float64, k int,
) (ranked []scoredChunk, ready bool, err error) {
	db, err := r.open(ctx, false)
	if err != nil {
		return nil, false, err
	}
	defer func() { err = closeWith(db, err) }()

	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%w: begin the index read: %w", errVectorStore, err)
	}
	ready, err = r.indexReady(ctx, transaction, spec, len(vector))
	if err != nil {
		return nil, false, rollback(transaction, err)
	}
	if !ready {
		return nil, false, rollback(transaction, nil)
	}

	scored, err := scanScoredChunks(ctx, transaction, vector)
	if err != nil {
		return nil, false, rollback(transaction, err)
	}
	if commitErr := transaction.Commit(); commitErr != nil {
		return nil, false, fmt.Errorf("%w: release the index read: %w", errVectorStore, commitErr)
	}

	// Nearest first, with the slug as a deterministic tie-break so an exact
	// distance tie does not depend on the order SQLite happened to return rows in.
	slices.SortFunc(scored, func(a, b scoredChunk) int {
		if order := cmp.Compare(a.distance, b.distance); order != 0 {
			return order
		}
		return cmp.Compare(a.slug, b.slug)
	})
	return scored[:min(k, len(scored))], true, nil
}

// scanScoredChunks scores every stored chunk against the query vector.
//
// This is the brute-force scan. At fewer than a hundred vectors it costs a few
// microseconds, which is three orders of magnitude below the point where an
// approximate nearest-neighbor index starts to pay for itself.
func scanScoredChunks(ctx context.Context, transaction *sql.Tx, vector []float64) ([]scoredChunk, error) {
	rows, err := transaction.QueryContext(ctx, "SELECT slug, embedding FROM "+chunksTable)
	if err != nil {
		return nil, fmt.Errorf("%w: read the chunks: %w", errVectorStore, err)
	}
	defer func() { _ = rows.Close() }()

	var scored []scoredChunk
	for rows.Next() {
		var (
			slug  string
			blob  []byte
			entry scoredChunk
		)
		if err := rows.Scan(&slug, &blob); err != nil {
			return nil, fmt.Errorf("%w: scan a chunk: %w", errVectorStore, err)
		}
		stored, err := deserializeVector(blob)
		if err != nil {
			return nil, err
		}
		entry.slug = slug
		entry.distance = cosineDistance(vector, stored)
		scored = append(scored, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read the chunks: %w", errVectorStore, err)
	}
	return scored, nil
}

// readyForSpec reports whether the stored generation is exactly the one spec
// describes.
//
// Every provenance input is compared, and any mismatch is "not ready" rather
// than an error: a stale index is a normal, expected state that the caller
// resolves by rebuilding.
func (r *Retriever) readyForSpec(
	ctx context.Context, transaction *sql.Tx, spec indexSpec, dimensions int,
) (bool, error) {
	var present int
	err := transaction.QueryRowContext(ctx,
		"SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", chunksTable,
	).Scan(&present)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("%w: look for the chunk table: %w", errVectorStore, err)
	}
	provenance := readMetadata(ctx, transaction)
	if provenance == nil {
		return false, nil
	}
	if provenance.FormatVersion != indexFormatVersion ||
		provenance.CorpusSHA256 != spec.corpusSHA256 ||
		provenance.EmbeddingModel != spec.embeddingModel ||
		provenance.ModelDigest != spec.modelDigest ||
		provenance.ChunkerVersion != spec.chunkerVersion ||
		provenance.ChunkCount != len(spec.chunks) {
		return false, nil
	}
	// Zero means "no dimensional constraint", which is what a standalone index
	// build asks for: it has no query vector to be compatible with yet.
	return dimensions == 0 || provenance.Dimensions == dimensions, nil
}

// querier is the read surface shared by *sql.DB and *sql.Tx, so the metadata
// read runs either standalone or inside the transaction that is about to
// decide whether to rebuild.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readMetadata returns the stored provenance, or nil.
//
// Nil covers every reason there is no usable metadata — the table does not
// exist yet, the row was never written, the read failed — because all three
// mean the same thing to every caller: there is no generation to trust. The
// caller's next step is to rebuild, and a rebuild reports its own failure.
func readMetadata(ctx context.Context, q querier) *Provenance {
	var provenance Provenance
	err := q.QueryRowContext(ctx, `
		SELECT format_version, corpus_sha256, embedding_model, model_digest,
		       dimensions, chunker_version, built_at, chunk_count
		FROM `+metadataTable+`
		WHERE singleton = 1
	`).Scan(
		&provenance.FormatVersion, &provenance.CorpusSHA256, &provenance.EmbeddingModel,
		&provenance.ModelDigest, &provenance.Dimensions, &provenance.ChunkerVersion,
		&provenance.BuiltAt, &provenance.ChunkCount,
	)
	if err != nil {
		return nil
	}
	return &provenance
}

// validateVectors checks one whole batch before any of it is stored.
//
// A half-valid generation is worse than none: it would satisfy the freshness
// rule by chunk count and then rank against vectors of the wrong shape.
func validateVectors(vectors [][]float64, expected int) (int, error) {
	if len(vectors) != expected {
		return 0, embeddingUnavailable("Embeddings endpoint returned a mismatched index batch.", nil)
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return 0, embeddingUnavailable("Embeddings endpoint returned an empty vector.", nil)
	}
	dimensions := len(vectors[0])
	for _, vector := range vectors {
		if len(vector) != dimensions {
			return 0, embeddingUnavailable("Embeddings endpoint returned inconsistent vector dimensions.", nil)
		}
	}
	return dimensions, nil
}

// open prepares the vector store.
func (r *Retriever) open(ctx context.Context, immediate bool) (*sql.DB, error) {
	path, err := stateDatabasePath(r.stateDir, vectorsDatabaseName)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errVectorStore, err)
	}
	db, err := openStateDatabase(ctx, path, r.busyTimeout, immediate)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errVectorStore, err)
	}
	return db, nil
}

// serializeVector packs a vector as little-endian IEEE-754 float32, one value
// after another.
//
// float32 rather than float64 halves the index on disk — a 768-dimension vector
// is 3 KB rather than 6 — and costs nothing that matters here, because the
// distance it feeds is rounded to four decimals before anyone sees it.
func serializeVector(vector []float64) []byte {
	blob := make([]byte, 4*len(vector))
	for index, value := range vector {
		binary.LittleEndian.PutUint32(blob[4*index:], math.Float32bits(float32(value)))
	}
	return blob
}

// deserializeVector unpacks what [serializeVector] wrote.
func deserializeVector(blob []byte) ([]float64, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("%w: a stored vector is %d bytes, which is not a whole number of float32 values",
			errVectorStore, len(blob))
	}
	vector := make([]float64, len(blob)/4)
	for index := range vector {
		vector[index] = float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[4*index:])))
	}
	return vector, nil
}

// cosineDistance is 1 minus the cosine similarity, so nearer is smaller and the
// ordering reads the way a distance should.
func cosineDistance(query, stored []float64) float64 {
	if len(query) != len(stored) || len(query) == 0 {
		// Unreachable while the provenance check holds — it compares
		// dimensionality — so this is the honest answer for the case that would
		// mean the check had failed: maximally far, never returned first.
		return math.Inf(1)
	}
	var dot, queryNorm, storedNorm float64
	for index, value := range query {
		dot += value * stored[index]
		queryNorm += value * value
		storedNorm += stored[index] * stored[index]
	}
	if queryNorm == 0 || storedNorm == 0 {
		// A zero vector has no direction, so it is neither near nor far. One is
		// the distance of orthogonality, which is the neutral answer.
		return 1
	}
	return 1 - dot/(math.Sqrt(queryNorm)*math.Sqrt(storedNorm))
}

// normalizeRunbook makes the corpus hash independent of how a file happened to
// be written: platform line endings and trailing whitespace change bytes
// without changing meaning, and a hash that moved for either would rebuild the
// whole index after a checkout on another operating system.
func normalizeRunbook(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
}

// ChunkRunbook splits one runbook into heading-bounded passages, each prefixed
// with its slug.
//
// The slug prefix is what lets a chunk carry its own provenance into the vector
// space: two runbooks that describe similar symptoms stay distinguishable, and
// a retrieved chunk names the document it came from without a join.
//
// It never returns nothing. A runbook with no headings is one chunk, and an
// empty one is one empty chunk, because a corpus entry that silently vanished
// would change the chunk count without changing the corpus hash.
func ChunkRunbook(slug, content string) []string {
	pieces := headingPattern.Split(content, -1)
	chunks := make([]string, 0, len(pieces))
	for _, piece := range pieces {
		if trimmed := strings.TrimSpace(piece); trimmed != "" {
			chunks = append(chunks, slug+": "+trimmed)
		}
	}
	if len(chunks) == 0 {
		return []string{slug + ": " + strings.TrimSpace(content)}
	}
	return chunks
}

// hashCorpus fingerprints the whole normalized corpus.
//
// The encoding is JSON — a slug-sorted array of [slug, content] pairs with no
// whitespace and every non-ASCII character escaped. It is spelled out by hand
// because encoding/json cannot produce it: encoding/json escapes the HTML
// characters this encoding leaves alone and leaves the non-ASCII characters
// this encoding escapes, and the runbooks contain em dashes. The hash decides
// whether a whole index generation is still fresh, so its bytes are pinned.
func hashCorpus(corpus [][2]string) string {
	encoded := make([]byte, 0, 4096)
	encoded = append(encoded, '[')
	for index, entry := range corpus {
		if index > 0 {
			encoded = append(encoded, ',')
		}
		encoded = append(encoded, '[')
		encoded = appendJSONString(encoded, entry[0])
		encoded = append(encoded, ',')
		encoded = appendJSONString(encoded, entry[1])
		encoded = append(encoded, ']')
	}
	encoded = append(encoded, ']')
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// appendJSONString writes one JSON string literal, escaping every non-ASCII
// rune.
//
// Invalid UTF-8 is the one input it cannot fingerprint faithfully: ranging a
// string yields U+FFFD for each bad byte, so a corrupt runbook hashes as though
// it held replacement characters. A runbook that is not valid UTF-8 is a broken
// runbook either way.
func appendJSONString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for _, character := range value {
		switch character {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			switch {
			case character >= 0x20 && character <= 0x7e:
				dst = append(dst, byte(character))
			case character > 0xFFFF:
				// A \u escape carries sixteen bits, so a rune outside the
				// basic plane needs the surrogate pair JSON defines for it.
				high, low := utf16.EncodeRune(character)
				dst = appendUnicodeEscape(dst, high)
				dst = appendUnicodeEscape(dst, low)
			default:
				dst = appendUnicodeEscape(dst, character)
			}
		}
	}
	return append(dst, '"')
}

// appendUnicodeEscape writes \uXXXX with lowercase hex. The case is part of the
// bytes the corpus hash covers, so it is not free to change.
func appendUnicodeEscape(dst []byte, character rune) []byte {
	const digits = "0123456789abcdef"
	return append(dst, '\\', 'u',
		digits[(character>>12)&0xF], digits[(character>>8)&0xF],
		digits[(character>>4)&0xF], digits[character&0xF])
}
