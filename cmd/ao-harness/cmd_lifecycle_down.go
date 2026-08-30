package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

const stopGrace = 5 * time.Second
const shutdownPollInterval = 50 * time.Millisecond

func runDown(e *env, args []string) error {
	flags := e.newFlagSet("down")
	all := flags.Bool("all", false, "stop every live instance in the registry")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("down takes no positional arguments (got %v)", rest)
	}

	type victim struct {
		id       string
		pid      int
		dataRoot string
		// Read before anything is stopped. Graceful shutdown withdraws the file.
		launcher     launcherRegistration
		bootstrap    harnessclient.Bootstrap
		hasBootstrap bool
	}
	var victims []victim
	var failures []error
	if *all {
		rows, err := e.listInstances()
		if err != nil {
			return err
		}
		live := 0
		for _, row := range rows {
			if row.Stale {
				continue
			}
			if err := row.Row.Validate(); err != nil {
				failures = append(failures, fmt.Errorf("instance %s: invalid registry identity: %w", row.ID, err))
				continue
			}
			live++
			if err := confirmInstanceRow(row.Row); err != nil {
				failures = append(failures, fmt.Errorf("instance %s: %w", row.ID, err))
				continue
			}
			bs, bsErr := bootstrapForTarget(target{ID: row.ID, DataRoot: row.DataRoot, DataDir: row.DataDir, Row: &row})
			if bsErr != nil {
				failures = append(failures, fmt.Errorf("instance %s: %w", row.ID, bsErr))
				continue
			}
			victims = append(victims, victim{id: row.ID, pid: row.PID, dataRoot: row.DataRoot, launcher: launcherRegistrationFor(bs, row.DataRoot), bootstrap: bs, hasBootstrap: true})
		}
		if live == 0 {
			if e.jsonOutput() {
				return e.writeJSON([]any{})
			}
			e.printf("no live instances\n")
			return nil
		}
	} else {
		t, err := e.resolveTarget()
		if err != nil {
			return err
		}
		if t.Row != nil {
			if err := t.Row.Validate(); err != nil {
				return fmt.Errorf("instance %s: invalid registry identity: %w", t.ID, err)
			}
		}
		pid, err := pidFor(t)
		if err != nil {
			return err
		}
		bs, err := bootstrapForTarget(t)
		if err != nil {
			return err
		}
		victims = append(victims, victim{id: t.ID, pid: pid, dataRoot: t.DataRoot, launcher: launcherRegistrationFor(bs, t.DataRoot), bootstrap: bs, hasBootstrap: true})
	}

	results := make([]map[string]any, 0, len(victims))
	for _, v := range victims {
		err := stopVictim(context.Background(), v.pid, v.bootstrap, v.hasBootstrap)
		if err == nil {
			if leaseErr := releaseDetachedHarnessLease(v.dataRoot); leaseErr != nil {
				err = leaseErr
			}
		}
		entry := map[string]any{"id": v.id, "pid": v.pid, "dataRoot": v.dataRoot, "stopped": err == nil}
		if err != nil {
			entry["error"] = err.Error()
			failures = append(failures, fmt.Errorf("instance %s (pid %d): %w", v.id, v.pid, err))
		}
		if v.launcher.valid() {
			killed, note := stopLauncherWindowVerified(v.launcher)
			entry["launcherPid"] = v.launcher.PID
			entry["launcherStopped"] = killed
			if note != "" {
				entry["launcherNote"] = note
			}
		}
		results = append(results, entry)
	}
	if e.jsonOutput() {
		if err := e.writeJSON(results); err != nil {
			return err
		}
	} else {
		for _, entry := range results {
			if entry["stopped"] == true {
				e.printf("stopped %v (pid %v)\n", entry["id"], entry["pid"])
			} else {
				e.printf("failed  %v (pid %v): %v\n", entry["id"], entry["pid"], entry["error"])
			}
			if entry["launcherStopped"] == true {
				e.printf("closed  launcher window (pid %v)\n", entry["launcherPid"])
			}
			if note, ok := entry["launcherNote"].(string); ok {
				e.printf("note    %s\n", note)
			}
		}
	}
	return errors.Join(failures...)
}

func stopVictim(ctx context.Context, pid int, bs harnessclient.Bootstrap, hasBootstrap bool) error {
	var authErr error
	if hasBootstrap {
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		client, err := harnessclient.Dial(dialCtx, bs, harnessclient.Options{})
		cancel()
		if err == nil {
			rpcCtx, rpcCancel := context.WithTimeout(ctx, stopGrace)
			_, rpcErr := client.Call(rpcCtx, "HarnessShutdown")
			rpcCancel()
			if rpcErr == nil {
				waitCtx, waitCancel := context.WithTimeout(ctx, stopGrace+10*time.Second)
				var waitErr error
				if bs.PIDNamespace != "" && bs.PIDNamespace != instanceinfo.CurrentPIDNamespace() {
					waitErr = waitForInstanceFileRemoval(waitCtx, bs.DataDir)
				} else {
					waitErr = harnessclient.WaitForExit(waitCtx, pid)
				}
				waitCancel()
				_ = client.Close()
				if waitErr != nil || (bs.PIDNamespace != "" && bs.PIDNamespace != instanceinfo.CurrentPIDNamespace()) {
					return waitErr
				}
				// Authenticated shutdown proves that the intended backend heard
				// the request. It does not prove that a child escaped its
				// process group. Reconcile the whole owned tree before releasing
				// the memory lease or allowing root reuse. With the root gone,
				// TerminateProcessVerified authorizes a surviving group member by
				// its captured birth identity, never by the recycled root PID.
				treeCtx, treeCancel := context.WithTimeout(ctx, stopGrace+10*time.Second)
				treeErr := reconcileStoppedTree(treeCtx, pid, bs)
				treeCancel()
				return treeErr
			}
			_ = client.Close()
			authErr = rpcErr
		} else {
			authErr = err
		}
	}
	err := harnessclient.TerminateProcessVerified(ctx, pid, instanceinfo.ProcessIdentity{StartTime: bs.ProcessStartTime, Executable: bs.ExecutablePath, Namespace: bs.PIDNamespace}, stopGrace)
	if err != nil && authErr != nil {
		return fmt.Errorf("authenticated shutdown failed: %v; verified process fallback failed: %w", authErr, err)
	}
	return err
}

func reconcileStoppedTree(ctx context.Context, pid int, bs harnessclient.Bootstrap) error {
	return harnessclient.TerminateProcessTreeVerified(ctx, pid, instanceinfo.ProcessIdentity{
		StartTime: bs.ProcessStartTime, Executable: bs.ExecutablePath, Namespace: bs.PIDNamespace,
	}, 0)
}

func waitForInstanceFileRemoval(ctx context.Context, dataDir string) error {
	if dataDir == "" {
		return errors.New("wait for shutdown: bootstrap has no data dir")
	}
	path := harnessclient.InstanceFilePath(dataDir)
	for {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect instance file %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for instance file %s to disappear: %w", path, ctx.Err())
		case <-time.After(shutdownPollInterval):
		}
	}
}

type launcherRegistration struct {
	PID                                                                   int
	Profile                                                               string
	StartTime, Executable, DataRoot, WebviewProfile, Namespace, BootNonce string
}

func launcherRegistrationFor(bs harnessclient.Bootstrap, dataRoot string) launcherRegistration {
	if bs.IdentityVersion != instanceinfo.IdentityVersion || bs.Mode == "" || bs.LauncherDataRoot == "" || bs.LauncherProfile != string(bs.Mode) {
		return launcherRegistration{}
	}
	got, gotErr := instanceinfo.CanonicalPath(bs.LauncherDataRoot)
	want, wantErr := instanceinfo.CanonicalPath(dataRoot)
	if gotErr != nil || wantErr != nil || got != want {
		return launcherRegistration{}
	}
	return launcherRegistration{PID: bs.LauncherPid, Profile: bs.LauncherProfile, StartTime: bs.LauncherProcessStartTime, Executable: bs.LauncherExecutablePath, DataRoot: bs.LauncherDataRoot, WebviewProfile: bs.LauncherWebviewProfile, Namespace: bs.LauncherPIDNamespace, BootNonce: bs.BootNonce}
}

func (r launcherRegistration) valid() bool {
	if r.PID <= 0 || parseStartTime(r.StartTime) <= 0 || r.Executable == "" || r.Profile == "" || r.DataRoot == "" || r.WebviewProfile == "" || r.Namespace != "windows" || r.BootNonce == "" {
		return false
	}
	if !instanceinfo.IsAbsolutePath(r.Executable) || !instanceinfo.IsAbsolutePath(r.WebviewProfile) {
		return false
	}
	switch r.Profile {
	case string(instanceinfo.ModeHarness), string(instanceinfo.ModeSoak), string(instanceinfo.ModePerf):
		return strings.EqualFold(filepath.Base(strings.ReplaceAll(r.WebviewProfile, `\`, `/`)), "webview2-"+r.Profile)
	default:
		return false
	}
}
