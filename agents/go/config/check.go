package config

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
)

// UnsetValue is what config:check prints for an optional setting the operator
// never set. An empty line would read as "set to nothing", which is a different
// state and, for OPENAI_API_KEY, a different outcome.
const UnsetValue = "(unset)"

// Setting is one resolved variable as config:check reports it.
type Setting struct {
	Variable string
	Value    string
}

// Describe renders the resolved configuration, sorted by variable name, with
// every secret masked.
//
// It reads the struct through the same field table Load uses, so a setting
// added to Config cannot be forgotten here.
func (c Config) Describe() []Setting {
	value := reflect.ValueOf(c)
	described := make([]Setting, 0, len(specs()))
	for _, spec := range specs() {
		field := value.Field(spec.index)
		rendered := ""
		if spec.pointer {
			if !field.IsNil() {
				// %v on the pointer would print an address; the pointee is the value
				// the operator set, and Secret's mask still applies through it.
				rendered = fmt.Sprintf("%v", field.Elem().Interface())
			}
		} else {
			rendered = fmt.Sprintf("%v", field.Interface())
		}
		if rendered == "" {
			rendered = UnsetValue
		}
		described = append(described, Setting{Variable: spec.variable, Value: rendered})
	}
	slices.SortFunc(described, func(a, b Setting) int { return strings.Compare(a.Variable, b.Variable) })
	return described
}

// Exit codes returned by Check.
//
// They live here rather than in cmd/agent because they are shared: state.Main
// returns them too, so an operator reads one table for every subcommand the
// binary offers. cmd/agent is package main and cannot be imported, so hosting
// them there would leave every library-side command entrypoint without them.
// config is a leaf — it imports nothing else in this module — so depending on it
// from a command cannot create a cycle.
const (
	// ExitValid means the configuration loaded and was printed.
	ExitValid = 0
	// ExitInvalid means the configuration was rejected; the problems are on errOut.
	ExitInvalid = 1
	// ExitUnreportable means the report itself could not be written. It is
	// distinct because at that point there is no channel left to explain on.
	ExitUnreportable = 2
)

// Check is the `config:check` entrypoint (Chapter 1.5).
//
// It loads settings exactly the way every runtime entrypoint does, prints the
// resolved effective configuration with secrets masked, and reports the process
// exit code — non-zero, with every validation problem on errOut, when a
// combination is invalid. It returns an exit code rather than an error because
// it *is* the process boundary: the operator-facing rendering of the problems is
// the deliverable, and a caller re-formatting it would lose the accumulated list.
func Check(out, errOut io.Writer) int {
	resolved, err := Load()

	// The report is rendered whole before anything is written, so a partial
	// configuration can never appear next to the error that rejected it.
	target, report, code := out, "", ExitValid
	if err != nil {
		target, report, code = errOut, invalidReport(err), ExitInvalid
	} else {
		report = resolvedReport(resolved)
	}
	if _, writeErr := io.WriteString(target, report); writeErr != nil {
		return ExitUnreportable
	}
	return code
}

// resolvedReport renders a valid configuration with every secret masked.
func resolvedReport(resolved Config) string {
	var report strings.Builder
	report.WriteString("Agent configuration is valid. Resolved settings (secrets masked):\n")
	for _, setting := range resolved.Describe() {
		fmt.Fprintf(&report, "- %s = %s\n", setting.Variable, setting.Value)
	}
	return report.String()
}

// invalidReport renders every problem as its own bullet, so an operator with
// three bad values sees three lines to fix.
func invalidReport(err error) string {
	var report strings.Builder
	report.WriteString("Agent configuration is invalid:\n")
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		fmt.Fprintf(&report, "- %v\n", err)
		return report.String()
	}
	for _, problem := range invalid.Problems {
		fmt.Fprintf(&report, "- %s\n", problem)
	}
	return report.String()
}
