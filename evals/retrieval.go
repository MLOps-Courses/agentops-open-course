package evals

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	RetrievalArtifactSchemaVersion = 3
	retrievalLimit                 = 3
)

const corpusDigestSize = sha256.Size * 2

var (
	retrievalIncidentRowPattern = regexp.MustCompile(
		`(?ms)^\s*\(\s*'([^']+)'\s*,\s*'[^']+'\s*,\s*'([^']+)'\s*,\s*'[^']+'\s*,\s*'[^']+'\s*,\s*'([^']+)'\s*,\s*'[^']+'\s*,\s*(?:NULL|'[^']*')\s*,\s*'([^']+)'\s*\)\s*,?`,
	)
	retrievalIncidentStartPattern = regexp.MustCompile(`(?m)^\s*\(\s*'INC-\d{3}'\s*,`)
)

type RetrievalMode string

const (
	RetrievalKeyword  RetrievalMode = "keyword"
	RetrievalSemantic RetrievalMode = "semantic"
)

type RetrievalCase struct {
	ID              string
	Query           string
	ExpectedRunbook string
}

type RetrievalSearchResult struct {
	Mode  RetrievalMode
	Slugs []string
}

type RetrievalClient interface {
	SearchRunbooks(context.Context, string, int) (RetrievalSearchResult, error)
}

// RetrievalRuntime identifies one disposable runtime. ID is never persisted;
// it only lets the harness prove that the two retrieval modes did not share
// derived state or a process.
type RetrievalRuntime struct {
	Client RetrievalClient
	Close  func() error
	ID     string
}

type RetrievalRuntimeFactory func(context.Context, RetrievalMode) (RetrievalRuntime, error)

type RetrievalRunConfig struct {
	Factory              RetrievalRuntimeFactory
	DataDir              string
	EmbeddingModel       string
	EmbeddingModelDigest string
	Source               SourceEvidence
}

type RetrievalRates struct {
	HitRateAt1 float64 `json:"hit_rate_at_1"`
	HitRateAt3 float64 `json:"hit_rate_at_3"`
}

// RetrievalArtifact is content-free evidence. It deliberately contains no
// query, result slug, runbook body, endpoint, tool payload, or provider error.
type RetrievalArtifact struct {
	CorpusDigest         string         `json:"corpus_digest"`
	EmbeddingModel       string         `json:"embedding_model"`
	EmbeddingModelDigest string         `json:"embedding_model_digest,omitzero"`
	Source               SourceEvidence `json:"source"`
	Keyword              RetrievalRates `json:"keyword"`
	Semantic             RetrievalRates `json:"semantic"`
	SchemaVersion        int            `json:"schema_version"`
	CaseCount            int            `json:"case_count"`
}

// LoadRetrievalCases derives the same query/runbook pairs as the former Python
// evaluation: incident title plus summary is the query, and the incident's own
// runbook field is ground truth.
func LoadRetrievalCases(dataDir string) ([]RetrievalCase, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("retrieval cases need an immutable data directory")
	}
	seedPath := filepath.Join(dataDir, "sql", "seed.sql")
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		return nil, fmt.Errorf("read retrieval seed %s: %w", seedPath, err)
	}
	incidentsBlock, err := sqlInsertBlock(string(seed), "incidents")
	if err != nil {
		return nil, err
	}
	matches := retrievalIncidentRowPattern.FindAllStringSubmatch(incidentsBlock, -1)
	if len(matches) == 0 {
		return nil, errors.New("retrieval seed has no parseable incident rows")
	}
	rowCount := len(retrievalIncidentStartPattern.FindAllStringIndex(incidentsBlock, -1))
	if len(matches) != rowCount {
		return nil, fmt.Errorf("retrieval seed parsed %d of %d incident rows", len(matches), rowCount)
	}
	knownRunbooks, err := slugsWithExtension(filepath.Join(dataDir, "runbooks"), ".md")
	if err != nil {
		return nil, err
	}

	cases := make([]RetrievalCase, 0, len(matches))
	seenIDs := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		id, title, runbook, summary := match[1], match[2], match[3], match[4]
		if !incidentIDPattern.MatchString(id) {
			return nil, fmt.Errorf("retrieval seed incident id %q is invalid", id)
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, fmt.Errorf("retrieval seed repeats incident %q", id)
		}
		if strings.TrimSpace(title) == "" || strings.TrimSpace(summary) == "" {
			return nil, fmt.Errorf("retrieval seed incident %q needs a title and summary", id)
		}
		if _, found := knownRunbooks[runbook]; !found {
			return nil, fmt.Errorf("retrieval seed incident %q names unknown runbook %q", id, runbook)
		}
		seenIDs[id] = struct{}{}
		cases = append(cases, RetrievalCase{
			ID: id, Query: title + ". " + summary, ExpectedRunbook: runbook,
		})
	}
	slices.SortFunc(cases, func(left, right RetrievalCase) int {
		return strings.Compare(left.ID, right.ID)
	})
	return cases, nil
}

func RunRetrievalEvaluation(ctx context.Context, config RetrievalRunConfig) (RetrievalArtifact, error) {
	if err := validateRetrievalRunConfig(config); err != nil {
		return RetrievalArtifact{}, err
	}
	cases, err := LoadRetrievalCases(config.DataDir)
	if err != nil {
		return RetrievalArtifact{}, err
	}
	digest, err := retrievalCorpusDigest(config.DataDir, cases)
	if err != nil {
		return RetrievalArtifact{}, err
	}

	keyword, keywordRuntimeID, err := evaluateRetrievalRuntime(
		ctx, config.Factory, RetrievalKeyword, "", cases,
	)
	if err != nil {
		return RetrievalArtifact{}, err
	}
	semantic, _, err := evaluateRetrievalRuntime(
		ctx, config.Factory, RetrievalSemantic, keywordRuntimeID, cases,
	)
	if err != nil {
		return RetrievalArtifact{}, err
	}
	artifact := RetrievalArtifact{
		SchemaVersion:        RetrievalArtifactSchemaVersion,
		Source:               config.Source,
		CorpusDigest:         digest,
		EmbeddingModel:       config.EmbeddingModel,
		EmbeddingModelDigest: config.EmbeddingModelDigest,
		CaseCount:            len(cases),
		Keyword:              keyword,
		Semantic:             semantic,
	}
	if err := ValidateRetrievalArtifact(artifact); err != nil {
		return RetrievalArtifact{}, err
	}
	return artifact, nil
}

func WriteRetrievalArtifact(path string, artifact RetrievalArtifact) error {
	if err := ValidateRetrievalArtifact(artifact); err != nil {
		return err
	}
	return WriteJSONArtifact(path, artifact)
}

func ValidateRetrievalArtifact(artifact RetrievalArtifact) error {
	var problems []error
	if artifact.SchemaVersion != RetrievalArtifactSchemaVersion {
		problems = append(problems, fmt.Errorf(
			"schema_version=%d, want %d", artifact.SchemaVersion, RetrievalArtifactSchemaVersion,
		))
	}
	if err := artifact.Source.Validate(); err != nil {
		problems = append(problems, err)
	}
	if !boundedIdentity(artifact.EmbeddingModel) {
		problems = append(problems, errors.New("embedding_model must be non-empty, trimmed, and single-line"))
	}
	if artifact.EmbeddingModelDigest != "" && !boundedIdentity(artifact.EmbeddingModelDigest) {
		problems = append(problems, errors.New("embedding_model_digest must be trimmed and single-line when set"))
	}
	if len(artifact.CorpusDigest) != corpusDigestSize {
		problems = append(problems, fmt.Errorf("corpus_digest must be %d lowercase hexadecimal characters", corpusDigestSize))
	} else if _, err := hex.DecodeString(artifact.CorpusDigest); err != nil || artifact.CorpusDigest != strings.ToLower(artifact.CorpusDigest) {
		problems = append(problems, errors.New("corpus_digest must be lowercase hexadecimal"))
	}
	if artifact.CaseCount < 1 {
		problems = append(problems, errors.New("case_count must be positive"))
	}
	problems = append(problems, validateRetrievalRates("keyword", artifact.Keyword)...)
	problems = append(problems, validateRetrievalRates("semantic", artifact.Semantic)...)
	return errors.Join(problems...)
}

func validateRetrievalRunConfig(config RetrievalRunConfig) error {
	var problems []error
	if strings.TrimSpace(config.DataDir) == "" {
		problems = append(problems, errors.New("retrieval evaluation needs an immutable data directory"))
	}
	if err := config.Source.Validate(); err != nil {
		problems = append(problems, err)
	}
	if !boundedIdentity(config.EmbeddingModel) {
		problems = append(problems, errors.New("retrieval evaluation needs a non-empty, trimmed, single-line embedding model"))
	}
	if config.EmbeddingModelDigest != "" && !boundedIdentity(config.EmbeddingModelDigest) {
		problems = append(problems, errors.New("retrieval evaluation embedding model digest must be trimmed and single-line when set"))
	}
	if config.Factory == nil {
		problems = append(problems, errors.New("retrieval evaluation needs a runtime factory"))
	}
	return errors.Join(problems...)
}

func evaluateRetrievalRuntime(
	ctx context.Context,
	factory RetrievalRuntimeFactory,
	mode RetrievalMode,
	previousRuntimeID string,
	cases []RetrievalCase,
) (rates RetrievalRates, runtimeID string, returnErr error) {
	runtime, err := factory(ctx, mode)
	if err != nil {
		return RetrievalRates{}, "", fmt.Errorf("start %s retrieval runtime: %w", mode, err)
	}
	if runtime.Close == nil {
		return RetrievalRates{}, "", fmt.Errorf("%s retrieval runtime has no cleanup", mode)
	}
	defer func() {
		returnErr = errors.Join(returnErr, runtime.Close())
	}()
	if runtime.ID == "" {
		return RetrievalRates{}, "", fmt.Errorf("%s retrieval runtime has no identity", mode)
	}
	if previousRuntimeID != "" && runtime.ID == previousRuntimeID {
		return RetrievalRates{}, "", errors.New("semantic retrieval must use a distinct isolated runtime")
	}
	if runtime.Client == nil {
		return RetrievalRates{}, "", fmt.Errorf("%s retrieval runtime has no client", mode)
	}
	rates, err = evaluateRetrievalCases(ctx, runtime.Client, mode, cases)
	if err != nil {
		return RetrievalRates{}, "", err
	}
	return rates, runtime.ID, nil
}

func evaluateRetrievalCases(
	ctx context.Context,
	client RetrievalClient,
	mode RetrievalMode,
	cases []RetrievalCase,
) (RetrievalRates, error) {
	hitsAt1, hitsAt3 := 0, 0
	for _, evalCase := range cases {
		result, err := client.SearchRunbooks(ctx, evalCase.Query, retrievalLimit)
		if err != nil {
			return RetrievalRates{}, fmt.Errorf("evaluate %s retrieval case %q: %w", mode, evalCase.ID, err)
		}
		if result.Mode != mode {
			return RetrievalRates{}, fmt.Errorf("%s runtime reported %s retrieval", mode, result.Mode)
		}
		if err := validateRetrievalRanking(result.Slugs, retrievalLimit); err != nil {
			return RetrievalRates{}, fmt.Errorf("evaluate %s retrieval case %q: %w", mode, evalCase.ID, err)
		}
		if len(result.Slugs) > 0 && result.Slugs[0] == evalCase.ExpectedRunbook {
			hitsAt1++
		}
		if slices.Contains(result.Slugs, evalCase.ExpectedRunbook) {
			hitsAt3++
		}
	}
	count := float64(len(cases))
	return RetrievalRates{
		HitRateAt1: float64(hitsAt1) / count,
		HitRateAt3: float64(hitsAt3) / count,
	}, nil
}

func validateRetrievalRanking(slugs []string, limit int) error {
	if len(slugs) > limit {
		return fmt.Errorf("search_runbooks returned %d results, limit is %d", len(slugs), limit)
	}
	seen := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		if !slugPattern.MatchString(slug) {
			return fmt.Errorf("search_runbooks returned invalid runbook slug %q", slug)
		}
		if _, duplicate := seen[slug]; duplicate {
			return fmt.Errorf("search_runbooks repeated runbook slug %q", slug)
		}
		seen[slug] = struct{}{}
	}
	return nil
}

func retrievalCorpusDigest(dataDir string, cases []RetrievalCase) (string, error) {
	digest := sha256.New()
	writeRetrievalDigestPart(digest, []byte("agentops-retrieval-corpus-v1"))
	for _, evalCase := range cases {
		writeRetrievalDigestPart(digest, []byte(evalCase.ID))
		writeRetrievalDigestPart(digest, []byte(evalCase.Query))
		writeRetrievalDigestPart(digest, []byte(evalCase.ExpectedRunbook))
	}
	writeRetrievalDigestPart(digest, []byte("runbooks"))
	directory := filepath.Join(dataDir, "runbooks")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", fmt.Errorf("read retrieval corpus %s: %w", directory, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("inspect retrieval corpus entry %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("retrieval corpus entry %s is not a regular file", entry.Name())
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return "", fmt.Errorf("read retrieval corpus entry %s: %w", name, err)
		}
		writeRetrievalDigestPart(digest, []byte(name))
		writeRetrievalDigestPart(digest, content)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeRetrievalDigestPart(digest hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}

func validateRetrievalRates(name string, rates RetrievalRates) []error {
	var problems []error
	for metric, value := range map[string]float64{
		"hit_rate_at_1": rates.HitRateAt1,
		"hit_rate_at_3": rates.HitRateAt3,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			problems = append(problems, fmt.Errorf("%s.%s must be between 0 and 1", name, metric))
		}
	}
	if rates.HitRateAt1 > rates.HitRateAt3 {
		problems = append(problems, fmt.Errorf("%s hit_rate_at_1 cannot exceed hit_rate_at_3", name))
	}
	return problems
}

func boundedIdentity(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n")
}
