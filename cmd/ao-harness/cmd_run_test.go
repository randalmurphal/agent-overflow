package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harnessrun"
)

func TestReadRunPlanBoundsUntrustedInput(t *testing.T) {
	old := os.Stdin
	defer func() { os.Stdin = old }()
	file, err := os.CreateTemp(t.TempDir(), "run-plan-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	os.Stdin = file
	if _, err := file.Write(bytes.Repeat([]byte{'x'}, (4<<20)+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := readRunPlan("-"); err == nil || !strings.Contains(err.Error(), "exceeds 4194304 bytes") {
		t.Fatalf("readRunPlan accepted oversized input: %v", err)
	}
}

func TestRunPlanAdapterHasNoArbitraryCommandDoor(t *testing.T) {
	plan := harnessrun.RunPlan{Version: harnessrun.PlanVersion, RunID: "r", Workload: "x", DataRoot: filepath.Join(t.TempDir(), "root"), Ownership: harnessrun.OwnershipFresh, Adapter: harnessrun.AdapterFunctional}
	err := plan.Validate()
	if err == nil || !strings.Contains(err.Error(), "scenario and output") {
		t.Fatalf("adapter error = %v", err)
	}
}

func TestRunPlanRejectsUnknownAdapterBeforeRootMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	plan := `{"version":1,"runId":"r","workload":"x","dataRoot":"` + filepath.ToSlash(root) + `","ownership":"fresh","adapter":"shell"}`
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnv(os.Stdout, os.Stderr)
	if err := runManaged(e, []string{"--plan", path}); err == nil || !strings.Contains(err.Error(), "unknown run adapter") {
		t.Fatalf("run error = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown adapter mutated root: %v", err)
	}
}

func TestRunGovernorReportsHostPressureReason(t *testing.T) {
	events := make(chan governor.Event, 1)
	events <- governor.Event{Reason: governor.ReasonAvailableFloor, AvailableBytes: 99, AvailableFloorBytes: 100}
	err := (&runGovernor{events: events}).safetyError()
	if err == nil || !strings.Contains(err.Error(), "available=99 floor=100 (available-floor)") {
		t.Fatalf("safety error = %v", err)
	}
}

func TestRunGovernorReportsMonitorFailure(t *testing.T) {
	events := make(chan governor.Event, 1)
	events <- governor.Event{Reason: governor.ReasonMonitorError, Error: "owner disappeared"}
	err := (&runGovernor{events: events}).safetyError()
	if err == nil || !strings.Contains(err.Error(), "owner disappeared") {
		t.Fatalf("monitor error = %v", err)
	}
}

func TestCompareAdapterArgsForwardAllPlanFields(t *testing.T) {
	plan := harnessrun.RunPlan{
		Capsule: "/tmp/capsule/manifest.json", Window: true, SampleMS: 750,
		Instrument: "none", PageID: "page-7", Output: "/tmp/artifacts/report.json",
		Pairs: 4, BaseDir: "/tmp/compare-runs", KeepRoots: true,
		Binary: "/tmp/agent-overflow", MockProvider: "/tmp/ao-mockprovider",
		CDP: "http://127.0.0.1:9222",
	}
	want := []string{"run", "--capsule", "/tmp/capsule/manifest.json", "--window=true", "--sample-ms", "750", "--instrument", "none", "--page-id", "page-7", "--out", "/tmp/artifacts/report.json", "--pairs", "4", "--base-dir", "/tmp/compare-runs", "--keep-roots", "--binary", "/tmp/agent-overflow", "--mock-provider", "/tmp/ao-mockprovider", "--cdp", "http://127.0.0.1:9222"}
	if got := compareAdapterArgs(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("compare args = %#v, want %#v", got, want)
	}
}

func TestFunctionalAdapterArgsForwardPageAndMonitorLeg(t *testing.T) {
	plan := harnessrun.RunPlan{Scenario: "/tmp/flow.json", Output: "/tmp/artifacts/flow.json", DataRoot: "/tmp/flow-root", Window: true, MonitorLeg: "instrumented-renderer", PageID: "page-9", Binary: "/tmp/agent-overflow", MockProvider: "/tmp/ao-mockprovider"}
	want := []string{"--spec", "/tmp/flow.json", "--report", "/tmp/artifacts/flow.json", "--data-dir", "/tmp/flow-root", "--supervised-root", "--leg", "instrumented-renderer", "--page-id", "page-9", "--headed", "--binary", "/tmp/agent-overflow", "--mock-provider", "/tmp/ao-mockprovider"}
	if got := functionalAdapterArgs(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("functional args = %#v, want %#v", got, want)
	}
}
