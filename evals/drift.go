package evals

import (
	"fmt"
	"math"
)

// TokenDriftTolerance is the fraction a run's total token count may move from
// the previous run before the harness says so out loud.
const TokenDriftTolerance = 0.25

// TokenDriftWarning compares this run's usage with the previous run stored at
// the same results path and returns the line a learner should read, or "" when
// there is nothing worth saying.
//
// This is a warning, never a gate. Token counts move for honest reasons — a
// longer runbook, a chattier model, one extra retry — and a run that answered
// every case correctly should not fail because it spent more to do it. Cost is
// something you watch and explain, not something a threshold decides for you.
func TokenDriftWarning(previous, current RunArtifact) (string, error) {
	// Two evalsets have no shared usage baseline. The results file is shared by
	// every evalset, so this is the ordinary case, not an error.
	if previous.EvalSet != current.EvalSet || previous.Model.Name != current.Model.Name {
		return "", nil
	}
	previousUsage, err := totalUsage(previous.Cases)
	if err != nil {
		return "", fmt.Errorf("previous run usage is invalid: %w", err)
	}
	currentUsage, err := totalUsage(current.Cases)
	if err != nil {
		return "", fmt.Errorf("current run usage is invalid: %w", err)
	}
	if previousUsage.TotalTokens <= 0 {
		return "", nil
	}
	change := (float64(currentUsage.TotalTokens) - float64(previousUsage.TotalTokens)) /
		float64(previousUsage.TotalTokens)
	if math.Abs(change) <= TokenDriftTolerance {
		return "", nil
	}
	return fmt.Sprintf(
		"cost drift: %d total tokens over %d model calls, %+.0f%% against the previous run's %d tokens over %d calls",
		currentUsage.TotalTokens, currentUsage.ModelCalls,
		change*100,
		previousUsage.TotalTokens, previousUsage.ModelCalls,
	), nil
}
