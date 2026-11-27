package main

import (
	"fmt"
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
	got, err := parseHarnessTiming(" pairing-probe=500ms, watermark=1s ,thread-poll=250ms,wake-retry=2s,unreachable-settle=3s,agent-row-linger=4s,")
	if err != nil {
		t.Fatal(err)
	}
	var want harnessTiming
	want.PairingProbe, want.Watermark = 500*time.Millisecond, time.Second
	want.Isolation.ThreadRequestPoll, want.Isolation.ThreadWakeRetryBase = 250*time.Millisecond, 2*time.Second
	want.Frontend = map[string]int64{"unreachable-settle": 3000, "agent-row-linger": 4000}
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
		"agent-row-linger=1s,agent-row-linger=2s",
	} {
		if _, err := parseHarnessTiming(bad); err == nil {
			t.Errorf("parseHarnessTiming(%q) accepted a malformed value", bad)
		}
	}
}

// Each timer whose destination bounds it takes the ends of what the
// destination holds and refuses one step past either, naming the timer: a
// page timer is whole milliseconds up to setTimeout's longest, the wake
// floor whole seconds, and a timer the App doubles or multiplies at most
// what that arithmetic fits.
func TestParseHarnessTimingRefusesWhatTheDestinationCannotHold(t *testing.T) {
	bounds := map[string]struct{ max, unit time.Duration }{
		"thread-poll":        {max: appservice.MaxThreadRequestPoll},
		"pr-update":          {max: appservice.MaxPRUpdateInterval},
		"pr-ci-live":         {max: appservice.MaxPRCILiveInterval},
		"merged-pr-poll":     {max: appservice.MaxMergedPRPollInterval},
		"wake-retry":         {max: appservice.MaxThreadWakeRetryBase},
		"wake-floor":         {unit: time.Second},
		"unreachable-settle": {max: (1<<31 - 1) * time.Millisecond, unit: time.Millisecond},
		"agent-row-linger":   {max: (1<<31 - 1) * time.Millisecond, unit: time.Millisecond},
	}
	for _, timer := range harnessTimers {
		want := bounds[timer.name]
		if timer.max != want.max || timer.unit != want.unit {
			t.Errorf("%s: bounded at most %s in units of %s, want %s and %s", timer.name, timer.max, timer.unit, want.max, want.unit)
		}
	}
	refused := func(entry, name string) {
		t.Helper()
		_, err := parseHarnessTiming(entry)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("parseHarnessTiming(%q) = %v, want a refusal naming %s", entry, err, name)
		}
	}
	taken := func(entry string) {
		t.Helper()
		if _, err := parseHarnessTiming(entry); err != nil {
			t.Errorf("parseHarnessTiming(%q): %v", entry, err)
		}
	}
	for name, bound := range bounds {
		unit := max(bound.unit, time.Nanosecond)
		taken(fmt.Sprintf("%s=%dns", name, unit))
		if bound.unit > 0 {
			refused(fmt.Sprintf("%s=%dns", name, bound.unit-1), name)
			refused(fmt.Sprintf("%s=%dns", name, bound.unit+bound.unit/2), name)
		}
		if bound.max > 0 {
			taken(fmt.Sprintf("%s=%dns", name, bound.max))
			refused(fmt.Sprintf("%s=%dns", name, bound.max+unit), name)
		}
	}
	// A page timer reaches the manifest in exact milliseconds.
	got, err := parseHarnessTiming("unreachable-settle=2147483647ms,agent-row-linger=1ms")
	if err != nil || got.Frontend["unreachable-settle"] != 1<<31-1 || got.Frontend["agent-row-linger"] != 1 {
		t.Fatalf("page timers = %v, %v; want 2147483647 and 1", got.Frontend, err)
	}
	refused("unreachable-settle=600h", "unreachable-settle")
	refused("wake-retry=600000h", "wake-retry")
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

// harnessTimerOwners is what each harness timer name means: the boot
// field, the App isolation field (whose consumer
// internal/app TestEveryIsolationIntervalReachesItsConsumer reads) or the
// page timer it sets. Written apart from harnessTimers, so a row pointed
// at the wrong owner reads the wrong owner here.
var harnessTimerOwners = map[string]string{
	"pairing-probe":         "PairingProbe",
	"watermark":             "Watermark",
	"thread-poll":           "Isolation.ThreadRequestPoll",
	"transfer-retry":        "Isolation.TransferPendingRetry",
	"pr-update":             "Isolation.PRUpdateInterval",
	"pr-update-retry":       "Isolation.PRUpdateRetryBase",
	"pr-ci-live":            "Isolation.PRCILiveInterval",
	"pr-ci-follow":          "Isolation.PRCIFollowInterval",
	"pr-ci-log-wait":        "Isolation.PRCILogWaitInterval",
	"merged-pr-poll":        "Isolation.MergedPRPollInterval",
	"pr-lookup-retry":       "Isolation.PRLookupRetryBase",
	"idle-reap":             "Isolation.IdleReapThreshold",
	"idle-reap-sweep":       "Isolation.IdleReapInterval",
	"wakeup-grace":          "Isolation.WakeupReapGrace",
	"wake-floor":            "Isolation.AgentWakeFloor",
	"watch-failure-window":  "Isolation.AgentWatchFailureWindow",
	"remote-watch-poll":     "Isolation.RemoteWatchPoll",
	"drain-stall":           "Isolation.ProviderEventDrainStall",
	"usage-probe-gap":       "Isolation.UsageProbeGap",
	"usage-poll":            "Isolation.UsagePollInterval",
	"wake-retry":            "Isolation.ThreadWakeRetryBase",
	"agent-settled-window":  "Isolation.AgentSettledWindow",
	"tracker-poll":          "Isolation.TrackerPollInterval",
	"tracker-approval-wait": "Isolation.TrackerApprovalWait",
	"unreachable-settle":    "page unreachable-settle",
	"agent-row-linger":      "page agent-row-linger",
}

// Set alone through the boot's own path (parseHarnessTiming, then
// isolationConfig), each harness timer reaches the owner its name means and
// nothing else; every name has a meaning and every meaning a name.
func TestEveryHarnessTimerReachesItsOwner(t *testing.T) {
	// Whole seconds, so every timer takes it, the wake floor included.
	const sentinel = 7 * time.Second
	reached := func(timing harnessTiming) []string {
		var set []string
		if timing.PairingProbe == sentinel {
			set = append(set, "PairingProbe")
		}
		if timing.Watermark == sentinel {
			set = append(set, "Watermark")
		}
		v := reflect.ValueOf(isolationConfig(harnessPaths{}, isolationOptions{Timing: timing}))
		for i := range v.NumField() {
			if d, ok := v.Field(i).Interface().(time.Duration); ok && d == sentinel {
				set = append(set, "Isolation."+v.Type().Field(i).Name)
			}
		}
		for name, ms := range timing.Frontend {
			if ms == sentinel.Milliseconds() {
				set = append(set, "page "+name)
			}
		}
		return set
	}
	named := map[string]bool{}
	for _, timer := range harnessTimers {
		named[timer.name] = true
		want, ok := harnessTimerOwners[timer.name]
		if !ok {
			t.Errorf("%s has no owner in harnessTimerOwners", timer.name)
			continue
		}
		timing, err := parseHarnessTiming(timer.name + "=" + sentinel.String())
		if err != nil {
			t.Fatalf("%s: %v", timer.name, err)
		}
		if got := reached(timing); !slices.Equal(got, []string{want}) {
			t.Errorf("%s reaches %v; want %s", timer.name, got, want)
		}
	}
	for name := range harnessTimerOwners {
		if !named[name] {
			t.Errorf("harnessTimerOwners names %s, which harnessTimers does not hold", name)
		}
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
