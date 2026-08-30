package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/harnessclient"
)

func monitorList(e *env, args []string) error {
	flags := e.newFlagSet("monitor list")
	asJSON := e.bindJSONFlag(flags)
	budget := e.bindQueryOutputBudget(flags)
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *asJSON {
		e.format = "json"
	}
	if len(rest) != 0 {
		return usagef("monitor list takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := requireHarnessProtocol(client, monitorRequirements()); err != nil {
			return err
		}
		raw, err := monitorQuery(ctx, e, client, "list", &monitorOptions{}, nil)
		if err != nil {
			return err
		}
		if e.jsonOutput() || budget.file != "" || budget.full {
			return e.writeQueryJSON(raw, budget)
		}
		if budget.maxBytes < 1 {
			return usagef("--max-bytes must be positive (or use --full / --file)")
		}
		if int64(len(raw)) > budget.maxBytes {
			return usagef("monitor list result is %d bytes, over --max-bytes %d; use --full or --file", len(raw), budget.maxBytes)
		}
		var result struct {
			V        int `json:"v"`
			Monitors []struct {
				ID               string   `json:"id"`
				Title            string   `json:"title"`
				CompatibilityLeg string   `json:"compatibilityLeg"`
				Perturbation     string   `json:"perturbation"`
				Requires         []string `json:"requires"`
			} `json:"monitors"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode monitor catalog: %w", err)
		}
		if result.V != 1 {
			return fmt.Errorf("monitor catalog has unsupported version %d", result.V)
		}
		for _, monitor := range result.Monitors {
			e.printf("%-30s %-22s %-22s %s\n", monitor.ID, monitor.CompatibilityLeg, monitor.Perturbation, strings.Join(monitor.Requires, ","))
		}
		return nil
	})
}

// writeMonitorResult keeps monitor output useful in a terminal without
// dumping bounded observation payloads. --full and --file retain the exact
// typed result for machine inspection.
func (e *env) writeMonitorResult(raw json.RawMessage, budget *queryOutputBudget, op string) error {
	if e.jsonOutput() || (budget != nil && (budget.file != "" || budget.full)) {
		return e.writeQueryJSON(raw, budget)
	}
	if budget != nil && budget.maxBytes < 1 {
		return usagef("--max-bytes must be positive (or use --full / --file)")
	}
	if budget != nil && int64(len(raw)) > budget.maxBytes {
		return usagef("monitor %s result is %d bytes, over --max-bytes %d; use --full or --file", op, len(raw), budget.maxBytes)
	}
	var result struct {
		V      int    `json:"v"`
		RunID  string `json:"runId"`
		Status string `json:"status"`
		Runs   []struct {
			RunID    string `json:"runId"`
			Status   string `json:"status"`
			Monitors []any  `json:"monitors"`
		} `json:"runs"`
		Monitors   []any `json:"monitors"`
		Heartbeats int   `json:"heartbeats"`
		Partial    bool  `json:"partial"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("decode monitor %s result: %w", op, err)
	}
	if result.V != 1 {
		return fmt.Errorf("monitor %s result has unsupported version %d", op, result.V)
	}
	if op != "last" && result.RunID == "" {
		return fmt.Errorf("monitor %s result has no runId", op)
	}
	if op == "start" && len(result.Monitors) == 0 {
		return fmt.Errorf("monitor start result has no monitors")
	}
	switch op {
	case "last":
		e.printf("monitor last: %d retained run(s)\n", len(result.Runs))
		for _, run := range result.Runs {
			e.printf("  %s  %s  monitors=%d\n", run.RunID, run.Status, len(run.Monitors))
		}
	case "start":
		e.printf("monitor run %s started (%d monitor(s))\n", orDash(result.RunID), len(result.Monitors))
	case "heartbeat":
		e.printf("monitor heartbeat %s  count=%d  partial=%t\n", orDash(result.RunID), result.Heartbeats, result.Partial)
	default:
		e.printf("monitor %s %s  monitors=%d", op, orDash(result.RunID), len(result.Monitors))
		if result.Status != "" {
			e.printf("  status=%s", result.Status)
		}
		if result.Partial {
			e.printf("  partial=true")
		}
		e.printf("\n")
	}
	return nil
}
