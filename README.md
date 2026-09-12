# deckk

Point it at a URL, get a PDF.

Primarily aimed at pitch decks, so angel investors, venture capitalists, and
other weird people can get the actual deck locally and can upload it to a
portfolio management tool, like [Signed](https://signed.com).

```
$ deckk "https://docsend.com/view/abc123"
PDF saved to ~/Desktop/deck.pdf
```

## Install

**Homebrew (macOS / Linux):**

```
brew install holman/tap/deckk
```

**With Go:**

```
go install github.com/holman/deckk/cmd/deckk@latest
```

Either way, you'll need Google Chrome (or Chromium) installed locally —
`deckk` drives it headlessly in the background. On macOS:

```
brew install --cask google-chrome
```

## Usage

```
deckk <url> [-o output.pdf] [--email you@example.com] [--headful]
```

Flags:

- `-o, --output` — output path (default: `~/Desktop/deck.pdf`)
- `--email` — email to use when a deck is gated. Defaults to your
  `git config user.email`. Only sent if the deck actually prompts for one.
- `--headful` — show the browser window (useful for debugging an adapter)
- `--timeout` — total timeout (default: `5m`)

## Adapters

`deckk` picks an adapter based on the URL. Today:

- **docsend** — handles `docsend.com/view/...` decks. Drives the viewer in
  headless Chrome, walks through each slide with the arrow key, captures a
  tight screenshot of just the slide, and stitches them into a PDF. Handles
  the email gate automatically if `--email` (or `git user.email`) is set.
- **canva** — handles `canva.com/design/.../view` decks and `canva.link`
  short links. Loads the public view page, reads the page count from the
  player, then walks through each slide with the arrow key. Canva plays
  per-slide animations on entry, so `deckk` waits for the rendered slide to
  stop changing before screenshotting it (about 5–7s per animated slide).
  Only works for publicly shared view links (no Canva login).
- **google-drive** — handles `drive.google.com/file/d/...` links (and the
  older `open?id=` / `uc?id=` forms). Drive doesn't convert anything, it
  just serves whatever was uploaded, so `deckk` downloads the file directly
  and keeps it if it's a PDF. If the link turns out to point at a native
  Slides deck, it falls through to the Slides export. Anything else (a
  `.pptx`, say) errors out with a hint to export it to PDF first. Works for
  files shared as "anyone with the link".
- **google-slides** — handles `docs.google.com/presentation/d/...` decks.
  No browser involved: Slides exposes the same PDF that File → Download
  produces at an export URL, so `deckk` just downloads it directly. Works
  for decks shared as "anyone with the link" (a sign-in-only deck errors
  out with a hint).
- **papermark** — handles `papermark.com/view/...` links. Papermark's
  viewer asks its API for the deck once and gets back a signed image URL per
  page, so `deckk` loads the page a single time, catches that response off
  the wire, and downloads the page images directly — no slide-by-slide
  paging, and Papermark only records one view. Handles the email gate
  automatically if `--email` (or `git user.email`) is set. Password-protected
  links and links that require an emailed verification code are not
  supported, and custom-domain Papermark links won't be recognized.
- **pitch** — handles `pitch.com/v/...` decks. Loads the public player,
  reads the slide counter, then walks through each slide with the arrow key
  and screenshots just the slide into a PDF. Only works for publicly shared
  player links.

Adding a new adapter means implementing one interface in `internal/adapter` and
registering it. PRs welcome.

## License

MIT
