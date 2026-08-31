// Package browser provides the capability-scoped built-in browser MCP server
// and its lazy, workspace-isolated page runtime. Which engine backs a page is
// chosen behind the driver.go seam from what the process can actually host:
// launcher-hosted WebView2 controllers, WebKit views embedded in the app's own
// desktop window, or a fake engine on the mocked boot modes. A process that can
// host none of those has no browser engine, and offers no browser tools.
package browser
