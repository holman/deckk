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
	// canva.link is Canva's short-link domain ("Share → Copy link" hands
	// out these). It 302s to the full canva.com/design/.../view URL, which
	// Chrome follows for us.
	return host == "canva.com" || strings.HasSuffix(host, ".canva.com") ||
		host == "canva.link" || strings.HasSuffix(host, ".canva.link")
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

// pageStateJS reads Canva's own idea of where we are in the deck. The
// /view player exposes a "Go to page" slider (role=slider) whose
// aria-valuemax is the real page count and aria-valuenow the current
// page. The URL hash also tracks the current page ("#3"). Both are far
// more trustworthy than counting stacked DOM boxes, so we prefer them and
// only fall back to the cluster heuristic if neither is present.
const pageStateJS = `
(() => {
  let total = 0, current = 0;
  const sl = document.querySelector('[role="slider"]');
  if (sl) {
    total = parseInt(sl.getAttribute('aria-valuemax') || '0', 10) || 0;
    current = parseInt(sl.getAttribute('aria-valuenow') || '0', 10) || 0;
  }
  if (!total) {
    const m = (document.body.innerText || '').match(/(\d+)\s*\/\s*(\d+)/);
    if (m) { current = parseInt(m[1], 10); total = parseInt(m[2], 10); }
  }
  const hm = (location.hash || '').match(/^#(\d+)$/);
  const hashPage = hm ? parseInt(hm[1], 10) : 0;
  if (!current && hashPage) current = hashPage;
  return { total, current, hashPage };
})()
`

type pageState struct {
	Total    int `json:"total"`
	Current  int `json:"current"`
	HashPage int `json:"hashPage"`
}

// Timing knobs for the animation wait. Canva plays each page's element
// animations (fade/rise/pan…) on entry and drives them from JS, so there's
// no DOM or Web Animations signal to hook — the only honest "done" is
// "the pixels stopped changing". We poll clipped screenshots and accept a
// frame once it has repeated stableSamples times in a row. maxSettle caps
// how long a looping element (GIF, video, infinite animation) can hold us
// up; after that we take the latest frame and move on.
const (
	settlePoll    = 300 * time.Millisecond
	stableSamples = 3
	maxSettle     = 12 * time.Second
	pageFlipWait  = 3 * time.Second
)

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

	var st pageState
	if err := chromedp.Run(bctx, chromedp.Evaluate(pageStateJS, &st)); err != nil {
		return nil, fmt.Errorf("read page state: %w", err)
	}
	// Prefer Canva's own page counter. Without it, fall back to the DOM
	// cluster heuristic, which is only an upper bound — the duplicate-frame
	// check below stops us early in that case.
	total := st.Total
	exact := total > 0
	if !exact {
		total = det.Count
		if total == 0 {
			total = det.ClusterSize
		}
	}
	if exact {
		fmt.Fprintf(os.Stderr, "deck has %d slide(s)\n", total)
	} else {
		fmt.Fprintf(os.Stderr, "deck has up to %d slide(s)\n", total)
	}

	var slides []adapter.Slide
	var lastHash [32]byte
	for i := 1; i <= total; i++ {
		if i > 1 {
			if err := chromedp.Run(bctx, chromedp.KeyEvent(kb.ArrowRight)); err != nil {
				return nil, fmt.Errorf("advance to slide %d: %w", i, err)
			}
			if exact {
				if err := waitForPage(bctx, i); err != nil {
					return nil, fmt.Errorf("advance to slide %d: %w", i, err)
				}
			}
		}
		if err := chromedp.Run(bctx, chromedp.Evaluate(hideOverlaysJS, nil)); err != nil {
			return nil, err
		}
		shot, err := captureSettled(bctx, playerRect)
		if err != nil {
			return nil, fmt.Errorf("screenshot slide %d: %w", i, err)
		}
		h := sha256.Sum256(shot)
		if i > 1 && h == lastHash {
			if exact {
				// Canva says there's another page but it rendered
				// identically to the last one. Most likely a genuinely
				// duplicated slide; keep it rather than silently dropping
				// a page the author put there.
				fmt.Fprintf(os.Stderr, "slide %d looks identical to slide %d; keeping it\n", i, i-1)
			} else {
				fmt.Fprintf(os.Stderr, "end of deck after %d slide(s)\n", len(slides))
				break
			}
		}
		lastHash = h
		slides = append(slides, adapter.Slide{Data: shot, ContentType: "image/png"})
		fmt.Fprintf(os.Stderr, "captured slide %d/%d\n", i, total)
	}

	if len(slides) == 0 {
		return nil, fmt.Errorf("no slides captured")
	}
	return slides, nil
}

// waitForPage blocks until Canva reports page n as current (slider value
// or URL hash), so a slow page flip can't be mistaken for "no change".
func waitForPage(ctx context.Context, n int) error {
	deadline := time.Now().Add(pageFlipWait)
	for {
		var st pageState
		if err := chromedp.Run(ctx, chromedp.Evaluate(pageStateJS, &st)); err != nil {
			return err
		}
		if st.Current == n || st.HashPage == n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("player stuck on page %d (wanted %d)", st.Current, n)
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(100*time.Millisecond)); err != nil {
			return err
		}
	}
}

// captureSettled screenshots the player repeatedly until the image stops
// changing (see the settle constants above), then returns the final frame.
// This is how we wait out Canva's entry animations without knowing
// anything about them.
func captureSettled(ctx context.Context, r viewportRect) ([]byte, error) {
	deadline := time.Now().Add(maxSettle)
	var (
		last    [32]byte
		shot    []byte
		repeats int
	)
	for {
		var err error
		shot, err = captureViewportClip(ctx, r)
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256(shot)
		if h == last {
			repeats++
			if repeats >= stableSamples-1 {
				return shot, nil
			}
		} else {
			repeats = 0
			last = h
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "slide still animating after %s; capturing as-is\n", maxSettle)
			return shot, nil
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(settlePoll)); err != nil {
			return nil, err
		}
	}
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
