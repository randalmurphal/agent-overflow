package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessclient"
)

func runPerf(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("perf needs a subcommand: start, stop, status, watch")
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
	// `report` is the spec's older name for the same document stop returns;
	// keeping it as an alias costs one line and saves a wrong guess.
	case "report":
		return perfStop(e, args[1:])
	default:
		return usagef("unknown perf subcommand %q (want start, stop, status, watch)", args[0])
	}
}

func perfStart(e *env, args []string) error {
	flags := e.newFlagSet("perf start")
	sampleMs := flags.Int("sample-ms", 0, "backend sampling interval (default 1000, floor 250)")
	longFrameMs := flags.Int("long-frame-ms", 0, "frame time above which a frame counts as long (bridge default 50)")
	var meters channelList
	flags.Var(&meters, "meter", "arm only this meter (repeatable: frames, longtask, loaf, layout-shift, event, memory, dom)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("perf start takes no positional arguments (got %v)", rest)
	}
	spec := map[string]any{}
	if *sampleMs > 0 {
		spec["sampleMs"] = *sampleMs
	}
	if *longFrameMs > 0 {
		spec["longFrameMs"] = *longFrameMs
	}
	if len(meters) > 0 {
		spec["meters"] = []string(meters)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		raw, err := client.Call(ctx, "HarnessPerfStart", spec)
		if err != nil {
			return uiQueryError(err)
		}
		return e.writeRawJSON(raw)
	})
}

func perfStop(e *env, args []string) error {
	flags := e.newFlagSet("perf stop")
	out := flags.String("out", "", "also write the full report JSON to this file")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("perf stop takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		raw, err := client.Call(ctx, "HarnessPerfStop")
		if err != nil {
			return err
		}
		if *out != "" {
			if err := os.WriteFile(*out, append(indentJSON(raw), '\n'), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", *out, err)
			}
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		report, err := decodePerfReport(raw)
		if err != nil {
			return err
		}
		e.printf("%s", renderPerfReport(report))
		if *out != "" {
			e.printf("report written to %s\n", *out)
		}
		return nil
	})
}

func perfStatus(e *env, args []string) error {
	flags := e.newFlagSet("perf status")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("perf status takes no positional arguments (got %v)", rest)
	}
	return e.call(context.Background(), "HarnessPerfStatus")
}

// perfSample is the frontend half of one `harness:perf` frame. The fields
// are the ones a watcher reads per line; anything else stays in -o json.
//
// There is deliberately no per-sample p95: the frame histogram is folded
// over the WHOLE run (constant memory over an hours-long soak) and only
// `perf stop` can answer a percentile. `maxMs` is the per-sample worst
// frame, which is what a live watcher can honestly show.
type perfSample struct {
	AtMs    int64 `json:"atMs"`
	Seq     int   `json:"seq"`
	Backend struct {
		HeapBytes        uint64 `json:"heapBytes"`
		Goroutines       int    `json:"goroutines"`
		RSSBytes         uint64 `json:"rssBytes"`
		ChildrenRSSBytes uint64 `json:"childrenRssBytes"`
	} `json:"backend"`
	Frontend *struct {
		FPS        float64 `json:"fps"`
		Frames     int     `json:"frames"`
		LongFrames int     `json:"longFrames"`
		MaxFrameMs float64 `json:"maxFrameMs"`
		LongTasks  int     `json:"longTasks"`
		DomNodes   int     `json:"domNodes"`
		HeapBytes  float64 `json:"heapBytes"`
	} `json:"frontend"`
	FrontendError string `json:"frontendError"`
}

func perfWatch(e *env, args []string) error {
	flags := e.newFlagSet("perf watch")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("perf watch takes no positional arguments (got %v)", rest)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
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
			}
		})
		defer cancelListen()

		channel := string(eventchan.HarnessPerf)
		// The ring keeps every perf frame on purpose (a sample is a point in
		// a series), so a watcher that attaches mid-run gets what it missed.
		if err := subscribeAndReplay(ctx, client, channelList{channel}, true); err != nil {
			return err
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

func perfWatchHeader() string {
	return fmt.Sprintf("%-6s %-6s %6s %8s %6s %9s %8s %9s %9s",
		"SEQ", "AT", "FPS", "MAXMS", "LONG", "JSHEAP", "DOM", "GOHEAP", "WEBVIEW")
}

func (e *env) printPerfSample(ev harnessclient.Event) error {
	if ev.Gap {
		return nil
	}
	if e.jsonOutput() {
		return e.printEvent(ev)
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
		return nil
	}
	front := sample.Frontend
	e.printf("%-6d %-6s %6.1f %8.1f %6d %9s %8d %9s %9s\n",
		sample.Seq, at, front.FPS, front.MaxFrameMs, front.LongFrames,
		humanBytes(uint64(front.HeapBytes)), front.DomNodes,
		humanBytes(sample.Backend.HeapBytes), humanBytes(sample.Backend.ChildrenRSSBytes))
	return nil
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

func indentJSON(raw json.RawMessage) []byte {
	var out []byte
	buf, err := json.MarshalIndent(json.RawMessage(raw), "", "  ")
	if err != nil {
		return raw
	}
	out = buf
	return out
}
