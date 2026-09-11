package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// Agent Skills — progressive-disclosure instructions (Chapter 3.2).
//
// A skill is a folder with a SKILL.md carrying name and description
// front-matter followed by instructions, plus optional assets and references.
// The model first sees only each skill's name and description and loads the
// full body on demand, which is what keeps the context small. Skills live in
// agents/data/skills alongside the shared dataset, and the format is the open
// Agent Skills standard that AI coding assistants use — the same files serve
// both tracks of this course unchanged.

// The three tools ADK's skill toolset constructs.
const (
	// ListSkillsToolName is ADK's redundant catalog tool. The catalog is
	// injected on every request, so this composition does not expose it.
	ListSkillsToolName = "list_skills"
	// LoadSkillToolName returns one reviewed SKILL.md body.
	LoadSkillToolName = "load_skill"
	// LoadSkillResourceToolName returns an arbitrary file from a skill
	// directory. This composition never exposes it; see [NewSkills].
	LoadSkillResourceToolName = "load_skill_resource"
)

// skillsDirectory is the sub-directory of the dataset holding the skills.
const skillsDirectory = "skills"

// skillSystemInstruction replaces ADK's built-in skills instruction.
//
// The default one is written for the full three-tool surface and tells the
// model, in its own numbered step 3, to reach for load_skill_resource. Since
// this toolset does not expose that tool, shipping the default text would
// instruct the model to call something that does not exist — which is worse
// than useless: it teaches the model that a named tool can be absent.
const skillSystemInstruction = "You can use specialized 'skills' to help you with complex tasks. " +
	"You MUST use the skill tools to interact with these skills.\n\n" +
	"A skill is a folder of reviewed instructions that extends your capabilities for a specialized task. " +
	"You are shown only each skill's name and description; the full instructions are loaded on demand.\n\n" +
	"This is very important:\n\n" +
	"1. If a skill seems relevant to the current request, you MUST call the `" + LoadSkillToolName +
	"` tool with `name=\"<SKILL_NAME>\"` to read its full instructions before proceeding.\n" +
	"2. Once you have read the instructions, follow them exactly as documented before replying. " +
	"If they list multiple steps, complete all of them, in order.\n"

// ErrSkills marks a failure to build the skill toolset.
var ErrSkills = errors.New("building the skill toolset")

// SkillsDir returns the directory holding the AgentOps Agent skills.
func SkillsDir(dataDir string) string { return filepath.Join(dataDir, skillsDirectory) }

// Skills is a least-privilege, instruction-only view of ADK's skill toolset.
//
// ADK always builds three tools — list_skills, load_skill and
// load_skill_resource — and its Config has no filter. Only load_skill reaches
// the model. list_skills repeats the catalog already injected on every request,
// while load_skill_resource returns arbitrary files from a skill directory.
// The policy plane's trust carve-out deliberately covers only the reviewed
// SKILL.md body returned by load_skill.
//
// ADK's own tool.FilterToolset cannot be used to apply that pin. Its wrapper
// implements only Name and Tools, and the LLM flow silently skips a toolset
// that does not also implement the request processor — so wrapping the skill
// toolset in it would drop the skills catalog and the system instruction
// entirely, and the model would never learn that any skill exists. This type
// filters the tools and forwards the catalog injection, which is the only
// combination that keeps both properties.
type Skills struct {
	inner *skilltoolset.SkillToolset
	tools []tool.Tool
}

// NewSkills builds the instruction-only skill toolset over a skills directory.
//
// Front-matter is preloaded, so a malformed SKILL.md fails at startup rather
// than on the turn a learner first asks for a procedure. Only the front-matter
// is preloaded, not the bodies and resources: the catalog is what reaches the
// model as instruction text on every request, and resources are never read at
// all because load_skill_resource is not exposed.
func NewSkills(ctx context.Context, dir string) (*Skills, error) {
	source, _, err := skill.WithFrontmatterPreloadSource(ctx, skill.NewFileSystemSource(os.DirFS(dir)))
	if err != nil {
		return nil, fmt.Errorf("%w from %s: %w", ErrSkills, dir, err)
	}
	inner, err := skilltoolset.New(ctx, skilltoolset.Config{
		Source:            source,
		SystemInstruction: skillSystemInstruction,
	})
	if err != nil {
		return nil, fmt.Errorf("%w from %s: %w", ErrSkills, dir, err)
	}

	// The allowlist is applied once, here, rather than on every Tools call. The
	// toolset's contents are fixed at construction — ADK builds all three tools
	// eagerly and its Tools method ignores the context it is handed — and the
	// policy plane needs the load_skill value at wiring time anyway, so the
	// filter and the trust carve-out read the same slice.
	allowed := []string{LoadSkillToolName}
	built, err := inner.Tools(nil)
	if err != nil {
		return nil, fmt.Errorf("%w from %s: %w", ErrSkills, dir, err)
	}
	kept := make([]tool.Tool, 0, len(allowed))
	for _, candidate := range built {
		if slices.Contains(allowed, candidate.Name()) {
			kept = append(kept, candidate)
		}
	}
	if len(kept) != len(allowed) {
		return nil, fmt.Errorf(
			"%w from %s: the toolset offered %d of the %d required tools %v",
			ErrSkills, dir, len(kept), len(allowed), allowed,
		)
	}
	return &Skills{inner: inner, tools: kept}, nil
}

// Name implements tool.Toolset.
func (s *Skills) Name() string { return s.inner.Name() }

// Tools implements tool.Toolset, returning only the allowed tools.
func (s *Skills) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return slices.Clone(s.tools), nil
}

// ProcessRequest forwards ADK's catalog injection.
//
// This method is why [Skills] exists rather than a tool.FilterToolset call: the
// LLM flow type-asserts each toolset for it and silently continues past a
// toolset that lacks it, so a filtered toolset that did not forward it would
// leave the model with two skill tools and no idea which skills they cover.
func (s *Skills) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := s.inner.ProcessRequest(ctx, req); err != nil {
		return fmt.Errorf("injecting the skills catalog: %w", err)
	}
	return nil
}

// LoadSkillTool returns the tool whose output is reviewed repository
// instruction.
//
// It exists for the policy plane's trust carve-out, which is keyed on this
// exact value. ADK Go exports no type for the tool — all three are instances of
// an unexported generic parameterized by internal types — so identity is the
// only thing an external package can key on, and it is also the only thing that
// is safe to key on: a name is something a remote MCP server chooses for
// itself.
func (s *Skills) LoadSkillTool() tool.Tool {
	for _, candidate := range s.tools {
		if candidate.Name() == LoadSkillToolName {
			return candidate
		}
	}
	// Unreachable: NewSkills refuses to return a toolset missing load_skill.
	return nil
}
