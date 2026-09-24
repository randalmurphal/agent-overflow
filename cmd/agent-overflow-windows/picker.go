//go:build windows

// picker.go owns the static HTML the WebView2 renders before / during
// backend boot: the distro picker, the WSL-not-installed page, and the
// routes that serve the shared loading and failure pages
// (internal/startuppage).
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"reflect"
	"strings"

	"agent-overflow/internal/startuppage"
	"agent-overflow/internal/wsllauncher"
)

// loadingStatus is the line the loading page shows while WSL boots the
// backend, before the backend reports a phase.
const loadingStatus = "Booting backend in WSL..."

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
// loading page with its script and report and the startup error page
// (startuppage.Serve), the WSL-not-installed page for /wsl-not-installed,
// and the connectivity error page. Anything else falls back to the picker
// so a stale URL doesn't blank-screen the WebView. The distro list is
// template-injected into a global JS variable so the page renders without
// an RPC round-trip.
func pickerAssetHandler(distros []wsllauncher.Distro, failurePage func() []byte, loading func() startuppage.Report) http.Handler {
	rendered, err := renderPicker(distros)
	if err != nil {
		log.Printf("render picker: %v", err)
		rendered = []byte(fmt.Sprintf(
			"<!doctype html><body style=\"font-family:sans-serif;background:#16161e;color:#f7768e;padding:32px\">Failed to render distro picker: %s</body>",
			template.HTMLEscapeString(err.Error()),
		))
	}
	loadingHTML := startuppage.Loading(loadingStatus)
	wslMissingHTML := []byte(wslNotInstalledPage)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if startuppage.Serve(w, r, loadingHTML, loading, failurePage) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/connectivity-error":
			_, _ = w.Write(failurePage())
		case "/wsl-not-installed":
			_, _ = w.Write(wslMissingHTML)
		default:
			_, _ = w.Write(rendered)
		}
	})
}

// boundMethodFQN is the name Wails v3 registers launcherApp's method under:
// `<pkgPath>.<TypeName>.<MethodName>` (see Bindings.Add in
// pkg/application/bindings.go). A page that calls the method needs that
// exact string; it is derived from reflect so a rename of launcherApp
// does not silently break the page's call.
func boundMethodFQN(method string) string {
	t := reflect.TypeOf((*launcherApp)(nil)).Elem()
	return fmt.Sprintf("%s.%s.%s", t.PkgPath(), t.Name(), method)
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

	fqnJSON, err := json.Marshal(boundMethodFQN("PickDistro"))
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
