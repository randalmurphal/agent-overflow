package browser

import "agent-overflow/internal/chromium"

// selectEngine picks the engine this process hosts pages in.
//
// A platform whose engine lives INSIDE this process (spec
// docs/specs/embedded-browser.md §6: WebKitGTK on Linux, WKWebView on macOS)
// answers `newNativeEngine` with a driver bound to the app's own desktop
// window. Every other case — no window at all (remote `--connect` clients,
// headless harness runs, tests), a platform whose engine lives in another
// process (Windows, §5), or a build without the native toolchain — keeps
// managed Chrome. The choice is made from a capability the caller supplies,
// never from `runtime.GOOS`: the same Linux binary runs both with a window and
// without one.
func selectEngine(installer *chromium.Installer, configDir string, opts ManagerOptions, events engineEvents) browserEngine {
	if engine := newNativeEngine(configDir, opts, events); engine != nil {
		return engine
	}
	return newCDPEngine(installer, events)
}
