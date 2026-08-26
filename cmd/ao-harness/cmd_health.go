package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/procrss"
	"agent-overflow/internal/uitrace"
)

// The generalized soak-check (spec §6): everything about one instance that
// is visible from the backend side, rolled into one screen with a verdict
// per concern.
//
// Two properties make it usable in a loop. Every FILE concern is
// since-last-check, through a cursor in the instance's own data dir, so a
// second run reports what happened between the two rather than the whole
// history again. And --watch appends timestamped lines instead of
// repainting, so a long watch is greppable evidence rather than a screen
// that only made sense while you were looking at it.

const defaultHealthInterval = 30 * time.Second

func runHealth(e *env, args []string) error {
	flags := e.newFlagSet("health")
	watch := flags.Bool("watch", false, "keep checking on an interval, appending one line per concern")
	interval := flags.Duration("interval", defaultHealthInterval, "how often --watch re-checks")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("health takes no positional arguments (got %v)", rest)
	}
	if *interval < time.Second {
		return usagef("--interval must be at least 1s")
	}

	if !*watch {
		report, err := e.collectHealth(context.Background())
		if err != nil {
			return err
		}
		if err := e.printHealth(report, false); err != nil {
			return err
		}
		if code := healthExitCode(report); code != exitOK {
			return exitCodeError{code: code, err: fmt.Errorf("health is red for instance %s", report.Instance)}
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for {
		report, err := e.collectHealth(ctx)
		if err != nil {
			// A watch must survive an instance that went away and came
			// back: print the failure as a line and keep the cadence.
			e.printf("%s red  health           %v\n", time.Now().Format(time.RFC3339), err)
		} else if err := e.printHealth(report, true); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

func (e *env) printHealth(report healthReport, watch bool) error {
	if e.jsonOutput() {
		return e.writeJSON(report)
	}
	if watch {
		e.printf("%s", renderHealthWatchLines(report))
		return nil
	}
	e.printf("%s", renderHealthReport(report))
	return nil
}

// collectHealth builds one rollup. It deliberately does NOT require the
// backend to be reachable: liveness and the evidence files are exactly
// what a reader wants when the process has died, so an attach failure
// degrades the RPC-backed sections rather than failing the command.
func (e *env) collectHealth(ctx context.Context) (healthReport, error) {
	t, err := e.resolveTarget()
	if err != nil {
		return healthReport{}, err
	}
	report := healthReport{At: time.Now().Format(time.RFC3339), Instance: t.ID}

	bs, bsErr := harnessclient.ReadInstanceFile(t.DataDir)
	alive := bsErr == nil && instanceinfo.ProcessAlive(bs.PID)
	report.Sections = append(report.Sections, processSection(t, bs, bsErr, alive))

	dataDir := t.DataDir
	if bsErr == nil && bs.DataDir != "" {
		dataDir = bs.DataDir
	}
	cursor := loadHealthCursor(dataDir)
	cursor.Instance = t.ID
	cursor.CheckedAt = report.At

	frontendSection, next := e.frontendErrorSection(dataDir, cursor.FrontendErrors)
	cursor.FrontendErrors = next
	report.Sections = append(report.Sections, frontendSection)

	oracleSection, next := e.oracleSection(dataDir, cursor.UITrace)
	cursor.UITrace = next
	report.Sections = append(report.Sections, oracleSection)

	backendSection, next := e.backendLogSection(dataDir, cursor.BackendStderr)
	cursor.BackendStderr = next
	report.Sections = append(report.Sections, backendSection)

	report.Sections = append(report.Sections, rssSection(bs.PID, alive))

	if !alive {
		report.Sections = append(report.Sections, healthSection{
			Name: "runtime", Status: healthWarn,
			Detail: "database, mocks, replay and perf not read: no live backend to ask",
		})
		if err := saveHealthCursor(dataDir, cursor); err != nil {
			return report, err
		}
		return report, nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	client, err := harnessclient.Dial(dialCtx, bs, harnessclient.Options{})
	if err != nil {
		report.Sections = append(report.Sections, healthSection{
			Name: "runtime", Status: healthWarn,
			Detail: "backend is running but refused a connection: " + err.Error(),
		})
	} else {
		defer client.Close()
		report.Sections = append(report.Sections,
			dbSection(ctx, client),
			mockSection(ctx, client),
			replaySection(ctx, client),
			perfSection(ctx, client))
	}
	if err := saveHealthCursor(dataDir, cursor); err != nil {
		return report, err
	}
	return report, nil
}

func processSection(t target, bs harnessclient.Bootstrap, bsErr error, alive bool) healthSection {
	if bsErr != nil {
		return healthSection{Name: "process", Status: healthRed,
			Detail: fmt.Sprintf("no instance file under %s (%v)", t.DataDir, bsErr)}
	}
	if !alive {
		return healthSection{Name: "process", Status: healthRed,
			Detail: fmt.Sprintf("pid %d is gone; data root %s", bs.PID, t.DataRoot)}
	}
	mode := "harness"
	uptime := "unknown uptime"
	if t.Row != nil {
		mode = string(t.Row.Mode)
		uptime = formatUptime(t.Row.StartedAt)
	}
	return healthSection{Name: "process", Status: healthOK,
		Detail: fmt.Sprintf("pid %d %s on port %d, %s", bs.PID, mode, bs.Port, uptime)}
}

func (e *env) frontendErrorSection(dataDir string, from healthFileCursor) (healthSection, healthFileCursor) {
	path := filepath.Join(dataDir, uitrace.DirName, uitrace.ErrorFileName)
	lines, next, rotated, err := scanNewLines(path, from)
	if err != nil {
		return healthSection{Name: "frontend-errors", Status: healthWarn,
			Detail: fmt.Sprintf("could not read %s: %v", path, err)}, from
	}
	scan := scanFrontendErrors(lines)
	switch {
	case scan.Faults > 0:
		// An uncaught fault in the renderer is red on its own: nothing
		// throws here in a healthy session.
		return healthSection{Name: "frontend-errors", Status: healthRed,
			Detail: fmt.Sprintf("%d new fault(s) since the last check (%d engine notice(s))%s",
				scan.Faults, scan.Notices, rotatedSuffix(rotated)),
			Lines: scan.Sample}, next
	case scan.Notices > 0:
		return healthSection{Name: "frontend-errors", Status: healthWarn,
			Detail: fmt.Sprintf("no faults; %d engine notice(s) (ResizeObserver loop: layout work outran a frame)%s",
				scan.Notices, rotatedSuffix(rotated))}, next
	default:
		return healthSection{Name: "frontend-errors", Status: healthOK,
			Detail: "none since the last check" + rotatedSuffix(rotated)}, next
	}
}

func (e *env) oracleSection(dataDir string, from healthFileCursor) (healthSection, healthFileCursor) {
	path := filepath.Join(dataDir, uitrace.DirName, uitrace.FileName)
	lines, next, rotated, err := scanNewLines(path, from)
	if err != nil {
		return healthSection{Name: "ui-oracles", Status: healthWarn,
			Detail: fmt.Sprintf("could not read %s: %v", path, err)}, from
	}
	counts := countOracleTriggers(lines)
	if len(counts) == 0 {
		detail := fmt.Sprintf("no triggers in %d new trace record(s)%s", len(lines), rotatedSuffix(rotated))
		if len(lines) == 0 {
			// A harness build ships with UI_TRACE unset, so an empty trace
			// is the norm rather than a silence worth flagging.
			detail = "no trace records (this build may have UI_TRACE unset)"
		}
		return healthSection{Name: "ui-oracles", Status: healthOK, Detail: detail}, next
	}
	return healthSection{Name: "ui-oracles", Status: healthWarn,
		Detail: formatOracleCounts(counts) + rotatedSuffix(rotated)}, next
}

func (e *env) backendLogSection(dataDir string, from healthFileCursor) (healthSection, healthFileCursor) {
	path := filepath.Join(dataDir, logDirName, backendStderrLog)
	lines, next, rotated, err := scanNewLines(path, from)
	if err != nil {
		return healthSection{Name: "backend-stderr", Status: healthWarn,
			Detail: fmt.Sprintf("could not read %s: %v", path, err)}, from
	}
	scan := scanBackendLog(lines)
	status := healthOK
	switch {
	case scan.Fatal > 0:
		status = healthRed
	case scan.Errors > 0:
		status = healthWarn
	}
	detail := fmt.Sprintf("%d new line(s): %d fatal, %d error, %d warn%s",
		scan.Total, scan.Fatal, scan.Errors, scan.Warns, rotatedSuffix(rotated))
	if scan.Total == 0 {
		// `up` captures stderr here; an instance started by `make harness`
		// logs to its own console instead, and an absent file is that, not
		// a silent backend.
		detail = "nothing new (absent when the instance was not started by `ao-harness up`)"
	}
	return healthSection{Name: "backend-stderr", Status: status, Detail: detail, Lines: scan.Sample}, next
}

func rssSection(pid int, alive bool) healthSection {
	if !alive || pid == 0 {
		return healthSection{Name: "rss", Status: healthOK, Detail: "not sampled: no live process"}
	}
	if !procrss.Supported() {
		return healthSection{Name: "rss", Status: healthOK, Detail: "not available off linux"}
	}
	tree, err := procrss.SampleAll(pid)
	if err != nil {
		return healthSection{Name: "rss", Status: healthWarn, Detail: "read /proc: " + err.Error()}
	}
	return healthSection{Name: "rss", Status: healthOK,
		Detail: fmt.Sprintf("tree %s (backend %s + %d child process(es) %s)",
			humanBytes(tree.TotalRSSBytes()), humanBytes(tree.Self.RSSBytes),
			len(tree.Children), humanBytes(tree.ChildrenRSSBytes))}
}

// dbSection asks the instance where its store is rather than composing a
// path, for the same reason `db` does: the backend is the only thing that
// knows for certain what it opened, and a guessed path would report a size
// for a file nothing is writing.
func dbSection(ctx context.Context, client *harnessclient.Client) healthSection {
	info, err := client.Info(ctx)
	if err != nil {
		return healthSection{Name: "database", Status: healthWarn, Detail: err.Error()}
	}
	path := info.DBPath
	if path == "" {
		return healthSection{Name: "database", Status: healthWarn, Detail: "the instance reported no database path"}
	}
	stat, err := os.Stat(path)
	if err != nil {
		return healthSection{Name: "database", Status: healthOK, Detail: "no database file yet at " + path}
	}
	// The write-ahead log routinely dwarfs the main file mid-session, so a
	// size that ignored it would understate what is on disk.
	total := stat.Size()
	for _, suffix := range []string{"-wal", "-shm"} {
		if extra, err := os.Stat(path + suffix); err == nil {
			total += extra.Size()
		}
	}
	return healthSection{Name: "database", Status: healthOK,
		Detail: fmt.Sprintf("%s (%s including wal/shm)", humanBytes(uint64(stat.Size())), humanBytes(uint64(total)))}
}

func mockSection(ctx context.Context, client *harnessclient.Client) healthSection {
	raw, err := client.Call(ctx, "HarnessListMocks")
	if err != nil {
		return healthSection{Name: "mocks", Status: healthWarn, Detail: err.Error()}
	}
	var mocks []struct {
		MockID   string `json:"mockId"`
		Protocol string `json:"protocol"`
		Scenario string `json:"scenario"`
		Exited   bool   `json:"exited"`
	}
	if err := json.Unmarshal(raw, &mocks); err != nil {
		return healthSection{Name: "mocks", Status: healthWarn, Detail: "decode: " + err.Error()}
	}
	live := 0
	for _, mock := range mocks {
		if !mock.Exited {
			live++
		}
	}
	return healthSection{Name: "mocks", Status: healthOK,
		Detail: fmt.Sprintf("%d registered, %d still live", len(mocks), live)}
}

func replaySection(ctx context.Context, client *harnessclient.Client) healthSection {
	raw, err := client.Call(ctx, "HarnessReplayStatus")
	if err != nil {
		return healthSection{Name: "replay", Status: healthWarn, Detail: err.Error()}
	}
	var status struct {
		State   string `json:"state"`
		Emitted int    `json:"emitted"`
		Total   int    `json:"total"`
		Bundle  string `json:"bundle"`
		LastErr string `json:"error"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return healthSection{Name: "replay", Status: healthOK, Detail: truncate(string(raw), 120)}
	}
	if status.State == "" || status.State == "idle" {
		return healthSection{Name: "replay", Status: healthOK, Detail: "idle"}
	}
	section := healthSection{Name: "replay", Status: healthOK,
		Detail: fmt.Sprintf("%s %s (%d/%d events)", status.State, orDash(status.Bundle), status.Emitted, status.Total)}
	if status.LastErr != "" {
		section.Status = healthWarn
		section.Detail += ": " + status.LastErr
	}
	return section
}

func perfSection(ctx context.Context, client *harnessclient.Client) healthSection {
	raw, err := client.Call(ctx, "HarnessPerfStatus")
	if err != nil {
		return healthSection{Name: "perf", Status: healthWarn, Detail: err.Error()}
	}
	var status struct {
		Active          bool   `json:"active"`
		RunID           string `json:"runId"`
		Samples         int    `json:"samples"`
		FrontendSamples int    `json:"frontendSamples"`
		ElapsedMs       int64  `json:"elapsedMs"`
		LastError       string `json:"lastError"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return healthSection{Name: "perf", Status: healthOK, Detail: truncate(string(raw), 120)}
	}
	if !status.Active {
		return healthSection{Name: "perf", Status: healthOK, Detail: "no run armed"}
	}
	section := healthSection{Name: "perf", Status: healthOK,
		Detail: fmt.Sprintf("%s: %d samples (%d answered by the page) over %dms",
			status.RunID, status.Samples, status.FrontendSamples, status.ElapsedMs)}
	if live := latestPerfSample(ctx, client); live != "" {
		section.Detail += "; " + live
	}
	if status.LastError != "" {
		section.Status = healthWarn
		section.Detail += "; last collect: " + status.LastError
	}
	return section
}

// latestPerfSample reads the newest frame off the harness:perf ring, which
// is where the live fps and long-frame counters live: HarnessPerfStatus
// counts ticks, and the percentiles only exist at HarnessPerfStop. The ring
// retains the whole run, so a subscribe-and-replay is a read of history
// rather than a wait for the next tick.
func latestPerfSample(ctx context.Context, client *harnessclient.Client) string {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	channel := string(eventchan.HarnessPerf)
	if err := client.Subscribe(readCtx, channel); err != nil {
		return ""
	}
	if err := client.Replay(readCtx, map[string]uint64{channel: 0}); err != nil {
		return ""
	}
	var newest *perfSample
	for _, event := range client.Events() {
		if event.Channel != channel || event.Gap {
			continue
		}
		var sample perfSample
		if json.Unmarshal(event.Data, &sample) != nil {
			continue
		}
		newest = &sample
	}
	if newest == nil {
		return "no sample yet"
	}
	if newest.Frontend == nil {
		return "last sample had no page answer"
	}
	return fmt.Sprintf("last sample %.1f fps, %d long frame(s), worst %.0fms, %d dom nodes",
		newest.Frontend.FPS, newest.Frontend.LongFrames, newest.Frontend.MaxFrameMs, newest.Frontend.DomNodes)
}

func rotatedSuffix(rotated bool) string {
	if rotated {
		return " (file rotated; rescanned from the start)"
	}
	return ""
}
