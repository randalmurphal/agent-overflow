package main

// `profile` is one scripted turn under the V8 sampling profiler, written
// out as a .cpuprofile plus the one split that answers the question it
// was built for: how much of a streaming turn is Svelte flushing effects
// versus marking reactions dirty on the write side.
//
// It reuses the bench drivers' page moves (`ui open`'s thread activation,
// `send --wait`'s turn wait) and adds nothing to the app's wire surface.
// What it does NOT do is reload the page. A reload is how `bench` gets a
// blank slate, and it is exactly wrong here: the recipe this encodes came
// out of a frame-drop investigation whose subject only appears on a page
// that has been alive long enough to accumulate one, and a fresh document
// profiles the mount instead.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harnessclient"
)

const (
	profileDirName = "profiles"
	// profileAttachSettleMs is the quiet window after attaching the
	// debugger and before touching the page. Attaching alone makes V8
	// deoptimize and the compositor re-tick; profiling into that is
	// profiling the instrument.
	profileAttachSettleMs = 2000
	// profileOpenSettleMs lets the opened thread finish mounting, so the
	// profile covers the TURN rather than the timeline's first paint.
	profileOpenSettleMs = 2500
	// profileSampleIntervalUs is 100µs: ten times V8's 1ms default,
	// because the frames this split is about (mark_reactions, update_effect)
	// are individually short and a 1ms interval buries them in noise.
	profileSampleIntervalUs = 100
	// profileTurnTimeout bounds the profiled turn, matching the bench.
	profileTurnTimeout = 90 * time.Second
)

func runProfile(e *env, args []string) error {
	return runProfileContext(context.Background(), e, args)
}

func runProfileContext(ctx context.Context, e *env, args []string) error {
	flags := e.newFlagSet("profile --thread <id|#N|last|title-prefix> --scenario <name> [message]")
	thread := flags.String("thread", "", "thread selector: id, #N from `threads`, `last`, or a unique title prefix")
	// No backquotes in this string: flag.PrintDefaults reads the first
	// backquoted word as the value PLACEHOLDER, so a reference to another
	// command renders as the name of this flag's argument.
	scenarioName := flags.String("scenario", "", "mock scenario to install before the turn (see 'ao-harness scenario list')")
	out := flags.String("out", "", "write the .cpuprofile here (default <dataDir>/"+profileDirName+"/profile-<timestamp>.cpuprofile)")
	cdp := bindCDPFlag(flags)
	settleMs := flags.Int("settle-ms", profileAttachSettleMs, "quiet window after attaching the debugger, before the thread is opened")
	openSettleMs := flags.Int("open-settle-ms", profileOpenSettleMs, "quiet window after opening the thread, before the profiler starts")
	intervalUs := flags.Int("interval-us", profileSampleIntervalUs, "V8 sampling interval in microseconds")
	timeout := flags.Duration("timeout", profileTurnTimeout, "how long to wait for the profiled turn to complete")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *thread == "" {
		return usagef("profile needs --thread <id|#N|last|title-prefix>")
	}
	if strings.TrimSpace(*scenarioName) == "" {
		return usagef("profile needs --scenario <name>: an unscripted turn is not a repeatable profile (see `ao-harness scenario list`)")
	}
	if *intervalUs < 1 {
		return usagef("--interval-us must be at least 1")
	}
	message := strings.TrimSpace(strings.Join(rest, " "))
	if message == "" {
		message = "profile " + *scenarioName
	}

	// The endpoint is resolved before anything is attached, so a caller who
	// named none is told which engines serve one at all rather than after a
	// connection and a settle.
	resolved, err := e.resolveTarget()
	if err != nil {
		return err
	}
	endpoint, err := resolveCDPEndpoint(*cdp, resolved)
	if err != nil {
		return err
	}

	var rollup profileRollup
	var path string
	err = e.withClient(ctx, func(client *harnessclient.Client, t target, bs harnessclient.Bootstrap) error {
		if err := requireActiveHarnessBoundary(t, bs); err != nil {
			return fmt.Errorf("profile requires an active memory watchdog: %w", err)
		}
		if err := requireHarnessProtocol(client, capabilityRequirements{
			Methods: []string{"HarnessSetScenario", "HarnessUIQuery", "HarnessEmit", "SendMessage"},
			Queries: []string{"element", "globals", "viewport"},
		}); err != nil {
			return err
		}
		// Both preconditions before anything is armed: a caller who typed a
		// bad selector or forgot the window should not learn it from a
		// half-finished profile.
		if err := probeBridge(ctx, e, client); err != nil {
			return err
		}
		row, err := resolveThreadSelector(ctx, client, *thread)
		if err != nil {
			return err
		}

		conn, page, err := attachCDP(ctx, endpoint, t, bs, e.pageID)
		if err != nil {
			return err
		}
		defer conn.Close()
		if !e.jsonOutput() {
			e.printf("profiling %s on %s\n", truncate(row.Title, 48), orDash(page.URL))
		}

		if err := sleepCtx(ctx, time.Duration(*settleMs)*time.Millisecond); err != nil {
			return err
		}
		if err := openThreadOnPage(ctx, e, client, row.ID); err != nil {
			return err
		}
		if err := sleepCtx(ctx, time.Duration(*openSettleMs)*time.Millisecond); err != nil {
			return err
		}

		raw, err := profileOneTurn(ctx, e, client, conn, profileTurn{
			threadID:   row.ID,
			scenario:   *scenarioName,
			message:    message,
			intervalUs: *intervalUs,
			timeout:    *timeout,
		})
		if err != nil {
			return err
		}

		path, err = writeCPUProfile(raw, t, *out)
		if err != nil {
			return err
		}
		doc, err := decodeCPUProfile(raw)
		if err != nil {
			// The document is already on disk, so say where: a profile that
			// this CLI cannot roll up is still one a viewer can open.
			return fmt.Errorf("%w (the raw profile is at %s)", err, path)
		}
		rollup = rollupCPUProfile(doc)
		return nil
	})
	if err != nil {
		return err
	}
	if e.jsonOutput() {
		return e.writeJSON(map[string]any{"profile": path, "rollup": rollup})
	}
	e.printf("%s", renderProfileRollup(rollup, path))
	return nil
}

// profileTurn is the scripted turn one profile covers.
type profileTurn struct {
	threadID   string
	scenario   string
	message    string
	intervalUs int
	timeout    time.Duration
}

// profileOneTurn is the recipe itself: arm the profiler, install the
// scenario, drive one turn, stop. The scenario is installed AFTER the
// profiler starts on purpose — installing it is an RPC that touches the
// page not at all, and starting the profiler first means the very first
// wire frame of the turn is already inside the sampled window.
func profileOneTurn(
	ctx context.Context,
	e *env,
	client *harnessclient.Client,
	conn *cdpclient.Conn,
	turn profileTurn,
) (json.RawMessage, error) {
	if _, err := conn.Call(ctx, "Profiler.enable", nil); err != nil {
		return nil, err
	}
	if _, err := conn.Call(ctx, "Profiler.setSamplingInterval",
		map[string]any{"interval": turn.intervalUs}); err != nil {
		return nil, err
	}
	if _, err := conn.Call(ctx, "Profiler.start", nil); err != nil {
		return nil, err
	}
	// From here every exit stops the profiler: leaving V8 sampling forever
	// is a slow page nobody can explain an hour later.
	stopped := false
	stop := func() (json.RawMessage, error) {
		if stopped {
			return nil, nil
		}
		stopped = true
		var result struct {
			Profile json.RawMessage `json:"profile"`
		}
		if err := conn.CallInto(ctx, &result, "Profiler.stop", nil); err != nil {
			return nil, err
		}
		return result.Profile, nil
	}

	if _, err := client.Call(ctx, "HarnessSetScenario", map[string]any{"name": turn.scenario}); err != nil {
		_, _ = stop()
		return nil, err
	}
	turnCtx, cancel := context.WithTimeout(ctx, turn.timeout)
	defer cancel()
	if _, err := awaitTurnAfterSend(turnCtx, client, turn.threadID, turn.message); err != nil {
		_, _ = stop()
		return nil, err
	}
	// The profiler keeps running through the reveal drain. The turn closing
	// is the WIRE finishing; the reveal queue then hands the result to the
	// reader over several more seconds, and that tail is main-thread work
	// like any other — a profile that stopped at turn completion attributed
	// none of it. A missing drain signal or a drain past the bound fails the
	// profile because the captured window would not cover what it claims.
	if err := waitForRevealDrain(ctx, e, client, benchDrainTimeout); err != nil {
		_, _ = stop()
		return nil, err
	}
	if !e.jsonOutput() {
		e.printf("turn completed and the reveal queue drained; stopping the profiler\n")
	}
	raw, err := stop()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("Profiler.stop returned no profile")
	}
	return raw, nil
}

// writeCPUProfile stores the profiler's own bytes. Not re-indented: a
// .cpuprofile is read by tooling (Chrome DevTools, speedscope), and
// pretty-printing tens of megabytes buys nothing but the file size.
func writeCPUProfile(raw json.RawMessage, t target, out string) (string, error) {
	path := out
	if path == "" {
		path = filepath.Join(t.DataDir, profileDirName,
			fmt.Sprintf("profile-%s.cpuprofile", time.Now().Format("20060102-150405")))
	}
	if err := atomicfile.Write(path, append([]byte(raw), '\n')); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
