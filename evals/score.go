package evals

import (
	"errors"
	"math"
)

type Score struct {
	Name    string  `json:"name"`
	Details string  `json:"-"`
	Value   float64 `json:"value"`
	Passed  bool    `json:"passed"`
	// Stochastic marks a score a second model produced rather than a rule. It never
	// serializes, so no artifact schema moves; it exists so `required_cases_passed`
	// can fold over deterministic evidence only. A judged failure still costs the run
	// its pass rate — it just cannot declare a safety case failed on its own.
	Stochastic bool `json:"-"`
}

// JudgeScoreName is the serialized name of the gateway judge's verdict.
const JudgeScoreName = "judge"

// stochasticScoreNames is the serialized half of the Stochastic flag above.
//
// A published artifact keeps only each score's name and value, so a reader of two
// artifacts — `eval:ab` is the one that matters — has no other way to tell a model's
// verdict from a rule's. Naming them here, beside the flag they mirror, is what lets
// a new stochastic scorer be added in one place rather than found by grep.
var stochasticScoreNames = map[string]struct{}{JudgeScoreName: {}}

// IsStochasticScoreName reports whether a serialized score name was produced by a
// second model rather than by a rule.
func IsStochasticScoreName(name string) bool {
	_, found := stochasticScoreNames[name]
	return found
}

func validateScoreValue(name string, value float64) error {
	if name == "" || !finiteUnitInterval(value) {
		return errors.New("score needs a name and a finite value between 0 and 1")
	}
	return nil
}

func NewBinaryScore(name string, passed bool, details string) Score {
	value := 0.0
	if passed {
		value = 1.0
	}
	return Score{Name: name, Value: value, Passed: passed, Details: details}
}

// NewStochasticBinaryScore is NewBinaryScore for a verdict a model produced.
func NewStochasticBinaryScore(name string, passed bool, details string) Score {
	score := NewBinaryScore(name, passed, details)
	score.Stochastic = true
	return score
}

func finiteUnitInterval(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}
