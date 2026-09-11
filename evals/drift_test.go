package evals

import (
	"strings"
	"testing"
)

func TestTokenDriftWarningSpeaksOnlyBeyondTheTolerance(t *testing.T) {
	t.Parallel()

	previous := driftArtifact(1000, 4)
	for name, test := range map[string]struct {
		tokens int64
		want   bool
	}{
		"identical":     {tokens: 1000, want: false},
		"at tolerance":  {tokens: 1250, want: false},
		"more tokens":   {tokens: 1400, want: true},
		"fewer tokens":  {tokens: 600, want: true},
		"small savings": {tokens: 900, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			warning, err := TokenDriftWarning(previous, driftArtifact(test.tokens, 4))
			if err != nil {
				t.Fatal(err)
			}
			if (warning != "") != test.want {
				t.Fatalf("warning = %q, want warning: %t", warning, test.want)
			}
			if test.want && !strings.Contains(warning, "cost drift") {
				t.Fatalf("warning = %q, want a readable drift line", warning)
			}
		})
	}
}

func TestTokenDriftWarningStaysQuietWithoutAComparableBaseline(t *testing.T) {
	t.Parallel()

	current := driftArtifact(5000, 4)
	otherEvalSet := driftArtifact(1000, 4)
	otherEvalSet.EvalSet.Digest = strings.Repeat("f", 64)
	otherModel := driftArtifact(1000, 4)
	otherModel.Model.Name = "another-model"
	empty := driftArtifact(0, 0)

	for name, previous := range map[string]RunArtifact{
		"other evalset": otherEvalSet,
		"other model":   otherModel,
		"no usage":      empty,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			warning, err := TokenDriftWarning(previous, current)
			if err != nil || warning != "" {
				t.Fatalf("warning/error = %q/%v, want silence", warning, err)
			}
		})
	}
}

func TestTokenDriftWarningRefusesInvalidUsage(t *testing.T) {
	t.Parallel()

	broken := driftArtifact(1000, 4)
	broken.Cases[0].Usage.InputTokens = -1
	if _, err := TokenDriftWarning(broken, driftArtifact(1000, 4)); err == nil ||
		!strings.Contains(err.Error(), "previous run usage") {
		t.Fatalf("error = %v, want invalid previous usage", err)
	}
	if _, err := TokenDriftWarning(driftArtifact(1000, 4), broken); err == nil ||
		!strings.Contains(err.Error(), "current run usage") {
		t.Fatalf("error = %v, want invalid current usage", err)
	}
}

func driftArtifact(totalTokens, modelCalls int64) RunArtifact {
	return RunArtifact{
		Model:   ModelEvidence{Provider: "provider", Name: "model"},
		EvalSet: EvalSetEvidence{ID: "set", Digest: strings.Repeat("a", 64)},
		Cases: []CaseResult{{
			ID: "case", Sample: 1, Usage: Usage{TotalTokens: totalTokens, ModelCalls: modelCalls},
		}},
	}
}
