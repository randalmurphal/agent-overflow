// Package diagenv names the opt-in diagnostic and isolated-boot
// environment variables, and the passthrough list the WSL-boundary
// launchers forward. It exists so the Windows launcher and dev supervisor
// can reference the full set without importing the packages that
// implement each one.
package diagenv

const (
	// Pprof enables the loopback pprof listener
	// (internal/observability/pprofserve).
	Pprof = "AGENT_OVERFLOW_PPROF"

	// RendererDiag makes the transport server send cross-origin
	// isolation headers (COOP/COEP/CORP) so the SPA runs
	// crossOriginIsolated and performance.measureUserAgentSpecificMemory
	// works in the renderer. Diagnostic mode only: COEP blocks
	// subresources that don't send CORP, so remote images in chat
	// markdown won't load while it is on.
	RendererDiag = "AGENT_OVERFLOW_RENDERER_DIAG"

	// HarnessRealBrowser lifts an isolated boot's fake-browser-engine pin
	// so the instance selects the real engine its deployment has
	// (docs/specs/embedded-browser.md §10 — the manual real-engine gate).
	// Read only by the mocked boot modes, and refused there whenever the
	// soak autopilot is armed; every other boot ignores it entirely. It
	// crosses the WSL boundary because the Windows harness's backend runs
	// inside the distro (`make harness-wsl`) while the operator sets the
	// variable in a WSL shell on the other side of two WSLENV hops.
	HarnessRealBrowser = "AO_HARNESS_REAL_BROWSER"
)

// Passthrough lists every variable the launchers forward across the WSL
// boundary via WSLENV.
func Passthrough() []string {
	return []string{Pprof, RendererDiag, HarnessRealBrowser}
}
