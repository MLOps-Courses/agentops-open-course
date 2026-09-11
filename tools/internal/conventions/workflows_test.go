package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// yamlWorkflowFixture is a repository whose only workflow ends in .yaml, and which
// breaks both contracts that used to glob *.yml alone. The fixture lives in testdata
// rather than in a committed .github/workflows so the repository's own workflow linters
// never collect a deliberately broken file; the test plants it where the checks look.
const yamlWorkflowFixture = "../../testdata/conventions/yaml-workflow"

// twoUploadWorkflowFixture is a repository whose one workflow uploads two artifacts and
// declares a single retention-days value, which the per-file literal count read as a
// balanced pair.
const twoUploadWorkflowFixture = "../../testdata/conventions/two-upload-workflow"

// plantWorkflowFixture copies one fixture repository's AGENTS.md and its single workflow
// into a temporary root, under the workflow's own name so the .yml and .yaml spellings
// stay part of what is being tested.
func plantWorkflowFixture(t *testing.T, fixture, workflow string) string {
	t.Helper()
	root := t.TempDir()
	workflows := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflows, 0o750); err != nil {
		t.Fatal(err)
	}
	for source, target := range map[string]string{
		"AGENTS.md": filepath.Join(root, "AGENTS.md"),
		workflow:    filepath.Join(workflows, workflow),
	} {
		content, err := os.ReadFile(filepath.Join(fixture, source))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// releasesPage carries the tokens the retention contract also requires, so the only
// problems left for these assertions are the ones the fixture workflow introduces.
func releasesPage() pageSet {
	return pageSet{
		"content/8. Community/8.2. Releases.md": "Evidence lives **7 days**; the immutable GitHub release and its OCI image outlive it.\n",
	}
}

// GitHub honors .yml and .yaml alike, so a workflow renamed by one letter used to walk
// straight past both of these gates while they reported green.
func TestWorkflowContractsSeeAYAMLWorkflow(t *testing.T) {
	root := plantWorkflowFixture(t, yamlWorkflowFixture, "publish.yaml")

	messages := problemMessages(checkPagesDeployment(root))
	if !strings.Contains(messages, `must be named "deploy"`) {
		t.Errorf("Pages deployment authority in a .yaml workflow was not seen: %s", messages)
	}

	messages = problemMessages(checkArtifactRetentionContract(root, releasesPage()))
	if !strings.Contains(messages, "retention 400 days, which exceeds the 7-day policy") {
		t.Errorf("artifact retention in a .yaml workflow was not seen: %s", messages)
	}
}

// One retention-days value cannot cover two upload steps. The count-based pairing let
// this workflow pass, and it also had no way to say which job was at fault.
func TestArtifactRetentionIsCheckedPerUploadStep(t *testing.T) {
	root := plantWorkflowFixture(t, twoUploadWorkflowFixture, "evidence.yml")

	messages := problemMessages(checkArtifactRetentionContract(root, releasesPage()))
	if !strings.Contains(messages, `job "collect" uploads an artifact without a numeric retention-days value`) {
		t.Errorf("upload step with no retention-days was not seen: %s", messages)
	}
	if strings.Contains(messages, `job "report"`) {
		t.Errorf("the job that declares a compliant retention-days was reported: %s", messages)
	}
}

// workflowFiles is the single reader both contracts go through, so it is what must see
// both spellings; a check that reintroduced its own glob would fail here first.
func TestWorkflowFilesListsBothSpellings(t *testing.T) {
	root := t.TempDir()
	workflows := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflows, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci.yml", "docs.yaml", "notes.md"} {
		if err := os.WriteFile(filepath.Join(workflows, name), []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := make([]string, 0, 2)
	for _, path := range workflowFiles(root) {
		found = append(found, filepath.Base(path))
	}
	if want := "ci.yml docs.yaml"; strings.Join(found, " ") != want {
		t.Fatalf("workflowFiles = %v, want %s", found, want)
	}
}
