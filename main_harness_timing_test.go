package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseHarnessTiming(t *testing.T) {
	got, err := parseHarnessTiming(" pairing-probe=500ms, watermark=1s ,thread-poll=250ms,transfer-retry=100ms,")
	if err != nil {
		t.Fatal(err)
	}
	want := harnessTiming{PairingProbe: 500 * time.Millisecond, Watermark: time.Second, ThreadPoll: 250 * time.Millisecond, TransferRetry: 100 * time.Millisecond}
	if got != want {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
	if got, err := parseHarnessTiming(""); err != nil || got != (harnessTiming{}) {
		t.Fatalf("empty value = %+v, %v; want the product timing", got, err)
	}
	for _, bad := range []string{
		"pairing-probe",
		"pairing-probe=",
		"pairing-probe=fast",
		"pairing-probe=0s",
		"pairing-probe=-1s",
		"probe=1s",
		"watermark=1s,watermark=2s",
	} {
		if _, err := parseHarnessTiming(bad); err == nil {
			t.Errorf("parseHarnessTiming(%q) accepted a malformed value", bad)
		}
	}
}

// TestHarnessTimingOnlyInHarnessBoot: the shortened intervals are a test
// isolation aid, read by the harness boot alone and handed to each owner
// from there.
func TestHarnessTimingOnlyInHarnessBoot(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		uses := strings.Contains(readRootSource(t, name), "os.Getenv(diagenv.HarnessTiming)")
		if uses != (name == "main_harness.go") {
			t.Errorf("%s reads diagenv.HarnessTiming: %v", name, uses)
		}
	}
	// The parsed value reaches each owner: the App's isolation pins, and the
	// transport and attached-computer manager bootTransport builds.
	for file, wirings := range map[string][]string{
		"main_harness.go": {
			`isolation\.Timing = timing`,
			`HarnessTiming:\s+timing,`,
			`ThreadRequestPoll:\s+opts\.Timing\.ThreadPoll,`,
			`TransferPendingRetry:\s+opts\.Timing\.TransferRetry,`,
		},
		"main.go": {
			`WatermarkInterval:\s+opts\.HarnessTiming\.Watermark,`,
			`bootAttachedBackends\(opts\.HarnessTiming\.PairingProbe\)`,
			`manager\.SetActivationProbe\(activationProbe\)`,
		},
	} {
		source := readRootSource(t, file)
		for _, wiring := range wirings {
			if !regexp.MustCompile(wiring).MatchString(source) {
				t.Errorf("%s no longer hands on the harness timing (%s)", file, wiring)
			}
		}
	}
}
