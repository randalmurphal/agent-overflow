# internal/screenshot/

Headless-Chromium-driven page capture for design-mode read_screenshot.

## What this package owns

- Resolving and downloading `chrome-headless-shell` from
  Chrome-for-Testing on first use, caching it under
  `<configDir>/headless-shell/<version>/`. Defenses on the download
  path: bounded manifest body, bounded zip-entry / aggregate sizes
  (zip-bomb resistance), HTTPS-only URLs, sanitized version segment
  before any filesystem op, symlink entries skipped, mode bits
  masked. NOTE: there is no SHA256 verification today — TLS to
  googlechromelabs.github.io is the integrity story; an explicit
  digest check is a reasonable follow-up if a custom mirror lands.
- A long-lived `Manager` owning a single `chromedp.Browser` per app
  process. Each `Capture` opens a fresh `chromedp.NewContext` (a new
  tab/Target inside the same browser process), so listeners and
  inflight requests from the previous capture don't bleed into the
  next one. We do NOT spin up a fresh BrowserContext per capture —
  cookies / localStorage / serviceWorkers ARE shared across captures,
  which is fine for the current trust model (every capture loads a
  loopback `/design/{threadID}/main/` URL the user trusts).
- The capture sequence: emulate user-visible viewport,
  `Page.navigate`, race `document.fonts.ready` against a 4 s soft
  cap (a cold-cache fetch of variable fonts can otherwise hang
  longer than the agent's per-tool timeout — FOUT in the screenshot
  beats a canceled tool call), scroll-to-bottom + 2×
  `requestAnimationFrame` to settle lazy content, then
  `Page.captureScreenshot{captureBeyondViewport, fromSurface}` for a
  full-page PNG. Capture height is capped — `MaxCaptureHeightPx`
  prevents an unbounded-height page (or accidental infinite scroll)
  from blowing process memory through a single full-page PNG decode.
- Pure-Go PNG → JPEG tile slicing. `internal/design/reactor.go`
  passes `MaxTiles: design.MaxScreenshotTiles` explicitly, so the
  per-tool vision-token budget is the single source of truth.

## What this package does NOT own

- The URL → on-disk-thread-dir mapping. The caller (the design package
  via App) constructs the URL it wants captured.
- The MCP tool surface. `internal/design/mcp.go` consumes the byte
  output and wraps it as image content blocks.
- The send-to-thread single-PNG flow. That remains an iframe-internal
  capture so a one-off chat attachment doesn't require a 200 MB
  Chromium download.

## Layout

- `doc.go` — package purpose.
- `installer.go` — `Install(ctx)` resolves the Stable channel from
  the Chrome-for-Testing manifest, downloads the platform zip with
  progress events, extracts (with zip-slip + zip-bomb defenses),
  returns the executable path. Cached idempotently on subsequent
  calls. The `InstallProgress` events fire over `InstallEventName`;
  no frontend listener is wired today, so first-capture downloads
  are silent — adding a "Downloading rendering engine…" toast is a
  natural follow-up.
- `browser.go` — `Manager` lifecycle: lazy install + boot on first
  `Capture`, persistent `chromedp.Browser`, `Close()` for graceful
  teardown. Transient install failures don't permanently brick the
  Manager — see the retry-on-failure logic in `ensureStarted`.
- `capture.go` — `runCapture(ctx, opts)` pipeline.
- `tiles.go` — `SliceTiles(pngBytes, opts) (SliceResult, error)`.

## Anti-patterns

- Do NOT spawn a fresh Chromium per capture. Reuse the long-lived
  browser; per-capture state isolation is a fresh tab via
  `chromedp.NewContext`, NOT a new BrowserContext (we don't need
  cookie isolation across captures of the same loopback URL).
- Do NOT skip `document.fonts.ready`. Fonts loaded after the initial
  paint are the most common fidelity miss. The 4 s soft cap is there
  to keep the wait bounded, not to bypass it — never await it without
  a race.
- Do NOT use `captureBeyondViewport` without first scrolling the
  document. Lazy-loaded / IntersectionObserver content doesn't
  paint until it enters the viewport.
- Do NOT capture without a height cap. An adversarial or accidental
  infinite-scroll page can decode to gigabytes of RGBA in memory;
  `MaxCaptureHeightPx` is the ceiling.
- Do NOT bundle the headless binary in the installer. The download-
  on-first-capture pattern is what Puppeteer / Playwright /
  Cypress ship; it's right for our distribution shape too.

## References

- `internal/design/AGENTS.md` — how the screenshot output flows
  through the `read_screenshot` MCP tool.
- Chrome-for-Testing manifest:
  `https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json`
