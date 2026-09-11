package conventions

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	glanceOpen     = `{{% admonition abstract "In one glance" %}}`
	maxCollapsible = 3
)

var (
	frontMatterPattern = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n`)
	fencePattern       = regexp.MustCompile(`^\s*(` + "`{3,}" + `|~{3,})\s*(\S*)`)
	machinePathPattern = regexp.MustCompile(`/home/[^ /]+|file:///|k3d-registry\.localhost`)
	collapsiblePattern = regexp.MustCompile(`^\{\{% collapsible \w+ "([^"]*)" %\}\}`)
	bareNumberLabel    = regexp.MustCompile(`\[(\d+(?:\.\d+)*)\]\(`)
	timeLinePattern    = regexp.MustCompile(`(?m)^- \*\*Time:\*\* (.+)$`)
	admonitionPattern  = regexp.MustCompile(`^\{\{% (?:admonition|collapsible) ([a-z-]+)(?: "[^"]*")? %\}\}$`)
	orderedItemPattern = regexp.MustCompile(`^(\s*)(\d+)\.\s`)
	fullPageLink       = regexp.MustCompile(`\[([0-8]\.\d+\.\s+[^]]+)\]\(\{\{< relref "([^"#]+)`)
	permalinkSlug      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	exerciseMode       = regexp.MustCompile(`\*\*Mode\*\*:\s*` + "`" + `(inspect|temporary experiment|keep|capstone carry-forward)` + "`")
	probabilistic      = regexp.MustCompile(`(?i)\bmay pass\b`)
	mandatoryRed       = regexp.MustCompile(`(?i)\b(?:must fail|fails? without|exits? non-zero)\b`)
	directoryRestore   = regexp.MustCompile(`git restore -- (?:agents/data|infra/k8s)(?:\s|` + "`" + `|$)`)
	// An H2 that ends in a question mark, with or without a trailing anchor override.
	interrogativeHeading = regexp.MustCompile(`\?\s*(?:\{#[^}]*\})?\s*$`)
)

var (
	glanceFields = []string{"**You will:**", "**You need:**", "**Time:**"}
	// A page closes by naming what the learner can now do, rather than by asking itself
	// whether it worked. One spelling per page kind: course pages, chapter indexes, and
	// the four pure lookup pages.
	closingHeads = []string{
		"## What you can do now",
		"## What this chapter proved",
		"## How to use this page later",
	}
	// The two pages whose H2s are the symptoms and terms a reader scans for. A question
	// is the entry a reader searches with there, so the one-per-page cap does not apply.
	scannableLookupPages = map[string]bool{
		"content/0. Overview/0.7. Troubleshooting.md": true,
		"content/0. Overview/0.8. Glossary.md":        true,
	}
	pageKinds          = []string{"concept", "hands-on", "reference", "orientation", "lookup"}
	allowedAdmonitions = map[string]bool{
		"abstract": true, "danger": true, "info": true, "note": true,
		"success": true, "tip": true, "warning": true,
	}
)

type numberedLine struct {
	text   string
	number int
}

type fencedBlock struct {
	body  []string
	start int
	end   int
}

// fenceRole says what a scanned line is to the fence structure around it.
type fenceRole int

const (
	fenceBody fenceRole = iota
	fenceOpen
	fenceClose
)

// scannedLine is one source line as the single fence scanner sees it.
type scannedLine struct {
	text     string
	language string // the info string, on an opening delimiter only
	number   int
	role     fenceRole
	inside   bool
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(text, "\n")
}

func linesOutsideFences(text string) []numberedLine { return linesByFence(text, false) }

func linesInsideFences(text string) []numberedLine { return linesByFence(text, true) }

// scanFences is the one place this package decides where a fenced block starts and ends.
//
// Two scanners used to answer that question differently. fencedBlocks tracked the
// opening delimiter's character and run length, as CommonMark requires; linesByFence
// toggled a boolean on any line starting with three backticks or three tildes. A legal
// four-backtick block quoting a three-backtick example therefore flipped the parity for
// the rest of the page, and every per-page rule that walks the lines outside fences
// silently stopped applying from there on. One scanner cannot disagree with itself.
func scanFences(text string) []scannedLine {
	lines := splitLines(text)
	scanned := make([]scannedLine, 0, len(lines))
	delimiter := ""
	for index, line := range lines {
		entry := scannedLine{number: index + 1, text: line, inside: delimiter != ""}
		match := fencePattern.FindStringSubmatch(line)
		switch {
		case delimiter == "" && match != nil:
			delimiter = match[1]
			entry.role, entry.language = fenceOpen, match[2]
		// A closing fence repeats the opener's character, runs at least as long, and
		// carries no info string. Anything else is content of the block still open.
		case delimiter != "" && match != nil && match[2] == "" &&
			match[1][0] == delimiter[0] && len(match[1]) >= len(delimiter):
			delimiter = ""
			entry.role = fenceClose
		}
		scanned = append(scanned, entry)
	}
	return scanned
}

func linesByFence(text string, inside bool) []numberedLine {
	result := make([]numberedLine, 0)
	for _, line := range scanFences(text) {
		// Both delimiter lines belong to neither set: they are the block's punctuation,
		// so they are neither its content nor the prose around it.
		if line.role != fenceBody || line.inside != inside {
			continue
		}
		result = append(result, numberedLine{number: line.number, text: line.text})
	}
	return result
}

func fencedBlocks(text, language string) []fencedBlock {
	var result []fencedBlock
	var blockLanguage string
	start := 0
	body := make([]string, 0)
	for _, line := range scanFences(text) {
		switch line.role {
		case fenceOpen:
			blockLanguage, start = line.language, line.number
			body = body[:0]
		case fenceClose:
			if blockLanguage == language {
				result = append(result, fencedBlock{start: start, end: line.number, body: slices.Clone(body)})
			}
		case fenceBody:
			if line.inside {
				body = append(body, line.text)
			}
		}
	}
	return result
}

func parseFrontMatter(text string) (map[string]any, string) {
	match := frontMatterPattern.FindStringSubmatch(text)
	if match == nil {
		return nil, "expected YAML front matter at the start of the file"
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(match[1]), &document); err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		return nil, fmt.Sprintf("front matter is not valid YAML (%s); quote values containing ': '", first)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, "front matter must be a YAML mapping"
	}
	metadata := make(map[string]any)
	if err := document.Content[0].Decode(&metadata); err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		return nil, fmt.Sprintf("front matter is not valid YAML (%s); quote values containing ': '", first)
	}
	return metadata, ""
}

func declaredKind(text string) string {
	match := timeLinePattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	found := make([]string, 0, 1)
	for _, kind := range pageKinds {
		pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(kind) + `([^A-Za-z0-9_-]|$)`)
		if pattern.MatchString(match[1]) {
			found = append(found, kind)
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

func checkFrontMatter(where, text string) []Problem {
	metadata, message := parseFrontMatter(text)
	if metadata == nil {
		return []Problem{problem(where, "%s", message)}
	}
	description, ok := metadata["description"].(string)
	if !ok || strings.TrimSpace(description) == "" {
		return []Problem{problem(where, "front matter must define a non-empty description")}
	}
	return nil
}

func checkPageURL(root, where, path, text string) []Problem {
	metadata, _ := parseFrontMatter(text)
	if metadata == nil {
		return nil
	}
	title, ok := metadata["title"].(string)
	if !ok || strings.TrimSpace(title) == "" {
		return []Problem{problem(where, "front matter must define a non-empty title (Hextra renders it as the page <h1>)")}
	}
	var problems []Problem
	if _, shadowsSlug := metadata["url"]; shadowsSlug {
		problems = append(problems, problem(where, "front matter must not define url; it shadows the reviewed Hugo slug and permalink contract"))
	}
	isHome := filepath.Clean(path) == filepath.Join(root, "content", "_index.md")
	slug, hasSlug := metadata["slug"].(string)
	if isHome {
		if _, manufactured := metadata["slug"]; manufactured {
			problems = append(problems, problem(where, "the home page stays at / and must not manufacture a slug"))
		}
	} else if !hasSlug || !permalinkSlug.MatchString(slug) {
		problems = append(problems, problem(where, "front matter slug must be explicit lowercase kebab-case, found %q", metadata["slug"]))
	}
	for _, line := range linesOutsideFences(text) {
		if strings.HasPrefix(line.text, "# ") {
			problems = append(problems, problem(where, "the page title lives in front matter; a Markdown H1 would publish a second <h1>"))
			break
		}
	}
	return problems
}

// checkHeadings requires a page to be sectioned, and caps its interrogative headings.
//
// The frame used to require every H2 to end with a question mark, which turned every
// page into one flat FAQ where no heading could carry more weight than its neighbors.
// The teaching-style contract inverts that: a heading states what its section proves,
// and one question per page is reserved for the tension the page actually resolves.
// The two exceptions are lookup pages whose headings are the symptoms and terms a
// reader scans for, so a question is the entry the reader is searching with.
func checkHeadings(where, text string) []Problem {
	sectioned := false
	questions := 0
	for _, line := range linesOutsideFences(text) {
		if !strings.HasPrefix(line.text, "## ") {
			continue
		}
		sectioned = true
		if interrogativeHeading.MatchString(line.text) {
			questions++
		}
	}
	if !sectioned {
		return []Problem{problem(where, "expected at least one H2 section heading")}
	}
	if questions > 1 && !scannableLookupPages[where] {
		return []Problem{problem(where, "a heading states what its section proves; %d H2s ask a question, and only one per page may", questions)}
	}
	return nil
}

func checkGlance(where, text string) []Problem {
	inside := false
	seen := make(map[string]bool)
	for _, line := range linesOutsideFences(text) {
		if strings.HasPrefix(line.text, "## ") {
			break
		}
		if line.text == glanceOpen {
			inside = true
			continue
		}
		if inside {
			for _, field := range glanceFields {
				seen[field] = seen[field] || strings.HasPrefix(line.text, "- "+field)
			}
		}
	}
	missing := make([]string, 0)
	for _, field := range glanceFields {
		if !seen[field] {
			missing = append(missing, field)
		}
	}
	// Count satisfied fields, not map keys: the loop above writes a key for every field
	// on every line, so a length comparison here accepted a block missing two of three.
	if inside && len(missing) == 0 {
		return nil
	}
	return []Problem{problem(where, `expected an "In one glance" abstract block between the H1 and the first H2 (missing: %s)`, strings.Join(missing, ", "))}
}

func checkClosing(where, text string) []Problem {
	var headings []string
	for _, line := range linesOutsideFences(text) {
		if strings.HasPrefix(line.text, "## ") {
			headings = append(headings, line.text)
		}
	}
	if len(headings) == 0 || slices.Contains(closingHeads, headings[len(headings)-1]) {
		return nil
	}
	allowed := make([]string, len(closingHeads))
	for index, heading := range closingHeads {
		allowed[index] = fmt.Sprintf("%q", strings.TrimPrefix(heading, "## "))
	}
	return []Problem{problem(where, "last H2 must be one of %s, not: %s", strings.Join(allowed, " | "), headings[len(headings)-1])}
}

func checkKind(where, text string) []Problem {
	if declaredKind(text) != "" {
		return nil
	}
	return []Problem{problem(where, "the Time line must name exactly one page kind: %s", strings.Join(pageKinds, ", "))}
}
