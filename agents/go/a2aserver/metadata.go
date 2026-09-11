package a2aserver

import (
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The A2A metadata contract, re-implemented.
//
// a2a-go validates every task it stores — the status message, the whole
// history, every artifact, and each of their metadata maps — but keeps
// validateTask and its helpers unexported inside the taskstore package. A store
// that skipped the checks would accept a task the in-memory store rejects, so
// the two implementations would disagree about what is storable, and the
// disagreement would only surface once someone swapped the store in production.
// This file is that contract, held to the same accept-list and the same
// messages.

// validateTask reports whether a task's metadata is representable on the wire.
//
// A nil task is accepted here exactly as the reference accepts it; the callers
// that need a task to key a row on refuse it themselves, with a better message.
func validateTask(task *a2a.Task) error {
	if task == nil {
		return nil
	}
	if err := validateMessage(task.Status.Message); err != nil {
		return err
	}
	for _, message := range task.History {
		if err := validateMessage(message); err != nil {
			return err
		}
	}
	for _, artifact := range task.Artifacts {
		if err := validateArtifact(artifact); err != nil {
			return err
		}
	}
	return validateMeta(task.Metadata)
}

// validateArtifact checks one artifact's parts and metadata.
func validateArtifact(artifact *a2a.Artifact) error {
	if artifact == nil {
		return nil
	}
	if err := validateParts(artifact.Parts); err != nil {
		return err
	}
	return validateMeta(artifact.Metadata)
}

// validateMessage checks one message's parts and metadata.
func validateMessage(message *a2a.Message) error {
	if message == nil {
		return nil
	}
	if err := validateParts(message.Parts); err != nil {
		return err
	}
	return validateMeta(message.Metadata)
}

// validateParts checks the metadata carried by each part.
func validateParts(parts a2a.ContentParts) error {
	for _, part := range parts {
		if err := validateMeta(part.Meta()); err != nil {
			return err
		}
	}
	return nil
}

// validateMeta checks one metadata map.
func validateMeta(meta map[string]any) error {
	return validateMetaValue(meta, map[string]struct{}{})
}

// validateMetaValue walks a metadata value, rejecting anything outside the
// JSON-representable accept-list and any cycle.
//
// The accept-list deliberately excludes every unsigned type: the A2A spec has
// no unsigned number, so a uint that round-tripped through JSON would come back
// as something else. Cycles are detected by pointer identity, which is what
// makes a self-referencing map a named failure instead of a stack overflow.
func validateMetaValue(value any, visiting map[string]struct{}) error {
	if value == nil {
		return nil
	}
	switch value.(type) {
	case bool, int, int8, int16, int32, int64, float32, float64, string:
		return nil
	}

	identity := fmt.Sprintf("%p", value)
	if _, ok := visiting[identity]; ok {
		return errors.New("circular reference in Metadata")
	}
	visiting[identity] = struct{}{}
	defer delete(visiting, identity)

	switch typed := value.(type) {
	case []any:
		for _, element := range typed {
			if err := validateMetaValue(element, visiting); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for _, element := range typed {
			if err := validateMetaValue(element, visiting); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf(
		"%T is not permitted in Metadata, must be one of nil, bool, int, float, string, []any, map[string]any",
		value,
	)
}
