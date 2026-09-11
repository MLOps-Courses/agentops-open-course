package main

import (
	"fmt"
	"path/filepath"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/state"
)

// recoveredState is the proof every stateful runtime constructor requires.
// Its fields and constructor are private so production code cannot manufacture
// the token without first passing through crash recovery.
type recoveredState struct {
	directory string
}

// recoverRuntimeState is the single boundary between configuration and every
// runtime that can read, migrate, or publish state. It deliberately creates
// nothing: recovery already treats a missing directory as a clean first boot.
func recoverRuntimeState(stateDir string) (recoveredState, error) {
	directory, err := filepath.Abs(stateDir)
	if err != nil {
		return recoveredState{}, fmt.Errorf("resolving the runtime state directory %s: %w", stateDir, err)
	}
	if err := state.RecoverInterruptedRestore(directory, state.RecoverOptions{}); err != nil {
		return recoveredState{}, fmt.Errorf("recovering an interrupted state restore: %w", err)
	}
	return recoveredState{directory: directory}, nil
}

// require proves a constructor is still being pointed at the generation the
// caller recovered. Comparing absolute clean paths also catches accidental
// reuse of a token after a test or future multi-runtime caller changes config.
func (r recoveredState) require(stateDir string) error {
	directory, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("resolving the runtime state directory %s: %w", stateDir, err)
	}
	if r.directory == "" || r.directory != directory {
		return fmt.Errorf("runtime state %s has not passed crash recovery", directory)
	}
	return nil
}
