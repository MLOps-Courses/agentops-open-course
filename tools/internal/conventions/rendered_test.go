package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderedLinksCannotEscapeOrTargetMissingFile(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	page := filepath.Join(site, "chapter", "page.html")
	if err := os.MkdirAll(filepath.Dir(page), 0o750); err != nil {
		t.Fatal(err)
	}
	if message := checkRenderedLink(site, page, "../../docs/assets/font.txt"); !strings.Contains(message, "escapes") {
		t.Fatalf("escape message = %q", message)
	}
	if message := checkRenderedLink(site, page, "../assets/missing.txt"); !strings.Contains(message, "missing") {
		t.Fatalf("missing message = %q", message)
	}
}

func TestRenderedLinksRequireExistingFragments(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	page := filepath.Join(site, "chapter", "index.html")
	target := filepath.Join(site, "target", "index.html")
	if err := os.MkdirAll(filepath.Dir(page), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte(`<html><body><h2 id="local-fragment">Local</h2></body></html>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`<html><body><h2 id=target-fragment>Target</h2></body></html>`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, href := range []string{"#local-fragment", "/target/#target-fragment"} {
		if message := checkRenderedLink(site, page, href); message != "" {
			t.Errorf("checkRenderedLink(%q) = %q, want success", href, message)
		}
	}
	if message := checkRenderedLink(site, page, "/target/#missing"); !strings.Contains(message, "fragment") {
		t.Fatalf("missing fragment message = %q", message)
	}
}

func TestCheckRenderedFindsHomepageMetadataAnd404Recovery(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	client := filepath.Join(root, "clients", "web")
	if err := os.MkdirAll(filepath.Join(site, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(client, 0o750); err != nil {
		t.Fatal(err)
	}
	document := `<!doctype html><html lang="en"><head><meta name="description" content="x"></head><body><main><h1>Page</h1></main></body></html>`
	for _, name := range []string{"index.html", "404.html"} {
		if err := os.WriteFile(filepath.Join(site, name), []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(client, "index.html"), []byte(`<html lang="en"><main><h1>x</h1><label>x</label><div aria-live="polite"></div><style>:focus-visible{} @media (max-width:1px){} @media (forced-colors: active){} @media (prefers-reduced-motion: reduce){}</style></main></html>`), 0o600); err != nil {
		t.Fatal(err)
	}
	problems := CheckRendered(root, site)
	messages := problemMessages(problems)
	if !strings.Contains(messages, "og:title") || !strings.Contains(messages, "route back home") {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestParseRenderedUsesAccessibleImageAlternative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.html")
	content := `<html lang="en"><main><h1>x</h1><a href="/"><img alt="Home"></a></main></html>`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseRendered(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.links) != 1 || parsed.links[0].name != "Home" {
		t.Fatalf("links = %#v", parsed.links)
	}
}

// aliasStub is what layouts/alias.html renders: a meta refresh for readers without
// scripting, and the hash forward that keeps a historical deep link on its anchor.
const aliasStub = `<html><head><meta http-equiv="refresh" content="0; url=/2-agents/2-1-first-agent/">` +
	`<script>location.replace("/2-agents/2-1-first-agent/" + location.hash);</script></head></html>`

func TestCheckRenderedRoutesBindsSlugsToEveryPublishedSurface(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	files := map[string]string{
		"hugo.toml": `baseURL = "https://example.test/"
[[permalinks]]
pattern = "/:sectionslugs/:slug/"
[permalinks.target]
kind = "page"
[[permalinks]]
pattern = "/:sectionslugs/"
[permalinks.target]
kind = "section"
`,
		"content/_index.md":                        "---\ntitle: Home\ndescription: x\n---\n",
		"content/2. Agents/_index.md":              "---\ntitle: Agents\ndescription: x\nslug: 2-agents\n---\n",
		"content/2. Agents/2.1. First Agent.md":    "---\ntitle: First\ndescription: x\nslug: 2-1-first-agent\n---\n",
		"site/index.html":                          renderedRouteFixture("https://example.test/", "/2-agents/", "/2-agents/2-1-first-agent/"),
		"site/2-agents/index.html":                 renderedRouteFixture("https://example.test/2-agents/", "/", "/2-agents/2-1-first-agent/"),
		"site/2-agents/2-1-first-agent/index.html": renderedRouteFixture("https://example.test/2-agents/2-1-first-agent/", "/", "/2-agents/"),
		"site/en.search-data.json": `{
  "/": {},
  "/2-agents/": {},
  "/2-agents/2-1-first-agent/": {},
  "/glossary": {}
}`,
		"site/sitemap.xml": `<?xml version="1.0"?><urlset><url><loc>https://example.test/</loc></url><url><loc>https://example.test/2-agents/</loc></url><url><loc>https://example.test/2-agents/2-1-first-agent/</loc></url></urlset>`,
	}
	for path, content := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pages, err := loadPages(root)
	if err != nil {
		t.Fatal(err)
	}
	if problems := checkRenderedRoutes(root, site, pages); len(problems) != 0 {
		t.Fatalf("valid rendered routes problems = %#v", problems)
	}
	// An undeclared rendered page is still unaccounted for: nothing in content/ claims
	// it, so nothing keeps it correct.
	legacy := filepath.Join(site, "legacy.html")
	if err := os.WriteFile(legacy, []byte(renderedRouteFixture("https://example.test/legacy.html")), 0o600); err != nil {
		t.Fatal(err)
	}
	if messages := problemMessages(checkRenderedRoutes(root, site, pages)); !strings.Contains(messages, "neither a source permalink nor a declared alias") {
		t.Fatalf("undeclared rendered page was accepted: %s", messages)
	}

	// The same file becomes legitimate the moment a page declares it — that declaration
	// is what binds the historical route to the page responsible for still serving it.
	declared := make(pageSet, len(pages))
	for where, text := range pages {
		declared[where] = text
	}
	for where, text := range declared {
		if strings.HasSuffix(where, "2.1. First Agent.md") {
			// The opening delimiter has no newline before it, so this lands on the
			// closing one and leaves the front matter well formed.
			declared[where] = strings.Replace(text, "\n---\n", "\naliases:\n  - \"/legacy.html\"\n---\n", 1)
			break
		}
	}
	if problems := checkRenderedRoutes(root, site, declared); len(problems) != 0 {
		t.Fatalf("declared alias was rejected: %#v", problems)
	}
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(files["site/2-agents/2-1-first-agent/index.html"], "https://example.test/2-agents/2-1-first-agent/", "https://example.test/wrong/", 1)
	if err := os.WriteFile(filepath.Join(site, "2-agents", "2-1-first-agent", "index.html"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if messages := problemMessages(checkRenderedRoutes(root, site, pages)); !strings.Contains(messages, "canonical") {
		t.Fatalf("canonical drift was accepted: %s", messages)
	}
}

// The ratchet used to validate only the ledger's keys — the pre-Hugo addresses — while
// the successor column, which is every URL the live site serves today, was read only to
// special-case the home page. A page renamed after the Hugo move therefore broke a
// reader's bookmark with the gate still green.
func TestCheckReleasedRoutesValidatesTheSuccessorColumn(t *testing.T) {
	plant := func(t *testing.T, slug string) (string, string, pageSet) {
		t.Helper()
		root := t.TempDir()
		site := filepath.Join(root, "site")
		ledger, readErr := os.ReadFile(filepath.Join("..", "..", "testdata", "conventions", "released-urls", "ledger.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		files := map[string]string{
			"data/released-urls.json":               string(ledger),
			"content/_index.md":                     "---\ntitle: Home\ndescription: x\n---\n",
			"content/2. Agents/_index.md":           "---\ntitle: Agents\ndescription: x\nslug: 2-agents\n---\n",
			"content/2. Agents/2.1. First Agent.md": "---\ntitle: First\ndescription: x\nslug: " + slug + "\naliases:\n  - \"/2. Agents/2.1. First Agent.html\"\n---\n",
			"site/index.html":                       "<html></html>",
			"site/2-agents/index.html":              "<html></html>",
			"site/2-agents/" + slug + "/index.html": "<html></html>",
			"site/2. Agents/2.1. First Agent.html":  aliasStub,
		}
		for path, content := range files {
			fullPath := filepath.Join(root, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		pages, loadErr := loadPages(root)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		return root, site, pages
	}

	root, site, pages := plant(t, "2-1-first-agent")
	if problems := checkReleasedRoutes(root, site, pages); len(problems) != 0 {
		t.Fatalf("consistent ledger problems = %#v", problems)
	}

	// The rename the ledger was supposed to catch: the page moves, the ledger does not.
	renamed, renamedSite, renamedPages := plant(t, "2-1-first")
	messages := problemMessages(checkReleasedRoutes(renamed, renamedSite, renamedPages))
	if !strings.Contains(messages, "is no page's permalink") {
		t.Fatalf("renamed page was accepted: %s", messages)
	}

	// A successor that no longer renders is the same broken bookmark, one step later.
	if err := os.Remove(filepath.Join(site, "2-agents", "2-1-first-agent", "index.html")); err != nil {
		t.Fatal(err)
	}
	messages = problemMessages(checkReleasedRoutes(root, site, pages))
	if !strings.Contains(messages, "did not render into the site") {
		t.Fatalf("unrendered successor was accepted: %s", messages)
	}
}

func renderedRouteFixture(canonical string, links ...string) string {
	var anchors strings.Builder
	for _, link := range links {
		anchors.WriteString(`<a href="` + link + `">Page</a>`)
	}
	return `<html lang="en"><head><link rel="canonical" href="` + canonical + `"><meta property="og:url" content="` + canonical + `"></head><body><main><h1>Page</h1>` + anchors.String() + `</main></body></html>`
}

// A redirect records two published addresses, not one: the key is what the course
// served before an in-era rename and the value is what it served after. Reading only
// the keys is what let 6.7's post-rename URL become a live 404 with the gate green.
func TestCheckReleasedRoutesProtectsRedirectTargets(t *testing.T) {
	const ledger = `{
  "format": 1,
  "releases": {"0.1.0": ["2. Agents/2.1. Old Name.html"]},
  "redirects": {"2. Agents/2.1. Old Name.html": "2. Agents/2.1. New Name.html"},
  "successors": {
    "2. Agents/2.1. Old Name.html": "/2-agents/2-1-first-agent/",
    "2. Agents/2.1. New Name.html": "/2-agents/2-1-first-agent/"
  }
}`
	plant := func(t *testing.T, aliases string) (string, string, pageSet) {
		t.Helper()
		root := t.TempDir()
		files := map[string]string{
			"data/released-urls.json":                  ledger,
			"content/_index.md":                        "---\ntitle: Home\ndescription: x\n---\n",
			"content/2. Agents/_index.md":              "---\ntitle: Agents\ndescription: x\nslug: 2-agents\n---\n",
			"content/2. Agents/2.1. First Agent.md":    "---\ntitle: First\ndescription: x\nslug: 2-1-first-agent\naliases:\n" + aliases + "---\n",
			"site/2-agents/2-1-first-agent/index.html": "<html></html>",
			"site/2. Agents/2.1. Old Name.html":        aliasStub,
			"site/2. Agents/2.1. New Name.html":        aliasStub,
		}
		for path, content := range files {
			fullPath := filepath.Join(root, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		pages, loadErr := loadPages(root)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		return root, filepath.Join(root, "site"), pages
	}

	root, site, pages := plant(t, "  - \"/2. Agents/2.1. Old Name.html\"\n")
	messages := problemMessages(checkReleasedRoutes(root, site, pages))
	if !strings.Contains(messages, "2.1. New Name.html") {
		t.Fatalf("unclaimed redirect target was accepted: %s", messages)
	}

	claimed, claimedSite, claimedPages := plant(t, "  - \"/2. Agents/2.1. Old Name.html\"\n  - \"/2. Agents/2.1. New Name.html\"\n")
	if problems := checkReleasedRoutes(claimed, claimedSite, claimedPages); len(problems) != 0 {
		t.Fatalf("both redirect sides claimed, problems = %#v", problems)
	}
}

// static/CNAME is the file GitHub Pages reads, so hugo.toml's baseURL and the Sitemap
// line in static/robots.txt are copies of it. A divergence is invisible in the build:
// every canonical URL is derived from baseURL, so a wrong baseURL validates against its
// own wrong host while the live site keeps returning 200 from the real one.
func TestCheckPublishedHostnameHoldsTheThreeCopiesTogether(t *testing.T) {
	plant := func(t *testing.T, cname, baseURL, sitemap string) string {
		t.Helper()
		root := t.TempDir()
		files := map[string]string{
			"static/CNAME":      cname,
			"static/robots.txt": "User-agent: *\nAllow: /\n\nSitemap: " + sitemap + "\n",
			"hugo.toml":         "baseURL = \"" + baseURL + "\"\n",
		}
		for path, content := range files {
			fullPath := filepath.Join(root, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}

	agreeing := plant(t, "course.example\n", "https://course.example/", "https://course.example/sitemap.xml")
	if problems := checkPublishedHostname(agreeing); len(problems) != 0 {
		t.Fatalf("agreeing hostname problems = %#v", problems)
	}

	for name, test := range map[string]struct {
		cname, baseURL, sitemap, want string
	}{
		"host drift":    {"course.example\n", "https://old.example/", "https://course.example/sitemap.xml", "but static/CNAME serves"},
		"path prefix":   {"course.example\n", "https://course.example/course/", "https://course.example/course/sitemap.xml", "no path prefix"},
		"two hosts":     {"course.example\nother.example\n", "https://course.example/", "https://course.example/sitemap.xml", "exactly one hostname"},
		"stale sitemap": {"course.example\n", "https://course.example/", "https://old.example/sitemap.xml", "must announce"},
	} {
		root := plant(t, test.cname, test.baseURL, test.sitemap)
		if messages := problemMessages(checkPublishedHostname(root)); !strings.Contains(messages, test.want) {
			t.Errorf("%s: problems = %q, want %q", name, messages, test.want)
		}
	}

	missing := t.TempDir()
	if messages := problemMessages(checkPublishedHostname(missing)); !strings.Contains(messages, "could not read the published hostname") {
		t.Errorf("missing CNAME: problems = %q", messages)
	}
}

// The alias stub is a meta refresh, which navigates to exactly the URL it was given and
// drops the incoming fragment. os.Stat could never see that, so layouts/alias.html could
// be deleted with CI green and every historical deep link silently landing at page top.
func TestCheckReleasedRoutesRequiresAliasStubsToForwardTheFragment(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	ledger, readErr := os.ReadFile(filepath.Join("..", "..", "testdata", "conventions", "released-urls", "ledger.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	stub := `<html><head><meta http-equiv="refresh" content="0; url=/2-agents/2-1-first-agent/">`
	files := map[string]string{
		"data/released-urls.json":                  string(ledger),
		"content/_index.md":                        "---\ntitle: Home\ndescription: x\n---\n",
		"content/2. Agents/_index.md":              "---\ntitle: Agents\ndescription: x\nslug: 2-agents\n---\n",
		"content/2. Agents/2.1. First Agent.md":    "---\ntitle: First\ndescription: x\nslug: 2-1-first-agent\naliases:\n  - \"/2. Agents/2.1. First Agent.html\"\n---\n",
		"site/index.html":                          "<html></html>",
		"site/2-agents/index.html":                 "<html></html>",
		"site/2-agents/2-1-first-agent/index.html": "<html></html>",
		"site/2. Agents/2.1. First Agent.html":     stub,
	}
	for path, content := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pages, loadErr := loadPages(root)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if messages := problemMessages(checkReleasedRoutes(root, site, pages)); !strings.Contains(messages, "discards the URL fragment") {
		t.Fatalf("fragment-dropping stub was accepted: %s", messages)
	}

	forwarding := stub + `<script>location.replace("/2-agents/2-1-first-agent/" + location.hash);</script></head></html>`
	if err := os.WriteFile(filepath.Join(site, "2. Agents", "2.1. First Agent.html"), []byte(forwarding), 0o600); err != nil {
		t.Fatal(err)
	}
	if problems := checkReleasedRoutes(root, site, pages); len(problems) != 0 {
		t.Fatalf("forwarding stub was rejected: %#v", problems)
	}
}

// Two diagrams on one page must not answer to one name: role="img" makes the SVG
// subtree presentational, so the label is the whole name a screen reader reads out.
func TestCheckRenderedDiagramNamesRejectsRepeatedLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.html")
	document := `<html lang="en"><main><h1>x</h1>` +
		`<div role="img" aria-label="Diagram on 1.4. Providers"><pre class="mermaid hx:mt-6">a</pre></div>` +
		`<div role="img" aria-label="Diagram on 1.4. Providers"><pre class="mermaid hx:mt-6">b</pre></div>` +
		`</main></html>`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseRendered(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.diagrams) != 2 {
		t.Fatalf("diagrams = %#v", parsed.diagrams)
	}
	if messages := problemMessages(checkRenderedDiagramNames("page.html", parsed)); !strings.Contains(messages, "two Mermaid diagrams") {
		t.Fatalf("repeated diagram name was accepted: %s", messages)
	}

	parsed.diagrams = []string{"The provider variable chooses one of two branches.", "A mise task loads the root environment."}
	if problems := checkRenderedDiagramNames("page.html", parsed); len(problems) != 0 {
		t.Fatalf("distinct diagram names were rejected: %#v", problems)
	}
}

// llms.txt is read one entry per line by whatever agent fetches it, so a description
// that kept its source newlines or its HTML entities reaches that consumer broken.
func TestCheckRenderedLLMsIndexRejectsBrokenEntries(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		site := t.TempDir()
		if err := os.WriteFile(filepath.Join(site, "llms.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return site
	}

	good := "# Course\n\n## 0. Overview\n- [0.0. Course](https://x.test/0-0-course/): One curated sentence.\n- [0.1. Agents](https://x.test/0-1-agents/): Another curated sentence.\n\n---\n"
	if problems := checkRenderedLLMsIndex(write(t, good)); len(problems) != 0 {
		t.Fatalf("well-formed index problems = %#v", problems)
	}

	for name, test := range map[string]struct{ body, want string }{
		"spilled description": {"- [0.0. Course](https://x.test/0-0-course/): In one glance\nYou will: run it.\n", "spills onto the next line"},
		"html entity":         {"- [0.0. Course](https://x.test/0-0-course/): An agent&rsquo;s answer.\n", "instead of the character"},
		"empty description":   {"- [0.0. Course](https://x.test/0-0-course/): \n", "carries no description"},
		"no entries":          {"# Course\n\n---\n", "lists no pages"},
	} {
		if messages := problemMessages(checkRenderedLLMsIndex(write(t, test.body))); !strings.Contains(messages, test.want) {
			t.Errorf("%s: problems = %q, want %q", name, messages, test.want)
		}
	}

	if messages := problemMessages(checkRenderedLLMsIndex(t.TempDir())); !strings.Contains(messages, "is missing") {
		t.Errorf("missing index: problems = %q", messages)
	}
}
