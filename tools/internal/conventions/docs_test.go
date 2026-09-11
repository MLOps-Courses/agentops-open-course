package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Skaffold rule used to walk "docs", the MkDocs source directory the Python course
// kept its pages in and which this repository has never had. It now walks the course
// pages, so these fixtures are the two shapes it has to tell apart.
func TestCheckSkaffoldRunbooksCoversCoursePages(t *testing.T) {
	for _, test := range []struct {
		fixture string
		want    string
	}{
		{fixture: "rejected.md", want: "run from `infra/` with `--filename skaffold.yaml`"},
		{fixture: "accepted.md"},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			root := t.TempDir()
			page := filepath.Join(root, "content", "6. Platform", "6.6. Platform Delivery.md")
			if err := os.MkdirAll(filepath.Dir(page), 0o750); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "conventions", "skaffold-runbook", test.fixture))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(page, content, 0o600); err != nil {
				t.Fatal(err)
			}
			problems := checkSkaffoldRunbooks(root)
			if test.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %#v", problems)
				}
				return
			}
			messages := problemMessages(problems)
			if len(problems) != 2 || !strings.Contains(messages, test.want) {
				t.Fatalf("problems = %#v", problems)
			}
		})
	}
}
