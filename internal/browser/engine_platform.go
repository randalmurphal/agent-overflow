package browser

import "log"

// selectEngine picks the engine this process hosts pages in. It is a table of
// four WIRING facts the caller supplies, never a `runtime.GOOS` check: the
// same binary runs windowed and windowless, and the answer has to differ.
//
//  1. FakeEngine — the harness and soak boots (spec §10). Their pane chrome and
//     host rect render against pages no real engine ever painted.
//  2. PaneHost — the executable built a CDP relay, which happens only on the
//     Windows/WSL deployment (§5). Pages become launcher-hosted WebView2
//     controllers.
//  3. HeadlessChromium — the serve boot asked for it (remote-access.md §7).
//     Pages become targets in a headless Chromium this process launches, one
//     per workspace profile. It is a POSITIVE option and never an inference
//     from the absence of a window: only `runServe` sets it, so `go test`,
//     `--connect` and every unwired boot keep answering no engine.
//  4. NativeWindow answering a real window, on a build whose platform half can
//     host one (§6: WebKitGTK on Linux, WKWebView on macOS).
//
// Nothing else has an engine. A remote `--connect` backend, a serve host with
// no Chromium on it, and `go test` get `unavailableEngine`, whose refusal is
// the whole windowless story: no pane, no browser tools, and one sentence
// saying so.
func selectEngine(configDir string, opts ManagerOptions, events engineEvents) browserEngine {
	switch {
	case opts.FakeEngine:
		return newFakeEngine()
	case opts.PaneHost != nil:
		return newHostedEngine(opts.PaneHost.Relay, opts.PaneHost.Directive, opts.Accelerators, events)
	case opts.HeadlessChromium != nil:
		engine, err := newHeadlessChromiumEngine(configDir, *opts.HeadlessChromium, events)
		if err != nil {
			// The ONE line a serve host gets about its missing browser. It
			// is logged here because this is where the outcome is known,
			// and it names the fix because a backend with no window has no
			// Settings screen in front of the person reading its journal.
			log.Printf("browser tools off: %v", err)
			return unavailableEngine{}
		}
		return engine
	}
	if engine := newNativeEngine(configDir, opts, events); engine != nil {
		return engine
	}
	return unavailableEngine{}
}
