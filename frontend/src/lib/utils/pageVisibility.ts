// documentHidden reports whether the page is currently hidden — which
// on the Windows/WSL build includes the launcher's minimised-window
// WebView2 suspension (cmd/agent-overflow-windows/webviewtrim.go).
// Guarded for non-DOM test environments. Pure and dependency-free so
// the transport layer can import it without picking up stores.
export function documentHidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden';
}
