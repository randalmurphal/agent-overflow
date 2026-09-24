//go:build windows

package main

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/wsllauncher"
)

// errLaunchFailed joins the probe's classes (wsllauncher.ErrBackend*):
// the backend process itself could not start.
var errLaunchFailed = errors.New("launch backend")

// stalledPhaseLimit bounds the backend-reported text a stall page repeats.
const stalledPhaseLimit = 160

// Only fixed copy, an HTTP status and the stalled startup phase enter the
// page. Wrapped errors can carry credentials, paths, or arbitrary server
// content and belong in launcher.log.
func startupFailureHTML(err error) []byte {
	title := "Agent Overflow could not start."
	detail := "The local backend could not be opened."
	action := "Close and reopen Agent Overflow. If this continues, check the launcher log below."
	var httpErr wsllauncher.BootstrapHTTPError
	var stalled *wsllauncher.BackendStalledError
	switch {
	case errors.Is(err, errLaunchFailed):
		detail = "The backend process could not start inside WSL."
	case errors.As(err, &httpErr):
		if httpErr.StatusCode >= 100 && httpErr.StatusCode <= 599 {
			detail = fmt.Sprintf("The local service returned HTTP %d instead of the expected startup response.", httpErr.StatusCode)
		}
		if httpErr.StatusCode == http.StatusInternalServerError {
			title = "Backend failed while starting."
		}
	case errors.Is(err, wsllauncher.ErrInvalidBootstrap):
		detail = "The local service answered, but its startup response was not valid for this app."
		action = "Close and reopen Agent Overflow. If this continues, install the latest build and check the launcher log below."
	case errors.As(err, &stalled):
		title, detail = stalledStartupCopy(stalled)
		action = "Close and reopen Agent Overflow to try again. If it stalls in the same phase, check the launcher log below."
	case errors.Is(err, wsllauncher.ErrBackendNotReady):
		title = "Backend did not finish starting."
		detail = "The local service returned HTTP 503 (not ready) and did not finish starting before the deadline."
	case errors.Is(err, wsllauncher.ErrBackendUnreachable):
		title = "Windows could not reach the backend."
		detail = "No HTTP response arrived from the backend's local port."
		action = "Close and reopen Agent Overflow. If this continues, check that localhostForwarding is enabled in %USERPROFILE%\\.wslconfig and that local connections are not blocked."
	}
	return []byte(fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8" /><title>Agent Overflow — startup failed</title>
<style>
html,body{margin:0;min-height:100%%;background:#16161e;color:#c0caf5}
body{display:flex;align-items:center;justify-content:center;min-height:100vh;padding:32px;box-sizing:border-box;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
.card{max-width:640px;font-size:14px;line-height:1.6}h1{font-size:18px;line-height:1.4;color:#f7768e}
code{background:#1a1b26;color:#7dcfff;padding:1px 6px;border-radius:4px;overflow-wrap:anywhere}
</style></head><body><main class="card"><h1>%s</h1><p>%s</p><p>%s</p>
<p>Startup details: <code>%%APPDATA%%\agent-overflow\launcher.log</code></p></main></body></html>`,
		template.HTMLEscapeString(title), template.HTMLEscapeString(detail), template.HTMLEscapeString(action)))
}

// stalledStartupCopy names the phase a starting backend stopped advancing
// in, as the update being finished when the boot follows an update, and
// whether the backend stopped responding altogether.
func stalledStartupCopy(stalled *wsllauncher.BackendStalledError) (title, detail string) {
	p := stalled.Progress
	where := "phase " + truncateRunes(p.Phase, stalledPhaseLimit)
	if d := truncateRunes(p.Detail, stalledPhaseLimit); d != "" {
		where += " (" + d + ")"
	}
	quiet := fmt.Sprintf("%d seconds", int(stalled.Quiet.Seconds()))
	update := ""
	if p.UpdatingTo != "" {
		update = "The update to " + startupprogress.DisplayVersion(truncateRunes(p.UpdatingTo, 40))
	}
	switch {
	case stalled.Unresponsive && update != "":
		return update + " stopped responding.", fmt.Sprintf("The backend stopped responding while finishing the update, in %s, for %s.", where, quiet)
	case stalled.Unresponsive:
		return "Backend stopped responding.", fmt.Sprintf("The backend stopped responding during startup, in %s, for %s.", where, quiet)
	case update != "":
		return update + " stalled.", fmt.Sprintf("Finishing the update stalled in %s and made no progress for %s.", where, quiet)
	}
	return "Backend stopped making progress.", fmt.Sprintf("Startup stalled in %s and made no progress for %s.", where, quiet)
}

func truncateRunes(s string, limit int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "..."
	}
	return s
}
