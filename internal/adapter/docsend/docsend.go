package docsend

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
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

func (a *Adapter) Name() string { return "docsend" }

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Host), "docsend.com")
}

var pageCounterRe = regexp.MustCompile(`(\d+)\s*/\s*(\d+)`)

// slideRect describes the currently-visible slide image in viewport pixels.
// Found=false means we walked the DOM and no candidate img was visible —
// typically the docsend upsell interstitial after the real deck.
type slideRect struct {
	Found      bool
	X, Y, W, H float64
	Src        string
	Ready      bool
}

// The IMG element is letterboxed (object-fit: contain) inside a taller
// container, so its bounding box doesn't match the actual rendered slide.
// Compute the real image area from naturalWidth/Height + the element box,
// so the clip is tight on the slide and not the surrounding white space.
const findVisibleSlideJS = `
(() => {
  const imgs = document.querySelectorAll('img.page-view');
  for (const img of imgs) {
    const r = img.getBoundingClientRect();
    if (r.width < 50 || r.height < 50) continue;
    if (window.getComputedStyle(img).visibility === 'hidden') continue;

    const nw = img.naturalWidth || r.width;
    const nh = img.naturalHeight || r.height;
    const scale = Math.min(r.width / nw, r.height / nh);
    const renderW = nw * scale;
    const renderH = nh * scale;
    const offsetX = (r.width - renderW) / 2;
    const offsetY = (r.height - renderH) / 2;

    return {
      found: true,
      x: r.x + offsetX,
      y: r.y + offsetY,
      w: renderW,
      h: renderH,
      src: img.src || '',
      ready: img.complete && img.naturalWidth > 0 && !(img.src || '').endsWith('blank.gif'),
    };
  }
  return { found: false };
})()
`

// hideOverlaysJS hides the cookie banner and any other floating UI
// (intercom chat, "next" arrow controls, etc.) so it doesn't bleed into
// the slide screenshots. We find the banner by text content and hide its
// nearest floating ancestor — broader rules risk taking out the slide
// viewer's own container.
const hideOverlaysJS = `
(() => {
  const isFloating = el => {
    const p = getComputedStyle(el).position;
    return p === 'fixed' || p === 'sticky' || p === 'absolute';
  };
  const hideFloatingAncestor = el => {
    let n = el;
    while (n && n !== document.body) {
      if (isFloating(n)) { n.style.setProperty('display', 'none', 'important'); return true; }
      n = n.parentElement;
    }
    return false;
  };

  // Cookie banner — match by visible text. Walk leaf-ish elements first
  // so we hide the banner itself, not a giant wrapper.
  const needles = ['we use cookies', 'cookie preferences', 'accept all cookies', 'manage cookies', 'privacy policy faqs'];
  const all = document.querySelectorAll('div, section, aside, footer, p, span');
  // Sort so leafier elements (shorter textContent) get checked first.
  const sorted = Array.from(all).sort((a, b) => (a.textContent || '').length - (b.textContent || '').length);
  for (const el of sorted) {
    const txt = (el.textContent || '').toLowerCase();
    if (txt && needles.some(n => txt.includes(n))) {
      if (hideFloatingAncestor(el)) break;
    }
  }

  // Iframe-based banners (e.g. Dropbox's CCPA iframe) — hide them too.
  for (const f of document.querySelectorAll('iframe')) {
    const src = f.src || '';
    if (/ccpa|consent|cookie|gdpr/i.test(src) || /ccpa|consent|cookie/i.test(f.id + ' ' + f.className)) {
      f.style.setProperty('display', 'none', 'important');
    }
  }

  // Common chat widgets that float on top of content.
  for (const sel of ['#intercom-frame', '.intercom-launcher-frame', '#hubspot-messages-iframe-container']) {
    document.querySelectorAll(sel).forEach(el => el.style.setProperty('display', 'none', 'important'));
  }

  // Hide the viewer's own navigation arrows so they don't appear on the
  // edges of our screenshots. We still drive navigation via keyboard.
  for (const sel of [
    '#nextPageButton', '#previousPageButton',
    '.carousel-control',
    '.preso-nav', '.preso-nav-left', '.preso-nav-right',
    'button[aria-label*="next" i]', 'button[aria-label*="previous" i]',
  ]) {
    document.querySelectorAll(sel).forEach(el => el.style.setProperty('display', 'none', 'important'));
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

	// Generous viewport so slides render at decent resolution; the deviceScaleFactor
	// in CaptureScreenshot's clip gives us another quality bump.
	if err := chromedp.Run(bctx,
		chromedp.EmulateViewport(1600, 1200),
		chromedp.Navigate(rawURL),
		chromedp.Sleep(4*time.Second),
		chromedp.Evaluate(hideOverlaysJS, nil),
	); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	// If the deck is gated behind an email prompt, fill it before we try to
	// find slides.
	if err := handleEmailGate(bctx, opts.Email); err != nil {
		return nil, err
	}

	total, err := detectTotalSlides(bctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "deck has %d slide(s) per viewer\n", total)

	slides := make([]adapter.Slide, 0, total)
	for i := 1; i <= total; i++ {
		if i > 1 {
			if err := chromedp.Run(bctx,
				chromedp.KeyEvent(kb.ArrowRight),
				chromedp.Sleep(250*time.Millisecond),
			); err != nil {
				return nil, fmt.Errorf("advance to slide %d: %w", i, err)
			}
		}

		// Cookie banner / next-arrow can re-render on navigation; re-hide
		// before each screenshot.
		if err := chromedp.Run(bctx, chromedp.Evaluate(hideOverlaysJS, nil)); err != nil {
			return nil, err
		}

		rect, err := waitForSlideReady(bctx, i)
		if err != nil {
			// Past the real deck (placeholder never resolves to a real image)
			// is the normal stop condition; downstream is the docsend upsell.
			fmt.Fprintf(os.Stderr, "warning: stopped at slide %d — %v\n", i, err)
			break
		}

		png, err := captureClip(bctx, rect)
		if err != nil {
			return nil, fmt.Errorf("screenshot slide %d: %w", i, err)
		}
		slides = append(slides, adapter.Slide{Data: png, ContentType: "image/png"})
	}

	if len(slides) == 0 {
		return nil, fmt.Errorf("no slides captured")
	}
	return slides, nil
}

func detectTotalSlides(ctx context.Context) (int, error) {
	var bodyText string
	if err := chromedp.Run(ctx, chromedp.Text("body", &bodyText, chromedp.ByQuery)); err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}
	m := pageCounterRe.FindStringSubmatch(bodyText)
	if m == nil {
		return 0, fmt.Errorf("could not detect slide count (no n/N marker)")
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bogus slide count %q", m[2])
	}
	return n, nil
}

// waitForSlideReady polls until the visible slide image has a real (non
// blank.gif) src loaded. Times out after ~10s.
func waitForSlideReady(ctx context.Context, n int) (slideRect, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var r slideRect
		if err := chromedp.Run(ctx, chromedp.Evaluate(findVisibleSlideJS, &r)); err != nil {
			return slideRect{}, err
		}
		if !r.Found {
			return slideRect{}, fmt.Errorf("no visible slide image (likely past the real deck)")
		}
		if r.Ready && r.W > 0 {
			return r, nil
		}
		if time.Now().After(deadline) {
			return slideRect{}, fmt.Errorf("slide image never loaded (src=%q)", lastSeg(r.Src))
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(200*time.Millisecond)); err != nil {
			return slideRect{}, err
		}
	}
}

func captureClip(ctx context.Context, r slideRect) ([]byte, error) {
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
				Scale:  2, // 2x for crisper PDFs
			}).
			Do(ctx)
		return err
	}))
	return buf, err
}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Shared snippet for finding the visible auth-form email input. Docsend
// also renders a *hidden* email field inside a separate "send feedback"
// form on the same page — picking the first DOM match would target that
// (and submit feedback to the deck owner). Always require visibility, and
// skip inputs nested in obvious non-auth forms.
const findVisibleEmailInputJS = `
function() {
  const inputs = document.querySelectorAll('input[type="email"], input[name="email"], input#email');
  for (const el of inputs) {
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) continue;
    if (getComputedStyle(el).visibility === 'hidden') continue;
    const form = el.closest('form');
    if (form && /feedback|doc-chat|comment/i.test(form.id + ' ' + (typeof form.className === 'string' ? form.className : ''))) {
      continue;
    }
    return el;
  }
  return null;
}
`

const detectEmailGateJS = `
(() => {
  const findVisibleEmailInput = ` + findVisibleEmailInputJS + `;
  return findVisibleEmailInput() !== null;
})()
`

// fillEmailGateJS fills the auth email input and submits its surrounding
// form. We dispatch input/change events so React-controlled inputs see
// the value, and prefer the form's own submit button so any client-side
// validation hooks fire correctly.
const fillEmailGateJS = `
(email) => {
  const findVisibleEmailInput = ` + findVisibleEmailInputJS + `;
  const input = findVisibleEmailInput();
  if (!input) return 'no visible email input';

  const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
  setter.call(input, email);
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  input.dispatchEvent(new Event('blur', { bubbles: true }));

  const form = input.closest('form');
  if (form) {
    const btn = form.querySelector('button[type="submit"], input[type="submit"]')
             || form.querySelector('button');
    if (btn) { btn.click(); return 'submitted via form button'; }
    form.submit();
    return 'submitted via form.submit()';
  }
  // No form ancestor — try a nearby "Continue/Submit/View" button.
  const btn = Array.from(document.querySelectorAll('button')).find(b => /continue|submit|view|access/i.test(b.textContent || ''));
  if (btn) { btn.click(); return 'submitted via nearby button'; }
  return 'no submit path found';
}
`

// handleEmailGate checks for an email-gated landing page. If gated, fills
// the form with the provided email and waits for the slide viewer to
// appear. If gated but no email is available, returns a clear error so
// the user can re-run with --email.
func handleEmailGate(ctx context.Context, email string) error {
	var gated bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(detectEmailGateJS, &gated)); err != nil {
		return fmt.Errorf("detect email gate: %w", err)
	}
	if !gated {
		return nil
	}

	if email == "" {
		return fmt.Errorf("this deck requires an email — re-run with --email=you@example.com (or set `git config --global user.email`)")
	}

	fmt.Fprintf(os.Stderr, "deck is email-gated; submitting %s…\n", email)

	var submitResult string
	script := fmt.Sprintf("(%s)(%q)", strings.TrimSpace(fillEmailGateJS), email)
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &submitResult)); err != nil {
		return fmt.Errorf("fill email gate: %w", err)
	}
	fmt.Fprintf(os.Stderr, "gate %s\n", submitResult)

	// Wait until the gate goes away and the viewer renders. Cap at ~15s.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var stillGated bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(detectEmailGateJS, &stillGated)); err != nil {
			return err
		}
		if !stillGated {
			// Give the viewer a beat to mount and load its first slide.
			return chromedp.Run(ctx,
				chromedp.Sleep(3*time.Second),
				chromedp.Evaluate(hideOverlaysJS, nil),
			)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("email gate did not clear after submitting %s (rejected?)", email)
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(500*time.Millisecond)); err != nil {
			return err
		}
	}
}
