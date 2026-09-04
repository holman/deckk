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
- `--timeout` — total timeout (default: `2m`)

## Adapters

`deckk` picks an adapter based on the URL. Today:

- **docsend** — handles `docsend.com/view/...` decks. Drives the viewer in
  headless Chrome, walks through each slide with the arrow key, captures a
  tight screenshot of just the slide, and stitches them into a PDF. Handles
  the email gate automatically if `--email` (or `git user.email`) is set.
- **canva** — handles `canva.com/design/.../view` decks. Loads the public
  view page, finds the slide player, then walks through each slide with
  the arrow key and screenshots them into a PDF. Only works for publicly
  shared view links (no Canva login).
- **google-slides** — handles `docs.google.com/presentation/d/...` decks.
  No browser involved: Slides exposes the same PDF that File → Download
  produces at an export URL, so `deckk` just downloads it directly. Works
  for decks shared as "anyone with the link" (a sign-in-only deck errors
  out with a hint).
- **pitch** — handles `pitch.com/v/...` decks. Loads the public player,
  reads the slide counter, then walks through each slide with the arrow key
  and screenshots just the slide into a PDF. Only works for publicly shared
  player links.

Adding a new adapter means implementing one interface in `internal/adapter` and
registering it. PRs welcome.

## License

MIT
