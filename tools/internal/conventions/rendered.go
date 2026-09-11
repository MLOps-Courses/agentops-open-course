package conventions

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

type renderedLink struct {
	href string
	name string
}

type renderedPage struct {
	canonical string
	meta      map[string]string
	ids       map[string]bool
	language  string
	links     []renderedLink
	diagrams  []string
	h1        int
	main      int
	jsonLD    int
}

func attribute(node *html.Node, key string) string {
	for _, item := range node.Attr {
		if item.Key == key {
			return item.Val
		}
	}
	return ""
}

func accessibleText(node *html.Node) string {
	if value := attribute(node, "aria-label"); value != "" {
		return strings.Join(strings.Fields(value), " ")
	}
	if value := attribute(node, "title"); value != "" {
		return strings.Join(strings.Fields(value), " ")
	}
	var values []string
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			values = append(values, current.Data)
		}
		if current.Type == html.ElementNode && current.Data == "img" {
			values = append(values, attribute(current, "alt"))
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(strings.Join(values, " ")), " ")
}

func wrapsMermaid(node *html.Node) bool {
	found := false
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if found {
			return
		}
		if current.Type == html.ElementNode && current.Data == "pre" &&
			slices.Contains(strings.Fields(attribute(current, "class")), "mermaid") {
			found = true
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return found
}

func parseRendered(path string) (renderedPage, error) {
	file, err := os.Open(path)
	if err != nil {
		return renderedPage{}, err
	}
	defer func() { _ = file.Close() }()
	document, err := html.Parse(file)
	if err != nil {
		return renderedPage{}, err
	}
	result := renderedPage{meta: make(map[string]string), ids: make(map[string]bool)}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			if id := attribute(node, "id"); id != "" {
				result.ids[id] = true
			}
			switch node.Data {
			case "html":
				result.language = attribute(node, "lang")
			case "h1":
				result.h1++
			case "main":
				result.main++
			case "a":
				result.links = append(result.links, renderedLink{href: attribute(node, "href"), name: accessibleText(node)})
			case "div":
				// Hextra wraps every Mermaid diagram in role="img", which makes the
				// SVG subtree presentational: this label is the whole name a screen
				// reader gets for the image.
				if attribute(node, "role") == "img" && wrapsMermaid(node) {
					result.diagrams = append(result.diagrams, strings.Join(strings.Fields(attribute(node, "aria-label")), " "))
				}
			case "link":
				if attribute(node, "rel") == "canonical" {
					result.canonical = attribute(node, "href")
				}
			case "meta":
				key := attribute(node, "property")
				if key == "" {
					key = attribute(node, "name")
				}
				if key != "" {
					result.meta[key] = attribute(node, "content")
				}
			case "script":
				if attribute(node, "type") == "application/ld+json" {
					result.jsonLD++
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	return result, nil
}

func pngDimensions(path string) (int, int, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 24)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, 0, false
	}
	if string(header[:8]) != "\x89PNG\r\n\x1a\n" || string(header[12:16]) != "IHDR" {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint32(header[16:20])), int(binary.BigEndian.Uint32(header[20:24])), true
}

func within(root, target string) bool {
	relativePath, err := filepath.Rel(root, target)
	return err == nil && relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator))
}

func checkRenderedLink(siteRoot, page, href string) string {
	parsed, err := url.Parse(href)
	if err != nil || href == "" || parsed.Scheme != "" || parsed.Host != "" || parsed.Path == "" && parsed.Fragment == "" {
		return ""
	}
	target := page
	if parsed.Path != "" {
		decoded, decodeErr := url.PathUnescape(parsed.Path)
		if decodeErr != nil {
			decoded = parsed.Path
		}
		target = filepath.Join(filepath.Dir(page), filepath.FromSlash(decoded))
		if strings.HasPrefix(decoded, "/") {
			target = filepath.Join(siteRoot, filepath.FromSlash(strings.TrimPrefix(decoded, "/")))
		}
	}
	target, _ = filepath.Abs(filepath.Clean(target))
	root, _ := filepath.Abs(siteRoot)
	if !within(root, target) {
		return fmt.Sprintf("rendered link escapes the published site: %q", href)
	}
	info, statErr := os.Stat(target)
	if statErr == nil && info.IsDir() {
		target = filepath.Join(target, "index.html")
	}
	if info, statErr = os.Stat(target); statErr != nil || !info.Mode().IsRegular() {
		return fmt.Sprintf("rendered link target is missing from the published site: %q", href)
	}
	if parsed.Fragment != "" {
		fragment, decodeErr := url.PathUnescape(parsed.Fragment)
		if decodeErr != nil {
			fragment = parsed.Fragment
		}
		targetPage, parseErr := parseRendered(target)
		if parseErr != nil {
			return fmt.Sprintf("rendered link target could not be parsed: %q", href)
		}
		if !targetPage.ids[fragment] {
			return fmt.Sprintf("rendered link fragment is missing from the target page: %q", href)
		}
	}
	return ""
}

func renderedSource(root, renderedRelative string) string {
	return renderedRouteSource(root, renderedRelative)
}

func checkWebClient(root string) []Problem {
	const where = "clients/web/index.html"
	text, err := readFile(filepath.Join(root, filepath.FromSlash(where)))
	if err != nil {
		return []Problem{problem(where, "could not read web client: %v", err)}
	}
	requirements := map[string]string{
		"<main": "a main landmark", "<h1": "one visible H1", "<label": "native form labels",
		`aria-live="polite"`: "a polite status announcement", ":focus-visible": "visible keyboard focus",
		"@media (max-width:": "a narrow-layout reflow rule", "@media (forced-colors: active)": "a forced-colors fallback",
		"@media (prefers-reduced-motion: reduce)": "a reduced-motion fallback",
	}
	keys := make([]string, 0, len(requirements))
	for key := range requirements {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var problems []Problem
	for _, key := range keys {
		if !strings.Contains(text, key) {
			problems = append(problems, problem(where, "web client needs %s", requirements[key]))
		}
	}
	document, parseErr := html.Parse(strings.NewReader(text))
	if parseErr != nil {
		return append(problems, problem(where, "could not parse web client: %v", parseErr))
	}
	counts := renderedPage{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "html":
				counts.language = attribute(node, "lang")
			case "main":
				counts.main++
			case "h1":
				counts.h1++
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	if counts.main != 1 || counts.h1 != 1 || counts.language == "" {
		problems = append(problems, problem(where, "web client needs one <main>, one <h1>, and a document language"))
	}
	return problems
}

// checkRenderedDiagramNames fails a page whose diagrams answer to the same name.
//
// The Mermaid render hook derives each name from the diagram's own
// `**Diagram in words:**` sentence. When that derivation misses, every diagram on the
// page falls back to the page title, and a reader tabbing the images list hears the
// same label twice with no way to tell the two pictures apart.
func checkRenderedDiagramNames(where string, page renderedPage) []Problem {
	var problems []Problem
	seen := make(map[string]bool, len(page.diagrams))
	for _, name := range page.diagrams {
		switch {
		case name == "":
			problems = append(problems, problem(where, "rendered Mermaid diagram has no accessible name"))
		case seen[name]:
			problems = append(problems, problem(where, "rendered page names two Mermaid diagrams %q; each needs its own accessible name", name))
		default:
			seen[name] = true
		}
	}
	return problems
}

// htmlEntity matches the named and numeric escapes an llms.txt consumer reads literally.
var htmlEntity = regexp.MustCompile(`&(?:[a-zA-Z]+|#[0-9]+);`)

// checkRenderedLLMsIndex holds the published llms.txt to its one-line-per-entry shape.
//
// The index is what an agent pointed at this course fetches first, and it is generated
// from whatever expression layouts/llms.txt happens to carry. Summarizing a page with
// its rendered content rather than its description put the glance block in all 74
// entries, complete with the block's own newlines — which split an entry across lines no
// parser can attribute to a page — and the entities Hugo made of its smart quotes.
func checkRenderedLLMsIndex(siteRoot string) []Problem {
	const where = "llms.txt"
	text, err := readFile(filepath.Join(siteRoot, where))
	if err != nil {
		return []Problem{problem(where, "the published llms.txt index is missing: %v", err)}
	}
	lines := strings.Split(text, "\n")
	var problems []Problem
	entries := 0
	for index, line := range lines {
		if !strings.HasPrefix(line, "- [") {
			continue
		}
		entries++
		_, description, separated := strings.Cut(line, "): ")
		if !separated || strings.TrimSpace(description) == "" {
			problems = append(problems, problem(where, "line %d: entry carries no description", index+1))
		}
		if entity := htmlEntity.FindString(line); entity != "" {
			problems = append(problems, problem(where, "line %d: entry ships %q instead of the character it stands for", index+1, entity))
		}
		if index+1 >= len(lines) {
			continue
		}
		if next := strings.TrimSpace(lines[index+1]); next != "" && !strings.HasPrefix(next, "- [") && !strings.HasPrefix(next, "#") {
			problems = append(problems, problem(where, "line %d: entry spills onto the next line, which reads %q", index+1, next))
		}
	}
	if entries == 0 {
		problems = append(problems, problem(where, "the published llms.txt index lists no pages"))
	}
	return problems
}

// checkRenderedFeeds holds every published RSS feed to carrying items.
//
// The failure this catches is silent by construction: Hextra selects a feed's items by
// comparing page Type against a section name, hugo.toml cascades type = "docs" onto every
// page, and the comparison therefore matched nothing — so all ten feeds served valid XML,
// correct channel metadata, and no items at all, for as long as nobody subscribed. A
// theme bump that reclaims layouts/list.rss.xml would re-empty them the same way.
func checkRenderedFeeds(siteRoot string) []Problem {
	var feeds []string
	err := filepath.WalkDir(siteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "index.xml" {
			feeds = append(feeds, path)
		}
		return nil
	})
	if err != nil {
		return []Problem{problem("index.xml", "could not walk the rendered feeds: %v", err)}
	}
	if len(feeds) == 0 {
		return []Problem{problem("index.xml", "the build published no RSS feed")}
	}
	slices.Sort(feeds)
	var problems []Problem
	for _, feed := range feeds {
		where, relErr := filepath.Rel(siteRoot, feed)
		if relErr != nil {
			where = feed
		}
		where = filepath.ToSlash(where)
		text, readErr := readFile(feed)
		if readErr != nil {
			problems = append(problems, problem(where, "could not read the published feed: %v", readErr))
			continue
		}
		if !strings.Contains(text, "<item>") {
			problems = append(problems, problem(where, "the published feed carries no items"))
		}
	}
	return problems
}

// CheckRendered validates generated semantics, recovery paths, and local links.
func CheckRendered(root, siteDirectory string) []Problem {
	if !filepath.IsAbs(siteDirectory) {
		siteDirectory = filepath.Join(root, siteDirectory)
	}
	info, err := os.Stat(siteDirectory)
	if err != nil || !info.IsDir() {
		return []Problem{problem(filepath.ToSlash(siteDirectory), "rendered site directory does not exist")}
	}
	var pages []string
	_ = filepath.WalkDir(siteDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && filepath.Ext(path) == ".html" {
			pages = append(pages, path)
		}
		return nil
	})
	slices.Sort(pages)
	if len(pages) == 0 {
		return []Problem{problem(filepath.ToSlash(siteDirectory), "rendered site contains no HTML pages")}
	}
	var problems []Problem
	// Alias stubs are Hugo's meta-refresh redirects for the routes the course published
	// before the Hugo permalink scheme. They are two lines of head and carry no <main>,
	// no navigation, and no content, so the per-page semantics below do not apply to
	// them; checkReleasedRoutes is what proves each one exists, points somewhere real, and
	// carries the fragment the incoming request arrived with.
	aliasStubs := make(map[string]bool)
	if sourcePages, loadErr := loadPages(root); loadErr == nil && len(sourcePages) > 0 {
		problems = append(problems, checkRenderedRoutes(root, siteDirectory, sourcePages)...)
		problems = append(problems, checkSourceFragments(siteDirectory, sourcePages)...)
		problems = append(problems, checkReleasedRoutes(root, siteDirectory, sourcePages)...)
		// Every canonical URL checkRenderedRoutes verifies is derived from baseURL
		// itself, so a baseURL naming the wrong host renders a site that validates
		// perfectly against that wrong host. The hostname is therefore checked against
		// the file GitHub Pages actually reads.
		problems = append(problems, checkPublishedHostname(root)...)
		for _, text := range sourcePages {
			for _, alias := range frontMatterAliases(text) {
				absolute, _ := filepath.Abs(filepath.Join(siteDirectory, filepath.FromSlash(alias)))
				aliasStubs[absolute] = true
			}
		}
	}
	// Derived from the one place the repository is named, so the edit link and the
	// slug ban in source.go cannot disagree about which repository this is.
	editBase := "https://github.com/" + RepositorySlug + "/edit/main/content"
	for _, path := range pages {
		if absolute, _ := filepath.Abs(path); aliasStubs[absolute] {
			continue
		}
		where := relative(siteDirectory, path)
		parsed, parseErr := parseRendered(path)
		if parseErr != nil {
			problems = append(problems, problem(where, "could not parse rendered page: %v", parseErr))
			continue
		}
		if parsed.language == "" {
			problems = append(problems, problem(where, "rendered <html> needs a language"))
		}
		if parsed.main != 1 {
			problems = append(problems, problem(where, "rendered page needs exactly one <main>, found %d", parsed.main))
		}
		if parsed.h1 != 1 {
			problems = append(problems, problem(where, "rendered page needs exactly one <h1>, found %d", parsed.h1))
		}
		problems = append(problems, checkRenderedDiagramNames(where, parsed)...)
		unnamed := 0
		for _, link := range parsed.links {
			if link.href != "" && link.name == "" && !strings.HasPrefix(link.href, "#") {
				unnamed++
			}
			if message := checkRenderedLink(siteDirectory, path, link.href); message != "" {
				problems = append(problems, problem(where, "%s", message))
			}
		}
		if unnamed > 0 {
			problems = append(problems, problem(where, "rendered page has %d links without accessible text", unnamed))
		}
		if source := renderedSource(root, where); source != "" {
			expected := editBase + "/" + (&url.URL{Path: source}).EscapedPath()
			found := false
			for _, link := range parsed.links {
				left, _ := url.PathUnescape(link.href)
				right, _ := url.PathUnescape(expected)
				found = found || left == right
			}
			if !found {
				problems = append(problems, problem(where, "rendered page is missing its source-edit action"))
			}
		}
	}

	homePath := filepath.Join(siteDirectory, "index.html")
	if _, err := os.Stat(homePath); err == nil {
		home, _ := parseRendered(homePath)
		for _, name := range []string{"description", "og:title", "og:description", "og:url", "og:image", "twitter:card", "twitter:image"} {
			if home.meta[name] == "" {
				problems = append(problems, problem("index.html", "homepage is missing %q metadata", name))
			}
		}
		if home.jsonLD != 1 {
			problems = append(problems, problem("index.html", "homepage needs exactly one Course JSON-LD record"))
		}
	} else {
		problems = append(problems, problem("index.html", "rendered homepage is missing"))
	}
	notFoundPath := filepath.Join(siteDirectory, "404.html")
	if _, err := os.Stat(notFoundPath); err == nil {
		notFound, _ := parseRendered(notFoundPath)
		recovery := false
		for _, link := range notFound.links {
			recovery = recovery || strings.HasSuffix(link.href, "index.html") || strings.HasSuffix(link.href, "/") || strings.Contains(strings.ToLower(link.name), "search")
		}
		if !recovery {
			problems = append(problems, problem("404.html", "404 page needs a named route back home or to search"))
		}
	} else {
		problems = append(problems, problem("404.html", "custom accessible 404 page is missing"))
	}
	card := filepath.Join(siteDirectory, "assets", "social-card.png")
	width, height, ok := pngDimensions(card)
	if !ok || width != 1200 || height != 630 {
		found := "<nil>"
		if ok {
			found = fmt.Sprintf("(%d, %d)", width, height)
		}
		problems = append(problems, problem("assets/social-card.png", "social preview must be a 1200x630 PNG, found %s", found))
	}
	problems = append(problems, checkRenderedLLMsIndex(siteDirectory)...)
	problems = append(problems, checkRenderedFeeds(siteDirectory)...)
	problems = append(problems, checkWebClient(root)...)
	return problems
}
