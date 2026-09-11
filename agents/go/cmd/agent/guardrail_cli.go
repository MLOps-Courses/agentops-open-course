package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
)

const guardrailFixtureLimit = 1 << 20

// checkRunbookGuardrail proves one explicit fixture triggers the in-process
// injection tripwire. It exists for the recoverable chaos drill; production
// tool output still enters through Policy.SecureToolOutput.
func checkRunbookGuardrail(arguments []string, out io.Writer) error {
	if len(arguments) != 2 {
		return fmt.Errorf("%s expects a fixture root and relative runbook path", guardrailCheckCommand)
	}
	file, err := os.OpenInRoot(arguments[0], arguments[1])
	if err != nil {
		return fmt.Errorf("open guardrail fixture: %w", err)
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, guardrailFixtureLimit+1))
	if err != nil {
		return fmt.Errorf("read guardrail fixture: %w", err)
	}
	if len(content) > guardrailFixtureLimit {
		return fmt.Errorf("guardrail fixture exceeds %d bytes", guardrailFixtureLimit)
	}
	neutralized, hits := policy.NeutralizeInjections(string(content))
	if hits < 1 || !strings.Contains(neutralized, policy.NeutralizedMarker) {
		return errors.New("runbook fixture contained no recognized injection marker")
	}
	_, err = fmt.Fprintf(out, "neutralized %d injection marker(s) in the temporary runbook\n", hits)
	return err
}
