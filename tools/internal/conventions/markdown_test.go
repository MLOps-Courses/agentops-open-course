package conventions

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckPageURLRequiresSlugAndRejectsURLShadowing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "content", "0. Overview", "0.7. Glossary.md")
	text := "---\ntitle: \"0.7. Glossary\"\ndescription: x\nslug: \"0-8-glossary\"\nurl: \"/wrong/\"\n---\n\n# Duplicate\n\n## Why?\n"
	problems := checkPageURL(root, "content/0. Overview/0.8. Glossary.md", path, text)
	messages := problemMessages(problems)
	if !strings.Contains(messages, "must not define url") || !strings.Contains(messages, "would publish a second <h1>") {
		t.Fatalf("problems = %#v", problems)
	}

	valid := "---\ntitle: \"0.7. Glossary\"\ndescription: x\nslug: \"0-8-glossary\"\n---\n\n## Why?\n"
	if problems := checkPageURL(root, "content/0. Overview/0.8. Glossary.md", path, valid); len(problems) != 0 {
		t.Fatalf("valid slug problems = %#v", problems)
	}
}

func TestCheckPageURLUsesSectionSlugAndLeavesHomeAtRoot(t *testing.T) {
	root := t.TempDir()
	sectionPath := filepath.Join(root, "content", "2. Agents", "_index.md")
	section := "---\ntitle: \"2. Agents\"\ndescription: x\nslug: \"2-agents\"\n---\n\n## Why?\n"
	if problems := checkPageURL(root, "content/2. Agents/_index.md", sectionPath, section); len(problems) != 0 {
		t.Fatalf("section slug problems = %#v", problems)
	}
	homePath := filepath.Join(root, "content", "_index.md")
	home := "---\ntitle: Home\ndescription: x\n---\n\n## Why?\n"
	if problems := checkPageURL(root, "content/_index.md", homePath, home); len(problems) != 0 {
		t.Fatalf("home route problems = %#v", problems)
	}
}

func TestBuildPageRoutesDerivesHierarchyAndRejectsCollisions(t *testing.T) {
	pages := pageSet{
		"content/_index.md":                         "---\ntitle: Home\ndescription: x\n---\n",
		"content/2. Agents/_index.md":               "---\ntitle: Agents\ndescription: x\nslug: 2-agents\n---\n",
		"content/2. Agents/2.1. First Agent.md":     "---\ntitle: First\ndescription: x\nslug: 2-1-first-agent\n---\n",
		"content/2. Agents/2.1. Duplicate Route.md": "---\ntitle: Duplicate\ndescription: x\nslug: 2-1-first-agent\n---\n",
		"content/3. Capabilities/_index.md":         "---\ntitle: Capabilities\ndescription: x\nslug: 3-capabilities\n---\n",
		"content/3. Capabilities/Duplicate Slug.md": "---\ntitle: Duplicate\ndescription: x\nslug: 2-1-first-agent\n---\n",
	}
	routes, problems := buildPageRoutes(pages)
	if got := routes["content/_index.md"].Path; got != "/" {
		t.Errorf("home route = %q, want /", got)
	}
	if got := routes["content/2. Agents/_index.md"].Path; got != "/2-agents/" {
		t.Errorf("section route = %q", got)
	}
	if got := routes["content/2. Agents/2.1. First Agent.md"].Path; got != "/2-agents/2-1-first-agent/" {
		t.Errorf("regular route = %q", got)
	}
	if !strings.Contains(problemMessages(problems), "same permalink") {
		t.Fatalf("collision problems = %#v", problems)
	}
	if !strings.Contains(problemMessages(problems), "duplicate regular-page slug") {
		t.Fatalf("duplicate slug problems = %#v", problems)
	}
}

func TestCheckPermalinkConfigRequiresSlugPatterns(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "hugo.toml")
	valid := `[[permalinks]]
pattern = "/:sectionslugs/:slug/"
[permalinks.target]
kind = "page"

[[permalinks]]
pattern = "/:sectionslugs/"
[permalinks.target]
kind = "section"
`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if problems := checkPermalinkConfig(root); len(problems) != 0 {
		t.Fatalf("valid permalink config problems = %#v", problems)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(valid, ":sectionslugs", ":sections", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if messages := problemMessages(checkPermalinkConfig(root)); !strings.Contains(messages, ":sectionslugs/:slug") {
		t.Fatalf("missing slug contract problems = %s", messages)
	}
}

func TestCheckFrontMatterUsesYAMLParser(t *testing.T) {
	text := "---\ntitle: Example\ndescription: invalid: colon\n---\n\n## Why?\n"
	problems := checkFrontMatter("content/example.md", text)
	if len(problems) != 1 || !strings.Contains(problems[0].Message, "not valid YAML") {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestCheckSnippetTargetsRequiresOneBoundedRegion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "infra", "example.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# --8<-- [start:trusted]\nvalue: 1\n# --8<-- [end:trusted]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text := `{{< include path="infra/example.yaml" region="trusted" lang="yaml" >}}` + "\n"
	if problems := checkSnippetTargets(root, "content/example.md", text); len(problems) != 0 {
		t.Fatalf("problems = %#v", problems)
	}
	if err := os.WriteFile(path, []byte("# --8<-- [start:trusted]\nvalue: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	problems := checkSnippetTargets(root, "content/example.md", text)
	if !strings.Contains(problemMessages(problems), "exactly one start and end marker") {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestCheckSnippetsRejectsFenceAndRetiredSyntax(t *testing.T) {
	text := "```go\n{{< include path=\"a.go\" region=\"b\" >}}\n```\n--8<-- \"a.go:b\"\n"
	messages := problemMessages(checkSnippets("content/example.md", text))
	if !strings.Contains(messages, "must not sit inside") || !strings.Contains(messages, "Material snippet syntax is retired") {
		t.Fatal(messages)
	}
}

// Hugo renders far more spellings than the canonical one. Each of these reaches a real
// reader, so each must reach the checker: a call the checker cannot see is a snippet
// nobody verifies against its source.
func TestCheckSnippetTargetsSeesEverySpellingHugoRenders(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "infra"), 0o750); err != nil {
		t.Fatal(err)
	}
	// The source exists but carries no markers, so any call the checker sees produces a
	// problem naming the region, and a call it misses produces silence.
	if err := os.WriteFile(filepath.Join(root, "infra", "example.yaml"), []byte("value: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"percent delimiter":   `{{% include path="infra/example.yaml" region="trusted" %}}`,
		"no inner space":      `{{<include path="infra/example.yaml" region="trusted">}}`,
		"extra whitespace":    "{{<   include    path=\"infra/example.yaml\"   region=\"trusted\"   >}}",
		"reversed order":      `{{< include region="trusted" path="infra/example.yaml" >}}`,
		"backtick values":     "{{< include path=`infra/example.yaml` region=`trusted` >}}",
		"bare values":         `{{< include path=infra/example.yaml region=trusted >}}`,
		"call spanning lines": "{{< include\n     path=\"infra/example.yaml\"\n     region=\"trusted\" >}}",
		"trailing prose":      `Text before {{< include path="infra/example.yaml" region="trusted" >}} and after.`,
	} {
		t.Run(name, func(t *testing.T) {
			messages := problemMessages(checkSnippetTargets(root, "content/example.md", text+"\n"))
			if !strings.Contains(messages, `"trusted"`) {
				t.Fatalf("checker did not see the call; problems = %q", messages)
			}
		})
	}

	// Names that merely start with "include", commented-out calls, and other shortcodes
	// must stay invisible, or the checker invents failures for text Hugo never renders.
	for name, text := range map[string]string{
		"different shortcode": `{{< relref "/0. Overview/0.1. Agents.md" >}}`,
		"longer name":         `{{< includes path="infra/example.yaml" region="trusted" >}}`,
		"hyphenated name":     `{{< include-extra path="infra/example.yaml" region="trusted" >}}`,
		"commented out":       `{{</* include path="infra/example.yaml" region="trusted" */>}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if problems := checkSnippetTargets(root, "content/example.md", text+"\n"); len(problems) != 0 {
				t.Fatalf("problems = %#v", problems)
			}
		})
	}
}

func TestCheckSnippetTargetsReportsAMissingPath(t *testing.T) {
	problems := checkSnippetTargets(t.TempDir(), "content/example.md", `{{< include "infra/example.yaml" >}}`+"\n")
	if !strings.Contains(problemMessages(problems), "must name a source path") {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestCheckExercisesPreservesTemporaryAndProbabilisticFailures(t *testing.T) {
	temporary := `## Your turn: what changes?

- **Mode**: ` + "`temporary experiment`" + `
- **Goal**: Change one thing.
- **Files to touch**: example.go.
- **Preflight**: Start clean.
- **Steps**: Change it, then run the gate.
- **Gate that proves completion**: The test is red.
- **Final state**: Restore it.
`
	if problems := checkExercises("content/example.md", temporary); len(problems) != 2 {
		t.Fatalf("temporary problems = %#v", problems)
	}
	probabilisticText := `## Your turn: what changes?

- **Mode**: ` + "`inspect`" + `
- **Goal**: Observe.
- **Files to touch**: None.
- **Preflight**: None.
- **Steps**: Read it.
- **Gate that proves completion**: It fails without the rule, but may pass.
- **Final state**: Clean.
`
	if !strings.Contains(problemMessages(checkExercises("content/example.md", probabilisticText)), "probabilistic evidence") {
		t.Fatal("mandatory probabilistic red-state was accepted")
	}
}

// A mode outside the closed set matches nothing, and the dirty-preflight and cleanup
// rules only run for `temporary experiment`. An unrecognized spelling therefore used to
// buy an exercise its way out of both rules while every required field was present.
func TestCheckExercisesRejectsAnUnknownMode(t *testing.T) {
	unknown := `## Your turn: what changes?

- **Mode**: ` + "`sandbox experiment`" + `
- **Goal**: Change one thing.
- **Files to touch**: example.go.
- **Preflight**: Start clean.
- **Steps**: Change it, then run the gate.
- **Gate that proves completion**: The test is red.
- **Final state**: Restore it.
`
	messages := problemMessages(checkExercises("content/example.md", unknown))
	if !strings.Contains(messages, "exercise mode must be one of") {
		t.Fatalf("unknown exercise mode was accepted: %s", messages)
	}
}

func TestCheckDiagramAlternatives(t *testing.T) {
	diagram := "```mermaid\nflowchart LR\nA --> B\n```\n"
	runPageRuleCases(t, checkDiagramAlternatives, []pageRuleCase{
		{name: "prose below", text: diagram + "\n**Diagram in words:** A leads to B.\n"},
		{name: "prose above", text: "**Diagram in words:** A leads to B.\n\n" + diagram},
		{name: "no prose", text: diagram, want: "needs adjacent `**Diagram in words:**` prose"},
		{
			// The rule is unconditional now; there is no allowlist to inherit from.
			name: "prose too far away",
			text: diagram + strings.Repeat("\nfiller\n", 6) + "**Diagram in words:** A leads to B.\n",
			want: "needs adjacent `**Diagram in words:**` prose",
		},
	})
}

// One scanner now answers where a fenced block starts and ends, so the line sets and
// the block bodies cannot drift apart. These are the cases where a length-blind toggle
// and a CommonMark scanner disagree.
func TestScanFencesTracksDelimiterCharacterAndLength(t *testing.T) {
	for _, test := range []struct {
		name    string
		text    string
		outside []int
		inside  []int
	}{
		{
			// The four-backtick block quotes a three-backtick example. The inner opener
			// is content, not a delimiter, so the parity survives it.
			name:    "longer fence quoting a shorter opener",
			text:    "before\n````markdown\n```go\ncode\n```\n````\nafter\n",
			outside: []int{1, 7, 8},
			inside:  []int{3, 4, 5},
		},
		{
			name:    "tilde block quoting backticks",
			text:    "~~~text\n```go\n~~~\nafter\n",
			outside: []int{4, 5},
			inside:  []int{2},
		},
		{
			name:    "closing fence longer than the opener",
			text:    "```go\ncode\n``````\nafter\n",
			outside: []int{4, 5},
			inside:  []int{2},
		},
		{
			name:    "unterminated fence at end of file",
			text:    "before\n```go\ncode\nmore\n",
			outside: []int{1},
			inside:  []int{3, 4, 5},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := lineNumbers(linesOutsideFences(test.text)); !slices.Equal(got, test.outside) {
				t.Errorf("outside = %v, want %v", got, test.outside)
			}
			if got := lineNumbers(linesInsideFences(test.text)); !slices.Equal(got, test.inside) {
				t.Errorf("inside = %v, want %v", got, test.inside)
			}
		})
	}
}

// fencedBlocks reads the same scan, so the inner opener cannot become a block of its own.
func TestFencedBlocksIgnoreAQuotedOpener(t *testing.T) {
	text := "````markdown\n```go\ncode\n```\n````\n"
	if blocks := fencedBlocks(text, "go"); len(blocks) != 0 {
		t.Fatalf("quoted opener became a Go block: %#v", blocks)
	}
	blocks := fencedBlocks(text, "markdown")
	if len(blocks) != 1 || blocks[0].start != 1 || blocks[0].end != 5 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if want := []string{"```go", "code", "```"}; !slices.Equal(blocks[0].body, want) {
		t.Fatalf("body = %#v, want %#v", blocks[0].body, want)
	}
}

// The parity bug's real cost was silence downstream: every rule that walks the lines
// outside fences stopped applying to the rest of the page after one nested block.
func TestPageRulesStillApplyAfterANestedFence(t *testing.T) {
	// One unmatched inner opener is all it took: the old toggle left the page parked
	// inside a fence, so nothing after this block was ever inspected again.
	text := "````markdown\n```go\n````\n\nExactly 16 tests pass.\n"
	problems := checkExactCountClaims("content/example.md", text)
	if len(problems) != 1 || !strings.Contains(problems[0].Message, "line 5:") {
		t.Fatalf("problems = %#v", problems)
	}
}

func lineNumbers(lines []numberedLine) []int {
	numbers := make([]int, len(lines))
	for index, line := range lines {
		numbers[index] = line.number
	}
	return numbers
}

func TestExactCountClaimsIgnoreFences(t *testing.T) {
	text := "Exactly 16 tests pass.\n```text\nExactly 4 tests pass.\n```\n"
	problems := checkExactCountClaims("content/example.md", text)
	if len(problems) != 1 || problems[0].Message != "line 1: replace brittle exact line/test/module count with derived or count-free evidence" {
		t.Fatalf("problems = %#v", problems)
	}
}

func problemMessages(problems []Problem) string {
	values := make([]string, len(problems))
	for index, item := range problems {
		values[index] = item.Message
	}
	return strings.Join(values, "\n")
}
