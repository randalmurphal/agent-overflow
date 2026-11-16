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
// (cdpLoadTracker), WebKitGTK from the view's is-loading property, WKWebView
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
	// faviconGen numbers favicon reads; only the newest read's answer is
	// kept.
	faviconGen uint64
}

// loadState answers the page's loading state and favicon.
func (p *managedPage) loadState() (bool, string) {
	p.metaMu.RLock()
	defer p.metaMu.RUnlock()
	return p.load.loading, p.load.favicon
}

// pageLoading is pageHooks.Loading. A load that stops starts a favicon read
// on an engine with a pane.
func (m *Manager) pageLoading(p *managedPage, loading bool, order uint64) {
	_, pane := m.engine.(paneHost)
	p.metaMu.Lock()
	if order <= p.load.order {
		p.metaMu.Unlock()
		return
	}
	p.load.order = order
	changed := p.load.loading != loading
	p.load.loading = loading
	read := changed && !loading && pane
	var gen uint64
	if read {
		p.load.faviconGen++
		gen = p.load.faviconGen
	}
	p.metaMu.Unlock()
	if !changed {
		return
	}
	m.emitThreadState(p.owner)
	if read {
		m.goLife(func() { m.refreshFavicon(p, gen) })
	}
}

// refreshFavicon reads the page's favicon and publishes it when it changed
// and no newer read began. A page that cannot answer (a CSP, a failed fetch,
// no icon, a non-http document) has none.
func (m *Manager) refreshFavicon(p *managedPage, gen uint64) {
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
	p.metaMu.Lock()
	if p.load.faviconGen != gen || p.load.favicon == icon {
		p.metaMu.Unlock()
		return
	}
	p.load.favicon = icon
	p.metaMu.Unlock()
	m.emitThreadState(p.owner)
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
