package browser

// selectEngine picks the engine this process hosts pages in. It is a table of
// three WIRING facts the caller supplies, never a `runtime.GOOS` check: the
// same binary runs windowed and windowless, and the answer has to differ.
//
//  1. FakeEngine — the harness and soak boots (spec §10). Their pane chrome and
//     host rect render against pages no real engine ever painted.
//  2. PaneHost — the executable built a CDP relay, which happens only on the
//     Windows/WSL deployment (§5). Pages become launcher-hosted WebView2
//     controllers.
//  3. NativeWindow answering a real window, on a build whose platform half can
//     host one (§6: WebKitGTK on Linux, WKWebView on macOS).
//
// Nothing else has an engine. A remote `--connect` backend, a headless serve
// mode, and `go test` get `unavailableEngine`, whose refusal is the whole
// windowless story: no pane, no browser tools, and one sentence saying so.
func selectEngine(configDir string, opts ManagerOptions, events engineEvents) browserEngine {
	switch {
	case opts.FakeEngine:
		return newFakeEngine()
	case opts.PaneHost != nil:
		return newHostedEngine(opts.PaneHost.Relay, opts.PaneHost.Directive, opts.Accelerators, events)
	}
	if engine := newNativeEngine(configDir, opts, events); engine != nil {
		return engine
	}
	return unavailableEngine{}
}
