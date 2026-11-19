package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"
)

// A page's loading state and favicon, for the companion's tab strip.
//
// Every engine reports loading through pageHooks.Loading: the CDP engines
// from Page.frameStartedLoading and frameStoppedLoading of the main frame
// in the browser's order (cdpLoadTracker), WebKitGTK from the view's is-loading property, WKWebView
// from its observed loading key, the fake engine around each navigation. The
// favicon is read when a load stops, in the page with shared JavaScript
// (faviconScript), so the request carries the page's own session. Only an
// engine with a pane (paneHost) reads one: headless Chromium and the fake
// engine have nowhere to show it and leave it empty.

// maxFaviconBytes bounds a favicon's image bytes. The data URL is about a
// third larger.
const maxFaviconBytes = 32 << 10

// faviconTimeout bounds one favicon read, the page's fetches included.
const faviconTimeout = 8 * time.Second

// pageLoad is a page's loading state and favicon. Guarded by
// managedPage.metaMu.
type pageLoad struct {
	// order is the highest Loading report applied.
	order   uint64
	loading bool
	favicon string
	// reports counts the Loading reports applied. A favicon read keeps its
	// answer only if no report was applied while it ran: a load that started
	// meanwhile is another document, whose stop reads again.
	reports uint64
	// attached is set once the page has its driver and context
	// (pageAttached). Reports arrive from the moment the engine page exists,
	// before that.
	attached bool
	// want is a read owed by a stopped load that no read has begun since;
	// reading is a read running. One read runs per page at a time.
	want, reading bool
	// reads counts the reads begun.
	reads uint64
	// unpushed is set by a change to anything the thread's state push shows
	// of the page (its info, loading or favicon) and cleared by the next push
	// to every client (state_push.go).
	unpushed bool
}

// loadState answers the page's loading state and favicon.
func (p *managedPage) loadState() (bool, string) {
	p.metaMu.RLock()
	defer p.metaMu.RUnlock()
	return p.load.loading, p.load.favicon
}

// pageLoading is pageHooks.Loading. Every report newer than the last applied
// is recorded, and a stop owes a favicon read on an engine with a pane,
// whether or not the start before it was seen: reports of one load can be
// handled in either order.
func (m *Manager) pageLoading(p *managedPage, loading bool, order uint64) {
	_, pane := m.engine.(paneHost)
	p.metaMu.Lock()
	if order <= p.load.order {
		p.metaMu.Unlock()
		return
	}
	p.load.order = order
	p.load.reports++
	if p.load.loading != loading {
		p.load.unpushed = true
	}
	p.load.loading = loading
	if !loading && pane {
		p.load.want = true
	}
	start := p.load.beginReadLocked()
	p.metaMu.Unlock()
	m.publishPages(p.owner)
	if start {
		m.runFaviconReads(p)
	}
}

// pageAttached marks p's driver and context set, which a favicon read uses,
// and begins a read a load that stopped before then owes. It runs after
// p.attach; the metaMu hold publishes those fields to the read.
func (m *Manager) pageAttached(p *managedPage) {
	p.metaMu.Lock()
	p.load.attached = true
	start := p.load.beginReadLocked()
	p.metaMu.Unlock()
	if start {
		m.runFaviconReads(p)
	}
}

// beginReadLocked reports whether the caller is to begin a read: one is owed,
// the page is attached, and none runs. The caller holds metaMu.
func (l *pageLoad) beginReadLocked() bool {
	if !l.want || !l.attached || l.reading {
		return false
	}
	l.reading = true
	return true
}

// runFaviconReads runs the page's reads off the caller, one after another
// while loads that stop meanwhile owe another.
func (m *Manager) runFaviconReads(p *managedPage) {
	run := func() {
		for m.readFaviconOnce(p) {
		}
	}
	if !m.goLife(run) {
		// The Manager is closing; nothing reads again.
		p.metaMu.Lock()
		p.load.reading = false
		p.metaMu.Unlock()
	}
}

// readFaviconOnce reads the favicon and publishes it unless a report arrived
// while it ran, and reports whether a read is owed again.
func (m *Manager) readFaviconOnce(p *managedPage) bool {
	p.metaMu.Lock()
	p.load.want = false
	p.load.reads++
	reports := p.load.reports
	p.metaMu.Unlock()
	icon := m.readFavicon(p)
	p.metaMu.Lock()
	if p.load.want {
		p.metaMu.Unlock()
		return true
	}
	p.load.reading = false
	if p.load.reports != reports || p.load.favicon == icon {
		p.metaMu.Unlock()
		return false
	}
	p.load.favicon = icon
	p.load.unpushed = true
	p.metaMu.Unlock()
	m.publishPages(p.owner)
	return false
}

// readFavicon reads the page's favicon. A page that cannot answer (a CSP, a
// failed fetch, no icon, a non-http document) has none.
func (m *Manager) readFavicon(p *managedPage) string {
	ctx, cancel := operationContext(context.Background(), p.ctx, faviconTimeout)
	defer cancel()
	value, _, err := runEvaluation(ctx, func(ctx context.Context, source string) (json.RawMessage, error) {
		return p.driver.Evaluate(ctx, source, false)
	}, faviconScript, nil, false)
	icon := ""
	if err == nil {
		text, _ := value.(string)
		icon = boundedFavicon(text)
	} else if p.ctx.Err() == nil {
		// The script answers "" for every page-side failure, so this is an
		// engine failure; the tab keeps no icon.
		log.Printf("browser: read the favicon of page %s: %v", p.id, err)
	}
	return icon
}

// faviconImageTypes are the image subtypes a favicon may be.
var faviconImageTypes = map[string]bool{
	"png": true, "jpeg": true, "gif": true, "webp": true, "avif": true, "bmp": true,
	"svg+xml": true, "x-icon": true, "vnd.microsoft.icon": true,
}

// boundedFavicon answers raw when it is a base64 image data URL of an
// allowed type within maxFaviconBytes, and "" otherwise: the page answers
// it, so nothing else reaches the SPA.
func boundedFavicon(raw string) string {
	rest, ok := strings.CutPrefix(raw, "data:image/")
	if !ok {
		return ""
	}
	subtype, payload, ok := strings.Cut(rest, ";base64,")
	if !ok || !faviconImageTypes[subtype] {
		return ""
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > maxFaviconBytes+2 {
		return ""
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) == 0 || len(data) > maxFaviconBytes {
		return ""
	}
	return raw
}

// faviconScript finds the document's icon (a link whose rel holds "icon",
// one sized for a tab first, then /favicon.ico) and fetches it in the page,
// so the request is the page's own: same-origin requests carry its cookies,
// cross-origin ones need the server's CORS consent. The first answer that is
// an image within the bound becomes a data URL; anything else is "".
var faviconScript = `async () => {
const max = ` + strconv.Itoa(maxFaviconBytes) + `;
if (location.protocol !== "http:" && location.protocol !== "https:") return "";
const links = [...document.querySelectorAll("link[rel][href]")].filter((l) => l.relList.contains("icon"));
const rank = (l) => /(^|\s)(16x16|32x32)(\s|$)/i.test(l.getAttribute("sizes") || "") ? 0 : (/(^|\s)any(\s|$)/i.test(l.getAttribute("sizes") || "") ? 1 : 2);
const urls = links.sort((a, b) => rank(a) - rank(b)).map((l) => l.href).filter(Boolean);
urls.push(new URL("/favicon.ico", location.href).href);
for (const url of [...new Set(urls)].slice(0, 4)) {
  const abort = new AbortController();
  const timer = setTimeout(() => abort.abort(), 3000);
  try {
    const response = await fetch(url, { signal: abort.signal });
    const type = (response.headers.get("content-type") || "").split(";")[0].trim().toLowerCase();
    if (!response.ok || !type.startsWith("image/") || !response.body) continue;
    const reader = response.body.getReader();
    const chunks = [];
    let size = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > max) {
        await reader.cancel();
        break;
      }
      chunks.push(value);
    }
    if (size === 0 || size > max) continue;
    return await new Promise((resolve, reject) => {
      const file = new FileReader();
      file.onload = () => resolve(String(file.result));
      file.onerror = () => reject(file.error);
      file.readAsDataURL(new Blob(chunks, { type }));
    });
  } catch (_) {
  } finally {
    clearTimeout(timer);
  }
}
return "";
}`
