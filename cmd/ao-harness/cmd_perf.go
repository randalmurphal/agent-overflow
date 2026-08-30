package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessclient"
)

var perfSubcommands = commandNames(perfCommandDescriptors())

func runPerf(e *env, args []string) error {
	if done, err := groupHelp(e, "perf", args, perfSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("perf needs a subcommand: %s", strings.Join(perfSubcommands, ", "))
	}
	switch args[0] {
	case "start":
		return perfStart(e, args[1:])
	case "stop":
		return perfStop(e, args[1:])
	case "status":
		return perfStatus(e, args[1:])
	case "watch":
		return perfWatch(e, args[1:])
	default:
		return usagef("unknown perf subcommand %q (want %s)", args[0], strings.Join(perfSubcommands, ", "))
	}
}

func perfStart(e *env, args []string) error {
	flags := e.newFlagSet("perf start")
	asJSON := e.bindJSONFlag(flags)
	sampleMs := flags.Int("sample-ms", 0, "backend sampling interval (default 1000, floor 250)")
	longFrameMs := flags.Int("long-frame-ms", 0, "frame time above which a frame counts as long (bridge default 50)")
	budgets := flags.String("budgets", "",
		"comma-separated main-thread budgets in ms for the busy-time fit report (bridge default 6,8,16)")
	var meters stringList
	flags.Var(&meters, "meter", "arm only this meter (repeatable: frames, busy, longtask, loaf, layout-shift, event, memory, dom)")
	var monitors stringList
	flags.Var(&monitors, "monitor", "arm a typed app-feel monitor (repeatable; use `monitor list` in the page bridge)")
	compatibilityLeg := flags.String("leg", "", "compatibility leg required by selected monitors")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if len(rest) != 0 {
		return usagef("perf start takes no positional arguments (got %v)", rest)
	}
	budgetsMs, err := parseBudgetsMs(*budgets)
	if err != nil {
		return err
	}
	spec := map[string]any{}
	if strings.TrimSpace(e.pageID) != "" {
		spec["pageId"] = strings.TrimSpace(e.pageID)
	}
	if *sampleMs > 0 {
		spec["sampleMs"] = *sampleMs
	}
	if *longFrameMs > 0 {
		spec["longFrameMs"] = *longFrameMs
	}
	if len(budgetsMs) > 0 {
		spec["budgetsMs"] = budgetsMs
	}
	if len(meters) > 0 {
		spec["meters"] = []string(meters)
	}
	if len(monitors) > 0 {
		spec["monitors"] = []string(monitors)
	}
	if strings.TrimSpace(*compatibilityLeg) != "" {
		spec["compatibilityLeg"] = strings.TrimSpace(*compatibilityLeg)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		requirements := capabilityRequirements{Methods: []string{"HarnessPerfStart", "HarnessUIQuery"}, Queries: []string{"perf"}}
		if len(monitors) > 0 {
			requirements.Queries = append(requirements.Queries, "monitor")
		}
		if err := requireHarnessProtocol(client, requirements); err != nil {
			return err
		}
		raw, err := client.Call(ctx, "HarnessPerfStart", spec)
		if err != nil {
			return uiQueryError(err)
		}
		return e.printPerfStatus(raw)
	})
}

// perfStatusResult is the shape both `perf start` and `perf status`
// answer with. Typed for the -o text line only; -o json stays the
// server's own bytes.
type perfStatusResult struct {
	Active          bool   `json:"active"`
	RunID           string `json:"runId"`
	SampleMs        int    `json:"sampleMs"`
	Samples         int    `json:"samples"`
	FrontendSamples int    `json:"frontendSamples"`
	ElapsedMs       int64  `json:"elapsedMs"`
	LastError       string `json:"lastError"`
}

// printPerfStatus is the -o text form of the two verbs that used to print
// a raw status document in both formats. The frontend-sample count is on
// the line on purpose: a run whose samples climb while frontendSamples
// stays at zero is the headless-instance mistake, and it is invisible in
// a line that only reports "active".
func (e *env) printPerfStatus(raw json.RawMessage) error {
	if e.jsonOutput() {
		return e.writeRawJSON(raw)
	}
	var status perfStatusResult
	if err := json.Unmarshal(raw, &status); err != nil {
		return e.writeRawJSON(raw)
	}
	state := "idle"
	if status.Active {
		state = "armed"
	}
	e.printf("perf %s %s  sample %dms  samples %d (frontend %d)  elapsed %s\n",
		state, orDash(status.RunID), status.SampleMs, status.Samples, status.FrontendSamples,
		(time.Duration(status.ElapsedMs) * time.Millisecond).Round(time.Second))
	if status.LastError != "" {
		e.printf("  last error: %s\n", truncate(status.LastError, 160))
	}
	return nil
}

func perfStop(e *env, args []string) error {
	flags := e.newFlagSet("perf stop")
	asJSON := e.bindJSONFlag(flags)
	out := flags.String("out", "", "also write the full report JSON to this file")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if len(rest) != 0 {
		return usagef("perf stop takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{"HarnessPerfStop", "HarnessUIQuery"}, Queries: []string{"perf"}}); err != nil {
			return err
		}
		raw, err := client.Call(ctx, "HarnessPerfStop")
		if err != nil {
			return err
		}
		if *out != "" {
			// atomicfile so a report read by another process is either the
			// old one or the whole new one, never a half-written prefix.
			if err := atomicfile.Write(*out, append(indentJSON(raw), '\n')); err != nil {
				return fmt.Errorf("write %s: %w", *out, err)
			}
		}
		report, err := decodePerfReport(raw)
		if err != nil {
			return err
		}
		gapErr := perfEventGapError(client)
		if e.jsonOutput() {
			if err := e.writeRawJSON(raw); err != nil {
				return err
			}
			return errors.Join(perfReportError(report), gapErr)
		}
		e.printf("%s", renderPerfReport(report))
		if *out != "" {
			e.printf("report written to %s\n", *out)
		}
		return errors.Join(perfReportError(report), gapErr)
	})
}

func perfStatus(e *env, args []string) error {
	flags := e.newFlagSet("perf status")
	asJSON := e.bindJSONFlag(flags)
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if len(rest) != 0 {
		return usagef("perf status takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		raw, err := client.Call(ctx, "HarnessPerfStatus")
		if err != nil {
			return err
		}
		return e.printPerfStatus(raw)
	})
}

// perfSample is the frontend half of one `harness:perf` frame. The fields
// are the ones a watcher reads per line; anything else stays in -o json.
//
// There is deliberately no per-sample p95 and no per-sample budget fit:
// both histograms are folded over the WHOLE run (constant memory over an
// hours-long soak) and only `perf stop` can answer a percentile or a fit
// percentage. `maxFrameMs` and `maxBusyMs` are the per-sample worsts, which
// is what a live watcher can honestly show.
type perfSample struct {
	AtMs    int64 `json:"atMs"`
	Seq     int   `json:"seq"`
	Backend struct {
		HeapBytes            uint64 `json:"heapBytes"`
		Goroutines           int    `json:"goroutines"`
		RSSBytes             uint64 `json:"rssBytes"`
		ChildrenRSSBytes     uint64 `json:"childrenRssBytes"`
		RSSAvailable         bool   `json:"rssAvailable"`
		WebviewRSSMeasurable bool   `json:"webviewRssMeasurable"`
	} `json:"backend"`
	Frontend *struct {
		FPS               float64  `json:"fps"`
		Frames            int      `json:"frames"`
		LongFrames        int      `json:"longFrames"`
		MaxFrameMs        float64  `json:"maxFrameMs"`
		BusyTicks         int      `json:"busyTicks"`
		MaxBusyMs         float64  `json:"maxBusyMs"`
		MeanBusyMs        float64  `json:"meanBusyMs"`
		Meters            []string `json:"meters"`
		UnavailableMeters []string `json:"unavailableMeters"`
		LongTasks         int      `json:"longTasks"`
		DomNodes          int      `json:"domNodes"`
		HeapBytes         float64  `json:"heapBytes"`
	} `json:"frontend"`
	FrontendError string `json:"frontendError"`
}

func perfWatch(e *env, args []string) error {
	flags := e.newFlagSet("perf watch")
	asJSON := e.bindJSONFlag(flags)
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if len(rest) != 0 {
		return usagef("perf watch takes no positional arguments (got %v)", rest)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		channel := string(eventchan.HarnessPerf)
		if err := client.Subscribe(ctx, channel); err != nil {
			return fmt.Errorf("subscribe to %s: %w", channel, err)
		}
		if !e.jsonOutput() {
			e.printf("%s\n", perfWatchHeader())
		}
		failed := make(chan error, 1)
		cancelListen := client.Listen(func(ev harnessclient.Event) {
			if err := e.printPerfSample(ev); err != nil {
				select {
				case failed <- err:
				default:
				}
				return
			}
			if err := perfEventGapError(client); err != nil {
				select {
				case failed <- err:
				default:
				}
			}
		})
		defer cancelListen()

		// The ring keeps every perf frame on purpose (a sample is a point in
		// a series), so a watcher that attaches mid-run gets what it missed.
		if err := client.Replay(ctx, map[string]uint64{channel: ringReplayCursor}); err != nil {
			return fmt.Errorf("replay %s: %w", channel, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-client.Done():
			return fmt.Errorf("the instance closed the connection")
		case err := <-failed:
			return err
		}
	})
}

// BUSYP50 is deliberately absent: a percentile needs the whole run. The
// two busy columns are this window's mean and worst, which is everything a
// per-sample line can honestly claim.
func perfWatchHeader() string {
	return fmt.Sprintf("%-6s %-6s %6s %8s %6s %8s %8s %9s %8s %9s %9s",
		"SEQ", "AT", "FPS", "MAXMS", "LONG", "BUSYAVG", "BUSYMAX", "JSHEAP", "DOM", "GOHEAP", "WEBVIEW")
}

func (e *env) printPerfSample(ev harnessclient.Event) error {
	if ev.Gap {
		if err := e.printEvent(ev); err != nil {
			return err
		}
		return fmt.Errorf("perf event stream has a sequence gap at %s seq %d", ev.Channel, ev.Seq)
	}
	if e.jsonOutput() {
		printErr := e.printEvent(ev)
		var sample struct {
			FrontendError string `json:"frontendError"`
		}
		if err := json.Unmarshal(ev.Data, &sample); err == nil && sample.FrontendError != "" {
			return errors.Join(printErr, fmt.Errorf("perf frontend collection failed: %s", sample.FrontendError))
		}
		return printErr
	}
	var sample perfSample
	if err := json.Unmarshal(ev.Data, &sample); err != nil {
		// A frame the watcher cannot decode is still evidence: print it raw
		// rather than dropping it, and keep watching.
		e.printf("%-6d %s\n", ev.Seq, truncate(string(ev.Data), 140))
		return nil
	}
	at := (time.Duration(sample.AtMs) * time.Millisecond).Round(time.Second)
	if sample.Frontend == nil {
		e.printf("%-6d %-6s %s\n", sample.Seq, at,
			"frontend: "+truncate(orDash(sample.FrontendError), 100))
		if sample.FrontendError != "" {
			return fmt.Errorf("perf frontend collection failed: %s", sample.FrontendError)
		}
		return nil
	}
	front := sample.Frontend
	measured := func(name string) bool {
		for _, meter := range front.Meters {
			if meter == name {
				return true
			}
		}
		return false
	}
	fps, maxFrame, long := "-", "-", "-"
	if measured("frames") {
		fps = fmt.Sprintf("%.1f", front.FPS)
		maxFrame = fmt.Sprintf("%.1f", front.MaxFrameMs)
		long = fmt.Sprint(front.LongFrames)
	}
	jsHeap, dom := "-", "-"
	if measured("memory") {
		jsHeap = humanBytes(uint64(front.HeapBytes))
	}
	if measured("dom") {
		dom = fmt.Sprint(front.DomNodes)
	}
	busyAvg, busyMax := "-", "-"
	if measured("busy") {
		busyAvg = busyCell(front.BusyTicks, front.MeanBusyMs)
		busyMax = busyCell(front.BusyTicks, front.MaxBusyMs)
	}
	e.printf("%-6d %-6s %6s %8s %6s %8s %8s %9s %8s %9s %9s\n",
		sample.Seq, at, fps, maxFrame, long,
		busyAvg, busyMax,
		jsHeap, dom,
		humanBytes(sample.Backend.HeapBytes), webviewRSSCell(sample.Backend.WebviewRSSMeasurable, sample.Backend.ChildrenRSSBytes))
	if sample.FrontendError != "" {
		return fmt.Errorf("perf frontend collection failed: %s", sample.FrontendError)
	}
	return nil
}

func webviewRSSCell(measurable bool, bytes uint64) string {
	if !measurable {
		return "-"
	}
	return humanBytes(bytes)
}

func perfReportError(report perfReport) error {
	if report.FrontendError != "" {
		return fmt.Errorf("perf frontend collection failed: %s", report.FrontendError)
	}
	if report.MonitorsError != "" {
		return fmt.Errorf("perf monitor collection failed: %s", report.MonitorsError)
	}
	return nil
}

func perfEventGapError(client *harnessclient.Client) error {
	if client == nil {
		return nil
	}
	channel := string(eventchan.HarnessPerf)
	for _, gap := range client.SequenceGaps() {
		if gap.Channel == channel {
			return fmt.Errorf("perf event stream has a sequence gap on %s (expected %d, observed %d)", channel, gap.Expected, gap.Observed)
		}
	}
	return nil
}

// busyCell prints a dash rather than 0.00 for a window that measured no
// tick. Zero busy time and no measurement are opposite findings, and a
// column of zeros is the one that lies.
func busyCell(ticks int, value float64) string {
	if ticks == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", value)
}

func humanBytes(n uint64) string {
	switch {
	case n == 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.2fG", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/float64(1<<10))
	default:
		return fmt.Sprint(n)
	}
}

// indentJSON re-indents the server's own bytes. MarshalIndent over a
// RawMessage reformats without re-marshalling, so object key order and
// integer spelling are the backend's, not Go's map iteration's.
func indentJSON(raw json.RawMessage) []byte {
	buf, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return raw
	}
	return buf
}
