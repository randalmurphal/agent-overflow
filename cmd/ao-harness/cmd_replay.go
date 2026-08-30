package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/harness"
	"agent-overflow/internal/harnessclient"
)

var recordSubcommands = commandNames(recordCommandDescriptors())

func runRecord(e *env, args []string) error {
	if done, err := groupHelp(e, "record", args, recordSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("record needs a subcommand: %s", strings.Join(recordSubcommands, ", "))
	}
	switch args[0] {
	case "start":
		return recordStart(e, args[1:])
	case "stop":
		return recordStop(e, args[1:])
	default:
		return usagef("unknown record subcommand %q (want %s)", args[0], strings.Join(recordSubcommands, ", "))
	}
}

// bundleMeta is one row of HarnessListBundles, and also what record
// start/stop answer with. Typed here rather than left raw because this
// CLI FORMATS its timestamp and COMPARES its name (the "no such bundle"
// listing), which is the bar for typing a result at all.
type bundleMeta struct {
	Name       string `json:"name"`
	ThreadID   string `json:"threadId"`
	CreatedAt  int64  `json:"createdAt"`
	EventCount int    `json:"eventCount"`
}

// bundleTime renders the backend's UnixMilli stamp the way `list` and
// `info` render theirs. A raw epoch in a table is a number the reader has
// to paste into another tool before it says anything.
func bundleTime(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}

func recordStart(e *env, args []string) error {
	flags := e.newFlagSet("record start <bundle-name>")
	thread := flags.String("thread", "", "thread whose event log is captured")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		return usagef("record start needs a bundle name")
	}
	if *thread == "" {
		return usagef("record start needs --thread <id> (the snapshot/event boundary is per thread)")
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{"HarnessRecordStart"}}); err != nil {
			return err
		}
		result, err := client.Call(ctx, "HarnessRecordStart", rest[0], *thread)
		if err != nil {
			return err
		}
		return e.printBundleMeta("recording", result)
	})
}

func recordStop(e *env, args []string) error {
	flags := e.newFlagSet("record stop")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("record stop takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{"HarnessRecordStop"}}); err != nil {
			return err
		}
		result, err := client.Call(ctx, "HarnessRecordStop")
		if err != nil {
			return err
		}
		return e.printBundleMeta("recorded", result)
	})
}

// printBundleMeta is the -o text form of the two record verbs, which used
// to print the raw result document in both formats. -o json stays exactly
// the server's bytes.
func (e *env) printBundleMeta(verb string, raw json.RawMessage) error {
	if e.jsonOutput() {
		return e.writeRawJSON(raw)
	}
	var meta bundleMeta
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Name == "" {
		return e.writeRawJSON(raw)
	}
	e.printf("%s %s (thread %s, %d event(s))\n", verb, meta.Name, orDash(meta.ThreadID), meta.EventCount)
	return nil
}

func runBundles(e *env, args []string) error {
	flags := e.newFlagSet("bundles")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("bundles takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		raw, err := client.Call(ctx, "HarnessListBundles")
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		bundles, err := decodeBundles(raw)
		if err != nil {
			return err
		}
		if len(bundles) == 0 {
			e.printf("no bundles\n")
			return nil
		}
		rows := make([][]string, 0, len(bundles))
		for _, bundle := range bundles {
			rows = append(rows, []string{bundle.Name, bundle.ThreadID, fmt.Sprint(bundle.EventCount), bundleTime(bundle.CreatedAt)})
		}
		return e.table([]string{"NAME", "THREAD", "EVENTS", "CREATED"}, rows)
	})
}

func decodeBundles(raw json.RawMessage) ([]bundleMeta, error) {
	var bundles []bundleMeta
	if err := json.Unmarshal(raw, &bundles); err != nil {
		return nil, fmt.Errorf("decode bundles: %w", err)
	}
	return bundles, nil
}

var replaySubcommands = commandNames(replayCommandDescriptors())

func runReplay(e *env, args []string) error {
	if done, err := groupHelp(e, "replay", args, replaySubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("replay needs a subcommand: %s", strings.Join(replaySubcommands, ", "))
	}
	switch args[0] {
	case "bundle":
		return replayStart(e, "HarnessReplayBundle", "replay bundle", "<bundle-name>", "bundle name", args[1:])
	case "file":
		return replayStart(e, "HarnessReplayStart", "replay file", "<event-log.jsonl>", "event-log path", args[1:])
	case "pause", "resume", "step", "stop", "status":
		return replayControl(e, args[0], args[1:])
	default:
		return usagef("unknown replay subcommand %q (want %s)", args[0], strings.Join(replaySubcommands, ", "))
	}
}

// replayStart drives the two entry points that take a source plus
// options. They differ only in what the first argument names — a saved
// bundle (which also restores its DB snapshot) or a raw event log
// (which replays events over whatever state is live).
func replayStart(e *env, method, command, positional, argName string, args []string) error {
	flags := e.newFlagSet(command + " " + positional)
	speed := flags.Float64("speed", 0, "playback rate multiplier (0 = recorded speed)")
	maxGapMs := flags.Int("max-gap-ms", 0, "cap any recorded gap in ms (0 = the server default, negative = uncapped)")
	startPaused := flags.Bool("start-paused", false, "begin paused so `replay step` releases one event at a time")
	threadFilter := flags.String("thread-filter", "", "drop records for other threads")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		return usagef("%s needs a %s", command, argName)
	}
	opts := harness.ReplayOptions{
		Speed:        *speed,
		MaxGapMs:     *maxGapMs,
		StartPaused:  *startPaused,
		ThreadFilter: *threadFilter,
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{method}}); err != nil {
			return err
		}
		if method == "HarnessReplayBundle" {
			if err := refuseUnknownBundle(ctx, client, rest[0]); err != nil {
				return err
			}
		}
		result, err := client.Call(ctx, method, rest[0], opts)
		if err != nil {
			return err
		}
		return e.printReplayStatus(result)
	})
}

// refuseUnknownBundle turns a mistyped bundle name into the list of names
// that exist, the way `scenario set --name` already answers. The backend's
// own error names a PATH inside its data dir, which reads as a filesystem
// problem rather than "you meant one of these three".
//
// A listing that itself fails is not fatal: fall through and let the
// replay call produce whatever error it produces, since the point here is
// a better message, not a second gate.
func refuseUnknownBundle(ctx context.Context, client *harnessclient.Client, name string) error {
	raw, err := client.Call(ctx, "HarnessListBundles")
	if err != nil {
		return nil
	}
	bundles, err := decodeBundles(raw)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(bundles))
	for _, bundle := range bundles {
		if bundle.Name == name {
			return nil
		}
		names = append(names, bundle.Name)
	}
	if len(names) == 0 {
		return fmt.Errorf("no such bundle %q — this instance has recorded none (`ao-harness record start <name> --thread <id>`)", name)
	}
	return fmt.Errorf("no such bundle %q (have: %s)", name, strings.Join(names, ", "))
}

func replayControl(e *env, verb string, args []string) error {
	flags := e.newFlagSet("replay " + verb)
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("replay %s takes no positional arguments (got %v)", verb, rest)
	}
	method := map[string]string{
		"pause":  "HarnessReplayPause",
		"resume": "HarnessReplayResume",
		"step":   "HarnessReplayStep",
		"stop":   "HarnessReplayStop",
		"status": "HarnessReplayStatus",
	}[verb]
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if verb != "status" {
			method := map[string]string{
				"pause": "HarnessReplayPause", "resume": "HarnessReplayResume",
				"step": "HarnessReplayStep", "stop": "HarnessReplayStop",
			}[verb]
			if err := requireHarnessProtocol(client, capabilityRequirements{Methods: []string{method}}); err != nil {
				return err
			}
		}
		result, err := client.Call(ctx, method)
		if err != nil {
			return err
		}
		return e.printReplayStatus(result)
	})
}

func (e *env) printReplayStatus(raw json.RawMessage) error {
	if e.jsonOutput() {
		return e.writeRawJSON(raw)
	}
	var status harness.ReplayStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return e.writeRawJSON(raw)
	}
	e.printf("%s %d/%d", status.State, status.Position, status.Total)
	if status.File != "" {
		e.printf(" %s", status.File)
	}
	if status.Error != "" {
		e.printf(" (%s)", status.Error)
	}
	e.printf("\n")
	return nil
}
