//go:build windows

// picker.go owns the static HTML the WebView2 renders before / during
// backend boot — the distro picker, the post-pick loading spinner, and
// the connectivity-error guidance for misconfigured WSL2 setups.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	"agent-overflow/internal/wsllauncher"
)

// loadingPage is the brief interstitial shown when we have a saved
// distro and skip straight to Launch. Inlined as a Go string rather
// than a separate //go:embed because it's small and a separate file
// would be more friction than it's worth.
const loadingPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>Agent Overflow</title>
  <style>
    html, body { margin: 0; padding: 0; height: 100%; background: #16161e; color: #fff; }
    body { display: flex; align-items: center; justify-content: center; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    .card { display: flex; flex-direction: column; align-items: center; gap: 16px; }
    .spinner { width: 32px; height: 32px; border: 3px solid #7aa2f7; border-top-color: transparent; border-radius: 50%; animation: spin 800ms linear infinite; }
    .label { font-size: 14px; color: #8b8ba0; }
    @keyframes spin { to { transform: rotate(360deg); } }
  </style>
</head>
<body><div class="card"><div class="spinner"></div><div class="label">Booting backend in WSL...</div></div></body>
</html>`

// connectivityErrorPage is shown when the WSL backend booted (we got
// the bootstrap line back) but the Windows host can't reach
// localhost:<port> over the WSL2 vEthernet bridge. The actionable
// mitigation is fixing localhostForwarding — we name it explicitly so
// the user has a search term that maps to a one-line config change.
const connectivityErrorPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>Agent Overflow — connection failed</title>
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
    <div class="title">Backend is running, but Windows can't reach it.</div>
    <div class="body">
      <p>The agent-overflow backend booted inside WSL successfully, but the Windows host failed to connect to it over <code>localhost</code>. This almost always means WSL2's <code>localhostForwarding</code> is disabled in your config.</p>
      <p>Fix it by adding the following to <code>%USERPROFILE%\.wslconfig</code> on Windows (create the file if it doesn't exist):</p>
      <pre>[wsl2]
localhostForwarding=true</pre>
      <p>Then restart WSL with <code>wsl --shutdown</code> in PowerShell, and relaunch Agent Overflow.</p>
      <p>See the WSL docs on <a href="https://learn.microsoft.com/en-us/windows/wsl/wsl-config">wsl-config</a> for more.</p>
    </div>
  </div>
</body>
</html>`

// pickerAssetHandler serves the static picker HTML for /picker, the
// loading HTML for /loading, and the connectivity error page for
// /connectivity-error. Anything else falls back to the picker so a
// stale URL doesn't blank-screen the WebView. The distro list is
// template-injected into a global JS variable so the page renders
// without an RPC round-trip.
func pickerAssetHandler(distros []wsllauncher.Distro) http.Handler {
	rendered, err := renderPicker(distros)
	if err != nil {
		log.Printf("render picker: %v", err)
		rendered = []byte(fmt.Sprintf(
			"<!doctype html><body style=\"font-family:sans-serif;background:#16161e;color:#f7768e;padding:32px\">Failed to render distro picker: %s</body>",
			template.HTMLEscapeString(err.Error()),
		))
	}
	loadingHTML := []byte(loadingPage)
	connectivityHTML := []byte(connectivityErrorPage)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/loading":
			_, _ = w.Write(loadingHTML)
		case "/connectivity-error":
			_, _ = w.Write(connectivityHTML)
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

	// Place the inline script before any other <script> in the page so
	// the consumer-side script reads __AO_DISTROS__ already populated.
	inj := fmt.Sprintf("<script>window.__AO_DISTROS__ = %s;</script>", string(payload))
	out := strings.Replace(pickerHTML, "</head>", inj+"</head>", 1)
	return []byte(out), nil
}
