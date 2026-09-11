package evals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type CalibrationSet struct {
	Digest        string            `json:"-"`
	Cases         []CalibrationCase `json:"cases"`
	SchemaVersion int               `json:"schema_version"`
}

type CalibrationCase struct {
	ID              string `json:"id"`
	Category        string `json:"category"`
	Question        string `json:"question"`
	ReferenceAnswer string `json:"reference_answer"`
	Answer          string `json:"answer"`
	ExpectedPass    bool   `json:"expected_pass"`
}

type CalibrationCaseResult struct {
	ID            string `json:"id"`
	Category      string `json:"category"`
	ExpectedPass  bool   `json:"expected_pass"`
	PredictedPass bool   `json:"predicted_pass"`
	Matched       bool   `json:"matched"`
}

// CalibrationResult is a measurement, not a verdict. Agreement says how often
// the configured judge matched a human label on a balanced labeled set; what
// counts as good enough for a given course, model, and risk is a human call.
type CalibrationResult struct {
	Source            SourceEvidence          `json:"source"`
	JudgeModel        ModelEvidence           `json:"judge_model"`
	CalibrationDigest string                  `json:"calibration_digest"`
	Cases             []CalibrationCaseResult `json:"cases"`
	SchemaVersion     int                     `json:"schema_version"`
	Matches           int                     `json:"matches"`
	Total             int                     `json:"total"`
	Agreement         float64                 `json:"agreement"`
	// A judge can be wrong in two directions, and they cost different things: a false
	// pass ships a bad answer, a false fail blocks a good one. One agreement number
	// hides which it is doing.
	FalsePass int `json:"false_pass"`
	FalseFail int `json:"false_fail"`
	// MajorityBaseline is the larger label class over the total — what a judge that
	// always answered the same way would score. Agreement at or below it is worth
	// nothing, which is the number the set's own label split makes easy to miss.
	MajorityBaseline float64 `json:"majority_baseline"`
}

type VerdictJudge interface {
	Judge(context.Context, JudgeInput) (JudgeVerdict, error)
}

func LoadCalibrationSet(path string) (CalibrationSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return CalibrationSet{}, fmt.Errorf("read judge calibration set %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytesReader(raw))
	decoder.DisallowUnknownFields()
	var set CalibrationSet
	if decodeErr := decoder.Decode(&set); decodeErr != nil {
		return CalibrationSet{}, fmt.Errorf("decode judge calibration set %s: %w", path, decodeErr)
	}
	if eofErr := requireJSONEOF(decoder); eofErr != nil {
		return CalibrationSet{}, fmt.Errorf("decode judge calibration set %s: %w", path, eofErr)
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return CalibrationSet{}, fmt.Errorf("canonicalize judge calibration set %s: %w", path, err)
	}
	digest := sha256.Sum256(canonical)
	set.Digest = hex.EncodeToString(digest[:])
	if err := set.Validate(); err != nil {
		return CalibrationSet{}, fmt.Errorf("validate judge calibration set %s: %w", path, err)
	}
	return set, nil
}

func (s CalibrationSet) Validate() error {
	var problems []error
	if s.SchemaVersion != 1 {
		problems = append(problems, fmt.Errorf("schema_version=%d, want 1", s.SchemaVersion))
	}
	if len(s.Cases) < 12 {
		problems = append(problems, errors.New("judge calibration needs at least 12 labeled cases"))
	}
	seen := make(map[string]struct{}, len(s.Cases))
	categories := map[string]int{"good": 0, "bad": 0, "hallucinated": 0}
	for index, calibrationCase := range s.Cases {
		if calibrationCase.ID == "" {
			problems = append(problems, fmt.Errorf("case %d has no id", index+1))
		}
		if _, exists := seen[calibrationCase.ID]; exists {
			problems = append(problems, fmt.Errorf("duplicate calibration id %q", calibrationCase.ID))
		}
		seen[calibrationCase.ID] = struct{}{}
		if _, known := categories[calibrationCase.Category]; !known {
			problems = append(problems, fmt.Errorf("case %q has invalid category %q", calibrationCase.ID, calibrationCase.Category))
		} else {
			categories[calibrationCase.Category]++
		}
		if calibrationCase.Question == "" || calibrationCase.ReferenceAnswer == "" || calibrationCase.Answer == "" {
			problems = append(problems, fmt.Errorf("case %q has an empty question, reference, or answer", calibrationCase.ID))
		}
	}
	if categories["good"] == 0 || categories["good"] != categories["bad"] || categories["bad"] != categories["hallucinated"] {
		problems = append(problems, errors.New("judge calibration must balance good, bad, and hallucinated answers"))
	}
	return errors.Join(problems...)
}

func Calibrate(
	ctx context.Context,
	set CalibrationSet,
	judge VerdictJudge,
	judgeModel ModelEvidence,
	source SourceEvidence,
) (CalibrationResult, error) {
	if err := source.Validate(); err != nil {
		return CalibrationResult{}, err
	}
	if judgeModel.Provider == "" || judgeModel.Name == "" {
		return CalibrationResult{}, errors.New("judge calibration needs a judge provider and model name")
	}
	result := CalibrationResult{
		SchemaVersion:     5,
		Source:            source,
		JudgeModel:        judgeModel,
		CalibrationDigest: set.Digest,
		Total:             len(set.Cases),
		Cases:             make([]CalibrationCaseResult, 0, len(set.Cases)),
	}
	expectedPasses := 0
	for _, calibrationCase := range set.Cases {
		verdict, err := judge.Judge(ctx, JudgeInput{
			Questions:        []string{calibrationCase.Question},
			Answers:          []string{calibrationCase.Answer},
			ReferenceAnswers: []string{calibrationCase.ReferenceAnswer},
		})
		if err != nil {
			return CalibrationResult{}, fmt.Errorf("judge calibration case %q: %w", calibrationCase.ID, err)
		}
		matched := verdict.Passed == calibrationCase.ExpectedPass
		switch {
		case matched:
			result.Matches++
		case verdict.Passed:
			result.FalsePass++
		default:
			result.FalseFail++
		}
		if calibrationCase.ExpectedPass {
			expectedPasses++
		}
		result.Cases = append(result.Cases, CalibrationCaseResult{
			ID:            calibrationCase.ID,
			Category:      calibrationCase.Category,
			ExpectedPass:  calibrationCase.ExpectedPass,
			PredictedPass: verdict.Passed,
			Matched:       matched,
		})
	}
	result.Agreement = float64(result.Matches) / float64(result.Total)
	result.MajorityBaseline = float64(max(expectedPasses, result.Total-expectedPasses)) / float64(result.Total)
	return result, nil
}

func WriteJSONArtifact(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create artifact directory %s: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".artifact-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set artifact permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode artifact %s: %w", path, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync artifact %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close artifact %s: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish artifact %s: %w", path, err)
	}
	return nil
}
