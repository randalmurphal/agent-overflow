// Webview-platform sniff shared by every keyboard/clipboard-convention
// site (keybindings, jump-hint modifier, terminal clipboard chords, the
// browser-history guard). Keyboard conventions follow the OS the webview
// runs on — never the backend's GOOS: a Windows webview driving a WSL
// backend still wants Windows chords.
//
// navigator.platform is deprecated but still returns "MacIntel" /
// "Win32" / "Linux …" in all three target webviews (WKWebView, WebView2,
// WebKitGTK). If it ever needs migrating (e.g. to userAgentData), this
// is the single place to change.
export function isMacPlatform(): boolean {
  return typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform);
}
