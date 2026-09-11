package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// The skills the committed dataset ships. They are directory names rather than
// domain identifiers — a procedure, not an incident — so they are spelled here.
const (
	incidentTriageSkill = "incident-triage"
	remediationSkill    = "remediation"
)

// courseSkillsDir returns the committed skills directory, relative to this
// package. It is the same directory the runtime reads, unmodified.
func courseSkillsDir(t *testing.T) string {
	t.Helper()

	dir := SkillsDir(filepath.Join("..", "..", "data"))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the committed skills directory is missing: %v", err)
	}
	return dir
}

// TestSkillsAreDiscovered proves the runtime accepts the committed ADK Skill
// metadata and injects its reviewed catalog.
func TestSkillsAreDiscovered(t *testing.T) {
	t.Parallel()

	skills, err := NewSkills(t.Context(), courseSkillsDir(t))
	if err != nil {
		t.Fatalf("NewSkills() error = %v, want nil", err)
	}
	instruction := skillCatalog(t, skills)
	for _, name := range []string{incidentTriageSkill, remediationSkill} {
		if !strings.Contains(instruction, name) {
			t.Errorf("the injected catalog never mentions the %q skill", name)
		}
	}
}

// TestSkillToolsetIsInstructionOnly is the security assertion of this file.
//
// ADK always builds three tools and its config has no filter. The third,
// list_skills repeats the catalog already injected in the request, while
// load_skill_resource returns arbitrary files from a skill directory. The
// policy plane's trust carve-out deliberately covers only a reviewed SKILL.md
// body. Exactly one tool must reach the model.
func TestSkillToolsetIsInstructionOnly(t *testing.T) {
	t.Parallel()

	skills, err := NewSkills(t.Context(), courseSkillsDir(t))
	if err != nil {
		t.Fatalf("NewSkills() error = %v, want nil", err)
	}
	listed, err := skills.Tools(nil)
	if err != nil {
		t.Fatalf("Tools() error = %v, want nil", err)
	}
	want := []string{LoadSkillToolName}
	if got := toolNames(listed); !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want exactly %v", got, want)
	}
	if skills.Name() == "" {
		t.Error("Name() is empty; ADK identifies a toolset by it")
	}

	// The returned slice is a copy: a caller that appended a tool must not be
	// able to widen what the next request offers.
	listed[0] = namedTool{name: LoadSkillResourceToolName}
	again, err := skills.Tools(nil)
	if err != nil {
		t.Fatalf("Tools() error = %v, want nil", err)
	}
	if got := toolNames(again); !reflect.DeepEqual(got, want) {
		t.Errorf("tools after mutation = %v, want %v", got, want)
	}
}

func TestLoadSkillUsesExactReviewedNames(t *testing.T) {
	t.Parallel()

	skills, err := NewSkills(t.Context(), courseSkillsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	runner, ok := skills.LoadSkillTool().(interface {
		Run(agent.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatal("load_skill does not implement the ADK function-tool contract")
	}
	toolContext := noConfirmationContext{Context: newContext(t)}
	result, err := runner.Run(toolContext, map[string]any{"name": remediationSkill})
	if err != nil {
		t.Fatal(err)
	}
	instructions, ok := result["instructions"].(string)
	if !ok || result["skill_name"] != remediationSkill || !strings.Contains(instructions, "approval") {
		t.Fatalf("result = %#v", result)
	}
	for _, name := range []string{"unknown", "../../secret", "remediation/SKILL.md", ""} {
		if _, err := runner.Run(toolContext, map[string]any{"name": name}); err == nil {
			t.Errorf("load_skill accepted %q", name)
		}
	}
}

type noConfirmationContext struct{ agent.Context }

func (noConfirmationContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// TestSkillCatalogSurvivesTheFilter is the trap this type exists to avoid.
//
// ADK's own tool.FilterToolset would have applied the allowlist and silently
// dropped the catalog injection with it, because the LLM flow skips a toolset
// that does not implement the request processor. The model would then hold two
// skill tools and have no idea which skills they cover — a capability loss with
// no error anywhere.
func TestSkillCatalogSurvivesTheFilter(t *testing.T) {
	t.Parallel()

	skills, err := NewSkills(t.Context(), courseSkillsDir(t))
	if err != nil {
		t.Fatalf("NewSkills() error = %v, want nil", err)
	}
	instruction := skillCatalog(t, skills)
	if !strings.Contains(instruction, "<available_skills>") {
		t.Error("the skills catalog was not injected into the system instruction")
	}
	if !strings.Contains(instruction, "`"+LoadSkillToolName+"`") {
		t.Errorf("the system instruction never tells the model to call %s", LoadSkillToolName)
	}
	// The replacement instruction exists precisely so the model is not told to
	// call a tool this toolset does not expose.
	if strings.Contains(instruction, LoadSkillResourceToolName) {
		t.Errorf("the system instruction names %s, which is not offered", LoadSkillResourceToolName)
	}

	// ADK reaches the injection through a structural assertion, so satisfying
	// the method set is what makes the wiring work at all.
	var _ interface {
		tool.Toolset
		ProcessRequest(agent.Context, *model.LLMRequest) error
	} = skills
}

// TestLoadSkillToolIsAddressableByIdentity covers the policy plane's trust
// carve-out.
//
// ADK Go exports no type for the tool — all three are instances of an
// unexported generic parameterized by internal types — so identity is the only
// thing an external package can key on. It is also the only thing that is safe
// to key on: a name is something a remote MCP server chooses for itself.
func TestLoadSkillToolIsAddressableByIdentity(t *testing.T) {
	t.Parallel()

	skills, err := NewSkills(t.Context(), courseSkillsDir(t))
	if err != nil {
		t.Fatalf("NewSkills() error = %v, want nil", err)
	}
	trusted := skills.LoadSkillTool()
	if trusted == nil {
		t.Fatal("LoadSkillTool() = nil")
	}
	if trusted.Name() != LoadSkillToolName {
		t.Errorf("LoadSkillTool().Name() = %q, want %q", trusted.Name(), LoadSkillToolName)
	}

	listed, err := skills.Tools(nil)
	if err != nil {
		t.Fatalf("Tools() error = %v, want nil", err)
	}
	found := false
	for _, candidate := range listed {
		if candidate == trusted {
			found = true
		}
	}
	if !found {
		t.Error("LoadSkillTool() is not one of the values the toolset offers")
	}
	// An impostor with the right name is a different value, which is exactly
	// what makes the identity carve-out a boundary.
	if any(namedTool{name: LoadSkillToolName}) == any(trusted) {
		t.Error("a look-alike compares equal to the genuine tool")
	}
}

// TestRemediationSkillUsesTheRuntimeConfirmationBoundary proves the reviewed
// procedure sends the model through the guarded tool rather than describing an
// approval of its own.
func TestRemediationSkillUsesTheRuntimeConfirmationBoundary(t *testing.T) {
	t.Parallel()

	path := filepath.Join(courseSkillsDir(t), remediationSkill, "SKILL.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, phrase := range []string{
		"then call the guarded tool",
		"ADK pauses before execution",
		"initiating message is not approval",
	} {
		if !strings.Contains(string(body), phrase) {
			t.Errorf("%s lost the phrase %q", path, phrase)
		}
	}
}

// TestMissingSkillsDirectoryFailsAtStartup keeps a misconfigured AGENT_DATA_DIR
// loud: an agent that silently starts with no skills answers every "load the
// runbook procedure" request with an apology.
func TestMissingSkillsDirectoryFailsAtStartup(t *testing.T) {
	t.Parallel()

	if _, err := NewSkills(t.Context(), filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("NewSkills() error = nil, want a startup failure")
	}
}

// TestMalformedSkillFailsAtStartup pins the preload: front-matter is parsed
// when the toolset is built, so a broken SKILL.md fails at startup rather than
// on the turn a learner first asks for the procedure.
func TestMalformedSkillFailsAtStartup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	broken := filepath.Join(dir, "broken")
	if err := os.MkdirAll(broken, 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v, want nil", err)
	}
	// A key outside ADK's six is a hard parse error, which is what makes the
	// front-matter dialect worth pinning.
	content := "---\nname: broken\ndescription: A skill with an unknown key.\nunknown-key: nope\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(broken, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v, want nil", err)
	}
	if _, err := NewSkills(t.Context(), dir); err == nil {
		t.Error("NewSkills() error = nil, want the malformed skill to be refused")
	}
}

func TestDuplicateSkillNamesFailAtStartup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, folder := range []string{"first", "second"} {
		path := filepath.Join(dir, folder)
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: duplicated\ndescription: A duplicated reviewed skill.\n---\n\nBody.\n"
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewSkills(t.Context(), dir); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v", err)
	}
}

// TestSkillsDirIsUnderTheDataset pins where skills live: beside the shared
// dataset, so one AGENT_DATA_DIR moves both.
func TestSkillsDirIsUnderTheDataset(t *testing.T) {
	t.Parallel()

	if got, want := SkillsDir("/tmp/data"), filepath.Join("/tmp/data", "skills"); got != want {
		t.Errorf("SkillsDir() = %q, want %q", got, want)
	}
}

// skillCatalog runs the request processor and returns the system instruction it
// produced.
func skillCatalog(t *testing.T, skills *Skills) string {
	t.Helper()

	request := &model.LLMRequest{Config: &genai.GenerateContentConfig{}}
	if err := skills.ProcessRequest(newContext(t), request); err != nil {
		t.Fatalf("ProcessRequest() error = %v, want nil", err)
	}
	if request.Config.SystemInstruction == nil {
		t.Fatal("ProcessRequest() injected no system instruction")
	}
	var injected strings.Builder
	for _, part := range request.Config.SystemInstruction.Parts {
		injected.WriteString(part.Text)
	}
	return injected.String()
}
