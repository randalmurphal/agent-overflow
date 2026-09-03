package main

import (
	"context"
	"path/filepath"

	"agent-overflow/internal/harnessclient"
)

func runInfo(e *env, args []string) error {
	flags := e.newFlagSet("info")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("info takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, t target, bs harnessclient.Bootstrap) error {
		info, err := client.Info(ctx)
		if err != nil {
			return err
		}
		stderrPath := filepath.Join(info.DataDir, logDirName, backendStderrLog)
		// The url this reports is one a reader is expected to open, so it
		// carries a fresh ticket rather than the spent boot one.
		pageURL := pageURLForTarget(ctx, e, bs)
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"id": t.ID, "mode": bs.Mode, "window": bs.Window, "worktree": bs.Worktree, "startedAt": bs.StartedAt, "url": pageURL, "port": bs.Port, "info": info, "backendStderr": stderrPath})
		}
		e.printf("instance %s (%s%s)\n", t.ID, orUnknown(string(bs.Mode)), windowSuffix(bs.Window))
		e.printf("  version   %s\n", info.Version)
		e.printf("  pid       %d\n", info.PID)
		e.printf("  url       %s\n", pageURL)
		e.printf("  worktree  %s\n", orUnknown(bs.Worktree))
		e.printf("  started   %s\n", orUnknown(bs.StartedAt))
		e.printf("  data root %s\n", info.DataRoot)
		e.printf("  data dir  %s\n", info.DataDir)
		e.printf("  home      %s\n", orUnknown(info.HomeDir))
		e.printf("  mock      %s\n", info.MockProvider)
		e.printf("  db        %s\n", info.DBPath)
		e.printf("  events    %s\n", info.EventLogDir)
		e.printf("  ui trace  %s\n", info.UITracePath)
		e.printf("  fe errors %s\n", info.FrontendErrorsPath)
		e.printf("  backend   %s\n", stderrPath)
		return nil
	})
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
