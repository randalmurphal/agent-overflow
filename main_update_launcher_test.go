//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/wsllauncher"
)

// The Windows launcher's update sequence against the real update commands:
// wsllauncher.UpdateSequence and its UpdateCommandRunner drive this test
// binary, re-executed through main(), behind a fake wsl.exe. It proves the
// argv the launcher sends is the argv the backend accepts, and that each
// outcome the backend reports lands the record, the database and the
// payload where the launcher expects them.

type launcherE2E struct {
	t        *testing.T
	dataRoot string
	stub     string
	mu       sync.Mutex
	// payloads maps each update command to the payload that ran it.
	payloads map[string][]string
	calls    []string
}

// wsl is the fake wsl.exe. A payload's update command runs as this test
// binary with the data root the test owns; anything else runs as given.
func (e *launcherE2E) wsl(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name != "wsl.exe" || len(args) < 4 || args[0] != "-d" || args[1] != "Ubuntu" || args[2] != "--exec" {
		e.t.Errorf("unexpected wsl.exe argv %q %q", name, args)
		return exec.CommandContext(ctx, "false")
	}
	program, rest := args[3], args[4:]
	if len(rest) == 0 || !strings.HasPrefix(rest[0], "__update-") {
		return exec.CommandContext(ctx, program, rest...)
	}
	e.mu.Lock()
	e.payloads[rest[0]] = append(e.payloads[rest[0]], program)
	e.mu.Unlock()
	executable, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	encoded, err := json.Marshal(append(append([]string(nil), rest...), "--data-dir", e.dataRoot))
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestUpdateCommandProcess$")
	cmd.Env = append(os.Environ(), updateCommandArgsEnv+"="+string(encoded), updateTrialStubEnv+"="+e.stub)
	return cmd
}

func (e *launcherE2E) note(call string) {
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()
}

type launcherE2EHost struct {
	wsllauncher.UpdatePayloads
	e *launcherE2E
}

func (h launcherE2EHost) HostFreeBytes(string) (uint64, bool) { return 0, false }
func (h launcherE2EHost) InvalidatePayloadRecord(supervise.LauncherRecord) error {
	h.e.note("invalidate")
	return nil
}
func (h launcherE2EHost) RecordPayload(supervise.LauncherRecord) error {
	h.e.note("record")
	return nil
}
func (h launcherE2EHost) PublishLauncher(supervise.LauncherRecord) error {
	h.e.note("publish")
	return nil
}
func (h launcherE2EHost) RemoveLauncherResidue(supervise.LauncherRecord) error {
	h.e.note("residue")
	return nil
}

func TestLauncherUpdateSequenceRunsTheRealCommands(t *testing.T) {
	kerneltest.IsolateSpawns(t)
	for _, tc := range []struct {
		name     string
		stub     string
		state    supervise.UpdateState
		reason   string
		database string
		stable   string
		calls    []string
		// next is the fingerprint of the launcher that starts afterwards,
		// and updatingTo what it tells its backend.
		next       string
		updatingTo string
	}{
		{"prepared commits", "prepare", supervise.UpdateCommitted, "", "trial", "new", []string{"invalidate", "record", "publish"},
			"target", "2.0.0"},
		{"failed rolls back", "fail", supervise.UpdateRolledBack,
			"database schema 90 is newer than this build knows (88)", "live", "old", nil,
			"previous", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &launcherE2E{t: t, dataRoot: t.TempDir(), stub: tc.stub, payloads: map[string][]string{}}
			dir := filepath.Join(e.dataRoot, appdirs.DirName)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(dir, "agent-overflow.db")
			if err := os.WriteFile(db, []byte("live"), 0o600); err != nil {
				t.Fatal(err)
			}

			id, err := wsllauncher.NewUpdateID()
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			stable := filepath.Join(bin, "agent-overflow")
			staged := wsllauncher.StagedPayloadPath(stable, id)
			for path, contents := range map[string]string{stable: "old", staged: "new"} {
				if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			config := t.TempDir()
			recordPath := supervise.LauncherRecordPath(config, "prod", "Ubuntu")
			if _, err := wsllauncher.BeginLauncherUpdate(recordPath, supervise.LauncherRecord{
				Distro: "Ubuntu", StablePayload: stable, StagedPayload: staged,
				StagedLauncher: wsllauncher.StagedLauncherPath(config, id),
				InstallPath:    filepath.Join(config, "agent-overflow.exe"), TargetFingerprint: "target",
			}, "1.0.0", "2.0.0", id, time.Now()); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			var details []string
			sequence := wsllauncher.UpdateSequence{
				RecordPath: recordPath,
				Host: launcherE2EHost{
					UpdatePayloads: wsllauncher.UpdatePayloads{Runner: wsllauncher.UpdateCommandRunner{Command: e.wsl, Logf: t.Logf}},
					e:              e,
				},
				Progress: func(p startupprogress.Progress) {
					mu.Lock()
					details = append(details, p.Detail)
					mu.Unlock()
				},
				Now:  time.Now,
				Logf: t.Logf,
			}
			end, err := sequence.Apply(t.Context(), id)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if end.State != tc.state || end.Reason != tc.reason {
				t.Fatalf("Apply = %+v, want %s %q", end, tc.state, tc.reason)
			}

			record, found, err := supervise.LoadLauncherRecord(recordPath)
			if err != nil || !found {
				t.Fatalf("record after Apply: found=%v err=%v", found, err)
			}
			if record.Update.State != tc.state || record.Update.Reason != tc.reason || record.Update.Attempts != 1 {
				t.Fatalf("record = %+v", record.Update)
			}
			if got, err := os.ReadFile(db); err != nil || string(got) != tc.database {
				t.Fatalf("database = %q (%v), want %q", got, err, tc.database)
			}
			if got, err := os.ReadFile(stable); err != nil || string(got) != tc.stable {
				t.Fatalf("stable payload = %q (%v), want %q", got, err, tc.stable)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("the staged payload survived: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "runtime", "app-update", "snapshot")); !os.IsNotExist(err) {
				t.Fatalf("the snapshot survived the settled update: %v", err)
			}

			e.mu.Lock()
			// The target's own commands run the snapshot and the trial; the
			// discard after the outcome runs through the version that will
			// keep running.
			wantPayloads := map[string][]string{
				supervise.UpdateSnapshotCommand: {staged},
				supervise.UpdateTrialRunCommand: {staged},
				supervise.UpdateDiscardCommand:  {stable},
			}
			if tc.state == supervise.UpdateCommitted {
				wantPayloads[supervise.UpdateDiscardCommand] = []string{staged}
			}
			for command, want := range wantPayloads {
				if got := e.payloads[command]; strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s ran through %q, want %q", command, got, want)
				}
			}
			if got := e.payloads[supervise.UpdateRestoreCommand]; len(got) != 0 {
				t.Errorf("restore ran through %q; the trial command restores its own failure", got)
			}
			if strings.Join(e.calls, ",") != strings.Join(tc.calls, ",") {
				t.Errorf("launcher steps = %q, want %q", e.calls, tc.calls)
			}
			e.mu.Unlock()
			mu.Lock()
			if !strings.Contains(strings.Join(details, "\n"), "Applying migration 1 of 1") {
				t.Errorf("the trial's progress never reached the launcher: %q", details)
			}
			mu.Unlock()

			// The launcher that starts next reads the record, and only the
			// first launch of a committed target tells its backend it
			// finishes the update; the backend accepts what it is told.
			decision, err := sequence.Reconcile(t.Context(), tc.next)
			if err != nil || decision.Action != wsllauncher.ReconcileLaunch {
				t.Fatalf("Reconcile = %+v, %v", decision, err)
			}
			flags, err := parseFlags(append([]string{"--print-url-fd", "0"}, wsllauncher.UpdatingToArgs(decision.UpdatingTo)...))
			if err != nil || flags.updatingTo != tc.updatingTo {
				t.Fatalf("the backend was told it finishes %q (%v), want %q", flags.updatingTo, err, tc.updatingTo)
			}
			again, err := sequence.Reconcile(t.Context(), tc.next)
			if err != nil || again.UpdatingTo != "" {
				t.Fatalf("a later launch = %+v, %v, want it to finish nothing", again, err)
			}
		})
	}
}
