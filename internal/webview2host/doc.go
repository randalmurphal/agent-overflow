// Package webview2host is the Windows launcher's embedded-browser pane
// host: a SECOND WebView2 environment, separate from the one Wails uses
// for the SPA, with one controller per browser tab positioned inside the
// launcher's own window.
//
// The package is split by what can be tested off-Windows:
//
//   - The wire contract (Directive, Report, the CDP tunnel frame shapes)
//     and every path/name rule are plain Go with no build tag, so the
//     Linux test suite covers validation, sanitisation and framing.
//   - The COM half (host_windows.go and its siblings) is behind
//     //go:build windows and holds no logic that is not about calling
//     WebView2. It is reached only through Host.
//
// The backend emits Directives on eventchan.BrowserHost and the launcher
// answers with the BrowserHostReport RPC over the same notification
// bridge connection; see internal/wsllauncher.
package webview2host
