package conventions

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml"
)

const (
	pagePermalinkPattern    = "/:sectionslugs/:slug/"
	sectionPermalinkPattern = "/:sectionslugs/"
)

type pageRoute struct {
	Kind string
	Path string
}

type pageRoutes map[string]pageRoute

func frontMatterSlug(where, text string) (string, []Problem) {
	metadata, message := parseFrontMatter(text)
	if metadata == nil {
		return "", []Problem{problem(where, "%s", message)}
	}
	slug, ok := metadata["slug"].(string)
	if !ok || !permalinkSlug.MatchString(slug) {
		return "", []Problem{problem(where, "front matter slug must be explicit lowercase kebab-case, found %q", metadata["slug"])}
	}
	return slug, nil
}

func sectionRouteSegments(pages pageSet, where string) ([]string, []Problem) {
	relative := strings.TrimPrefix(where, "content/")
	directory := filepath.ToSlash(filepath.Dir(relative))
	if directory == "." {
		return nil, nil
	}
	parts := strings.Split(directory, "/")
	segments := make([]string, 0, len(parts))
	var problems []Problem
	for index := range parts {
		section := "content/" + strings.Join(parts[:index+1], "/") + "/_index.md"
		text, exists := pages[section]
		if !exists {
			problems = append(problems, problem(where, "parent section %s has no _index.md route authority", section))
			continue
		}
		slug, slugProblems := frontMatterSlug(section, text)
		problems = append(problems, slugProblems...)
		if slug != "" {
			segments = append(segments, slug)
		}
	}
	return segments, problems
}

func buildPageRoutes(pages pageSet) (pageRoutes, []Problem) {
	paths := make([]string, 0, len(pages))
	for where := range pages {
		paths = append(paths, where)
	}
	slices.Sort(paths)

	routes := make(pageRoutes, len(paths))
	owners := make(map[string]string, len(paths))
	slugOwners := make(map[string]string, len(paths))
	var problems []Problem
	for _, where := range paths {
		if where == landingPage {
			routes[where] = pageRoute{Kind: "home", Path: "/"}
			owners["/"] = where
			continue
		}
		segments, sectionProblems := sectionRouteSegments(pages, where)
		problems = append(problems, sectionProblems...)
		kind := "page"
		if filepath.Base(where) == "_index.md" {
			kind = "section"
		} else {
			slug, slugProblems := frontMatterSlug(where, pages[where])
			problems = append(problems, slugProblems...)
			if slug != "" {
				if owner, duplicate := slugOwners[slug]; duplicate {
					problems = append(problems, problem(where, "pages %s and %s use duplicate regular-page slug %q", owner, where, slug))
				} else {
					slugOwners[slug] = where
				}
				segments = append(segments, slug)
			}
		}
		route := "/" + strings.Join(segments, "/") + "/"
		if owner, duplicate := owners[route]; duplicate {
			problems = append(problems, problem(where, "pages %s and %s resolve to the same permalink %s", owner, where, route))
		} else {
			owners[route] = where
		}
		routes[where] = pageRoute{Kind: kind, Path: route}
	}
	return routes, problems
}

func checkPermalinkConfig(root string) []Problem {
	const where = "hugo.toml"
	document, err := toml.LoadFile(filepath.Join(root, where))
	if err != nil {
		return []Problem{problem(where, "could not read Hugo permalink configuration: %v", err)}
	}
	entries, ok := document.Get("permalinks").([]*toml.Tree)
	if !ok {
		return []Problem{problem(where, "permalinks must define page %q and section %q patterns", pagePermalinkPattern, sectionPermalinkPattern)}
	}
	found := make(map[string][]string)
	for _, entry := range entries {
		pattern, _ := entry.Get("pattern").(string)
		target, _ := entry.Get("target").(*toml.Tree)
		if target == nil {
			continue
		}
		kind, _ := target.Get("kind").(string)
		found[kind] = append(found[kind], pattern)
	}
	wanted := map[string]string{"page": pagePermalinkPattern, "section": sectionPermalinkPattern}
	var problems []Problem
	for kind, pattern := range wanted {
		patterns := found[kind]
		if len(patterns) != 1 || patterns[0] != pattern {
			problems = append(problems, problem(where, "%s permalinks must use exactly %q, found %q", kind, pattern, patterns))
		}
	}
	return problems
}

func routeFile(siteRoot, route string) string {
	if route == "/" {
		return filepath.Join(siteRoot, "index.html")
	}
	return filepath.Join(siteRoot, filepath.FromSlash(strings.Trim(route, "/")), "index.html")
}

func loadBaseURL(root string) string {
	document, err := toml.LoadFile(filepath.Join(root, "hugo.toml"))
	if err != nil {
		return ""
	}
	baseURL, _ := document.Get("baseURL").(string)
	if baseURL == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/"
}

// checkPublishedHostname holds the published hostname to one authority.
//
// static/CNAME is the file GitHub Pages reads, so it owns the name; hugo.toml's baseURL
// and static/robots.txt's Sitemap line are copies of it, and a divergence is silent —
// the site keeps returning 200 while every canonical link, sitemap entry, Open Graph tag
// and absolute asset reference points at a host that is not serving.
func checkPublishedHostname(root string) []Problem {
	const cnameWhere = "static/CNAME"
	text, err := readFile(filepath.Join(root, "static", "CNAME"))
	if err != nil {
		return []Problem{problem(cnameWhere, "could not read the published hostname: %v", err)}
	}
	var hosts []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			hosts = append(hosts, trimmed)
		}
	}
	if len(hosts) != 1 {
		return []Problem{problem(cnameWhere, "GitHub Pages serves exactly one hostname, found %d", len(hosts))}
	}
	host := hosts[0]

	baseURL := loadBaseURL(root)
	if baseURL == "" {
		return []Problem{problem("hugo.toml", "baseURL must be set to the hostname %s serves, %q", cnameWhere, host)}
	}
	parsed, parseErr := url.Parse(baseURL)
	if parseErr != nil {
		return []Problem{problem("hugo.toml", "baseURL %q is not a URL: %v", baseURL, parseErr)}
	}
	var problems []Problem
	if !strings.EqualFold(parsed.Host, host) {
		problems = append(problems, problem("hugo.toml", "baseURL host is %q, but %s serves %q", parsed.Host, cnameWhere, host))
	}
	// A project-pages baseURL carrying a path prefix would relocate every root-relative
	// asset the rendered pages emit, on a site served from the apex of its own hostname.
	if parsed.Path != "/" {
		problems = append(problems, problem("hugo.toml", "baseURL must have no path prefix, found %q", parsed.Path))
	}
	// hugo.toml disables enableRobotsTXT, so static/robots.txt is served verbatim and its
	// Sitemap line is a third copy of the hostname that no build step regenerates.
	const robotsWhere = "static/robots.txt"
	robots, robotsErr := readFile(filepath.Join(root, "static", "robots.txt"))
	if robotsErr != nil {
		return append(problems, problem(robotsWhere, "could not read the served robots file: %v", robotsErr))
	}
	sitemap := "Sitemap: " + strings.TrimRight(baseURL, "/") + "/sitemap.xml"
	if !slices.Contains(strings.Split(robots, "\n"), sitemap) {
		problems = append(problems, problem(robotsWhere, "served robots file must announce %q", sitemap))
	}
	return problems
}

func readSearchRoutes(siteRoot string) map[string]bool {
	paths, _ := filepath.Glob(filepath.Join(siteRoot, "*.search-data.json"))
	if len(paths) != 1 {
		return nil
	}
	content, err := os.ReadFile(paths[0])
	if err != nil {
		return nil
	}
	// The Hextra search artifact is an object keyed by each page's relative permalink.
	var values map[string]any
	if err := json.Unmarshal(content, &values); err != nil {
		return nil
	}
	routes := make(map[string]bool, len(values))
	for route := range values {
		// Hextra always adds this synthetic termbase entry. It has no content page,
		// canonical URL, sitemap entry, or slug authority, so it is not a course route.
		if route == "/glossary" {
			continue
		}
		routes[route] = true
	}
	return routes
}

func readSitemapRoutes(siteRoot string) map[string]bool {
	content, err := os.ReadFile(filepath.Join(siteRoot, "sitemap.xml"))
	if err != nil {
		return nil
	}
	var sitemap struct {
		URLs []struct {
			Location string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(content, &sitemap); err != nil {
		return nil
	}
	routes := make(map[string]bool, len(sitemap.URLs))
	for _, entry := range sitemap.URLs {
		parsed, parseErr := url.Parse(entry.Location)
		if parseErr == nil {
			routes[parsed.EscapedPath()] = true
		}
	}
	return routes
}

func routeFromRenderedLink(href string) string {
	parsed, err := url.Parse(href)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return ""
	}
	path, err := url.PathUnescape(parsed.Path)
	if err != nil {
		return ""
	}
	if path != "/" && !strings.HasSuffix(path, "/") {
		return ""
	}
	return path
}

func checkRenderedRoutes(root, siteRoot string, pages pageSet) []Problem {
	routes, problems := buildPageRoutes(pages)
	baseURL := loadBaseURL(root)
	if baseURL == "" {
		problems = append(problems, problem("hugo.toml", "baseURL must be set to verify canonical page metadata"))
	}
	searchRoutes := readSearchRoutes(siteRoot)
	if searchRoutes == nil {
		problems = append(problems, problem("site/*.search-data.json", "rendered search route index is missing or invalid"))
	}
	sitemapRoutes := readSitemapRoutes(siteRoot)
	if sitemapRoutes == nil {
		problems = append(problems, problem("site/sitemap.xml", "rendered sitemap is missing or invalid"))
	}

	expectedFiles := make(map[string]string, len(routes))
	navigationRoutes := make(map[string]bool, len(routes))
	for source, route := range routes {
		path := routeFile(siteRoot, route.Path)
		absolute, _ := filepath.Abs(path)
		expectedFiles[absolute] = source
		parsed, err := parseRendered(path)
		if err != nil {
			problems = append(problems, problem(source, "expected permalink %s did not render to %s", route.Path, relative(root, path)))
			continue
		}
		expectedCanonical := strings.TrimRight(baseURL, "/") + route.Path
		if parsed.canonical != expectedCanonical {
			problems = append(problems, problem(source, "rendered canonical URL must be %q, found %q", expectedCanonical, parsed.canonical))
		}
		if parsed.meta["og:url"] != expectedCanonical {
			problems = append(problems, problem(source, "rendered Open Graph URL must be %q, found %q", expectedCanonical, parsed.meta["og:url"]))
		}
		if searchRoutes != nil && !searchRoutes[route.Path] {
			problems = append(problems, problem(source, "permalink %s is absent from the rendered search index", route.Path))
		}
		if sitemapRoutes != nil && !sitemapRoutes[route.Path] {
			problems = append(problems, problem(source, "permalink %s is absent from the rendered sitemap", route.Path))
		}
		for _, link := range parsed.links {
			if target := routeFromRenderedLink(link.href); target != "" {
				navigationRoutes[target] = true
			}
		}
	}
	for source, route := range routes {
		if !navigationRoutes[route.Path] {
			problems = append(problems, problem(source, "permalink %s is absent from rendered navigation and page links", route.Path))
		}
	}

	// Aliases are the course's URL-stability contract, not stray output: every one is a
	// route the site published under a previous release, declared in the front matter of
	// the page that now serves it and proved against data/released-urls.json by
	// checkReleasedRoutes. Anything rendered that is neither a permalink nor a declared
	// alias is still unaccounted for and still fails.
	aliasFiles := make(map[string]string)
	for source, text := range pages {
		for _, alias := range frontMatterAliases(text) {
			absolute, _ := filepath.Abs(filepath.Join(siteRoot, filepath.FromSlash(alias)))
			aliasFiles[absolute] = source
		}
	}

	_ = filepath.WalkDir(siteRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".html" {
			return nil
		}
		absolute, _ := filepath.Abs(path)
		if _, expected := expectedFiles[absolute]; expected || filepath.Base(path) == "404.html" {
			return nil
		}
		if _, aliased := aliasFiles[absolute]; aliased {
			return nil
		}
		problems = append(problems, problem(relative(siteRoot, path), "rendered HTML page is neither a source permalink nor a declared alias"))
		return nil
	})
	if searchRoutes != nil && len(searchRoutes) != len(routes) {
		problems = append(problems, problem("site/*.search-data.json", "search route count is %d, want %d source permalinks", len(searchRoutes), len(routes)))
	}
	if sitemapRoutes != nil && len(sitemapRoutes) != len(routes) {
		problems = append(problems, problem("site/sitemap.xml", "sitemap route count is %d, want %d source permalinks", len(sitemapRoutes), len(routes)))
	}
	return problems
}

func renderedRouteSource(root, renderedRelative string) string {
	if !strings.HasSuffix(renderedRelative, "index.html") {
		return ""
	}
	pages, err := loadPages(root)
	if err != nil {
		return ""
	}
	routes, _ := buildPageRoutes(pages)
	renderedPath := filepath.ToSlash(renderedRelative)
	for source, route := range routes {
		if filepath.ToSlash(strings.TrimPrefix(routeFile("", route.Path), string(filepath.Separator))) == renderedPath {
			return strings.TrimPrefix(source, "content/")
		}
	}
	return ""
}

func checkSourceFragments(siteRoot string, pages pageSet) []Problem {
	routes, _ := buildPageRoutes(pages)
	pattern := regexp.MustCompile(`\{\{<\s+relref\s+"([^"#]+)#([^"#]+)"[^}]*>\}\}`)
	var problems []Problem
	for where, text := range pages {
		for _, line := range linesOutsideFences(text) {
			for _, match := range pattern.FindAllStringSubmatch(line.text, -1) {
				target := "content/" + strings.TrimPrefix(match[1], "/")
				route, exists := routes[target]
				if !exists {
					problems = append(problems, problem(where, "line %d: relref fragment target does not exist: %s", line.number, match[1]))
					continue
				}
				page, err := parseRendered(routeFile(siteRoot, route.Path))
				if err != nil {
					problems = append(problems, problem(where, "line %d: rendered relref target is missing: %s", line.number, route.Path))
					continue
				}
				if !page.ids[match[2]] {
					problems = append(problems, problem(where, "line %d: rendered relref target %s has no fragment %q", line.number, route.Path, match[2]))
				}
			}
		}
	}
	return problems
}

// frontMatterAliases returns the historical routes a page declares it still answers for.
func frontMatterAliases(text string) []string {
	metadata, _ := parseFrontMatter(text)
	if metadata == nil {
		return nil
	}
	declared, ok := metadata["aliases"].([]any)
	if !ok {
		return nil
	}
	aliases := make([]string, 0, len(declared))
	for _, value := range declared {
		if alias, isString := value.(string); isString {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// checkReleasedRoutes is the successor to the Python course's released-URL ratchet.
//
// Hugo's permalink scheme shares not one URL with the MkDocs scheme the course published
// under through v0.7.0, so every one of those addresses would have become a 404. The
// ledger records them; this check proves each is still claimed by exactly one page and
// still renders. A page rename that drops an alias fails here rather than in a reader's
// browser six months later.
func checkReleasedRoutes(root, siteRoot string, pages pageSet) []Problem {
	const where = "data/released-urls.json"
	text, err := readFile(filepath.Join(root, "data", "released-urls.json"))
	if err != nil {
		return []Problem{problem(where, "could not read the released-URL ledger: %v", err)}
	}
	var ledger struct {
		Releases   map[string][]string `json:"releases"`
		Redirects  map[string]string   `json:"redirects"`
		Successors map[string]string   `json:"successors"`
	}
	if err := json.Unmarshal([]byte(text), &ledger); err != nil {
		return []Problem{problem(where, "could not parse the released-URL ledger: %v", err)}
	}

	published := make(map[string]bool)
	for _, routes := range ledger.Releases {
		for _, route := range routes {
			published[route] = true
		}
	}
	// Both sides of a redirect are published addresses. A page renamed inside the
	// pre-Hugo era served its new name too, so reading only the keys left that second
	// address outside the ratchet — which is how 6.7's post-rename URL became a live
	// 404 with every gate green.
	for route, target := range ledger.Redirects {
		published[route] = true
		published[target] = true
	}

	claimed := make(map[string]string)
	var problems []Problem
	for source, page := range pages {
		for _, alias := range frontMatterAliases(page) {
			route := strings.TrimPrefix(alias, "/")
			if owner, taken := claimed[route]; taken {
				problems = append(problems, problem(source, "alias %s is already claimed by %s", alias, owner))
				continue
			}
			claimed[route] = source
			if !published[route] {
				problems = append(problems, problem(source, "alias %s is not a route the course ever published; remove it or record it in %s", alias, where))
			}
		}
	}

	for _, route := range slices.Sorted(maps.Keys(published)) {
		successor, recorded := ledger.Successors[route]
		if !recorded {
			problems = append(problems, problem(where, "published route %q has no recorded successor", route))
			continue
		}
		if _, served := claimed[route]; !served {
			problems = append(problems, problem(where, "published route %q is not declared as an alias by any page; readers would get a 404", route))
			continue
		}
		// The home page cannot alias itself: Hugo renders it at / and index.html already.
		if successor == "/" {
			continue
		}
		stub, readErr := os.ReadFile(filepath.Join(siteRoot, filepath.FromSlash(route)))
		if readErr != nil {
			problems = append(problems, problem(where, "published route %q did not render a redirect into the site", route))
			continue
		}
		// A meta refresh navigates to exactly the URL it was given, so the stub has to
		// forward the incoming fragment in script or every historical deep link lands at
		// page top. Reading the body is what stops layouts/alias.html from being deleted
		// with nothing failing.
		if !bytes.Contains(stub, []byte("location.hash")) {
			problems = append(problems, problem(where, "published route %q renders a redirect that discards the URL fragment; layouts/alias.html must forward location.hash", route))
		}
	}

	// The successor column is the live half of the ratchet, and it used to be read only
	// to special-case the home page. That left the ledger protecting the pre-Hugo
	// addresses while every URL the site serves today went unchecked: renaming a page's
	// slug moved its permalink, the ledger kept pointing at the old one, and the check
	// still passed. A successor must therefore still be a permalink some page owns, and
	// must still render — so a rename either updates this ledger or fails here.
	routes, _ := buildPageRoutes(pages)
	live := make(map[string]bool, len(routes))
	for _, route := range routes {
		live[route.Path] = true
	}
	for _, route := range slices.Sorted(maps.Keys(ledger.Successors)) {
		successor := ledger.Successors[route]
		if !live[successor] {
			problems = append(problems, problem(where,
				"route %q records successor %q, which is no page's permalink; a rename must update this ledger", route, successor))
			continue
		}
		if _, statErr := os.Stat(routeFile(siteRoot, successor)); statErr != nil {
			problems = append(problems, problem(where,
				"route %q records successor %q, which did not render into the site", route, successor))
		}
	}
	return problems
}
