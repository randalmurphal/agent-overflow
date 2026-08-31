// Package diagenv names the opt-in diagnostic environment variables and
// the passthrough list the WSL-boundary launchers forward. It exists so
// the Windows launcher and dev supervisor can reference the full set
// without importing the packages that implement each diagnostic.
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
)

// Passthrough lists every diagnostic variable the launchers forward
// across the WSL boundary via WSLENV.
func Passthrough() []string {
	return []string{Pprof, RendererDiag}
}
