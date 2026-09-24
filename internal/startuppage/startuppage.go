// Package startuppage renders the pages a native shell shows before the
// app's own page: the loading page, which follows the latest startup report
// through /loading.json, and failure pages from fixed copy. The Windows
// launcher and the desktop update helper serve them from their window's
// asset handler (Serve).
package startuppage

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sync/atomic"
	"time"

	"agent-overflow/internal/startupprogress"
)

// The routes Serve answers.
const (
	LoadingPath = "/loading"
	ScriptPath  = "/loading.js"
	ReportPath  = "/loading.json"
	FailurePath = "/startup-error"
)

// Loading is the loading page. status is the line it shows until a report
// names a phase; Script then fills the page from ReportPath without a reload.
func Loading(status string) []byte {
	return []byte(fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>Agent Overflow</title>
  <style>
    html, body { margin: 0; padding: 0; height: 100%%; background: #16161e; color: #fff; }
    body { display: flex; align-items: center; justify-content: center; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    .card { display: flex; flex-direction: column; align-items: center; gap: 12px; max-width: 560px; padding: 0 24px; text-align: center; }
    .spinner { width: 32px; height: 32px; border: 3px solid #7aa2f7; border-top-color: transparent; border-radius: 50%%; animation: spin 800ms linear infinite; margin-bottom: 4px; }
    .title { font-size: 16px; color: #c0caf5; }
    .label { font-size: 14px; color: #8b8ba0; overflow-wrap: anywhere; }
    .meta { font-size: 12px; color: #565f89; min-height: 16px; font-variant-numeric: tabular-nums; }
    @keyframes spin { to { transform: rotate(360deg); } }
  </style>
</head>
<body><main class="card"><div class="spinner"></div>
<div class="title" id="ao-loading-title">Starting Agent Overflow</div>
<div class="label" id="ao-loading-status">%s</div>
<div class="meta" id="ao-loading-meta"></div></main>
<script src="%s"></script></body>
</html>`, template.HTMLEscapeString(status), ScriptPath))
}

// Script polls ReportPath and writes the report into the page's
// ao-loading-* elements. A page keeps its own status text until the report
// names a phase.
const Script = `(function () {
  "use strict";
  var title = document.getElementById("ao-loading-title");
  var status = document.getElementById("ao-loading-status");
  var meta = document.getElementById("ao-loading-meta");
  function clock(ms) {
    var s = Math.floor(ms / 1000);
    return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
  }
  function render(r) {
    if (title && r.title) title.textContent = r.title;
    if (status && r.phase) status.textContent = r.status;
    if (!meta) return;
    var parts = [];
    if (r.steps > 0) parts.push("Step " + r.step + " of " + r.steps);
    if (r.elapsedMs >= 1000) parts.push(clock(r.elapsedMs) + " elapsed");
    meta.textContent = parts.join(" \u00b7 ");
  }
  function poll() {
    fetch("/loading.json", { cache: "no-store" })
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (r) { if (r) render(r); })
      .catch(function () {})
      .then(function () { setTimeout(poll, 500); });
  }
  poll();
})();
`

// Status is what ReportPath reports: when the current launch began and the
// latest startup report. The launch writes it while the asset handler reads
// it.
type Status struct {
	startedMs atomic.Int64
	progress  atomic.Pointer[startupprogress.Progress]
}

// Begin starts the elapsed clock for a launch and forgets an earlier
// launch's report.
func (s *Status) Begin(started time.Time) {
	s.progress.Store(nil)
	s.startedMs.Store(started.UnixMilli())
}

// SetProgress records the latest report.
func (s *Status) SetProgress(p startupprogress.Progress) { s.progress.Store(&p) }

// ClearProgress forgets the latest report, whose process is not the one the
// page now waits for.
func (s *Status) ClearProgress() { s.progress.Store(nil) }

// Report is the ReportPath body.
type Report struct {
	Title     string `json:"title"`
	Status    string `json:"status"`
	Phase     string `json:"phase,omitempty"`
	Step      int    `json:"step"`
	Steps     int    `json:"steps"`
	ElapsedMs int64  `json:"elapsedMs"`
}

// Report is the page's state at now. A report that names the update being
// finished titles the page as an update.
func (s *Status) Report(now time.Time) Report {
	r := Report{Title: "Starting Agent Overflow"}
	if started := s.startedMs.Load(); started > 0 {
		r.ElapsedMs = max(0, now.UnixMilli()-started)
	}
	if p := s.progress.Load(); p != nil {
		r.Status, r.Phase, r.Step, r.Steps = p.Status(), p.Phase, p.Step, p.Steps
		if p.UpdatingTo != "" {
			r.Title = "Updating Agent Overflow"
		}
	}
	return r
}

// Serve answers LoadingPath with loading, ScriptPath, ReportPath from
// report and FailurePath with failure's current page, none of them
// cacheable. It reports false, writing nothing, for any other path.
func Serve(w http.ResponseWriter, r *http.Request, loading []byte, report func() Report, failure func() []byte) bool {
	switch r.URL.Path {
	case LoadingPath:
		writePage(w, "text/html; charset=utf-8", loading)
	case ScriptPath:
		writePage(w, "text/javascript; charset=utf-8", []byte(Script))
	case ReportPath:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(report())
	case FailurePath:
		writePage(w, "text/html; charset=utf-8", failure())
	default:
		return false
	}
	return true
}

func writePage(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// Failure is a failure page's fixed copy. Only fixed copy and bounded,
// escaped values enter it: an error's text can carry credentials, paths or
// server content and belongs in the log.
type Failure struct {
	Title, Detail string
	// Action is what to do next; empty for none.
	Action string
	// Log is where the details are, named on the page.
	Log string
	// Retry is the bound method the page's Retry button calls, as Wails
	// registers it (<package path>.<type>.<method>); empty for no button.
	Retry string
}

// HTML renders the page.
func (f Failure) HTML() []byte {
	action := ""
	if f.Action != "" {
		action = "<p>" + template.HTMLEscapeString(f.Action) + "</p>"
	}
	retry := ""
	if f.Retry != "" {
		retry = retryControl(f.Retry)
	}
	return []byte(fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8" /><title>Agent Overflow — startup failed</title>
<style>
html,body{margin:0;min-height:100%%;background:#16161e;color:#c0caf5}
body{display:flex;align-items:center;justify-content:center;min-height:100vh;padding:32px;box-sizing:border-box;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
.card{max-width:640px;font-size:14px;line-height:1.6}h1{font-size:18px;line-height:1.4;color:#f7768e}
code{background:#1a1b26;color:#7dcfff;padding:1px 6px;border-radius:4px;overflow-wrap:anywhere}
button{font:inherit;color:#16161e;background:#7aa2f7;border:0;border-radius:6px;padding:6px 14px;cursor:pointer}
button:disabled{opacity:.6;cursor:default}#ao-retry-error{color:#f7768e}
</style></head><body><main class="card"><h1>%s</h1><p>%s</p>%s%s
<p>Startup details: <code>%s</code></p></main></body></html>`,
		template.HTMLEscapeString(f.Title), template.HTMLEscapeString(f.Detail), action, retry,
		template.HTMLEscapeString(f.Log)))
}

// retryControl is the Retry button. It calls method the way a Wails page
// calls a bound method: a direct POST to Wails' /wails/runtime endpoint,
// whose shape (object=0, method=0, args.{call-id,methodName,args}) is Wails
// v3's CallBinding.
func retryControl(method string) string {
	return `<p><button id="ao-retry" type="button">Retry the upgrade</button></p>
<p id="ao-retry-error" hidden></p>
<script>(function () {
  "use strict";
  var method = "` + template.JSEscapeString(method) + `";
  var button = document.getElementById("ao-retry");
  var error = document.getElementById("ao-retry-error");
  button.addEventListener("click", function () {
    button.disabled = true;
    error.hidden = true;
    var callId = Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2);
    fetch("/wails/runtime", {
      method: "POST",
      headers: { "Content-Type": "application/json", "x-wails-client-id": "ao-retry-" + callId },
      body: JSON.stringify({ object: 0, method: 0, args: { "call-id": callId, methodName: method, args: [] } })
    }).then(function (res) {
      if (res.ok) return;
      return res.text().then(function (text) {
        throw new Error(text || "Retry returned status " + res.status);
      });
    }).catch(function (err) {
      button.disabled = false;
      error.textContent = "Retry failed: " + (err && err.message ? err.message : err);
      error.hidden = false;
    });
  });
})();</script>`
}
