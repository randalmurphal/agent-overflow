package design

import (
	"bytes"
	_ "embed"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// modernScreenshotBundleRaw is the UMD build of modern-screenshot vendored
// from `frontend/node_modules/modern-screenshot@4.7.0/dist/index.js`. We
// self-host because the iframe is `sandbox="allow-scripts"` (no
// `allow-same-origin`), which gives it an opaque origin. Under that
// sandbox WebKitGTK refuses dynamic ESM imports of cross-origin modules
// (esm.sh / unpkg) — the user saw "Failed to fetch dynamically imported
// module: https://esm.sh/modern-screenshot@4.13.0" on every capture. A
// `<script src="/design/_aoassets/modern-screenshot.js">` tag injection
// against our own loopback file server (with CORS headers below) bypasses
// the module-loader restriction because cross-origin classic-script
// loading is treated as no-cors and only exposes a runtime global.
//
// Bump deliberately by recopying from the frontend's pnpm-resolved
// version; the assets/ subdir is the single source of truth at runtime.
//
//go:embed assets/modern-screenshot.js
var modernScreenshotBundleRaw []byte

// modernScreenshotBundle is the runtime-patched bundle we actually
// serve. modern-screenshot internally creates a hidden helper iframe
// (`__SANDBOX__`) inside the document being captured to read default
// browser styles. Inside our sandbox=allow-scripts parent iframe (no
// allow-same-origin) that helper iframe gets its OWN opaque origin —
// distinct from the parent's opaque origin — and any read of
// `helperIframe.contentDocument` is rejected with:
//   "Blocked a frame with origin 'null' from accessing a cross-origin
//    frame."
// The library has no public option to skip this code path. We patch
// by short-circuiting the iframe-creating expression with `false &&`:
// the syntax stays valid, the iframe is never constructed,
// `context.sandbox` stays undefined, and the downstream
// default-styles lookup degrades to an empty Map (which the library
// already handles — `if (!u) return new Map;`). The trade is some
// loss of default-style fidelity in the captured PNG, which is
// preferable to the screenshot path hard-failing on every call.
//
// modernScreenshotSandboxPatched is the byte form we expect to find
// in the patched output. TestModernScreenshotBundle_PatchesOutSandboxIframe
// guards against an upstream version bump shifting the minified
// output past our search pattern — when that fires, the engineer
// re-derives the pattern from the new bundle and updates the
// constants.
const (
	modernScreenshotSandboxOriginal = `r&&(t=r.createElement("iframe")`
	modernScreenshotSandboxPatched  = `false&&(t=r.createElement("iframe")`
)

var modernScreenshotBundle = bytes.Replace(
	modernScreenshotBundleRaw,
	[]byte(modernScreenshotSandboxOriginal),
	[]byte(modernScreenshotSandboxPatched),
	1,
)

// modernScreenshotPath is the URL path the iframe-injected capture
// script imports modern-screenshot from. Stripped of the /design
// mount prefix it's `/_aoassets/modern-screenshot.js`; the leading
// underscore guarantees it cannot collide with a sanitized thread id
// (sanitizeSegment in workdir.go strips "/" + dotfile-prefixes; an
// underscore is allowed but no real thread id starts with "_aoassets").
const modernScreenshotPath = "/_aoassets/modern-screenshot.js"

// FileHandler returns an http.Handler that serves files from the
// per-thread working directories under baseDir. Mount it at the prefix
// "/design/" — the dispatcher must use http.StripPrefix("/design", ...)
// before invoking. The remaining path is interpreted as
// {threadId}/{main|options/.../...}/{file}, with the special prefix
// /_aoassets/ reserved for embedded runtime helpers (currently just
// modernScreenshotPath).
//
// Path-traversal protection comes from http.FileServer + http.Dir +
// the workdir manager's own segment sanitization on writes; an attacker
// who somehow placed a "../" segment under the base dir would still be
// stopped here because http.Dir cleans the request path before resolving.
//
// HTML responses are wrapped in InjectionMiddleware so the diagnostic
// capture script lands in the iframe's <head> — that's how console
// errors flow back to the agent.
func FileHandler(baseDir string) http.Handler {
	fs := http.FileServer(http.Dir(baseDir))
	return InjectionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == modernScreenshotPath {
			serveModernScreenshot(w, r)
			return
		}
		fs.ServeHTTP(w, r)
	}))
}

// serveModernScreenshot returns the embedded UMD bundle. CORS is open
// because the iframe loads scripts from its opaque sandbox origin and
// would otherwise see this as a cross-origin fetch; same reason we
// expose the asset as a classic <script src> instead of an ESM import.
// Cache aggressively: the URL is keyed by the deploy (any change to
// modernScreenshotBundle requires a new binary), so the browser cache
// hit on every iframe reload is safe.
func serveModernScreenshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(modernScreenshotBundle)))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(modernScreenshotBundle)
}

// InjectionMiddleware wraps an HTTP handler and prepends a postMessage
// diagnostic-capture script to text/html response bodies. Non-HTML
// responses pass through untouched, streamed in their original byte
// shape so a 50 MB image asset doesn't get fully buffered into RAM.
// Bodies that don't parse as HTML5 (no <head>, no <html>) fall back
// to the buffered original — better to serve the agent's intended
// output than mangle a non-HTML asset that happened to be served
// with text/html.
//
// Buffering caps at maxInjectionBufferBytes; once exceeded the writer
// flushes-and-streams the remainder so a malicious oversized asset
// still doesn't OOM. This means we can't always inject the script —
// callers writing >32 MB of HTML get the original body verbatim.
func InjectionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := newInjectingResponseWriter(w)
		next.ServeHTTP(rw, r)
		rw.finalize()
	})
}

// maxInjectionBufferBytes caps how much body we buffer to inject the
// capture script. Most agent-rendered HTML is well under 1 MB. The 32
// MB headroom keeps unusual cases (embedded base64 SVG sprites, large
// inline data sets) injectable while bounding the worst-case memory
// footprint of an HTML response.
const maxInjectionBufferBytes = 32 << 20

type injectingResponseWriter struct {
	http.ResponseWriter
	// state machine: pendingHeaders → buffering (HTML) | streaming (non-HTML).
	mode injectMode
	// buf accumulates bytes while mode == injectModeBuffering. Released
	// to flushTo (HTML branch) or written through to the underlying
	// writer when we transition to streaming.
	buf *bytes.Buffer
	// statusCode is captured on WriteHeader; defaulted to 200 if Write
	// fires before WriteHeader (http.FileServer hot path).
	statusCode int
	// headerSent is the local-state mirror of "WriteHeader was called
	// or implied"; the real ResponseWriter still hasn't seen our
	// headers until commit() runs.
	headerCommitted bool
}

type injectMode int

const (
	// injectModePending — we haven't yet decided whether to buffer or
	// stream. Header reads are mirrored from the underlying writer;
	// Write triggers the decision.
	injectModePending injectMode = iota
	// injectModeBuffering — Content-Type starts with text/html. We
	// accumulate up to maxInjectionBufferBytes in buf; if exceeded we
	// switch to streaming and flush what we have.
	injectModeBuffering
	// injectModeStreaming — pass-through. Headers were committed
	// directly to the underlying writer; Write forwards. Used for any
	// non-HTML response and for HTML responses that exceed the buffer
	// cap.
	injectModeStreaming
)

func newInjectingResponseWriter(real http.ResponseWriter) *injectingResponseWriter {
	return &injectingResponseWriter{
		ResponseWriter: real,
		mode:           injectModePending,
	}
}

func (w *injectingResponseWriter) Header() http.Header {
	// We don't maintain a separate header map; header writes go to the
	// underlying writer directly and we read them on first Write to
	// pick a mode. http.FileServer reads its own headers back; passing
	// the underlying map keeps that path correct.
	return w.ResponseWriter.Header()
}

func (w *injectingResponseWriter) WriteHeader(status int) {
	if w.headerCommitted {
		return
	}
	w.statusCode = status
	w.headerCommitted = true
	w.pickMode()
	if w.mode == injectModeStreaming {
		w.ResponseWriter.WriteHeader(status)
	}
	// Buffering branch defers WriteHeader until finalize so we can
	// rewrite Content-Length / strip ETag if injection happens.
}

func (w *injectingResponseWriter) Write(p []byte) (int, error) {
	if !w.headerCommitted {
		// http.FileServer commonly calls Write directly for 200
		// responses; treat as implicit WriteHeader(200).
		w.WriteHeader(http.StatusOK)
	}
	switch w.mode {
	case injectModeStreaming:
		return w.ResponseWriter.Write(p)
	case injectModeBuffering:
		if w.buf == nil {
			w.buf = &bytes.Buffer{}
		}
		// Keep buffering until we exceed the cap; on overflow flush
		// and switch to streaming so we don't grow indefinitely.
		if int64(w.buf.Len())+int64(len(p)) > int64(maxInjectionBufferBytes) {
			w.flushBufferedAsStream()
			return w.ResponseWriter.Write(p)
		}
		return w.buf.Write(p)
	default:
		// Should not happen — pickMode runs in WriteHeader.
		return w.ResponseWriter.Write(p)
	}
}

// pickMode is called once, on first WriteHeader. Decides whether to
// buffer (HTML — we may inject) or stream (everything else).
func (w *injectingResponseWriter) pickMode() {
	ct := w.ResponseWriter.Header().Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ct), "text/html") {
		w.mode = injectModeBuffering
		return
	}
	w.mode = injectModeStreaming
}

// flushBufferedAsStream commits the headers (with original
// Content-Length intact) and writes the buffered bytes to the real
// writer; subsequent Write calls go straight through.
func (w *injectingResponseWriter) flushBufferedAsStream() {
	w.ResponseWriter.WriteHeader(w.statusCode)
	if w.buf != nil && w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		w.buf.Reset()
	}
	w.mode = injectModeStreaming
}

// finalize is called after the upstream handler returns. For the
// buffering branch it commits the response (injecting the script if
// the body parses as HTML5). For pending/streaming branches it's a
// no-op or implicit-status flush.
func (w *injectingResponseWriter) finalize() {
	if !w.headerCommitted {
		// Handler returned without calling Write or WriteHeader.
		// http.FileServer can do this for 304 Not Modified responses
		// where the body is empty.
		w.statusCode = http.StatusOK
		w.headerCommitted = true
		w.pickMode()
		if w.mode != injectModeBuffering {
			w.ResponseWriter.WriteHeader(w.statusCode)
			return
		}
	}
	if w.mode != injectModeBuffering {
		// Already streamed; nothing to do.
		return
	}
	body := []byte(nil)
	if w.buf != nil {
		body = w.buf.Bytes()
	}
	injected, ok := injectDiagnosticScript(body)
	if !ok {
		w.commit(body)
		return
	}
	// Content-Length matches the rewritten body. ETag and
	// Last-Modified are stripped because http.FileServer derived them
	// from the on-disk byte length / mtime, both of which are no longer
	// authoritative for the body the client receives. Without the strip,
	// a conditional GET against the original ETag would short-circuit
	// to 304 and the iframe would replay an un-injected response.
	h := w.ResponseWriter.Header()
	h.Set("Content-Length", strconv.Itoa(len(injected)))
	h.Del("ETag")
	h.Del("Last-Modified")
	w.commit(injected)
}

func (w *injectingResponseWriter) commit(body []byte) {
	w.ResponseWriter.WriteHeader(w.statusCode)
	if len(body) > 0 {
		_, _ = w.ResponseWriter.Write(body)
	}
}

// injectDiagnosticScript parses the HTML body, prepends the diagnostic
// capture script to <head> (creating <head> if absent), and re-renders.
// Returns the rewritten body and true on success; returns the original
// body and false if the HTML is malformed enough that we'd rather
// serve it verbatim than mangle it.
func injectDiagnosticScript(body []byte) ([]byte, bool) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return body, false
	}
	htmlNode := findElement(doc, "html")
	if htmlNode == nil {
		return body, false
	}
	headNode := findElement(htmlNode, "head")
	if headNode == nil {
		headNode = &html.Node{Type: html.ElementNode, Data: "head"}
		// Prepend <head> so it's the first element child of <html>.
		if htmlNode.FirstChild != nil {
			htmlNode.InsertBefore(headNode, htmlNode.FirstChild)
		} else {
			htmlNode.AppendChild(headNode)
		}
	}
	scriptNode := &html.Node{
		Type: html.ElementNode,
		Data: "script",
	}
	scriptNode.AppendChild(&html.Node{
		Type: html.TextNode,
		Data: diagnosticCaptureScript,
	})
	if headNode.FirstChild != nil {
		headNode.InsertBefore(scriptNode, headNode.FirstChild)
	} else {
		headNode.AppendChild(scriptNode)
	}

	var out bytes.Buffer
	if err := html.Render(&out, doc); err != nil {
		return body, false
	}
	return out.Bytes(), true
}

func findElement(node *html.Node, tag string) *html.Node {
	if node == nil {
		return nil
	}
	if node.Type == html.ElementNode && node.Data == tag {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findElement(child, tag); found != nil {
			return found
		}
	}
	return nil
}

// diagnosticCaptureScript is the small <script> the file server
// prepends into every served HTML document. It does two jobs:
//
//  1. Capture runtime signals (console errors/warnings, window errors,
//     unhandled rejections) and post them to the parent window. The
//     frontend forwards the batches to the backend via
//     IngestDiagnosticBatch.
//
//  2. Self-render the document to a PNG when the parent posts a
//     `capture` request, then post the result back. This runs INSIDE
//     the iframe so the strict `sandbox="allow-scripts"` (without
//     allow-same-origin) is preserved — the parent cannot reach into
//     the iframe's contentDocument, but the iframe can render itself
//     and ship the bytes back over postMessage.
//
//     modern-screenshot is fetched lazily from esm.sh on the first
//     capture so the script stays small and a thread that never asks
//     for a screenshot pays no network cost.
//
// Kept compact and dependency-free so the diagnostics path stays
// robust even under unusual document modes.
const diagnosticCaptureScript = `(function(){
  if (window.__aoDesignBootstrap) return;
  window.__aoDesignBootstrap = true;
  var pending = [];
  var flushTimer = null;
  function severityFromConsole(method) {
    return method === 'error' ? 'error' : (method === 'warn' ? 'warn' : 'info');
  }
  function post(diag) {
    pending.push(diag);
    if (flushTimer) return;
    flushTimer = setTimeout(function(){
      flushTimer = null;
      try { parent.postMessage({ aoDesign: 'diagnostics', items: pending.splice(0) }, '*'); } catch (_) {}
    }, 60);
  }
  function captureConsole(method) {
    var orig = console[method];
    console[method] = function() {
      try {
        var args = Array.prototype.slice.call(arguments);
        var msg = args.map(function(a){
          if (a instanceof Error) return a.stack || a.message;
          if (typeof a === 'string') return a;
          try { return JSON.stringify(a); } catch (_) { return String(a); }
        }).join(' ');
        post({ severity: severityFromConsole(method), message: msg, source: 'console.' + method });
      } catch (_) {}
      return orig.apply(this, arguments);
    };
  }
  captureConsole('error');
  captureConsole('warn');
  window.addEventListener('error', function(ev) {
    post({
      severity: 'error',
      message: ev.message || 'Uncaught error',
      source: 'window.onerror',
      url: ev.filename || '',
      line: ev.lineno || 0,
      column: ev.colno || 0,
      stack: ev.error && ev.error.stack ? String(ev.error.stack) : ''
    });
  });
  window.addEventListener('unhandledrejection', function(ev) {
    var reason = ev.reason;
    var message = '';
    var stack = '';
    if (reason instanceof Error) { message = reason.message; stack = reason.stack || ''; }
    else { try { message = JSON.stringify(reason); } catch (_) { message = String(reason); } }
    post({ severity: 'error', message: 'Unhandled promise rejection: ' + message, source: 'unhandledrejection', stack: stack });
  });
  // Screenshot round-trip. The parent issues:
  //   postMessage({ aoDesign: 'capture', requestId: '...' }, '*')
  // We render document.documentElement via modern-screenshot (lazy
  // imported on first call so the script stays tiny) and post back:
  //   postMessage({ aoDesign: 'capture-result', requestId, pngBase64 })
  // or:
  //   postMessage({ aoDesign: 'capture-error', requestId, error })
  // The parent forwards either to IngestScreenshot / FailScreenshot.
  var domToPngLoader = null;
  function loadDomToPng() {
    if (!domToPngLoader) {
      // We self-host modern-screenshot at /design/_aoassets/... because
      // this iframe is sandbox="allow-scripts" without allow-same-origin
      // — its origin is opaque, and WebKitGTK refuses dynamic ESM
      // imports of cross-origin modules from that context. A classic
      // <script src> tag works because cross-origin script loading is
      // a no-cors browser fetch that only exposes the runtime global.
      // The relative URL is resolved against document.baseURI, which is
      // /design/{threadId}/main/, so the request lands at
      // /design/_aoassets/modern-screenshot.js on our loopback file
      // server. The server returns the embedded UMD bundle with CORS
      // headers so the response is consumable from the opaque origin.
      domToPngLoader = new Promise(function(resolve, reject){
        if (window.modernScreenshot && window.modernScreenshot.domToPng) {
          resolve(window.modernScreenshot.domToPng);
          return;
        }
        var script = document.createElement('script');
        script.src = '/design/_aoassets/modern-screenshot.js';
        script.async = true;
        script.onload = function(){
          if (window.modernScreenshot && window.modernScreenshot.domToPng) {
            resolve(window.modernScreenshot.domToPng);
          } else {
            reject(new Error('modern-screenshot loaded but no domToPng global'));
          }
        };
        script.onerror = function(){
          reject(new Error('failed to load /design/_aoassets/modern-screenshot.js'));
        };
        document.head.appendChild(script);
      });
    }
    return domToPngLoader;
  }
  async function capture(requestId) {
    try {
      var domToPng = await loadDomToPng();
      // font: false skips modern-screenshot's embedWebFont pass.
      // That pass walks document.styleSheets and fetches every
      // @import / @font-face URL via fetch() to inline the font
      // bytes as data URLs. Our iframe is sandbox="allow-scripts"
      // with no allow-same-origin, so its document has an OPAQUE
      // origin — every fetch() from inside it (including to its own
      // document URL) is cross-origin and gets blocked with
      // "Unsafe attempt to load URL ... Domains, protocols and ports
      // must match". Without font: false, the very first capture
      // call rejects with that error and the agent's read_screenshot
      // tool / the user's "Send to thread" button both hard-fail.
      // Trade: web fonts referenced via <link> won't be embedded in
      // the PNG; the canvas falls back to the next family in the
      // stack. Acceptable for design previews — layout is what we
      // care about, not exact glyph rendering.
      var dataUrl = await domToPng(document.documentElement, {
        backgroundColor: '#ffffff',
        font: false
      });
      var base64 = '';
      var commaIdx = dataUrl.indexOf(',');
      if (commaIdx >= 0) base64 = dataUrl.slice(commaIdx + 1);
      try { parent.postMessage({ aoDesign: 'capture-result', requestId: requestId, pngBase64: base64 }, '*'); } catch (_) {}
    } catch (err) {
      var message = (err && err.message) ? err.message : String(err);
      try { parent.postMessage({ aoDesign: 'capture-error', requestId: requestId, error: message }, '*'); } catch (_) {}
    }
  }
  window.addEventListener('message', function(ev){
    var data = ev && ev.data;
    if (!data || typeof data !== 'object') return;
    if (data.aoDesign === 'capture' && typeof data.requestId === 'string') {
      capture(data.requestId);
    }
  });
})();
`
