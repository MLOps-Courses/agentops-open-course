package conventions

// Cross-page references. An image destination and a relref both address something
// that has to exist, and both fail silently in Hugo when it does not.

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// checkImageFiles requires every referenced capture to exist on disk.
//
// This is the half of the image contract checkImages cannot enforce from the page
// text alone. Hugo resolves a leading-slash destination through resources.Get and
// renders an ordinary <img> when that lookup misses, so a capture deleted, renamed,
// or never committed leaves a broken image in a published page and a green build.
// Pairing the two makes the whole reference checkable: the page states the path, and
// this proves an asset answers to it.
func checkImageFiles(root string, pages pageSet) []Problem {
	var problems []Problem
	// Sorted so a run reports the same pages in the same order every time.
	paths := slices.Sorted(maps.Keys(pages))
	for _, where := range paths {
		for _, line := range linesOutsideFences(pages[where]) {
			for _, match := range markdownImage.FindAllStringSubmatch(line.text, -1) {
				destination, found := strings.CutPrefix(match[2], "/images/")
				if !found {
					continue
				}
				asset := filepath.Join(root, "assets", "images", filepath.FromSlash(destination))
				if _, err := os.Stat(asset); err != nil {
					problems = append(problems, problem(where, "line %d: image %q has no file at assets/images/%s", line.number, match[2], destination))
				}
			}
		}
	}
	return problems
}

// pageDestination matches the one way a course page addresses another course page:
// Hugo's relref shortcode, in prose and in the chapter tables alike, as in
// `[2.2. Models]({{< relref "/2. Agents/2.2. Models.md" >}})`. Course filenames carry
// spaces, so a raw Markdown destination would have to percent-escape them and would no
// longer be a followable path — which is why checkPageLinkTargets reads the same shape.
var pageDestination = regexp.MustCompile(`\{\{[%<]\s*(?:relref|ref)\s+"([^"]+)"`)

// linkedBasenames collects the basename of every page a chapter index actually links.
//
// Fenced blocks are excluded because a shortcode quoted inside a ```markdown example
// documents the syntax; it does not put the page in the learner's reading chain.
func linkedBasenames(text string) map[string]bool {
	linked := make(map[string]bool)
	for _, line := range linesOutsideFences(text) {
		for _, match := range pageDestination.FindAllStringSubmatch(line.text, -1) {
			// An anchor addresses a section of the page, so it still links the page.
			target, _, _ := strings.Cut(match[1], "#")
			if target = strings.TrimSpace(target); target != "" {
				linked[path.Base(target)] = true
			}
		}
	}
	return linked
}

// checkChapterIndexCoverage requires the chapter that owns a page to link it.
//
// checkNavigation already proves data/nav.yaml lists every page, but the sidebar is not
// the course's primary reading device — the in-page chain is. Four pages were correctly
// in the navigation and absent from their chapter index, so a learner following the
// chapter never reached roughly ten thousand words of the newest material.
//
// The rule reads link destinations rather than the index's bytes. A substring search
// passed on any mention of the filename, including a sentence saying the page was
// retired and an example inside a fence — neither of which a learner can follow.
func checkChapterIndexCoverage(pages pageSet) []Problem {
	linked := make(map[string]map[string]bool)
	missing := make(map[string][]string)
	for where := range pages {
		directory, base := path.Split(where)
		if base == "_index.md" || directory == "content/" {
			continue
		}
		index := directory + "_index.md"
		text, ok := pages[index]
		if !ok {
			continue
		}
		if _, parsed := linked[index]; !parsed {
			linked[index] = linkedBasenames(text)
		}
		if !linked[index][base] {
			missing[index] = append(missing[index], base)
		}
	}
	indexes := make([]string, 0, len(missing))
	for index := range missing {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	problems := make([]Problem, 0, len(indexes))
	for _, index := range indexes {
		slices.Sort(missing[index])
		problems = append(problems, problem(index, "chapter index omits pages: %s", strings.Join(missing[index], ", ")))
	}
	return problems
}
