package main

import (
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
)

// clearCDPEnv makes a test's answer independent of the developer's shell:
// both defaults are read at flag-bind time, and a machine with
// AO_CDP_PORT exported would silently pass the "no endpoint" tests.
func clearCDPEnv(t *testing.T) {
	t.Helper()
	t.Setenv(cdpURLEnv, "")
	t.Setenv(cdpPortEnv, "")
}

func TestDefaultCDPSpecPrefersTheURLEnv(t *testing.T) {
	t.Setenv(cdpPortEnv, "9224")
	t.Setenv(cdpURLEnv, "ws://127.0.0.1:9224/devtools/page/AB")
	if got := defaultCDPSpec(); got != "ws://127.0.0.1:9224/devtools/page/AB" {
		t.Fatalf("defaultCDPSpec() = %q, want the ws url", got)
	}
	t.Setenv(cdpURLEnv, "")
	if got := defaultCDPSpec(); got != "9224" {
		t.Fatalf("defaultCDPSpec() = %q, want the port", got)
	}
}

// An absent endpoint is an UNDER-SPECIFIED invocation (exit 2), and the
// refusal has to say the one thing an operator cannot guess: which
// engines serve this protocol at all. A WebKitGTK harness window looks
// identical to a WebView2 one from the shell.
func TestResolveCDPEndpointRefusalNamesTheEngineRequirement(t *testing.T) {
	row := instanceinfo.Instance{Row: instanceinfo.Row{Identity: instanceinfo.Identity{ID: "abcd1234", Mode: instanceinfo.ModeSoak}}}
	_, err := resolveCDPEndpoint("", target{ID: row.ID, Row: &row})
	if err == nil {
		t.Fatal("an absent endpoint must be refused")
	}
	var usage usageErr
	if !errors.As(err, &usage) {
		t.Fatalf("an absent --cdp is a usage error, got %T", err)
	}
	message := err.Error()
	for _, want := range []string{"--cdp", cdpPortEnv, "WebKitGTK", "9224"} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, message)
		}
	}
}

// The instance's own mode names its port. "soak 9224, harness 9225" is a
// coin flip an operator should not have to call when the registry row
// already knows.
func TestResolveCDPEndpointNamesThisInstancesPort(t *testing.T) {
	row := instanceinfo.Instance{Row: instanceinfo.Row{Identity: instanceinfo.Identity{ID: "abcd1234", Mode: instanceinfo.ModeHarness}}}
	_, err := resolveCDPEndpoint("", target{ID: row.ID, Row: &row})
	if err == nil || !strings.Contains(err.Error(), "--cdp 9225") {
		t.Fatalf("refusal should point at this instance's own port: %v", err)
	}
}

func TestResolveCDPEndpointNamesPerfPort(t *testing.T) {
	row := instanceinfo.Instance{Row: instanceinfo.Row{Identity: instanceinfo.Identity{ID: "abcd1234", Mode: instanceinfo.ModePerf}}}
	_, err := resolveCDPEndpoint("", target{ID: row.ID, Row: &row})
	if err == nil || !strings.Contains(err.Error(), "--cdp 9226") {
		t.Fatalf("refusal should point at the perf instance's own port: %v", err)
	}
}

func TestResolveCDPEndpointParsesAPort(t *testing.T) {
	endpoint, err := resolveCDPEndpoint("9225", target{})
	if err != nil {
		t.Fatalf("resolveCDPEndpoint: %v", err)
	}
	if endpoint.HTTPBase != "http://127.0.0.1:9225" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
}

// `bench --trace` with no endpoint must fail BEFORE it attaches to
// anything: a bench resets the instance on its first repeat, and a caller
// who forgot the flag should get their state back untouched.
func TestBenchTraceWithoutAnEndpointFailsBeforeAttaching(t *testing.T) {
	clearCDPEnv(t)
	code, stdout, stderr := run(t, "bench", "burst-stream", "--trace", "--registry-dir", t.TempDir())
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitUsage, stdout, stderr)
	}
	if !strings.Contains(stderr, "no devtools endpoint") {
		t.Fatalf("stderr did not name the missing endpoint:\n%s", stderr)
	}
	if strings.Contains(stdout, "bench burst-stream: run") {
		t.Fatalf("the bench started driving before refusing:\n%s", stdout)
	}
}

func TestBenchRefusesProfilerOwnedLegsBeforeResolvingAnInstance(t *testing.T) {
	for _, leg := range []string{"cpu-profile", "allocation"} {
		code, stdout, stderr := run(t, "bench", "burst-stream", "--leg", leg,
			"--registry-dir", t.TempDir())
		if code != exitUsage {
			t.Fatalf("%s: exit = %d, want %d\nstdout: %s\nstderr: %s", leg, code, exitUsage, stdout, stderr)
		}
		if !strings.Contains(stderr, "not implemented by `ao-harness bench`") {
			t.Fatalf("%s: refusal does not name the owning profiler: %s", leg, stderr)
		}
	}
}

func TestBenchCleanMemoryRejectsFrontendMetersBeforeResolvingAnInstance(t *testing.T) {
	code, stdout, stderr := run(t, "bench", "burst-stream", "--leg", "clean-memory", "--meter", "frames",
		"--registry-dir", t.TempDir())
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitUsage, stdout, stderr)
	}
	if !strings.Contains(stderr, "clean-memory leg cannot use frontend meters") {
		t.Fatalf("refusal does not identify the contaminated clean-memory leg: %s", stderr)
	}
}

func TestParseBenchOptionsReadsOptionsAfterWorkload(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	opts, rest, err := parseBenchOptions(e, []string{"burst-stream", "--leg", "clean-memory", "--meter", "frames", "--registry-dir", t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || len(*opts.meters) != 1 || (*opts.meters)[0] != "frames" {
		t.Fatalf("rest=%v meters=%v", rest, *opts.meters)
	}
}

func TestProfileNeedsAThreadAndAScenario(t *testing.T) {
	clearCDPEnv(t)
	code, _, stderr := run(t, "profile", "--registry-dir", t.TempDir())
	if code != exitUsage || !strings.Contains(stderr, "--thread") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	code, _, stderr = run(t, "profile", "--thread", "last", "--registry-dir", t.TempDir())
	if code != exitUsage || !strings.Contains(stderr, "--scenario") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// And with both, the missing endpoint is the next refusal — still
	// before anything is attached.
	code, _, stderr = run(t, "profile", "--thread", "last", "--scenario", "bench-burst-stream",
		"--registry-dir", t.TempDir())
	if code != exitUsage || !strings.Contains(stderr, "no devtools endpoint") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
}
