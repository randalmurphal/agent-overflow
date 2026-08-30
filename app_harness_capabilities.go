package main

import "slices"

// harnessProtocolRevision is the wire contract revision understood by the
// ao-harness CLI. A revision change means an existing command may no longer
// be safe to run against the instance, so clients must check it before any
// state-changing or instrumenting operation.
const harnessProtocolRevision = 1

// HarnessCapabilitiesResult describes the complete harness control surface.
// The catalog is returned by the backend that owns the surface rather than
// copied into the CLI, so a client can refuse a missing method or frontend
// feature before it starts a destructive operation.
type HarnessCapabilitiesResult struct {
	ProtocolRevision int                       `json:"protocolRevision"`
	Methods          []string                  `json:"methods"`
	Meters           []string                  `json:"meters"`
	Actions          []string                  `json:"actions"`
	Queries          []string                  `json:"queries"`
	Workloads        []string                  `json:"workloads"`
	Build            HarnessBuildCapabilities  `json:"build"`
	Assets           HarnessAssetsCapabilities `json:"assets"`
}

type HarnessBuildCapabilities struct {
	Version string `json:"version"`
	Stamp   string `json:"stamp,omitempty"`
}

type HarnessAssetsCapabilities struct {
	Freshness string `json:"freshness"`
	Digest    string `json:"digest,omitempty"`
}

var harnessCapabilityMeters = []string{
	"busy", "dom", "event", "frames", "layout-shift", "loaf", "longtask", "memory",
}

var harnessCapabilityActions = []string{"open", "reload"}

var harnessCapabilityQueries = []string{"element", "globals", "monitor", "open", "perf", "reload", "viewport"}

// HarnessCapabilities returns the protocol revision and all named controls
// available on this instance. Slices are copied so callers cannot mutate the
// receiver's catalog through the returned result.
func (h *Harness) HarnessCapabilities() (HarnessCapabilitiesResult, error) {
	h.mu.Lock()
	methods := slices.Clone(h.wireMethods)
	h.mu.Unlock()
	if methods == nil {
		methods = []string{}
	}
	return HarnessCapabilitiesResult{
		ProtocolRevision: harnessProtocolRevision,
		Methods:          methods,
		Meters:           slices.Clone(harnessCapabilityMeters),
		Actions:          slices.Clone(harnessCapabilityActions),
		Queries:          slices.Clone(harnessCapabilityQueries),
		Workloads:        harnessCapabilityWorkloadNames(),
		Build: HarnessBuildCapabilities{
			Version: version,
			Stamp:   buildStamp(),
		},
		Assets: HarnessAssetsCapabilities{Freshness: h.paths.AssetsFreshness, Digest: h.paths.AssetsDigest},
	}, nil
}

func harnessCapabilityWorkloadNames() []string {
	// Keep this list next to the frontend protocol catalog, not in the CLI.
	// The server package cannot import cmd/ao-harness without creating a cycle.
	return []string{
		"active-multi-pane", "burst-stream", "giant-turn", "many-threads",
		"multi-pane-stream", "subagent-fanout",
	}
}
