// Package browser provides the capability-scoped built-in browser MCP server
// and its lazy, workspace-isolated page runtime. Which engine backs a page is
// chosen behind the driver.go seam from what the process can actually host:
// managed Chrome, launcher-hosted WebView2 controllers, or WebKit views
// embedded in the app's own desktop window.
package browser
