package pitch

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/holman/deckk/internal/adapter"
)

func init() {
	adapter.Register(&Adapter{})
}

type Adapter struct{}

func (a *Adapter) Name() string { return "pitch" }

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	return host == "pitch.com" || strings.HasSuffix(host, ".pitch.com")
}

type viewportRect struct {
	X, Y, W, H float64
}

// findSlideJS returns the rect of the currently-visible slide plus the
// deck's slide counter. Pitch's public player (`/v/...`) renders the active
// slide into a `.slide-wrapper`/`.slide` element inside `.player-v2--stage`,
// at a clean 16:9 box, with the chrome (controls, counter) docked below it.
// The counter ("3 / 13") comes from the chrome's slide-count control; we
// fall back to scanning the body text if the class ever changes.
const findSlideJS = `
(() => {
  const el = document.querySelector('.slide-wrapper') || document.querySelector('.slide');
  if (!el) return null;
  const r = el.getBoundingClientRect();
  if (r.width < 50 || r.height < 50) return null;

  let total = 0, current = 0;
  const countEl = document.querySelector('.player-v2-chrome-controls-slide-count');
  const text = (countEl && countEl.textContent) || document.body.innerText || '';
  const m = text.match(/(\d+)\s*\/\s*(\d+)/);
  if (m) { current = parseInt(m[1], 10); total = parseInt(m[2], 10); }

  return { x: r.left, y: r.top, w: r.width, h: r.height, total, current };
})()
`

// hideOverlaysJS strips the player chrome and any popovers/banners that would
// otherwise leak into the screenshots. The slide clip sits above the chrome
// so there's usually no overlap, but Pitch floats a "make a presentation like
// this one" branding popover and the odd menu over the stage, so we hide them
// defensively along with the usual cookie/consent banners.
const hideOverlaysJS = `
(() => {
  const hide = el => el && el.style.setProperty('display', 'none', 'important');
  const selectors = [
    '.player-v2-chrome',
    '.player-branding-popover',
    '.player-burger-menu-panel',
    '.player-menu-popover',
    '.player-options-menu',
    '#onetrust-banner-sdk',
    '#onetrust-consent-sdk',
    '[id*="cookie" i][class*="banner" i]',
    '[class*="cookie" i][class*="banner" i]',
    '[data-testid*="cookie" i]',
    '[data-testid*="consent" i]',
    '#intercom-frame', '.intercom-launcher-frame',
    '#hubspot-messages-iframe-container',
  ];
  for (const sel of selectors) {
    document.querySelectorAll(sel).forEach(hide);
  }

  // Consent iframes by src (OneTrust/TrustArc/Cookiebot/etc).
  document.querySelectorAll('iframe').forEach(f => {
    const src = (f.src || '') + ' ' + (f.id || '') + ' ' + (typeof f.className === 'string' ? f.className : '');
    if (/onetrust|trustarc|cookiebot|consent|cookie|gdpr|ccpa/i.test(src)) hide(f);
  });
})()
`

func (a *Adapter) Fetch(ctx context.Context, rawURL string, opts adapter.Options) ([]adapter.Slide, error) {
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if opts.Headful {
		allocOpts = append(allocOpts, chromedp.Flag("headless", false))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer cancelAlloc()

	bctx, cancelB := chromedp.NewContext(allocCtx)
	defer cancelB()

	if err := chromedp.Run(bctx,
		chromedp.EmulateViewport(1600, 1200),
		chromedp.Navigate(rawURL),
		chromedp.Sleep(5*time.Second),
		chromedp.Evaluate(hideOverlaysJS, nil),
	); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	det, err := findSlide(bctx)
	if err != nil {
		return nil, err
	}
	if det == nil {
		return nil, fmt.Errorf("could not find slide player on the page")
	}

	// The counter is authoritative for how many slides to expect; if it's
	// missing (markup change), fall back to a generous cap and rely on the
	// hash-repeat stop to find the end.
	maxSlides := det.Total
	if maxSlides == 0 {
		maxSlides = 200
		fmt.Fprintf(os.Stderr, "no slide counter found; walking until the deck repeats\n")
	} else {
		fmt.Fprintf(os.Stderr, "deck has %d slide(s)\n", maxSlides)
	}

	var slides []adapter.Slide
	var lastHash [32]byte
	for i := 1; i <= maxSlides; i++ {
		if i > 1 {
			if err := chromedp.Run(bctx, chromedp.KeyEvent(kb.ArrowRight)); err != nil {
				return nil, fmt.Errorf("advance to slide %d: %w", i, err)
			}
		}
		if err := chromedp.Run(bctx,
			chromedp.Sleep(700*time.Millisecond),
			chromedp.Evaluate(hideOverlaysJS, nil),
		); err != nil {
			return nil, err
		}

		rect, err := findSlide(bctx)
		if err != nil {
			return nil, err
		}
		if rect == nil {
			fmt.Fprintf(os.Stderr, "slide %d vanished; stopping\n", i)
			break
		}

		shot, err := captureViewportClip(bctx, viewportRect{X: rect.X, Y: rect.Y, W: rect.W, H: rect.H})
		if err != nil {
			return nil, fmt.Errorf("screenshot slide %d: %w", i, err)
		}
		h := sha256.Sum256(shot)
		if i > 1 && h == lastHash {
			// ArrowRight didn't change the rendered slide. Could be a slow
			// transition or we're past the end of the deck — give it one
			// more beat before deciding.
			if err := chromedp.Run(bctx, chromedp.Sleep(1500*time.Millisecond)); err != nil {
				return nil, err
			}
			rect, err = findSlide(bctx)
			if err != nil {
				return nil, err
			}
			if rect != nil {
				shot, err = captureViewportClip(bctx, viewportRect{X: rect.X, Y: rect.Y, W: rect.W, H: rect.H})
				if err != nil {
					return nil, fmt.Errorf("screenshot slide %d: %w", i, err)
				}
				h = sha256.Sum256(shot)
			}
			if rect == nil || h == lastHash {
				fmt.Fprintf(os.Stderr, "end of deck after %d slide(s)\n", len(slides))
				break
			}
		}
		lastHash = h
		slides = append(slides, adapter.Slide{Data: shot, ContentType: "image/png"})
	}

	if len(slides) == 0 {
		return nil, fmt.Errorf("no slides captured")
	}
	return slides, nil
}

// slideInfo is the parsed result of findSlideJS.
type slideInfo struct {
	X, Y, W, H float64
	Total      int `json:"total"`
	Current    int `json:"current"`
}

func findSlide(ctx context.Context) (*slideInfo, error) {
	var info *slideInfo
	if err := chromedp.Run(ctx, chromedp.Evaluate(findSlideJS, &info)); err != nil {
		return nil, fmt.Errorf("find slide: %w", err)
	}
	return info, nil
}

func captureViewportClip(ctx context.Context, r viewportRect) ([]byte, error) {
	var buf []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		buf, err = page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithClip(&page.Viewport{
				X:      r.X,
				Y:      r.Y,
				Width:  r.W,
				Height: r.H,
				Scale:  2,
			}).
			Do(ctx)
		return err
	}))
	return buf, err
}
