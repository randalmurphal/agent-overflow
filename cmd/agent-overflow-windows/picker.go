//go:build windows

// picker.go owns the static HTML the WebView2 renders before / during
// backend boot — the distro picker, the loading page with the backend's
// startup progress, and the startup/connectivity-error guidance for
// failed WSL boots.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/wsllauncher"
)

// loadingPage is shown when we have a saved distro and skip straight to
// Launch. loadingScript fills it from /loading.json without a reload.
const loadingPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>Agent Overflow</title>
  <style>
    html, body { margin: 0; padding: 0; height: 100%; background: #16161e; color: #fff; }
    body { display: flex; align-items: center; justify-content: center; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    .card { display: flex; flex-direction: column; align-items: center; gap: 12px; max-width: 560px; padding: 0 24px; text-align: center; }
    .spinner { width: 32px; height: 32px; border: 3px solid #7aa2f7; border-top-color: transparent; border-radius: 50%; animation: spin 800ms linear infinite; margin-bottom: 4px; }
    .title { font-size: 16px; color: #c0caf5; }
    .label { font-size: 14px; color: #8b8ba0; overflow-wrap: anywhere; }
    .meta { font-size: 12px; color: #565f89; min-height: 16px; font-variant-numeric: tabular-nums; }
    @keyframes spin { to { transform: rotate(360deg); } }
  </style>
</head>
<body><main class="card"><div class="spinner"></div>
<div class="title" id="ao-loading-title">Starting Agent Overflow</div>
<div class="label" id="ao-loading-status">Booting backend in WSL...</div>
<div class="meta" id="ao-loading-meta"></div></main>
<script src="/loading.js"></script></body>
</html>`

// loadingScript polls /loading.json and writes the report into the
// page's ao-loading-* elements. A page keeps its own status text until
// the backend reports a phase.
const loadingScript = `(function () {
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

// loadingStatus is what /loading.json reports: when the current launch
// began and the backend's latest startup report. The launch writes it and
// the asset server reads it concurrently.
type loadingStatus struct {
	startedMs atomic.Int64
	progress  atomic.Pointer[startupprogress.Progress]
}

// begin starts the elapsed clock for a launch and forgets an earlier
// launch's progress.
func (s *loadingStatus) begin(started time.Time) {
	s.progress.Store(nil)
	s.startedMs.Store(started.UnixMilli())
}

func (s *loadingStatus) setProgress(p startupprogress.Progress) { s.progress.Store(&p) }

func (s *loadingStatus) clearProgress() { s.progress.Store(nil) }

// loadingReport is the /loading.json body.
type loadingReport struct {
	Title     string `json:"title"`
	Status    string `json:"status"`
	Phase     string `json:"phase,omitempty"`
	Step      int    `json:"step"`
	Steps     int    `json:"steps"`
	ElapsedMs int64  `json:"elapsedMs"`
}

func (s *loadingStatus) report(now time.Time) loadingReport {
	r := loadingReport{Title: "Starting Agent Overflow"}
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

// wslNotInstalledPage is shown when ListDistros returns an empty
// slice — either wsl.exe isn't on PATH (WSL not installed at all) or
// it ran but reported no distros. Both have the same actionable
// mitigation: `wsl --install -d <name>` from PowerShell, which
// installs WSL itself if missing AND a Linux distro on top. Naming
// the command explicitly gives the user a copyable string instead of
// a search-engine-distance-of-one hop.
//
// We deliberately do NOT fall through to the picker's empty state
// here. The picker page is a "pick from these" UI; pointing it at an
// empty list invites confusion ("which one of nothing should I
// click?"). A dedicated error page with the install command is the
// honest signal.
const wslNotInstalledPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>Agent Overflow — WSL required</title>
  <style>
    html, body { margin: 0; padding: 0; height: 100%; background: #16161e; color: #fff; }
    body { display: flex; align-items: center; justify-content: center; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; padding: 32px; box-sizing: border-box; }
    .card { max-width: 640px; }
    .title { font-size: 18px; font-weight: 600; color: #f7768e; margin-bottom: 16px; }
    .body { font-size: 14px; line-height: 1.6; color: #c0caf5; }
    code { background: #1a1b26; color: #7dcfff; padding: 1px 6px; border-radius: 4px; font-size: 13px; }
    pre { background: #1a1b26; color: #c0caf5; padding: 12px 16px; border-radius: 6px; font-size: 12px; line-height: 1.5; overflow-x: auto; }
    a { color: #7aa2f7; }
  </style>
</head>
<body>
  <div class="card">
    <div class="title">WSL is required, and no distro was found.</div>
    <div class="body">
      <p>Agent Overflow runs its backend inside a WSL distribution. The launcher couldn't find one — either WSL2 isn't installed, or it's installed but no Linux distro is registered.</p>
      <p>From an elevated PowerShell, run:</p>
      <pre>wsl --install -d Ubuntu</pre>
      <p>This installs WSL2 itself if it's missing and a default Ubuntu distro on top. Reboot if prompted, then relaunch Agent Overflow.</p>
      <p>Already have WSL but want a different distro? <code>wsl --list --online</code> shows the available images; pick one and pass it to <code>wsl --install -d &lt;name&gt;</code>.</p>
      <p>See the <a href="https://learn.microsoft.com/en-us/windows/wsl/install">WSL install docs</a> for more.</p>
    </div>
  </div>
</body>
</html>`

// pickerAssetHandler serves the static picker HTML for /picker, the
// loading HTML for /loading with its script and /loading.json report, the
// WSL-not-installed page for /wsl-not-installed, and the
// startup/connectivity error pages. Anything else falls back to the picker
// so a stale URL doesn't blank-screen the WebView. The distro list is
// template-injected into a global JS variable so the page renders without
// an RPC round-trip.
func pickerAssetHandler(distros []wsllauncher.Distro, failurePage func() []byte, loading func() loadingReport) http.Handler {
	rendered, err := renderPicker(distros)
	if err != nil {
		log.Printf("render picker: %v", err)
		rendered = []byte(fmt.Sprintf(
			"<!doctype html><body style=\"font-family:sans-serif;background:#16161e;color:#f7768e;padding:32px\">Failed to render distro picker: %s</body>",
			template.HTMLEscapeString(err.Error()),
		))
	}
	loadingHTML := []byte(loadingPage)
	wslMissingHTML := []byte(wslNotInstalledPage)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/loading":
			_, _ = w.Write(loadingHTML)
		case "/loading.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = w.Write([]byte(loadingScript))
		case "/loading.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(loading())
		case "/connectivity-error", "/startup-error":
			_, _ = w.Write(failurePage())
		case "/wsl-not-installed":
			_, _ = w.Write(wslMissingHTML)
		default:
			_, _ = w.Write(rendered)
		}
	})
}

// renderPicker injects the distro list into picker.html via a script
// tag that defines window.__AO_DISTROS__. We deliberately do not use
// html/template against picker.html — the file is hand-written HTML
// + JS that we want to keep readable in source. Instead we inject a
// single safe-encoded script block at the </head> seam.
func renderPicker(distros []wsllauncher.Distro) ([]byte, error) {
	type pickerDistro struct {
		Name    string `json:"name"`
		Default bool   `json:"default"`
		Version int    `json:"version"`
		State   string `json:"state"`
	}
	pd := make([]pickerDistro, 0, len(distros))
	for _, d := range distros {
		pd = append(pd, pickerDistro{
			Name: d.Name, Default: d.Default, Version: d.Version, State: d.State,
		})
	}
	payload, err := json.Marshal(pd)
	if err != nil {
		return nil, err
	}

	// Wails v3 registers bound methods under the FQN
	// `<pkgPath>.<TypeName>.<MethodName>` (see Bindings.Add in
	// pkg/application/bindings.go). The picker JS needs that exact
	// string to reach PickDistro via wails.Call.ByName — derive it from
	// reflect so a rename of launcherApp doesn't silently break the
	// picker click handler.
	t := reflect.TypeOf((*launcherApp)(nil)).Elem()
	fqn := fmt.Sprintf("%s.%s.PickDistro", t.PkgPath(), t.Name())
	fqnJSON, err := json.Marshal(fqn)
	if err != nil {
		return nil, err
	}

	// Place the inline script before any other <script> in the page so
	// the consumer-side script reads __AO_DISTROS__ already populated.
	inj := fmt.Sprintf(
		"<script>window.__AO_DISTROS__ = %s; window.__AO_PICK_DISTRO_FQN__ = %s;</script>",
		string(payload), string(fqnJSON),
	)
	out := strings.Replace(pickerHTML, "</head>", inj+"</head>", 1)
	return []byte(out), nil
}
