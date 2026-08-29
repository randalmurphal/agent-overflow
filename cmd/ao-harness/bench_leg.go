package main

import (
	"fmt"
	"strings"
)

// Bench legs name the question a run answers. An instrument is the extra
// observer used to answer it. Keeping the pair explicit prevents a trace or
// allocator from being mistaken for a clean renderer measurement.
const (
	benchLegCleanMemory = "clean-memory"
	benchLegFrameCPU    = "frame-cpu"
	benchLegCPUProfile  = "cpu-profile"
	benchLegAllocation  = "allocation"
	benchLegTrace       = "trace"
	benchLegCorrectness = "correctness"

	benchInstrumentNone       = "none"
	benchInstrumentPerf       = "perf"
	benchInstrumentCPUProfile = "cpu-profile"
	benchInstrumentAllocation = "allocation"
	benchInstrumentTrace      = "trace"
)

var benchLegInstruments = map[string]map[string]bool{
	benchLegCleanMemory: {benchInstrumentNone: true},
	benchLegFrameCPU:    {benchInstrumentPerf: true},
	benchLegCPUProfile:  {benchInstrumentCPUProfile: true},
	benchLegAllocation:  {benchInstrumentAllocation: true},
	benchLegTrace:       {benchInstrumentTrace: true},
	// The default bench keeps the existing completion oracle while using the
	// normal perf meter. This is a correctness run with an observational
	// instrument, not a trace or allocation run.
	benchLegCorrectness: {benchInstrumentNone: true, benchInstrumentPerf: true},
}

func benchLegNames() []string {
	return []string{benchLegCleanMemory, benchLegFrameCPU, benchLegCPUProfile, benchLegAllocation, benchLegTrace, benchLegCorrectness}
}

func validateBenchLeg(leg, instrument string) error {
	leg = strings.TrimSpace(leg)
	instrument = strings.TrimSpace(instrument)
	allowed, ok := benchLegInstruments[leg]
	if !ok {
		return fmt.Errorf("unknown bench leg %q (want %s)", leg, strings.Join(benchLegNames(), ", "))
	}
	if !allowed[instrument] {
		return fmt.Errorf("instrument %q is not legal for bench leg %q", instrument, leg)
	}
	return nil
}

func benchInstrumentKind(perf benchPerfSpec, tracing bool) string {
	if tracing {
		return benchInstrumentTrace
	}
	if perf.Meters != nil && len(perf.Meters) == 0 {
		return benchInstrumentNone
	}
	return benchInstrumentPerf
}
