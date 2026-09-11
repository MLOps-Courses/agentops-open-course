package conventions

import (
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Hugo honors both shortcode delimiters, arbitrary whitespace including newlines, either
// attribute order, and quoted, backtick-quoted, or bare values. Matching only the canonical
// spelling meant every other spelling the site renders for real passed unchecked, so match
// the call first and read its attributes separately. snippetIncludeOpener catches a call
// that merely starts on a line, which is what the fenced-block guard needs.
var (
	snippetIncludePattern = regexp.MustCompile(`(?s)\{\{[<%]\s*include(\s.*?|\s*)(?:/\s*)?[>%]\}\}`)
	snippetIncludeOpener  = regexp.MustCompile(`\{\{[<%]\s*include(?:\s|/|[>%]\}|$)`)
	shortcodeParamPattern = regexp.MustCompile("(?s)([A-Za-z][A-Za-z0-9_-]*)\\s*=\\s*(?:\"([^\"]*)\"|`([^`]*)`|([^\\s]+))")
)

// shortcodeParam returns the value Hugo would hand `.Get name`, and whether the call set it.
func shortcodeParam(arguments, name string) (string, bool) {
	for _, match := range shortcodeParamPattern.FindAllStringSubmatch(arguments, -1) {
		if match[1] != name {
			continue
		}
		// Exactly one of the quoted, backtick, or bare alternatives captures; an all-empty
		// match is a genuine `region=""`, which the caller must still see as set.
		for _, group := range match[2:] {
			if group != "" {
				return group, true
			}
		}
		return "", true
	}
	return "", false
}

// lineNumber converts a byte offset in text into the 1-based line the offset sits on.
func lineNumber(text string, offset int) int {
	return 1 + strings.Count(text[:offset], "\n")
}

func checkCollapsibles(where, text string) []Problem {
	var problems []Problem
	count := 0
	for _, line := range splitLines(text) {
		match := collapsiblePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		count++
		if !strings.HasPrefix(match[1], "Deeper: ") {
			problems = append(problems, problem(where, `collapsible summary must start with "Deeper: ": %s`, match[1]))
		}
	}
	if count > maxCollapsible {
		problems = append(problems, problem(where, "%d collapsibles; at most %d keep a page readable", count, maxCollapsible))
	}
	return problems
}

func checkLinkLabels(where, text string) []Problem {
	matches := bareNumberLabel.FindAllStringSubmatch(text, -1)
	problems := make([]Problem, 0, len(matches))
	for _, match := range matches {
		problems = append(problems, problem(where, "link label must be the page name, not a bare number: [%s]", match[1]))
	}
	return problems
}

func checkSnippets(where, text string) []Problem {
	var problems []Problem
	for _, line := range linesInsideFences(text) {
		if snippetIncludeOpener.MatchString(line.text) {
			problems = append(problems, problem(where, "line %d: include shortcode must not sit inside a fenced code block", line.number))
		}
	}
	for _, line := range linesOutsideFences(text) {
		if strings.HasPrefix(strings.TrimSpace(line.text), "--8<--") {
			problems = append(problems, problem(where, "line %d: Material snippet syntax is retired; use the include shortcode", line.number))
		}
	}
	return problems
}

func checkSnippetTargets(root, where, text string) []Problem {
	var problems []Problem
	// Scanned over the whole text rather than line by line: a shortcode call may legally
	// span several lines, which a per-line loop cannot see at all.
	for _, location := range snippetIncludePattern.FindAllStringSubmatchIndex(text, -1) {
		arguments := text[location[2]:location[3]]
		line := lineNumber(text, location[0])
		relative, hasPath := shortcodeParam(arguments, "path")
		region, hasRegion := shortcodeParam(arguments, "region")
		if !hasPath {
			problems = append(problems, problem(where, "line %d: include shortcode must name a source path", line))
			continue
		}
		cleaned := filepath.Clean(filepath.FromSlash(relative))
		if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			problems = append(problems, problem(where, "line %d: snippet source is outside the repository: %q", line, relative))
			continue
		}
		source, err := readFile(filepath.Join(root, cleaned))
		if err != nil {
			problems = append(problems, problem(where, "line %d: snippet source does not exist: %q", line, relative))
			continue
		}
		if !hasRegion || region == "" {
			problems = append(problems, problem(where, "line %d: trusted snippet must name a source region: %q", line, relative))
			continue
		}
		start := "--8<-- [start:" + region + "]"
		end := "--8<-- [end:" + region + "]"
		if strings.Count(source, start) != 1 || strings.Count(source, end) != 1 {
			problems = append(problems, problem(where, "line %d: snippet region %q needs exactly one start and end marker in %s", line, region, relative))
		} else if strings.Index(source, start) >= strings.Index(source, end) {
			problems = append(problems, problem(where, "line %d: snippet region %q ends before it starts", line, region))
		}
	}
	return problems
}

func checkGoBlocks(where, text string) []Problem {
	if !strings.HasPrefix(where, "content/2. ") && !strings.HasPrefix(where, "content/3. ") {
		return nil
	}
	var undeclared []string
	for _, block := range fencedBlocks(text, "go") {
		var first string
		for _, line := range block.body {
			if strings.TrimSpace(line) != "" {
				first = strings.TrimSpace(line)
				break
			}
		}
		if !strings.HasPrefix(first, "// simplified") && !strings.HasPrefix(first, "// illustrative") && !strings.HasPrefix(first, "// pseudocode") {
			undeclared = append(undeclared, strconv.Itoa(block.start))
		}
	}
	if len(undeclared) == 0 {
		return nil
	}
	return []Problem{problem(where, "%d unowned go blocks (lines %s): make it an include of the real source, or open the block with a `// simplified` comment", len(undeclared), strings.Join(undeclared, ", "))}
}

func checkAdmonitions(where, text string) []Problem {
	var problems []Problem
	for _, line := range linesOutsideFences(text) {
		match := admonitionPattern.FindStringSubmatch(line.text)
		if match != nil && !allowedAdmonitions[match[1]] {
			problems = append(problems, problem(where, "line %d: unsupported admonition type %q", line.number, match[1]))
		}
	}
	return problems
}

func checkOrderedLists(where, text string) []Problem {
	var problems []Problem
	for _, line := range linesOutsideFences(text) {
		match := orderedItemPattern.FindStringSubmatch(line.text)
		if match != nil && match[2] != "1" {
			problems = append(problems, problem(where, "line %d: ordered Markdown items must use `1.`, not `%s.`", line.number, match[2]))
		}
	}
	return problems
}

func checkPageLinkTargets(where, text string) []Problem {
	var problems []Problem
	for _, match := range fullPageLink.FindAllStringSubmatch(text, -1) {
		target, err := url.PathUnescape(match[2])
		if err != nil {
			target = match[2]
		}
		stem := strings.TrimSuffix(filepath.Base(target), filepath.Ext(target))
		if match[1] != stem && !strings.HasPrefix(match[1], stem+" —") {
			problems = append(problems, problem(where, "link label %q targets page %q; use the target's full page name", match[1], stem))
		}
	}
	return problems
}

func checkHandsOnAction(where, text string) []Problem {
	if declaredKind(text) != "hands-on" {
		return nil
	}
	var headings []int
	for _, line := range linesOutsideFences(text) {
		if strings.HasPrefix(line.text, "## ") {
			headings = append(headings, line.number)
		}
	}
	limit := len(splitLines(text)) + 1
	if len(headings) > 2 {
		limit = headings[2]
	}
	for _, language := range []string{"bash", "console", "shell", "sh"} {
		for _, block := range fencedBlocks(text, language) {
			if block.start < limit {
				return nil
			}
		}
	}
	return []Problem{problem(where, "hands-on page must reach a bash/shell/console command within its first two H2 sections")}
}

// exerciseOpeners are the three ways this course starts an exercise: a headed
// "## Your turn:" section, and the two inline forms for exercises deliberately kept
// out of the sidebar because they are optional. Each inline form is listed both bare
// and bolded, so emphasizing a label can never silently remove its block from checking.
var exerciseOpeners = []string{
	"## Your turn:",
	"Exercise:", "**Exercise:**",
	"Optional exercise:", "**Optional exercise:**",
}

func isExerciseOpener(line string) bool {
	for _, opener := range exerciseOpeners {
		if strings.HasPrefix(line, opener) {
			return true
		}
	}
	return false
}

func exerciseSections(text string) []fencedBlock {
	lines := splitLines(text)
	var result []fencedBlock
	for index, line := range lines {
		// The inline forms carry a bold label so a reader skimming for what they may
		// skip sees it without reading the sentence. Match both, because an unbolded
		// line would otherwise drop out of validation entirely and pass vacuously.
		if !isExerciseOpener(line) {
			continue
		}
		end := len(lines)
		for cursor := index + 1; cursor < len(lines); cursor++ {
			if strings.HasPrefix(lines[cursor], "## ") {
				end = cursor
				break
			}
		}
		result = append(result, fencedBlock{start: index + 1, end: end, body: lines[index:end]})
	}
	return result
}

func checkExercises(where, text string) []Problem {
	fields := [][]string{
		{"**Mode**:"},
		{"**Goal**:"},
		{"**Files to touch**:"},
		{"**Preflight**:"},
		{"**Steps**:"},
		{"**Gate that proves completion**:"},
		{"**Final state**:"},
	}
	var problems []Problem
	for _, block := range exerciseSections(text) {
		section := strings.Join(block.body, "\n")
		for _, alternatives := range fields {
			found := false
			for _, field := range alternatives {
				found = found || strings.Contains(section, field)
			}
			if !found {
				problems = append(problems, problem(where, "line %d: exercise is missing %s", block.start, strings.Join(alternatives, " or ")))
			}
		}
		// The mode is a closed set, and the safety rules below only run for one of its
		// values. A mode spelled any other way matches nothing here and quietly takes a
		// temporary experiment out of the dirty-preflight and cleanup rules, so an
		// unrecognized value is reported instead of skipped. The `**Mode**:` guard keeps
		// an exercise that omits the field to the one problem it already earns above.
		mode := exerciseMode.FindStringSubmatch(section)
		if mode == nil && strings.Contains(section, "**Mode**:") {
			problems = append(problems, problem(where, "line %d: exercise mode must be one of `inspect`, `temporary experiment`, `keep`, or `capstone carry-forward`", block.start))
		}
		if mode != nil && mode[1] == "temporary experiment" {
			if !strings.Contains(section, "git diff --quiet --") && !strings.Contains(section, "test ! -e ") {
				problems = append(problems, problem(where, "line %d: temporary experiment needs a target-specific dirty preflight", block.start))
			}
			if !strings.Contains(section, "git restore --") && !strings.Contains(section, "rm --") {
				problems = append(problems, problem(where, "line %d: temporary experiment needs target-specific cleanup", block.start))
			}
		}
		if strings.Contains(section, "git checkout --") {
			problems = append(problems, problem(where, "line %d: use scoped `git restore -- <files>` instead of deprecated checkout cleanup", block.start))
		}
		if directoryRestore.MatchString(section) {
			problems = append(problems, problem(where, "line %d: cleanup restores a directory and can discard unrelated learner work", block.start))
		}
		if probabilistic.MatchString(section) && mandatoryRed.MatchString(section) {
			problems = append(problems, problem(where, "line %d: probabilistic evidence cannot satisfy a mandatory red-state claim", block.start))
		}
	}
	return problems
}

// checkDiagramAlternatives requires prose beside every Mermaid diagram.
//
// It used to accept a hash allowlist for diagrams reviewed before the rule existed.
// Every one of those diagrams now carries prose, so the allowlist, its file, and the
// stale-hash detector are gone — the rule holds for the whole corpus unconditionally,
// which is the only version of it a reader can rely on.
func checkDiagramAlternatives(where, text string) []Problem {
	lines := splitLines(text)
	var problems []Problem
	for _, block := range fencedBlocks(text, "mermaid") {
		from := max(0, block.start-5)
		to := min(len(lines), block.end+5)
		if strings.Contains(strings.Join(lines[from:to], "\n"), "**Diagram in words:**") {
			continue
		}
		problems = append(problems, problem(where, "line %d: Mermaid diagram needs adjacent `**Diagram in words:**` prose", block.start))
	}
	return problems
}

func checkMachinePaths(where, text string) []Problem {
	var problems []Problem
	for index, line := range splitLines(text) {
		if machinePathPattern.MatchString(line) {
			problems = append(problems, problem(where, "line %d: found a machine-specific path or obsolete registry hostname", index+1))
		}
	}
	return problems
}

func checkExactCountClaims(where, text string) []Problem {
	pattern := regexp.MustCompile(`(?i)\bexactly\s+(?:\d+|one|two|three|four|five|six|seven|eight|nine|ten)\s+(?:lines?|tests?|modules?)\b`)
	var problems []Problem
	for _, line := range linesOutsideFences(text) {
		if pattern.MatchString(line.text) {
			problems = append(problems, problem(where, "line %d: replace brittle exact line/test/module count with derived or count-free evidence", line.number))
		}
	}
	return problems
}

func sortedDifference(left, right map[string]bool) []string {
	var values []string
	for value := range left {
		if !right[value] {
			values = append(values, value)
		}
	}
	slices.Sort(values)
	return values
}

// closingCadenceKeepers are the seven pages allowed to close on the time-marker
// construction — one per chapter, chosen as that chapter's highest-stakes page —
// plus one page whose match is mid-page prose rather than a closer.
//
// An explicit allowlist rather than a heuristic, following the same reasoning the
// diagram rule used: re-adding the cadence anywhere else means editing this list
// and re-arguing the choice, which is exactly the friction the rule wants.
var closingCadenceKeepers = map[string]bool{
	"content/0. Overview/0.2. Evidence.md":          true,
	"content/1. Setup/1.0. System.md":               true,
	"content/2. Agents/2.1. First Agent.md":         true,
	"content/3. Capabilities/3.1. Tools.md":         true,
	"content/5. Gateway/5.5. Gateway Security.md":   true,
	"content/6. Platform/6.6. Platform Delivery.md": true,
	"content/8. Community/8.7. Capstone.md":         true,
}

// timeMarkerParagraph matches a paragraph opening on a short time marker — "An hour
// ago", "Ten minutes ago", "Seven chapters ago" — which is the shape of the closing
// construction this rule caps.
var timeMarkerParagraph = regexp.MustCompile(`^[A-Z][^.!?]{0,45}\bago\b`)

// checkClosingCadence caps one closing construction across the whole corpus.
//
// A corpus-level rule rather than a per-page property: the construction is excellent
// once and strong for five pages, and by the seventh consecutive page a reader has
// learned to skip the last paragraph of every page — which is exactly where the
// consolidation lives. At most one per chapter, and never two in the same one.
func checkClosingCadence(pages pageSet) []Problem {
	offenders := make(map[string][]string)
	chapters := make(map[string]int)
	for where, text := range pages {
		if !closesOnTimeMarker(text) {
			continue
		}
		chapter, _ := path.Split(where)
		chapters[chapter]++
		if !closingCadenceKeepers[where] {
			offenders[chapter] = append(offenders[chapter], where)
		}
	}
	var problems []Problem
	for chapter, pages := range offenders {
		slices.Sort(pages)
		for _, where := range pages {
			problems = append(problems, problem(where, "closes on the time-marker cadence; at most one page per chapter may, and %s already does", chapterKeeper(chapter)))
		}
	}
	for chapter, count := range chapters {
		if count > 1 && len(offenders[chapter]) == 0 {
			problems = append(problems, problem(strings.TrimSuffix(chapter, "/"), "%d pages close on the time-marker cadence; at most one per chapter may", count))
		}
	}
	slices.SortFunc(problems, func(left, right Problem) int { return strings.Compare(left.String(), right.String()) })
	return problems
}

func chapterKeeper(chapter string) string {
	for where := range closingCadenceKeepers {
		if directory, _ := path.Split(where); directory == chapter {
			return path.Base(where)
		}
	}
	return "no page in it"
}

// closesOnTimeMarker looks only after the page's final H2. A time marker earlier in
// a page is ordinary prose — 0.6 opens a section with "A tutorial written six months
// ago…" — and only the closing paragraph is what this rule is about.
func closesOnTimeMarker(text string) bool {
	lines := linesOutsideFences(text)
	last := -1
	for index, line := range lines {
		if strings.HasPrefix(line.text, "## ") {
			last = index
		}
	}
	if last < 0 {
		return false
	}
	for _, line := range lines[last+1:] {
		if timeMarkerParagraph.MatchString(line.text) {
			return true
		}
	}
	return false
}

// clockTime matches a time of day, which is the shape of the scene-setting headings the
// teaching frame retired ("Ana gets paged at 02:14", "The 03:00 restart you keep doing by
// hand"). Ports are written `:8000` with no leading digits, and latency figures carry a
// unit, so neither matches.
var clockTime = regexp.MustCompile(`\b\d{1,2}:\d{2}\b`)

// checkSceneHeadings keeps a time of day out of a heading.
//
// An H2 names its technical subject and what the section does with it, so it reads as a
// claim in a sidebar. A clock time names a moment in the worked example instead, which is
// exactly the framing the course moved away from: the incident domain illustrates the
// mechanics and is not what anyone is here to learn.
func checkSceneHeadings(where, text string) []Problem {
	var problems []Problem
	for _, line := range linesOutsideFences(text) {
		if strings.HasPrefix(line.text, "## ") && clockTime.MatchString(line.text) {
			problems = append(problems, problem(where, "line %d: heading names a moment in the worked example rather than what the section teaches", line.number))
		}
	}
	return problems
}

// retiredNarrative are the fictional on-call engineer and retailer the course used to tell
// its story through. The seed data stays — it is the measuring device that makes grounding
// checkable — but the narrative around it is gone, because a reader is here to learn how to
// build and operate an agent, not to follow a character through a night shift.
//
// Where a human role carries the teaching, name the role: "the approving engineer".
var retiredNarrative = []*regexp.Regexp{
	regexp.MustCompile(`\bAna\b`),
	regexp.MustCompile(`\bNorthwind\b`),
}

func checkRetiredNarrative(where, text string) []Problem {
	var problems []Problem
	for _, line := range linesOutsideFences(text) {
		for _, pattern := range retiredNarrative {
			if pattern.MatchString(line.text) {
				problems = append(problems, problem(where, "line %d: reintroduces the retired narrative persona or brand; name the role the teaching needs instead", line.number))
				break
			}
		}
	}
	return problems
}

// markdownImage matches an image with its alt text and destination, so both can be
// judged. The negative lookbehind Go lacks is unnecessary here: a link is `[text](…)`
// while an image is `![text](…)`, and the leading `!` is what this anchors on.
var markdownImage = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)`)

// checkImages holds every course image to the two rules a reader depends on.
//
// Alt text is the whole content of the image for anyone who cannot see it, and an
// empty alt says "this image carries nothing" — which is false for every capture the
// course ships. The path rule catches the one mistake that renders as a broken image
// rather than as an error: assets/ is mounted to the site root, so a destination
// under /assets/images/ resolves to nothing while /images/ resolves correctly.
func checkImages(where, text string) []Problem {
	var problems []Problem
	for _, line := range linesOutsideFences(text) {
		for _, match := range markdownImage.FindAllStringSubmatch(line.text, -1) {
			if strings.TrimSpace(match[1]) == "" {
				problems = append(problems, problem(where, "line %d: image needs alt text describing what a reader would learn from it", line.number))
			}
			if strings.HasPrefix(match[2], "/assets/images/") {
				problems = append(problems, problem(where, "line %d: image path %q silently 404s; assets/ mounts to the site root, so use /images/…", line.number, match[2]))
			}
		}
	}
	return problems
}

// openerWordLimit is the course's own rule: an H2 opens with a concrete sentence of 25
// words or fewer. It exists because a reader arriving mid-page from search or a cross-link
// lands on that sentence first, and the habit that breaks it is systematic rather than
// sloppy — the definition, its appositive gloss, and the enumerated consequences all stack
// before the first full stop. Breaking at the dash or colon costs no words and puts the
// definition first, which is why this is worth a checker rather than a style note.
const openerWordLimit = 25

// openerNoise is markup that renders as fewer words than it is written with: shortcodes
// disappear, a link keeps only its label, and emphasis marks are not words at all.
var (
	openerShortcode = regexp.MustCompile(`\{\{[<%].*?[>%]\}\}`)
	openerLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	openerHTMLTag   = regexp.MustCompile(`<[^>]*>`)
	openerSentence  = regexp.MustCompile(`[.!?](?:\s|$)`)
)

// checkHeadingOpeners fails an H2 whose first prose sentence runs past openerWordLimit.
//
// Only prose counts. A section that opens on a list, a table, a fence, a shortcode block,
// or a raw HTML anchor is measuring something the rule was never about, so those are
// skipped rather than guessed at.
func checkHeadingOpeners(where, text string) []Problem {
	var problems []Problem
	lines := linesOutsideFences(text)
	for index, line := range lines {
		if !strings.HasPrefix(line.text, "## ") {
			continue
		}
		paragraph, ok := openingParagraph(lines[index+1:])
		if !ok {
			continue
		}
		if words := openerWords(paragraph); words > openerWordLimit {
			problems = append(problems, problem(where, "line %d: H2 opens on a %d-word sentence; the rule is %d — split it in two at the dash or colon, so the definition lands first and its consequences follow", line.number, words, openerWordLimit))
		}
	}
	return problems
}

// openingParagraph returns the first prose paragraph after a heading, and false when the
// section opens on anything that is not prose.
func openingParagraph(rest []numberedLine) (string, bool) {
	var collected []string
	for _, line := range rest {
		trimmed := strings.TrimSpace(line.text)
		if trimmed == "" {
			if len(collected) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			return "", false
		}
		if len(collected) == 0 && !prose(trimmed) {
			return "", false
		}
		collected = append(collected, trimmed)
	}
	if len(collected) == 0 {
		return "", false
	}
	return strings.Join(collected, " "), true
}

func prose(trimmed string) bool {
	for _, prefix := range []string{"- ", "* ", "|", ">", "{{", "<", "1. ", "!["} {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return true
}

// openerWords counts the words a reader actually reads in the paragraph's first sentence.
func openerWords(paragraph string) int {
	cleaned := openerShortcode.ReplaceAllString(paragraph, "")
	cleaned = openerLink.ReplaceAllString(cleaned, "$1")
	cleaned = openerHTMLTag.ReplaceAllString(cleaned, "")
	cleaned = strings.NewReplacer("`", "", "*", "", "_", "").Replace(cleaned)
	if bound := openerSentence.FindStringIndex(cleaned); bound != nil {
		cleaned = cleaned[:bound[0]]
	}
	return len(strings.Fields(cleaned))
}
