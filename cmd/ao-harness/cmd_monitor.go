package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"

	"agent-overflow/internal/harnessclient"
)

var monitorSubcommands = commandNames(monitorCommandDescriptors())

func runMonitor(e *env, args []string) error {
	if done, err := groupHelp(e, "monitor", args, monitorSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("monitor needs a subcommand: %s", strings.Join(monitorSubcommands, ", "))
	}
	switch args[0] {
	case "list":
		return monitorList(e, args[1:])
	case "start":
		return monitorStart(e, args[1:])
	case "heartbeat":
		return monitorHeartbeat(e, args[1:])
	case "overlap":
		return monitorOverlap(e, args[1:])
	case "status", "collect":
		return monitorCollect(e, args[1:], args[0] == "status")
	case "stop", "cleanup":
		return monitorStop(e, args[1:], args[0] == "cleanup")
	case "last":
		return monitorLast(e, args[1:])
	default:
		return usagef("unknown monitor subcommand %q (want %s)", args[0], strings.Join(monitorSubcommands, ", "))
	}
}

// monitorOptions is shared by every monitor verb. A monitor operation is a
// page query, so the output budget applies to both JSON and text summaries.
// The page id is resolved once per invocation and placed in every request.
type monitorOptions struct {
	asJSON     *bool
	budget     *queryOutputBudget
	runID      string
	monitor    stringList
	leg        string
	atMs       float64
	hasAtMs    bool
	timeout    float64
	hasTimeout bool
}

var monitorIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func validMonitorRunID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func bindMonitorFlags(e *env, name string, allowMonitors bool) (*monitorOptions, *flag.FlagSet) {
	flags := e.newFlagSet(name)
	opts := &monitorOptions{asJSON: e.bindJSONFlag(flags), budget: e.bindQueryOutputBudget(flags)}
	flags.StringVar(&opts.runID, "run-id", "", "monitor run id")
	flags.Float64Var(&opts.atMs, "at-ms", -1, "explicit non-negative page timestamp")
	flags.StringVar(&opts.leg, "leg", "", "compatibility leg required by selected monitors")
	if allowMonitors {
		flags.Var(&opts.monitor, "monitor", "monitor ID to arm (repeatable; positional IDs are also accepted)")
		flags.Float64Var(&opts.timeout, "heartbeat-timeout-ms", 0, "heartbeat gap before a run is partial (default 3000)")
	}
	return opts, flags
}

func (o *monitorOptions) finish(e *env, rest []string) ([]string, error) {
	if *o.asJSON {
		e.format = "json"
	}
	if o.hasAtMs && (!finiteNonNegative(o.atMs)) {
		return nil, usagef("--at-ms must be a finite non-negative number")
	}
	if o.hasTimeout && (!finiteNonNegative(o.timeout) || o.timeout <= 0) {
		return nil, usagef("--heartbeat-timeout-ms must be positive")
	}
	if o.leg != "" && !validMonitorLeg(o.leg) {
		return nil, usagef("unknown monitor compatibility leg %q", o.leg)
	}
	if o.runID != "" && !validMonitorRunID(o.runID) {
		return nil, usagef("--run-id must be a non-empty identifier without whitespace or control characters (max 128 bytes)")
	}
	return rest, nil
}

func noteMonitorFlags(opts *monitorOptions, flags *flag.FlagSet) {
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "at-ms":
			opts.hasAtMs = true
		case "heartbeat-timeout-ms":
			opts.hasTimeout = true
		}
	})
}

func finiteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validMonitorLeg(value string) bool {
	switch value {
	case "clean-renderer", "instrumented-renderer", "functional", "provider-source", "platform":
		return true
	default:
		return false
	}
}

// monitorPageID refuses to guess between attached pages. Explicit page IDs
// are checked against HarnessInfo too, which turns a stale page selector into
// a clear error before the mutating operation reaches the bridge.
func (e *env) monitorPageID(ctx context.Context, client *harnessclient.Client) (string, error) {
	info, err := client.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("monitor page preflight: %w", err)
	}
	return selectMonitorPage(info.FrontendPages, e.pageID, info.DataRoot)
}

func selectMonitorPage(pages []harnessclient.HarnessPageIdentity, selected, dataRoot string) (string, error) {
	want := strings.TrimSpace(selected)
	if want != "" {
		for _, page := range pages {
			if page.PageID == want {
				if page.Marker == "" || page.Origin == "" {
					return "", fmt.Errorf("monitor page %q has incomplete ownership identity", want)
				}
				return want, nil
			}
		}
		return "", fmt.Errorf("monitor page %q is not attached to harness %s (attached pages: %s)", want, dataRoot, pageList(pages))
	}
	if len(pages) != 1 {
		return "", fmt.Errorf("monitor requires exactly one attached frontend page, found %d; pass --page-id <id> (use `ao-harness info`)", len(pages))
	}
	page := pages[0]
	if page.PageID == "" || page.Marker == "" || page.Origin == "" {
		return "", errors.New("monitor page preflight found an attached page without a complete ownership identity")
	}
	return page.PageID, nil
}

func pageList(pages []harnessclient.HarnessPageIdentity) string {
	if len(pages) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(pages))
	for _, page := range pages {
		ids = append(ids, page.PageID)
	}
	return strings.Join(ids, ", ")
}

func monitorQuery(ctx context.Context, e *env, client *harnessclient.Client, op string, opts *monitorOptions, extra map[string]any) (json.RawMessage, error) {
	pageID, err := e.monitorPageID(ctx, client)
	if err != nil {
		return nil, err
	}
	spec := map[string]any{"kind": "monitor", "op": op, "pageId": pageID}
	for key, value := range extra {
		spec[key] = value
	}
	return e.queryUI(ctx, client, spec)
}

func monitorRequirements() capabilityRequirements {
	return capabilityRequirements{Methods: []string{"HarnessInfo", "HarnessUIQuery"}, Queries: []string{"monitor"}}
}

func monitorStart(e *env, args []string) error {
	opts, flags := bindMonitorFlags(e, "monitor start", true)
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	noteMonitorFlags(opts, flags)
	o, err := opts.finish(e, rest)
	if err != nil {
		return err
	}
	for _, id := range o {
		if !monitorIDPattern.MatchString(id) {
			return usagef("monitor ID %q must be a non-empty kebab-case identifier", id)
		}
		opts.monitor = append(opts.monitor, id)
	}
	if len(opts.monitor) == 0 {
		return usagef("monitor start needs at least one --monitor ID")
	}
	seen := make(map[string]struct{}, len(opts.monitor))
	for _, id := range opts.monitor {
		if _, exists := seen[id]; exists {
			return usagef("monitor ID %q was supplied more than once", id)
		}
		seen[id] = struct{}{}
	}
	ids := append([]string(nil), opts.monitor...)
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, monitorRequirements()); err != nil {
			return err
		}
		extra := map[string]any{"monitorIds": ids}
		if opts.runID != "" {
			extra["runId"] = opts.runID
		}
		if opts.leg != "" {
			extra["compatibilityLeg"] = opts.leg
		}
		if opts.hasAtMs {
			extra["atMs"] = opts.atMs
		}
		if opts.timeout != 0 {
			extra["heartbeatTimeoutMs"] = opts.timeout
		}
		raw, err := monitorQuery(ctx, e, client, "start", opts, extra)
		if err != nil {
			return err
		}
		return e.writeMonitorResult(raw, opts.budget, "start")
	})
}

func monitorRunIDAndOptions(e *env, args []string, name string) (*monitorOptions, []string, error) {
	opts, flags := bindMonitorFlags(e, name, false)
	rest, err := e.parse(flags, args)
	if err != nil {
		return nil, nil, err
	}
	noteMonitorFlags(opts, flags)
	if len(rest) == 1 && opts.runID == "" {
		opts.runID = rest[0]
		rest = nil
	}
	if len(rest) != 0 {
		return nil, nil, usagef("%s takes one run ID (or --run-id), got %v", name, rest)
	}
	if opts.runID == "" {
		return nil, nil, usagef("%s needs --run-id <id>", name)
	}
	if _, err := opts.finish(e, nil); err != nil {
		return nil, nil, err
	}
	return opts, nil, nil
}

func monitorHeartbeat(e *env, args []string) error {
	opts, _, err := monitorRunIDAndOptions(e, args, "monitor heartbeat")
	if err != nil {
		return err
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, monitorRequirements()); err != nil {
			return err
		}
		extra := map[string]any{"runId": opts.runID}
		if opts.hasAtMs {
			extra["atMs"] = opts.atMs
		}
		raw, err := monitorQuery(ctx, e, client, "heartbeat", opts, extra)
		if err != nil {
			return err
		}
		return e.writeMonitorResult(raw, opts.budget, "heartbeat")
	})
}

func monitorOverlap(e *env, args []string) error {
	flags := e.newFlagSet("monitor overlap <run-id> --with-run-id <run-id>")
	opts := &monitorOptions{asJSON: e.bindJSONFlag(flags), budget: e.bindQueryOutputBudget(flags)}
	flags.StringVar(&opts.runID, "run-id", "", "first live monitor run id")
	withRunID := flags.String("with-run-id", "", "second live monitor run id")
	flags.Float64Var(&opts.atMs, "at-ms", -1, "explicit non-negative page timestamp")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	noteMonitorFlags(opts, flags)
	if len(rest) > 2 {
		return usagef("monitor overlap takes at most two run IDs")
	}
	if len(rest) >= 1 && opts.runID == "" {
		opts.runID = rest[0]
	}
	if len(rest) == 2 && *withRunID == "" {
		*withRunID = rest[1]
	}
	if !validMonitorRunID(opts.runID) || !validMonitorRunID(*withRunID) {
		return usagef("monitor overlap needs two non-empty run IDs (use <run-id> --with-run-id <run-id>)")
	}
	if opts.runID == *withRunID {
		return usagef("monitor overlap needs two different run IDs")
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, monitorRequirements()); err != nil {
			return err
		}
		extra := map[string]any{"runId": opts.runID, "withRunId": *withRunID}
		if opts.hasAtMs {
			extra["atMs"] = opts.atMs
		}
		raw, err := monitorQuery(ctx, e, client, "overlap", opts, extra)
		if err != nil {
			return err
		}
		if e.jsonOutput() || opts.budget.file != "" || opts.budget.full {
			return e.writeQueryJSON(raw, opts.budget)
		}
		if opts.budget.maxBytes < 1 || int64(len(raw)) > opts.budget.maxBytes {
			return usagef("monitor overlap result exceeds --max-bytes; use --full or --file")
		}
		var result struct {
			V         int     `json:"v"`
			RunID     string  `json:"runId"`
			WithRunID string  `json:"withRunId"`
			AtMs      float64 `json:"atMs"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode monitor overlap result: %w", err)
		}
		if result.V != 1 || result.RunID == "" || result.WithRunID == "" {
			return fmt.Errorf("monitor overlap result is malformed")
		}
		e.printf("monitor overlap %s with %s at %.3fms\n", result.RunID, result.WithRunID, result.AtMs)
		return nil
	})
}

func monitorCollect(e *env, args []string, status bool) error {
	name := "monitor collect"
	op := "collect"
	if status {
		name, op = "monitor status", "collect"
	}
	opts, _, err := monitorRunIDAndOptions(e, args, name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, monitorRequirements()); err != nil {
			return err
		}
		extra := map[string]any{"runId": opts.runID}
		if opts.hasAtMs {
			extra["atMs"] = opts.atMs
		}
		raw, err := monitorQuery(ctx, e, client, op, opts, extra)
		if err != nil {
			return err
		}
		displayOp := op
		if status {
			displayOp = "status"
		}
		return e.writeMonitorResult(raw, opts.budget, displayOp)
	})
}

func monitorStop(e *env, args []string, cleanup bool) error {
	name := "monitor stop"
	if cleanup {
		name = "monitor cleanup"
	}
	opts, _, err := monitorRunIDAndOptions(e, args, name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, monitorRequirements()); err != nil {
			return err
		}
		extra := map[string]any{"runId": opts.runID}
		if opts.hasAtMs {
			extra["atMs"] = opts.atMs
		}
		raw, err := monitorQuery(ctx, e, client, "stop", opts, extra)
		if err != nil {
			return err
		}
		return e.writeMonitorResult(raw, opts.budget, "stop")
	})
}

func monitorLast(e *env, args []string) error {
	flags := e.newFlagSet("monitor last")
	opts := &monitorOptions{asJSON: e.bindJSONFlag(flags), budget: e.bindQueryOutputBudget(flags)}
	flags.StringVar(&opts.runID, "run-id", "", "only this retained monitor run")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef("monitor last takes at most one run ID")
	}
	if len(rest) == 1 {
		opts.runID = rest[0]
	}
	noteMonitorFlags(opts, flags)
	if opts.runID != "" && !validMonitorRunID(opts.runID) {
		return usagef("run ID %q must be a non-empty identifier without whitespace or control characters", opts.runID)
	}
	if *opts.asJSON {
		e.format = "json"
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, monitorRequirements()); err != nil {
			return err
		}
		extra := map[string]any{}
		if opts.runID != "" {
			extra["runId"] = opts.runID
		}
		raw, err := monitorQuery(ctx, e, client, "last", opts, extra)
		if err != nil {
			return err
		}
		return e.writeMonitorResult(raw, opts.budget, "last")
	})
}
