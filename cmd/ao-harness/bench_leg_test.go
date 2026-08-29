package main

import "testing"

func TestBenchLegMatrix(t *testing.T) {
	valid := [][2]string{
		{benchLegCleanMemory, benchInstrumentNone},
		{benchLegFrameCPU, benchInstrumentPerf},
		{benchLegCPUProfile, benchInstrumentCPUProfile},
		{benchLegAllocation, benchInstrumentAllocation},
		{benchLegTrace, benchInstrumentTrace},
		{benchLegCorrectness, benchInstrumentNone},
		{benchLegCorrectness, benchInstrumentPerf},
	}
	for _, pair := range valid {
		if err := validateBenchLeg(pair[0], pair[1]); err != nil {
			t.Errorf("validateBenchLeg(%q, %q): %v", pair[0], pair[1], err)
		}
	}
	for _, pair := range [][2]string{
		{benchLegCleanMemory, benchInstrumentPerf},
		{benchLegFrameCPU, benchInstrumentTrace},
		{benchLegTrace, benchInstrumentPerf},
		{benchLegAllocation, benchInstrumentCPUProfile},
	} {
		if err := validateBenchLeg(pair[0], pair[1]); err == nil {
			t.Errorf("validateBenchLeg(%q, %q) accepted an illegal pair", pair[0], pair[1])
		}
	}
	if err := validateBenchLeg("unknown", benchInstrumentNone); err == nil {
		t.Fatal("unknown leg was accepted")
	}
}

func TestBenchInstrumentKindDistinguishesCleanMemory(t *testing.T) {
	if got := benchInstrumentKind(benchPerfSpec{Meters: []string{}}, false); got != benchInstrumentNone {
		t.Fatalf("clean-memory instrument = %q, want %q", got, benchInstrumentNone)
	}
	if got := benchInstrumentName(benchPerfSpec{Meters: []string{}}, false); got != benchInstrumentNone {
		t.Fatalf("clean-memory identity instrument = %q, want %q", got, benchInstrumentNone)
	}
	if got := benchInstrumentKind(benchPerfSpec{}, false); got != benchInstrumentPerf {
		t.Fatalf("default instrument = %q, want %q", got, benchInstrumentPerf)
	}
}
