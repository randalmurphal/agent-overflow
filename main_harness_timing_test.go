package main

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appservice "agent-overflow/internal/app"
)

func TestParseHarnessTiming(t *testing.T) {
	got, err := parseHarnessTiming(" pairing-probe=500ms, watermark=1s ,thread-poll=250ms,wake-retry=2s,unreachable-settle=3s,replay-timeout=4s,")
	if err != nil {
		t.Fatal(err)
	}
	var want harnessTiming
	want.PairingProbe, want.Watermark = 500*time.Millisecond, time.Second
	want.Isolation.ThreadRequestPoll, want.Isolation.ThreadWakeRetryBase = 250*time.Millisecond, 2*time.Second
	want.Frontend = map[string]int64{"unreachable-settle": 3000, "replay-timeout": 4000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
	if got, err := parseHarnessTiming(""); err != nil || !reflect.DeepEqual(got, harnessTiming{}) {
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
		"replay-timeout=1s,replay-timeout=2s",
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
		for _, variable := range []string{"diagenv.HarnessTiming", "diagenv.HarnessOlderComputer"} {
			uses := strings.Contains(readRootSource(t, name), "os.Getenv("+variable+")")
			if uses != (name == "main_harness.go") {
				t.Errorf("%s reads %s: %v", name, variable, uses)
			}
		}
	}
	// Only the harness boot hands its timing to the transport: a --soak, a
	// desktop or a served boot leaves it zero, so its manifest serves no
	// frontend timing.
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sets := regexp.MustCompile(`\bHarnessTiming:\s`).MatchString(readRootSource(t, name))
		if sets != (name == "main_harness.go") {
			t.Errorf("%s sets bootTransportOptions.HarnessTiming: %v", name, sets)
		}
	}
	// The parsed value reaches each owner: the App's isolation pins, and the
	// transport and attached-computer manager bootTransport builds.
	for file, wirings := range map[string][]string{
		"main_harness.go": {
			`isolation\.Timing = timing`,
			`HarnessTiming:\s+timing,`,
			`config := opts\.Timing\.Isolation`,
			`config\.OmitOwnerOps = opts\.OlderComputer`,
			`config\.OmitAgentMessageRunMembers = opts\.OlderComputer`,
			`isolation\.OlderComputer = envTruthy\(os\.Getenv\(diagenv\.HarnessOlderComputer\)\)`,
			`HarnessOlderComputer:\s+isolation\.OlderComputer,`,
		},
		"main.go": {
			`WatermarkInterval:\s+opts\.HarnessTiming\.Watermark,`,
			`FrontendTiming:\s+opts\.HarnessTiming\.Frontend,`,
			`bootAttachedBackends\(opts\.HarnessTiming\.PairingProbe\)`,
			`manager\.SetActivationProbe\(activationProbe\)`,
			`if opts\.HarnessOlderComputer \{\s+appOptions\.Skip, appOptions\.Reshape = olderComputerSurface\(\)`,
			`OmitAgentMessageRunMembers:\s+opts\.HarnessReceiver != nil && opts\.HarnessOlderComputer,`,
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

// Every harness timer reaches exactly one owner, and no other timer's: a
// timer the boot hands on, one field of the App's isolation, or the page's
// manifest entry of its name. Set alone, each changes that and nothing else.
func TestEveryHarnessTimerReachesOneOwner(t *testing.T) {
	const sentinel = 7 * time.Millisecond
	durations := func(config appservice.IsolationConfig) []string {
		var set []string
		v := reflect.ValueOf(config)
		for i := range v.NumField() {
			if d, ok := v.Field(i).Interface().(time.Duration); ok && d == sentinel {
				set = append(set, v.Type().Field(i).Name)
			}
		}
		return set
	}
	owners := map[string]string{}
	for _, timer := range harnessTimers {
		timing, err := parseHarnessTiming(timer.name + "=7ms")
		if err != nil {
			t.Fatalf("%s: %v", timer.name, err)
		}
		isolated := durations(isolationConfig(harnessPaths{}, isolationOptions{Timing: timing}))
		booted := 0
		if timing.PairingProbe == sentinel {
			booted++
		}
		if timing.Watermark == sentinel {
			booted++
		}
		paged := 0
		if timing.Frontend[timer.name] == sentinel.Milliseconds() {
			paged = len(timing.Frontend)
		}
		if n := len(isolated) + booted + paged; n != 1 {
			t.Errorf("%s reaches %d owners (isolation %v, boot %d, page %d); want one", timer.name, n, isolated, booted, paged)
			continue
		}
		owner := "page " + timer.name
		switch {
		case len(isolated) == 1:
			owner = isolated[0]
		case timing.PairingProbe == sentinel:
			owner = "PairingProbe"
		case timing.Watermark == sentinel:
			owner = "Watermark"
		}
		if other, taken := owners[owner]; taken {
			t.Errorf("%s and %s both set %s", other, timer.name, owner)
		}
		owners[owner] = timer.name
	}
}

// The page names the same timers the boot serves it: an entry the page
// does not name fails its manifest, and one the boot does not serve never
// moves the page's timer.
func TestFrontendTimersMatchThePage(t *testing.T) {
	var served []string
	for _, timer := range harnessTimers {
		if timer.field == nil {
			served = append(served, timer.name)
		}
	}
	source, err := os.ReadFile("frontend/src/lib/transport/harnessMode.ts")
	if err != nil {
		t.Fatal(err)
	}
	list := regexp.MustCompile(`(?s)const FRONTEND_TIMERS[^\[]*\[(.*?)\]`).FindSubmatch(source)
	if list == nil {
		t.Fatal("harnessMode.ts no longer declares FRONTEND_TIMERS")
	}
	var named []string
	for _, m := range regexp.MustCompile(`'([a-z-]+)'`).FindAllSubmatch(list[1], -1) {
		named = append(named, string(m[1]))
	}
	slices.Sort(served)
	slices.Sort(named)
	if !slices.Equal(served, named) {
		t.Fatalf("the boot serves the page %v; harnessMode.ts FRONTEND_TIMERS names %v", served, named)
	}
}
