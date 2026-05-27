package canva

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

func (a *Adapter) Name() string { return "canva" }

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	return host == "canva.com" || strings.HasSuffix(host, ".canva.com")
}

type viewportRect struct {
	X, Y, W, H float64
}

// detectPlayerJS figures out (a) the rect of the slide player and (b) how
// many real slides the deck has. Canva's /view page renders all pages
// stacked on top of each other at the same coordinates — only the
// "active" one is visible — so we exploit that: the largest cluster of
// same-sized, same-position boxes is the deck. The cluster also contains
// a few non-slide overlays (end card, share/replay, "more from Canva"),
// so we filter to elements whose content actually looks like a rendered
// slide: an image / SVG / canvas / background-image that fills most of
// the box. Returns viewport-space rect for CDP's screenshot clip.
const detectPlayerJS = `
(() => {
  const all = Array.from(document.querySelectorAll('div, section, article, figure'));
  const buckets = new Map();
  for (const el of all) {
    const r = el.getBoundingClientRect();
    if (r.width < 400 || r.height < 250) continue;
    const ar = r.width / r.height;
    if (ar < 0.5 || ar > 3) continue;
    const key = Math.round(r.width) + 'x' + Math.round(r.height) + '@' + Math.round(r.left) + ',' + Math.round(r.top);
    if (!buckets.has(key)) buckets.set(key, { rect: r, els: [] });
    buckets.get(key).els.push(el);
  }
  let best = null;
  for (const v of buckets.values()) {
    if (!best || v.els.length > best.els.length) best = v;
  }
  if (!best || best.els.length < 1) return null;

  const looksLikeSlide = el => {
    // Real slide content: a sizeable image/svg/canvas descendant, or a
    // background-image. End-card overlays are usually plain text + buttons
    // with no large pixel content.
    const r = el.getBoundingClientRect();
    const minArea = (r.width * r.height) * 0.3;
    const media = el.querySelectorAll('img, svg, canvas, picture, video');
    for (const m of media) {
      const mr = m.getBoundingClientRect();
      if (mr.width * mr.height >= minArea) return true;
    }
    const stack = [el, ...el.querySelectorAll('div')];
    for (const n of stack.slice(0, 200)) {
      const bg = getComputedStyle(n).backgroundImage;
      if (bg && bg !== 'none' && /url\(/i.test(bg)) {
        const nr = n.getBoundingClientRect();
        if (nr.width * nr.height >= minArea) return true;
      }
    }
    return false;
  };

  const kept = best.els.filter(looksLikeSlide);
  const r = best.rect;
  return {
    x: r.x, y: r.y, w: r.width, h: r.height,
    clusterSize: best.els.length,
    count: kept.length,
  };
})()
`

// hideOverlaysJS strips chrome that would otherwise leak into the
// screenshots: cookie banner, persistent CTAs, Canva's top nav, chat
// widgets. As a final pass it sweeps any fixed/sticky element pinned to
// the edges with consent-y text, which catches OneTrust / TrustArc /
// Cookiebot / Canva's own variants without us enumerating every selector.
const hideOverlaysJS = `
(() => {
  const hide = el => el && el.style.setProperty('display', 'none', 'important');
  const selectors = [
    '#onetrust-banner-sdk',
    '#onetrust-consent-sdk',
    '#truste-consent-track',
    '#truste-consent-content',
    '[id*="cookie" i][class*="banner" i]',
    '[class*="cookie" i][class*="banner" i]',
    '[data-testid*="cookie" i]',
    '[data-testid*="consent" i]',
    'header',
    '[role="banner"]',
    '[data-testid*="header" i]',
    '[data-testid*="top-bar" i]',
    '[data-testid*="signup" i]',
    '[data-testid*="sign-up" i]',
    '[data-testid*="cta" i]',
    '[data-testid*="footer" i]',
    'footer',
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

  // Last-resort cookie sweep: any fixed/sticky element near a viewport
  // edge with consent-y text. Match on text content so renamed/obfuscated
  // banners still get caught.
  const needles = /(cookie|consent|gdpr|accept all|manage preferences|privacy choices)/i;
  const vh = window.innerHeight;
  document.querySelectorAll('div, section, aside').forEach(el => {
    const pos = getComputedStyle(el).position;
    if (pos !== 'fixed' && pos !== 'sticky') return;
    const r = el.getBoundingClientRect();
    if (r.height > vh * 0.6) return;
    const nearEdge = r.bottom > vh * 0.5 || r.top < vh * 0.2;
    if (!nearEdge) return;
    const txt = (el.textContent || '').slice(0, 400);
    if (needles.test(txt)) hide(el);
  });

  // Floating action buttons (Present, Share, Edit, …) — walk up to the
  // nearest small floating wrapper so we hide just the chip, not its
  // anchored container.
  for (const sel of [
    'button[aria-label*="Present" i]',
    'button[aria-label*="Share" i]',
    'button[aria-label*="Download" i]',
    'button[aria-label*="Sign" i]',
    'button[aria-label*="Edit" i]',
  ]) {
    document.querySelectorAll(sel).forEach(b => {
      let n = b;
      for (let i = 0; i < 4 && n; i++) {
        const p = getComputedStyle(n).position;
        if (p === 'fixed' || p === 'sticky' || p === 'absolute') { hide(n); return; }
        n = n.parentElement;
      }
      hide(b);
    });
  }
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

	var det *struct {
		X, Y, W, H  float64
		ClusterSize int `json:"clusterSize"`
		Count       int `json:"count"`
	}
	if err := chromedp.Run(bctx, chromedp.Evaluate(detectPlayerJS, &det)); err != nil {
		return nil, fmt.Errorf("detect player: %w", err)
	}
	if det == nil || det.ClusterSize == 0 {
		return nil, fmt.Errorf("could not find slide player on the page")
	}

	playerRect := viewportRect{X: det.X, Y: det.Y, W: det.W, H: det.H}
	// The detected count is a heuristic upper bound — overlays (end card,
	// "more from Canva") can sneak through `looksLikeSlide`, and the
	// fallback path uses the unfiltered cluster size. Treat it as a safety
	// cap and stop early once ArrowRight stops producing a new image.
	maxSlides := det.Count
	if maxSlides == 0 {
		maxSlides = det.ClusterSize
	}
	fmt.Fprintf(os.Stderr, "deck has up to %d slide(s)\n", maxSlides)

	var slides []adapter.Slide
	var lastHash [32]byte
	for i := 1; i <= maxSlides; i++ {
		if i > 1 {
			if err := chromedp.Run(bctx, chromedp.KeyEvent(kb.ArrowRight)); err != nil {
				return nil, fmt.Errorf("advance to slide %d: %w", i, err)
			}
		}
		if err := chromedp.Run(bctx,
			chromedp.Sleep(500*time.Millisecond),
			chromedp.Evaluate(hideOverlaysJS, nil),
		); err != nil {
			return nil, err
		}
		shot, err := captureViewportClip(bctx, playerRect)
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
			shot, err = captureViewportClip(bctx, playerRect)
			if err != nil {
				return nil, fmt.Errorf("screenshot slide %d: %w", i, err)
			}
			h = sha256.Sum256(shot)
			if h == lastHash {
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
