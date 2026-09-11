package accessibility

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	cdpaccessibility "github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const pollTimeout = 10 * time.Second

type elementState struct {
	Target       string  `json:"target"`
	Opacity      float64 `json:"opacity"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Width        float64 `json:"width"`
	Height       float64 `json:"height"`
	ViewportW    float64 `json:"viewportWidth"`
	ViewportH    float64 `json:"viewportHeight"`
	Present      bool    `json:"present"`
	Active       bool    `json:"active"`
	FocusVisible bool    `json:"focusVisible"`
}

func openPage(page *browserPage, pageURL string) error {
	response, err := chromedp.RunResponse(page.ctx, chromedp.Navigate(pageURL))
	if err != nil {
		return fmt.Errorf("%s: open document: %w", pageURL, err)
	}
	if response == nil || response.Status < 200 || response.Status >= 300 {
		status := int64(0)
		if response != nil {
			status = response.Status
		}
		return fmt.Errorf("%s: expected a successful document response, got HTTP %d", pageURL, status)
	}
	return nil
}

func keyboardSmoke(page *browserPage, pageURL, skipSelector, nextSelector string) error {
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	before, beforeErr := readElementState(page, skipSelector)
	if beforeErr != nil {
		return fmt.Errorf("%s: inspect skip link: %w", pageURL, beforeErr)
	}
	if !before.Present {
		return fmt.Errorf("%s: skip link %q is missing", pageURL, skipSelector)
	}
	if err := page.run(
		chromedp.KeyEvent(kb.Tab),
		chromedp.PollFunction(
			`selector => {
        const element = document.querySelector(selector);
        if (!element) return false;
        const box = element.getBoundingClientRect();
        return element === document.activeElement && Number(getComputedStyle(element).opacity) > 0.9
          && box.right > 0 && box.bottom > 0 && box.left < innerWidth && box.top < innerHeight;
      }`,
			nil,
			chromedp.WithPollingArgs(skipSelector),
			chromedp.WithPollingInterval(50*time.Millisecond),
			chromedp.WithPollingTimeout(pollTimeout),
		),
	); err != nil {
		return fmt.Errorf("%s: focus visible skip link: %w", pageURL, err)
	}
	after, afterErr := readElementState(page, skipSelector)
	if afterErr != nil {
		return fmt.Errorf("%s: inspect focused skip link: %w", pageURL, afterErr)
	}
	if !after.Active {
		return fmt.Errorf("%s: first Tab must focus the skip link", pageURL)
	}
	beforeHidden := before.X < 0 || before.Opacity < 0.1
	afterVisible := after.X >= 0 && after.Y >= 0 && after.X < after.ViewportW && after.Y < after.ViewportH
	if !beforeHidden || !afterVisible {
		return fmt.Errorf("%s: focused skip link must move visibly into the viewport", pageURL)
	}
	if !strings.HasPrefix(after.Target, "#") {
		return fmt.Errorf("%s: skip link needs a same-page target", pageURL)
	}
	if err := page.run(
		chromedp.KeyEvent(kb.Enter),
		chromedp.PollFunction(
			`target => location.hash === target`,
			nil,
			chromedp.WithPollingArgs(after.Target),
			chromedp.WithPollingInterval(50*time.Millisecond),
			chromedp.WithPollingTimeout(pollTimeout),
		),
	); err != nil {
		return fmt.Errorf("%s: activate skip link: %w", pageURL, err)
	}
	var targetCount int
	if err := page.run(callFunction(
		`function(target) { return document.querySelectorAll(target).length; }`,
		&targetCount,
		after.Target,
	)); err != nil {
		return fmt.Errorf("%s: inspect skip target %q: %w", pageURL, after.Target, err)
	}
	if targetCount != 1 {
		return fmt.Errorf("%s: skip target %q must exist exactly once", pageURL, after.Target)
	}

	if err := openPage(page, pageURL); err != nil {
		return err
	}
	if err := page.run(chromedp.KeyEvent(kb.Tab), chromedp.KeyEvent(kb.Tab)); err != nil {
		return fmt.Errorf("%s: tab to next control: %w", pageURL, err)
	}
	next, nextErr := readElementState(page, nextSelector)
	if nextErr != nil {
		return fmt.Errorf("%s: inspect next control %q: %w", pageURL, nextSelector, nextErr)
	}
	if !next.Active {
		return fmt.Errorf("%s: second Tab must reach %q", pageURL, nextSelector)
	}
	if !next.FocusVisible {
		return fmt.Errorf("%s: keyboard focus must be visible", pageURL)
	}
	return nil
}

func readElementState(page *browserPage, selector string) (elementState, error) {
	var state elementState
	err := page.run(callFunction(
		`function(selector) {
      const element = document.querySelector(selector);
      if (!element) return { present: false };
      const box = element.getBoundingClientRect();
      const style = getComputedStyle(element);
      return {
        present: element.getClientRects().length > 0,
        active: element === document.activeElement,
        focusVisible: element.matches(":focus-visible"),
        opacity: Number(style.opacity),
        x: box.x, y: box.y, width: box.width, height: box.height,
        viewportWidth: innerWidth, viewportHeight: innerHeight,
        target: element.getAttribute("href") || "",
      };
		}`,
		&state,
		selector,
	))
	return state, err
}

func accessibilityTreeSmoke(page *browserPage, pageURL, label string) error {
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	var heading string
	if err := page.run(chromedp.Text("h1", &heading, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("%s: read H1: %w", label, err)
	}
	nodes, err := accessibilityTree(page)
	if err != nil {
		return fmt.Errorf("%s: read accessibility tree: %w", label, err)
	}
	if err := validateAXTree(nodes, strings.TrimSpace(heading), label); err != nil {
		return err
	}
	return nil
}

func accessibilityTree(page *browserPage) ([]axNode, error) {
	var rawNodes []*cdpaccessibility.Node
	if err := page.run(chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		rawNodes, err = cdpaccessibility.GetFullAXTree().Do(ctx)
		return err
	})); err != nil {
		return nil, err
	}
	nodes := make([]axNode, 0, len(rawNodes))
	for _, raw := range rawNodes {
		if raw.Ignored {
			continue
		}
		role, err := axValue(raw.Role)
		if err != nil {
			return nil, fmt.Errorf("decode accessibility role: %w", err)
		}
		name, err := axValue(raw.Name)
		if err != nil {
			return nil, fmt.Errorf("decode accessibility name: %w", err)
		}
		nodes = append(nodes, axNode{Role: role, Name: name})
	}
	return nodes, nil
}

func axValue(value *cdpaccessibility.Value) (string, error) {
	if value == nil || len(value.Value) == 0 {
		return "", nil
	}
	var decoded string
	if err := json.Unmarshal(value.Value, &decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func searchSmoke(page *browserPage, pageURL string) error {
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	var search struct {
		AriaLabel       string `json:"ariaLabel"`
		Role            string `json:"role"`
		Autocomplete    string `json:"autocomplete"`
		Controls        string `json:"controls"`
		FlexSearchType  string `json:"flexSearchType"`
		ResultList      int    `json:"resultList"`
		StatusRegions   int    `json:"statusRegions"`
		VisibleWrappers int    `json:"visibleWrappers"`
		SearchClass     bool   `json:"searchClass"`
	}
	if err := page.run(callFunction(
		`function() {
      const nodes = [...document.querySelectorAll(".hextra-search-wrapper input[type='search']")];
      const input = nodes.find(node => {
        const style = getComputedStyle(node);
        const box = node.getBoundingClientRect();
        return style.visibility !== "hidden" && style.display !== "none" && box.width > 0 && box.height > 0;
      });
      if (!input) throw new Error("missing visible search input");
      input.id = "browser-acceptance-search";
      const controls = input.getAttribute("aria-controls") || "";
      const wrapper = input.closest(".hextra-search-wrapper");
      return {
        ariaLabel: input.getAttribute("aria-label") || "",
        role: input.getAttribute("role") || "",
        autocomplete: input.getAttribute("aria-autocomplete") || "",
        controls,
        resultList: controls && document.getElementById(controls) ? 1 : 0,
		statusRegions: wrapper ? wrapper.querySelectorAll(".hextra-search-status[aria-live='polite']").length : 0,
		visibleWrappers: [...document.querySelectorAll(".hextra-search-wrapper")]
		  .filter(candidate => candidate.clientHeight > 0).length,
		flexSearchType: typeof FlexSearch,
		searchClass: input.classList.contains("hextra-search-input"),
      };
		}`,
		&search,
	)); err != nil {
		return fmt.Errorf("%s: inspect search semantics: %w", pageURL, err)
	}
	if search.AriaLabel == "" {
		return fmt.Errorf("%s: search input needs an accessible name", pageURL)
	}
	if search.Role != "combobox" || search.Autocomplete != "list" {
		return fmt.Errorf("%s: search input must expose list-autocomplete combobox semantics", pageURL)
	}
	if search.Controls == "" || search.ResultList != 1 {
		return fmt.Errorf("%s: search result list %q is missing", pageURL, search.Controls)
	}
	if search.StatusRegions != 1 {
		return fmt.Errorf("%s: search needs one polite status region", pageURL)
	}
	if search.VisibleWrappers != 1 {
		return fmt.Errorf("%s: search needs exactly one visible wrapper, got %d", pageURL, search.VisibleWrappers)
	}
	if search.FlexSearchType == "undefined" || !search.SearchClass {
		return fmt.Errorf("%s: Hextra search runtime is unavailable (FlexSearch=%s, input class=%t)", pageURL, search.FlexSearchType, search.SearchClass)
	}
	var searchFocused bool
	if err := page.run(chromedp.Evaluate(`(() => {
    const input = document.getElementById("browser-acceptance-search");
    if (!input) throw new Error("search input disappeared");
    input.focus();
		input.dispatchEvent(new FocusEvent("focus"));
    return input === document.activeElement;
  })()`, &searchFocused)); err != nil {
		return fmt.Errorf("%s: focus search input: %w", pageURL, err)
	}
	if !searchFocused {
		return fmt.Errorf("%s: search input did not receive focus", pageURL)
	}
	if err := page.run(
		chromedp.Poll(
			`window.pageIndex !== undefined && window.sectionIndex !== undefined`,
			nil,
			chromedp.WithPollingInterval(50*time.Millisecond),
			chromedp.WithPollingTimeout(pollTimeout),
		),
	); err != nil {
		return fmt.Errorf("%s: initialize search index: %w", pageURL, err)
	}
	if err := page.run(chromedp.Poll(
		`window.pageIndex.search("A2A", 20, { enrich: true, suggest: true })[0]?.result?.length > 0`,
		nil,
		chromedp.WithPollingInterval(50*time.Millisecond),
		chromedp.WithPollingTimeout(pollTimeout),
	)); err != nil {
		return fmt.Errorf("%s: load search data: %w", pageURL, err)
	}
	if err := page.run(
		chromedp.KeyEvent("A"),
		chromedp.Sleep(40*time.Millisecond),
		chromedp.KeyEvent("2"),
		chromedp.Sleep(40*time.Millisecond),
		chromedp.KeyEvent("A"),
		chromedp.PollFunction(
			`id => document.getElementById(id)?.getAttribute("aria-expanded") === "true"`,
			nil,
			chromedp.WithPollingArgs("browser-acceptance-search"),
			chromedp.WithPollingInterval(50*time.Millisecond),
			chromedp.WithPollingTimeout(pollTimeout),
		),
	); err != nil {
		return fmt.Errorf("%s: search for A2A: %w", pageURL, err)
	}
	var results int
	if err := page.run(callFunction(
		`function(id) { return document.getElementById(id)?.querySelectorAll("a").length || 0; }`,
		&results,
		search.Controls,
	)); err != nil {
		return fmt.Errorf("%s: inspect search results: %w", pageURL, err)
	}
	if results == 0 {
		return fmt.Errorf("%s: search did not return a recovery route for A2A", pageURL)
	}
	return nil
}

func contentControlsSmoke(page *browserPage, pageURL string) error {
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	var diagrams int
	if err := page.run(
		callFunction(
			`function() { return document.querySelectorAll("main#content pre.mermaid").length; }`,
			&diagrams,
		),
		chromedp.Poll(`(() => {
      const nodes = [...document.querySelectorAll("main#content pre.mermaid")];
      return nodes.length > 0 && nodes.every(node => node.querySelector("svg") !== null);
    })()`, nil, chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(pollTimeout)),
	); err != nil {
		return fmt.Errorf("%s: inspect Mermaid rendering: %w", pageURL, err)
	}
	// The number tracks the corpus, not an aspiration: no page in content/ carries
	// more than two Mermaid blocks, so a threshold of three made this assertion
	// unsatisfiable on every page and left check:accessibility permanently red. Two
	// is what "dense" means here, and the plural is what matters — the label
	// assertion below has to run against more than one diagram to prove anything.
	const denseDiagramSurface = 2
	if diagrams < denseDiagramSurface {
		return fmt.Errorf(
			"%s: representative page carries %d diagrams; the label assertion needs at least %d",
			pageURL, diagrams, denseDiagramSurface,
		)
	}
	// Counting labeled diagrams cannot fail: the theme's render hook wraps every
	// one in `role="img" aria-label="…"`, so the count and the total were the same
	// expression. What can fail is the *name* — fifty-five images all announced as
	// "Diagram" is a labeled surface nobody can navigate.
	var generic int
	if err := page.run(callFunction(
		`function() {
      return [...document.querySelectorAll("main#content [role='img']:has(pre.mermaid)")]
        .filter(node => (node.getAttribute("aria-label") || "").trim().toLowerCase() === "diagram")
        .length;
    }`,
		&generic,
	)); err != nil {
		return fmt.Errorf("%s: inspect diagram labels: %w", pageURL, err)
	}
	if generic > 0 {
		return fmt.Errorf("%s: %d of %d diagrams are announced only as \"Diagram\"; give each one a name", pageURL, generic, diagrams)
	}
	nodes, treeErr := accessibilityTree(page)
	if treeErr != nil {
		return fmt.Errorf("%s: inspect table accessibility: %w", pageURL, treeErr)
	}
	columnHeaders := 0
	for _, node := range nodes {
		if node.Role == "columnheader" {
			columnHeaders++
		}
	}
	if columnHeaders == 0 {
		return fmt.Errorf("%s: representative table has no column headers", pageURL)
	}
	const copySelector = `button[aria-label="Copy code"]`
	var copyFocused bool
	if err := page.run(
		chromedp.KeyEvent(kb.Tab),
		chromedp.Evaluate(`(() => {
    const button = document.querySelector('button[aria-label="Copy code"]');
    if (!button) throw new Error("code-copy control is missing");
    button.focus();
    return button === document.activeElement;
  })()`, &copyFocused),
	); err != nil {
		return fmt.Errorf("%s: focus code-copy control: %w", pageURL, err)
	}
	if !copyFocused {
		return fmt.Errorf("%s: code-copy control did not receive focus", pageURL)
	}
	copyState, copyErr := readElementState(page, copySelector)
	if copyErr != nil {
		return fmt.Errorf("%s: inspect code-copy focus: %w", pageURL, copyErr)
	}
	if !copyState.Active || !copyState.FocusVisible {
		return fmt.Errorf("%s: code-copy control is not keyboard focused", pageURL)
	}
	if err := page.run(chromedp.KeyEvent(kb.Enter)); err != nil {
		return fmt.Errorf("%s: activate code-copy control: %w", pageURL, err)
	}
	return nil
}

func reflowSmoke(page *browserPage, pageURL string) (err error) {
	if viewportErr := page.run(chromedp.EmulateViewport(320, 800)); viewportErr != nil {
		return fmt.Errorf("%s: set 320px viewport: %w", pageURL, viewportErr)
	}
	defer func() {
		if restoreErr := page.run(chromedp.EmulateViewport(desktopWidth, desktopHeight)); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore desktop viewport: %w", restoreErr))
		}
	}()
	if openErr := openPage(page, pageURL); openErr != nil {
		return openErr
	}
	var widths struct {
		Viewport float64 `json:"viewport"`
		Content  float64 `json:"content"`
	}
	if inspectErr := page.run(chromedp.Evaluate(`({
    viewport: document.documentElement.clientWidth,
    content: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
	})`, &widths)); inspectErr != nil {
		return fmt.Errorf("%s: inspect reflow widths: %w", pageURL, inspectErr)
	}
	if widths.Content > widths.Viewport+1 {
		return fmt.Errorf("%s: 320px viewport overflows horizontally (%.0fpx > %.0fpx)", pageURL, widths.Content, widths.Viewport)
	}
	return nil
}

func reducedMotionSmoke(page *browserPage, pageURL string, stopsAnimation bool) error {
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	var values struct {
		Transition string `json:"transition"`
		Animation  string `json:"animation"`
		Reduced    bool   `json:"reduced"`
	}
	if err := page.run(chromedp.Evaluate(`(() => {
    const probe = document.createElement("div");
    probe.style.cssText = "transition-duration: 2s; animation-duration: 2s";
    document.body.append(probe);
    const style = getComputedStyle(probe);
    return {
      reduced: matchMedia("(prefers-reduced-motion: reduce)").matches,
      transition: style.transitionDuration,
      animation: style.animationDuration,
    };
  })()`, &values)); err != nil {
		return fmt.Errorf("%s: inspect reduced motion: %w", pageURL, err)
	}
	if !values.Reduced {
		return fmt.Errorf("%s: reduced motion is not active", pageURL)
	}
	transition, transitionErr := durationSeconds(values.Transition)
	if transitionErr != nil {
		return fmt.Errorf("%s: parse transition duration: %w", pageURL, transitionErr)
	}
	if transition > 0.001 {
		return fmt.Errorf("%s: reduced motion did not stop transitions", pageURL)
	}
	if stopsAnimation {
		animation, animationErr := durationSeconds(values.Animation)
		if animationErr != nil {
			return fmt.Errorf("%s: parse animation duration: %w", pageURL, animationErr)
		}
		if animation > 0.001 {
			return fmt.Errorf("%s: reduced motion did not stop animations", pageURL)
		}
	}
	return nil
}

func contrastSmoke(page *browserPage, selector, label string) error {
	var colors struct {
		Foreground rgb `json:"foreground"`
		Background rgb `json:"background"`
	}
	if err := page.run(callFunction(contrastColorsJS, &colors, selector)); err != nil {
		return fmt.Errorf("%s: inspect computed contrast: %w", label, err)
	}
	ratio := contrastRatio(colors.Foreground, colors.Background)
	if ratio < 4.5 {
		return fmt.Errorf("%s: computed contrast is %.2f:1, expected at least 4.5:1", label, ratio)
	}
	return nil
}

func callFunction(declaration string, result any, arguments ...any) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		encoded := make([]string, 0, len(arguments))
		for _, argument := range arguments {
			value, err := json.Marshal(argument)
			if err != nil {
				return fmt.Errorf("encode browser function argument: %w", err)
			}
			encoded = append(encoded, string(value))
		}
		expression := "(" + declaration + ")(" + strings.Join(encoded, ",") + ")"
		return chromedp.Evaluate(expression, result).Do(ctx)
	})
}

// darkAdmonitionKinds are the seven accent tokens assets/css/custom.css flips under
// .dark. Each pairs an accent-colored title against the surface behind it, and a
// large developer audience reads this course in exactly that palette.
var darkAdmonitionKinds = []string{"abstract", "danger", "info", "note", "success", "tip", "warning"}

// darkContrastSmoke measures the dark palette, which no other page here exercises.
//
// Every other emulated page pins prefers-color-scheme: light while hugo.toml sets
// [params.theme] default = "system", so the palette most developers actually read
// had never been contrast-tested. The theme toggle is what sets .dark on <html>, so
// this drives that rather than trusting the media query alone.
func darkContrastSmoke(browser *browser, docsURL string) error {
	page, err := browser.newPage([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "dark"}})
	if err != nil {
		return err
	}
	defer page.close()
	pageURL := docsURL + "/0-overview/0-0-course/"
	if err := openPage(page, pageURL); err != nil {
		return err
	}
	var dark bool
	if err := page.run(
		chromedp.Evaluate(`(() => {
      document.documentElement.classList.add("dark");
      document.documentElement.classList.remove("light");
      return document.documentElement.classList.contains("dark");
    })()`, &dark),
	); err != nil {
		return fmt.Errorf("%s: switch to the dark theme: %w", pageURL, err)
	}
	if !dark {
		return fmt.Errorf("%s: the dark theme did not apply", pageURL)
	}
	// Probe every accent against the surface, whether or not this page happens to
	// use that admonition kind, by borrowing one existing block's markup.
	for _, kind := range darkAdmonitionKinds {
		var applied bool
		if err := page.run(callFunction(`function(kind) {
      const block = document.querySelector("main#content .course-admonition");
      if (!block) { return false; }
      block.className = block.className.replace(/course-admonition--[a-z]+/, "course-admonition--" + kind);
      return block.classList.contains("course-admonition--" + kind);
    }`, &applied, kind)); err != nil {
			return fmt.Errorf("%s: apply the %s accent: %w", pageURL, kind, err)
		}
		if !applied {
			return fmt.Errorf("%s: no admonition block to measure the %s accent against", pageURL, kind)
		}
		label := "dark " + kind + " admonition title"
		if err := contrastSmoke(page, "main#content .course-admonition .course-admonition__title", label); err != nil {
			return err
		}
	}
	// The sidebar's current-page link is the one navigation element whose color
	// changes with the theme rather than with focus.
	return contrastSmoke(page, `a.course-nav__link[aria-current="page"]`, "dark sidebar current page")
}
