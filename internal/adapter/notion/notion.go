package notion

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/holman/deckk/internal/adapter"
)

func init() {
	adapter.Register(&Adapter{})
}

// Adapter turns a published Notion page into a PDF. Unlike the slide-deck
// adapters it doesn't screenshot anything: it drives headless Chrome to
// fully render the page (Notion virtualizes blocks and collapses toggles),
// then asks Chrome to print it, which yields a real text PDF.
type Adapter struct{}

func (a *Adapter) Name() string { return "notion" }

// pageIDRe matches the 32-hex-digit page ID Notion appends to page URLs
// ("…/Investment-Memo-0123456789abcdef0123456789abcdef").
var pageIDRe = regexp.MustCompile(`[0-9a-f]{32}`)

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	switch {
	case host == "notion.so" || strings.HasSuffix(host, ".notion.so"):
		return true
	case strings.HasSuffix(host, ".notion.site"):
		// Published sites: the root ("foo.notion.site/") is a page too.
		return true
	case host == "notion.com" || strings.HasSuffix(host, ".notion.com"):
		// notion.com also hosts marketing and help pages. Public pages
		// served from it (app.notion.com/p/…) carry a page ID in the path.
		return pageIDRe.MatchString(strings.ToLower(u.Path))
	}
	return false
}

// Fetch satisfies adapter.Adapter, but Notion pages are long-form documents
// rather than slides, so the CLI uses FetchPDF.
func (a *Adapter) Fetch(ctx context.Context, rawURL string, opts adapter.Options) ([]adapter.Slide, error) {
	return nil, fmt.Errorf("notion produces a whole PDF; use FetchPDF")
}

// Notion bounces user agents it considers too old to an "unsupported
// browser" page, so this has to track a reasonably current Chrome.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"

const (
	viewportWidth = 1200
	// Notion only mounts blocks that intersect the viewport, so we grow the
	// viewport to the page's full height to force everything to render.
	// Chrome copes with very tall viewports, but cap it so a pathological
	// page can't ask for something absurd.
	maxViewportHeight = 40000
	// Extra room below the measured content so the last blocks sit well
	// inside the render window.
	viewportSlack = 400
)

// metricsJS summarizes how much of the page is currently rendered. The
// expand loop runs until these stop changing.
const metricsJS = `
(() => {
  const pc = document.querySelector('.notion-page-content');
  if (!pc) return null;
  // Measure the content itself, not the scroller: Notion pads the bottom
  // of the scroller relative to the viewport, so its scrollHeight creeps
  // up every time we grow the viewport and would never settle.
  const sc = document.querySelector('.notion-scroller.vertical');
  const r = pc.getBoundingClientRect();
  return {
    height: r.bottom + (sc ? sc.scrollTop : window.scrollY),
    blocks: pc.querySelectorAll('[data-block-id]').length,
    text: (pc.innerText || '').length,
    collapsed: pc.querySelectorAll('[role="button"][aria-expanded="false"]').length,
    pendingImages: Array.from(pc.querySelectorAll('img')).filter(i => !i.complete).length,
  };
})()
`

// expandJS opens every collapsed toggle (toggle blocks and toggle headings
// both render their caret as a role=button with aria-expanded=false).
// Nested toggles only mount once their parent is open, so the caller loops
// until this returns 0.
const expandJS = `
(() => {
  const btns = document.querySelectorAll('.notion-page-content [role="button"][aria-expanded="false"]');
  btns.forEach(b => b.click());
  return btns.length;
})()
`

// hideCSS strips the page chrome that isn't part of the document: the
// "sign up to start building in Notion" banner, the top bar, the floating
// table of contents, presence avatars, and any overlay/popover layer.
const hideCSS = `
header,
.notion-topbar,
.notion-floating-table-of-contents,
.notion-presence-container,
.notion-overlay-container,
.notion-page-controls,
.notion-print-ignore {
  display: none !important;
}
.notion-image-block,
.notion-callout-block,
.notion-quote-block,
.notion-code-block,
.notion-table-block tr {
  break-inside: avoid;
}
`

type metrics struct {
	Height        float64 `json:"height"`
	Blocks        int     `json:"blocks"`
	Text          int     `json:"text"`
	Collapsed     int     `json:"collapsed"`
	PendingImages int     `json:"pendingImages"`
}

// FetchPDF renders the page in headless Chrome and prints it to PDF.
func (a *Adapter) FetchPDF(ctx context.Context, rawURL string, opts adapter.Options) ([]byte, error) {
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent(userAgent),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if opts.Headful {
		allocOpts = append(allocOpts, chromedp.Flag("headless", false))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer cancelAlloc()

	bctx, cancelB := chromedp.NewContext(allocCtx)
	defer cancelB()

	// Record the HTTP status of the page document itself. Notion serves
	// its app shell with a 200 and renders errors client-side, except for
	// rate limiting, where the document comes back 429 and the page stays
	// blank forever; without this we'd sit out the whole wait and then
	// blame the page's sharing settings.
	var docStatus atomic.Int64
	chromedp.ListenTarget(bctx, func(ev interface{}) {
		if e, ok := ev.(*network.EventResponseReceived); ok && e.Type == network.ResourceTypeDocument {
			docStatus.CompareAndSwap(0, e.Response.Status)
		}
	})

	if err := chromedp.Run(bctx,
		network.Enable(),
		// Notion follows the system theme; a dark-mode PDF is nobody's goal.
		emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{
			{Name: "prefers-color-scheme", Value: "light"},
		}),
		chromedp.EmulateViewport(viewportWidth, 1000),
		chromedp.Navigate(rawURL),
	); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	if err := waitForPage(bctx, &docStatus); err != nil {
		return nil, err
	}

	if err := renderEverything(bctx); err != nil {
		return nil, err
	}

	if err := chromedp.Run(bctx,
		chromedp.Evaluate(injectStyleJS(hideCSS), nil),
		chromedp.Sleep(500*time.Millisecond),
	); err != nil {
		return nil, fmt.Errorf("prepare for print: %w", err)
	}

	var pdfBuf []byte
	err := chromedp.Run(bctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		pdfBuf, _, err = page.PrintToPDF().
			WithPrintBackground(true).
			WithPaperWidth(8.5).
			WithPaperHeight(11).
			WithMarginTop(0.4).
			WithMarginBottom(0.4).
			WithMarginLeft(0.4).
			WithMarginRight(0.4).
			Do(ctx)
		return err
	}))
	if err != nil {
		return nil, fmt.Errorf("print to pdf: %w", err)
	}
	return pdfBuf, nil
}

// waitForPage polls until Notion has mounted the page body, and turns the
// common failure modes (rate limiting, private page, unsupported-browser
// bounce) into readable errors.
func waitForPage(ctx context.Context, docStatus *atomic.Int64) error {
	deadline := time.Now().Add(45 * time.Second)
	for {
		switch status := docStatus.Load(); {
		case status == 429:
			return fmt.Errorf("notion rate-limited this request (HTTP 429); wait a minute and try again")
		case status >= 400:
			return fmt.Errorf("notion returned HTTP %d for the page (is it published to the web?)", status)
		}

		var state struct {
			Ready       bool   `json:"ready"`
			URL         string `json:"url"`
			Unsupported bool   `json:"unsupported"`
			Private     bool   `json:"private"`
		}
		err := chromedp.Run(ctx, chromedp.Evaluate(`
(() => ({
  ready: !!document.querySelector('.notion-page-content'),
  url: location.href,
  unsupported: location.pathname.includes('unsupported-browser'),
  private: location.pathname.startsWith('/login') ||
           /page (is private|doesn.t exist|not found)|you don.t have access|request access/i.test(document.body.innerText || ''),
}))()`, &state))
		if err != nil {
			return fmt.Errorf("wait for page: %w", err)
		}
		if state.Unsupported {
			return fmt.Errorf("notion rejected the browser as unsupported; the adapter's user agent probably needs bumping")
		}
		if state.Ready {
			return nil
		}
		if state.Private {
			return fmt.Errorf("this page isn't public; only pages published to the web (Share → Publish) are supported")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for the Notion page to render at %s (is it published to the web?)", state.URL)
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(500*time.Millisecond)); err != nil {
			return err
		}
	}
}

// renderEverything defeats Notion's lazy rendering: it grows the viewport
// to the page's height so every block mounts, clicks open every toggle,
// and repeats until the page stops changing. Then it waits for images.
func renderEverything(ctx context.Context) error {
	var (
		viewportH int64 = 1000
		lastSig   string
		stable    int
		capped    bool
	)
	for i := 0; i < 60; i++ {
		var m *metrics
		if err := chromedp.Run(ctx, chromedp.Evaluate(metricsJS, &m)); err != nil {
			return fmt.Errorf("measure page: %w", err)
		}
		if m == nil {
			return fmt.Errorf("notion page content disappeared while rendering")
		}

		// Grow the viewport to cover the content so every block mounts.
		want := int64(m.Height) + viewportSlack
		if want > maxViewportHeight {
			want = maxViewportHeight
			if !capped {
				capped = true
				fmt.Fprintf(os.Stderr, "page is very tall; content below %dpx may be missing\n", maxViewportHeight)
			}
		}
		if want > viewportH {
			viewportH = want
			if err := chromedp.Run(ctx,
				chromedp.EmulateViewport(viewportWidth, viewportH),
				chromedp.Sleep(1200*time.Millisecond),
			); err != nil {
				return fmt.Errorf("resize viewport: %w", err)
			}
			stable = 0
			continue
		}

		var clicked int
		if err := chromedp.Run(ctx, chromedp.Evaluate(expandJS, &clicked)); err != nil {
			return fmt.Errorf("expand toggles: %w", err)
		}
		if clicked > 0 {
			fmt.Fprintf(os.Stderr, "expanded %d toggle(s)\n", clicked)
			if err := chromedp.Run(ctx, chromedp.Sleep(800*time.Millisecond)); err != nil {
				return err
			}
			stable = 0
			continue
		}

		sig := fmt.Sprintf("%d/%d", m.Blocks, m.Text)
		if sig == lastSig {
			stable++
			if stable >= 2 {
				break
			}
		} else {
			stable = 0
		}
		lastSig = sig
		if err := chromedp.Run(ctx, chromedp.Sleep(700*time.Millisecond)); err != nil {
			return err
		}
	}

	// Give images a bounded chance to finish loading; a missing image is a
	// worse outcome than a slow one, but not worth hanging forever.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var m *metrics
		if err := chromedp.Run(ctx, chromedp.Evaluate(metricsJS, &m)); err != nil {
			return err
		}
		if m == nil || m.PendingImages == 0 {
			return nil
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(500*time.Millisecond)); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "some images were still loading; continuing anyway\n")
	return nil
}

func injectStyleJS(css string) string {
	return fmt.Sprintf(`(() => { const s = document.createElement('style'); s.textContent = %q; document.head.appendChild(s); })()`, css)
}
