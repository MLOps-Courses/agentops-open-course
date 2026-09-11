package accessibility

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

const (
	desktopWidth  = 1280
	desktopHeight = 900
)

type browser struct {
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	denyProxy   *httptest.Server
}

func startBrowser(ctx context.Context, chromePath string) (*browser, error) {
	// Chrome sends every non-loopback request to this non-forwarding proxy. The
	// explicit bypass list keeps both acceptance fixtures directly reachable.
	denyProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "external requests are disabled", http.StatusForbidden)
	}))
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options,
		chromedp.ExecPath(chromePath),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("disable-quic", true),
		chromedp.ProxyServer(denyProxy.URL),
		chromedp.Flag("proxy-bypass-list", "localhost;127.0.0.1;[::1]"),
	)
	allocator, allocCancel := chromedp.NewExecAllocator(ctx, options...)
	browserContext, cancel := chromedp.NewContext(allocator)
	if err := chromedp.Run(browserContext); err != nil {
		cancel()
		allocCancel()
		denyProxy.Close()
		return nil, fmt.Errorf("start Chrome %q: %w", chromePath, err)
	}
	return &browser{ctx: browserContext, cancel: cancel, allocCancel: allocCancel, denyProxy: denyProxy}, nil
}

func (browser *browser) close() error {
	closeContext, closeCancel := context.WithTimeout(context.WithoutCancel(browser.ctx), 5*time.Second)
	defer closeCancel()
	err := chromedp.Cancel(closeContext)
	browser.cancel()
	browser.allocCancel()
	browser.denyProxy.Close()
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("close Chrome: %w", err)
	}
	return nil
}

type browserPage struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (browser *browser) newPage(features []*emulation.MediaFeature) (*browserPage, error) {
	ctx, cancel := chromedp.NewContext(browser.ctx)
	page := &browserPage{ctx: ctx, cancel: cancel}
	actions := []chromedp.Action{
		chromedp.EmulateViewport(desktopWidth, desktopHeight),
		emulation.SetEmulatedMedia().WithFeatures(features),
	}
	if err := page.run(actions...); err != nil {
		cancel()
		return nil, fmt.Errorf("initialize browser page: %w", err)
	}
	return page, nil
}

func (page *browserPage) close() {
	page.cancel()
}

func (page *browserPage) run(actions ...chromedp.Action) error {
	return chromedp.Run(page.ctx, actions...)
}
