package conventions

import (
	"strings"
	"testing"
)

const ecosystemPageWhere = "content/0. Overview/0.4. Ecosystem.md"

// The port inventory is graded inside one collapsible, and cutting the page on that
// heading inline reported a rename as "every stable port is missing" — a message that
// sends an author to the port table rather than to the line that moved.
func TestPortContractsNameTheHeadingTheyReadFrom(t *testing.T) {
	root := t.TempDir()

	messages := problemMessages(checkPortContracts(root, pageSet{ecosystemPageWhere: "No collapsible here.\n"}))
	if !strings.Contains(messages, "expected the heading") || !strings.Contains(messages, "every port the course binds") {
		t.Errorf("renamed port heading was not named: %s", messages)
	}
	if strings.Contains(messages, "stable port inventory drifted") {
		t.Errorf("a missing heading was reported as an inventory drift: %s", messages)
	}

	documented := `{{% collapsible note "Deeper: every port the course binds" %}}` + "\n\nThe course binds `:8080`.\n"
	messages = problemMessages(checkPortContracts(root, pageSet{ecosystemPageWhere: documented}))
	if strings.Contains(messages, "expected the heading") {
		t.Errorf("the heading is present and was still reported: %s", messages)
	}
	if !strings.Contains(messages, "stable port inventory drifted") {
		t.Errorf("a page documenting one port was not reported as drifted: %s", messages)
	}
}
