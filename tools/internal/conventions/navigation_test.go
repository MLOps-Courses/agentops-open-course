package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckNavigationRejectsMissingPage(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "nav.yaml"), []byte("- page: _index.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pages := pageSet{
		"content/_index.md":              "",
		"content/9. Ghost/9.0. Ghost.md": "",
	}
	problems := checkNavigation(root, pages)
	if !strings.Contains(problemMessages(problems), "navigation omits pages") {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestCheckImageFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets", "images"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "images", "present.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	pages := pageSet{
		// A capture that exists, a capture that does not, and one inside a fence that
		// only documents the syntax — the third must not be treated as a reference.
		"content/1. Setup/1.0. System.md": "![A present capture](/images/present.png)\n" +
			"![A missing capture](/images/missing.png)\n" +
			"```markdown\n![An example](/images/never-committed.png)\n```\n",
	}
	messages := problemMessages(checkImageFiles(root, pages))
	if !strings.Contains(messages, "/images/missing.png") {
		t.Fatalf("missing capture not reported: %s", messages)
	}
	if strings.Contains(messages, "present.png") {
		t.Fatalf("existing capture reported: %s", messages)
	}
	if strings.Contains(messages, "never-committed.png") {
		t.Fatalf("fenced example reported: %s", messages)
	}
}

func TestCheckChapterIndexCoverage(t *testing.T) {
	tests := []struct {
		name  string
		index string
		want  string
	}{
		{
			name:  "linked",
			index: `- [3.8. Streaming]({{< relref "/3. Capabilities/3.8. Streaming.md" >}})`,
			want:  "",
		},
		{
			name:  "linked to a section of the page",
			index: `- [3.8. Streaming]({{< relref "/3. Capabilities/3.8. Streaming.md#backpressure" >}})`,
			want:  "",
		},
		{
			name:  "orphaned",
			index: "- nothing here links the chapter's own pages\n",
			want:  "chapter index omits pages: 3.8. Streaming.md",
		},
		{
			// A sentence that only names the file leaves the learner with nothing to
			// follow, and the retirement note is the case that reads most like coverage.
			name:  "named in prose but never linked",
			index: "The old 3.8. Streaming.md page was folded into 3.6 and removed.\n",
			want:  "chapter index omits pages: 3.8. Streaming.md",
		},
		{
			// A shortcode inside a fence documents the syntax; it renders as sample text
			// and links nothing.
			name:  "linked only inside a fenced example",
			index: "```markdown\n[3.8. Streaming]({{< relref \"/3. Capabilities/3.8. Streaming.md\" >}})\n```\n",
			want:  "chapter index omits pages: 3.8. Streaming.md",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pages := pageSet{
				"content/_index.md":                           "the landing page owns no chapter",
				"content/3. Capabilities/_index.md":           test.index,
				"content/3. Capabilities/3.8. Streaming.md":   "",
				"content/3. Capabilities/3.7. Multi-Agent.md": "",
			}
			// 3.7 is linked in every case, so each rejecting case proves the check
			// names only the page that is actually missing.
			pages["content/3. Capabilities/_index.md"] += "\n- [3.7. Multi-Agent]({{< relref \"/3. Capabilities/3.7. Multi-Agent.md\" >}})\n"
			problems := checkChapterIndexCoverage(pages)
			if test.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %#v", problems)
				}
				return
			}
			if !strings.Contains(problemMessages(problems), test.want) {
				t.Fatalf("problems = %#v", problems)
			}
		})
	}
}

func TestCompareContractReportsAuthorityAndDrift(t *testing.T) {
	if problems := compareContract("content/example.md", "pin", "", "1"); len(problems) != 1 || !strings.Contains(problems[0].Message, "authoritative") {
		t.Fatalf("missing authority problems = %#v", problems)
	}
	problems := compareContract("content/example.md", "pin", "2.0.0", "1.9.0")
	if len(problems) != 1 || problems[0].Message != `pin drifted: expected "2.0.0", found "1.9.0"` {
		t.Fatalf("drift problems = %#v", problems)
	}
}

func TestDependencyAuditContractRejectsRetiredPythonInventory(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "scripts", "check-licenses.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("check_repository_licenses\n\"${lib_dir}/trivy-repository.sh\" licenses\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pages := pageSet{
		"content/8. Community/8.1. License.md": "trivy-repository.sh three Go modules agents/go/go.mod evals/go.mod tools/go.mod pip-licenses",
		"content/4. Quality/4.1. Linting.md":   "govulncheck check:vuln agents/go evals tools",
	}
	problems := checkDependencyAuditContract(root, pages)
	if !strings.Contains(problemMessages(problems), "retired Python dependency auditing") {
		t.Fatalf("problems = %#v", problems)
	}
}
