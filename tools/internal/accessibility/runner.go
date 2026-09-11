package accessibility

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/chromedp/cdproto/emulation"
)

const (
	defaultSiteDirectory   = "site"
	defaultClientDirectory = "clients/web"
	skipLinkSelector       = "body > a[href='#content']"
	navbarHomeSelector     = ".hextra-max-navbar-width a[href='/']"
	contentRootSelector    = "main#content .content"
)

var documentSurfaces = []struct {
	Path  string
	Label string
}{
	{Path: "/", Label: "homepage"},
	{Path: "/0-overview/0-4-ecosystem/", Label: "dense diagram page"},
	{Path: "/3-capabilities/3-6-a2a/", Label: "A2A page"},
	{Path: "/4-quality/4-6-security/", Label: "security page"},
	{Path: "/8-community/8-7-capstone/", Label: "capstone page"},
	{Path: "/404.html", Label: "404 recovery page"},
}

type Config struct {
	SiteDirectory   string
	ClientDirectory string
	ChromePath      string
}

func Run(ctx context.Context, config Config) (resultErr error) {
	config = withDefaults(config)
	if err := requireFile(filepath.Join(config.SiteDirectory, "index.html"), "site/index.html is missing; run `mise run build:docs`"); err != nil {
		return err
	}
	if err := requireFile(filepath.Join(config.ClientDirectory, "index.html"), "clients/web/index.html is missing"); err != nil {
		return err
	}
	chromePath, resolveErr := resolveChromePath(config.ChromePath)
	if resolveErr != nil {
		return resolveErr
	}

	docs := startStaticServer(config.SiteDirectory, nil)
	defer docs.Close()
	rpc := newFakeRPC()
	client := startStaticServer(config.ClientDirectory, rpc)
	defer client.Close()

	browser, startErr := startBrowser(ctx, chromePath)
	if startErr != nil {
		return startErr
	}
	defer func() {
		resultErr = errors.Join(resultErr, browser.close())
	}()

	light := []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "light"}}
	page, pageErr := browser.newPage(light)
	if pageErr != nil {
		return pageErr
	}
	defer page.close()
	homepage := docs.URL + "/"
	if err := keyboardSmoke(page, homepage, skipLinkSelector, navbarHomeSelector); err != nil {
		return err
	}
	if err := searchSmoke(page, homepage); err != nil {
		return err
	}
	for _, surface := range documentSurfaces {
		if err := accessibilityTreeSmoke(page, docs.URL+surface.Path, surface.Label); err != nil {
			return err
		}
	}
	// 6.0 rather than 0.4: contentControlsSmoke needs a page carrying more than one
	// diagram as well as a header-bearing table and a code-copy control, and 0.4 has
	// exactly one diagram. 0.4 stays in documentSurfaces above for its tree.
	densePage := docs.URL + "/6-platform/6-0-platform/"
	if err := contentControlsSmoke(page, densePage); err != nil {
		return err
	}
	for _, pageURL := range []string{homepage, docs.URL + "/4-quality/4-6-security/"} {
		if err := reflowSmoke(page, pageURL); err != nil {
			return err
		}
	}
	if err := openPage(page, homepage); err != nil {
		return err
	}
	for _, check := range []struct {
		Selector string
		Label    string
	}{
		{Selector: contentRootSelector + " > p", Label: "documentation body text"},
		{Selector: contentRootSelector + " p a", Label: "documentation content link"},
	} {
		if err := contrastSmoke(page, check.Selector, check.Label); err != nil {
			return err
		}
	}

	if err := darkContrastSmoke(browser, docs.URL); err != nil {
		return err
	}

	webClient := client.URL + "/index.html"
	if err := keyboardSmoke(page, webClient, ".skip-link", "#base-url"); err != nil {
		return err
	}
	if err := accessibilityTreeSmoke(page, webClient, "web client"); err != nil {
		return err
	}
	if err := webClientInteractionSmoke(page, webClient, rpc); err != nil {
		return err
	}
	if err := reflowSmoke(page, webClient); err != nil {
		return err
	}
	if err := openPage(page, webClient); err != nil {
		return err
	}
	if err := contrastSmoke(page, "header h1", "web client heading"); err != nil {
		return err
	}
	if err := addClientContrastBadges(page); err != nil {
		return fmt.Errorf("web client: add contrast badges: %w", err)
	}
	for _, state := range []string{"working", "input-required", "completed"} {
		if err := contrastSmoke(page, "#contrast-"+state, "web client "+state+" state"); err != nil {
			return err
		}
	}

	reducedFeatures := []*emulation.MediaFeature{
		{Name: "prefers-color-scheme", Value: "light"},
		{Name: "prefers-reduced-motion", Value: "reduce"},
	}
	reduced, reducedErr := browser.newPage(reducedFeatures)
	if reducedErr != nil {
		return reducedErr
	}
	defer reduced.close()
	if err := reducedMotionSmoke(reduced, homepage, false); err != nil {
		return err
	}
	if err := reducedMotionSmoke(reduced, webClient, true); err != nil {
		return err
	}

	forcedFeatures := []*emulation.MediaFeature{
		{Name: "prefers-color-scheme", Value: "light"},
		{Name: "forced-colors", Value: "active"},
	}
	forced, forcedErr := browser.newPage(forcedFeatures)
	if forcedErr != nil {
		return forcedErr
	}
	defer forced.close()
	if err := forcedColorsSmoke(forced, webClient); err != nil {
		return err
	}

	if math.Abs(contrastRatio(rgb{0, 0, 0}, rgb{255, 255, 255})-21) >= 0.001 {
		return errors.New("contrast calculation self-test failed")
	}
	return nil
}

func withDefaults(config Config) Config {
	if config.SiteDirectory == "" {
		config.SiteDirectory = defaultSiteDirectory
	}
	if config.ClientDirectory == "" {
		config.ClientDirectory = defaultClientDirectory
	}
	return config
}

func requireFile(path, message string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New(message)
	}
	return nil
}
