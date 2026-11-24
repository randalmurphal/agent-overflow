package providerupdate

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// updateTo makes `update` install version, as the provider's updater would.
func (r *rig) updateTo(version string, out Output) func(context.Context) (Output, error) {
	return func(context.Context) (Output, error) {
		r.setInstalled(ProviderClaude, version)
		return out, nil
	}
}

func TestARunThatInstallsTheUpdateSucceeds(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	r.runner.on("update", r.updateTo("2.1.325", Output{Text: "Successfully updated from 2.1.300 to version 2.1.325"}))
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatalf("Run: %v", err)
	}
	running := r.waitFrame(mark, func(statuses []Status) bool {
		return statusOf(statuses, ProviderClaude).Run.Phase == PhaseRunning
	})
	if run := statusOf(running, ProviderClaude).Run; run.FromVersion != "2.1.300" {
		t.Errorf("running = %+v", run)
	}
	status := r.waitRunSettled(mark, ProviderClaude)
	want := Run{Phase: PhaseSucceeded, FromVersion: "2.1.300", ToVersion: "2.1.325"}
	if status.Run != want {
		t.Fatalf("run = %+v, want %+v", status.Run, want)
	}
	frames := r.allFrames()
	if len(frames) < 4 {
		t.Fatalf("frames = %d, want two checks, running and the outcome at least", len(frames))
	}
	for i, frame := range frames {
		if len(frame) != 3 || frame[0].Provider != ProviderClaude || frame[1].Provider != ProviderCodex || frame[2].Provider != ProviderAgy {
			t.Fatalf("frame %d = %+v, want claude, codex, then agy", i, frame)
		}
	}
	// The codex half of a claude frame is codex's own state, not a blank.
	if last := frames[len(frames)-1]; last[1].LatestVersion != "0.169.0" {
		t.Errorf("codex in the last frame = %+v", last[1])
	}
	// The outcome frame already carries the re-check of the new binary.
	if status.InstalledVersion != "2.1.325" || status.Available || status.LatestVersion != "2.1.325" {
		t.Errorf("status = %+v, want the installed update and nothing more to offer", status)
	}
	update := r.runner.lastOf("update")
	if update.Binary != "/opt/claude" || !slices.Equal(update.Args, []string{"update"}) || update.Dir != "/home/u" ||
		update.Env.Custom["PROXY_FOR"] != ProviderClaude {
		t.Errorf("update ran %+v", update)
	}
	r.mu.Lock()
	afterRuns := r.afterRuns
	r.mu.Unlock()
	if afterRuns != 1 {
		t.Errorf("AfterRun ran %d times", afterRuns)
	}
	if err := r.svc.Run(ProviderClaude); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("Run after the update = %v, want ErrNoUpdate", err)
	}
}

func TestRunFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(*rig) func(context.Context) (Output, error)
		error  string
		output string
	}{
		{
			name: "exit 0 but unchanged",
			answer: func(r *rig) func(context.Context) (Output, error) {
				return r.updateTo("2.1.300", Output{Text: "Current version: 2.1.300\nAlready up to date"})
			},
			error:  "the update command finished, but the installed version is still 2.1.300",
			output: "Current version: 2.1.300\nAlready up to date",
		},
		{
			name: "exit 0 but unreadable",
			answer: func(r *rig) func(context.Context) (Output, error) {
				return r.updateTo("", Output{})
			},
			error: "the installed version could not be read",
		},
		{
			name: "non-zero exit",
			answer: func(*rig) func(context.Context) (Output, error) {
				return func(context.Context) (Output, error) {
					return Output{Text: "Updating...\r\n\x1b[31mError: EACCES\x1b[0m\r\n", ExitCode: 1}, nil
				}
			},
			error:  "the update command exited with status 1",
			output: "Updating...\nError: EACCES",
		},
		{
			name: "runner error",
			answer: func(*rig) func(context.Context) (Output, error) {
				return func(context.Context) (Output, error) { return Output{}, errors.New("exec: no such file") }
			},
			error: "could not run the update command: exec: no such file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.check()
			r.runner.on("update", tc.answer(r))
			mark := r.mark()
			if err := r.svc.Run(ProviderClaude); err != nil {
				t.Fatalf("Run: %v", err)
			}
			status := r.waitRunSettled(mark, ProviderClaude)
			if status.Run.Phase != PhaseFailed || !strings.Contains(status.Run.Error, tc.error) || status.Run.Output != tc.output {
				t.Fatalf("run = %+v, want failed with %q and output %q", status.Run, tc.error, tc.output)
			}
			// A run that left no readable version is re-checked like any
			// other: nothing can be offered against an unknown version.
			if r.installedVersion(ProviderClaude) == "" {
				if status.Available || status.CheckError == "" {
					t.Errorf("status = %+v, want the unreadable version reported", status)
				}
				r.setInstalled(ProviderClaude, "2.1.300")
				r.check()
			} else if !status.Available {
				// A failed run leaves the update offered, so the user can
				// try again.
				t.Errorf("status = %+v", status)
			}
			r.runner.on("update", r.updateTo("2.1.325", Output{}))
			mark = r.mark()
			if err := r.svc.Run(ProviderClaude); err != nil {
				t.Fatalf("second Run: %v", err)
			}
			if status := r.waitRunSettled(mark, ProviderClaude); status.Run.Phase != PhaseSucceeded || status.Run.Error != "" || status.Run.Output != "" {
				t.Errorf("second run = %+v", status.Run)
			}
		})
	}
}

// An update command that outlived its run may still be changing the CLI, so
// no later run of that provider may start beside it, however often the
// update is offered again. The other provider is unaffected.
func TestAnUpdaterThatSurvivedRefusesEveryLaterRun(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	r.runner.on("update", func(ctx context.Context) (Output, error) {
		return Output{Text: "Downloading..."}, errors.Join(ErrCommandSurvived, context.DeadlineExceeded)
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	status := r.waitRunSettled(mark, ProviderClaude)
	if status.Run.Phase != PhaseFailed || !strings.Contains(status.Run.Error, "could not be stopped") || !status.Available {
		t.Fatalf("status = %+v, want a failed run that still offers the update", status)
	}

	r.runner.on("update", r.updateTo("2.1.325", Output{}))
	for range 2 {
		if claude, _ := r.check(); !claude.Available {
			t.Fatalf("claude = %+v, want the update still offered", claude)
		}
		if err := r.svc.Run(ProviderClaude); !errors.Is(err, ErrUpdaterSurvived) {
			t.Fatalf("Run after a surviving updater = %v, want ErrUpdaterSurvived", err)
		}
	}
	if n := r.runner.ran("update"); n != 1 {
		t.Fatalf("update ran %d times, want only the run whose command survived", n)
	}

	r.runner.on(npmCodex.Launcher+" update", func(context.Context) (Output, error) {
		r.setInstalled(ProviderCodex, "0.169.0")
		return Output{}, nil
	})
	mark = r.mark()
	if err := r.svc.Run(ProviderCodex); err != nil {
		t.Fatalf("codex Run: %v", err)
	}
	if status := r.waitRunSettled(mark, ProviderCodex); status.Run.Phase != PhaseSucceeded {
		t.Fatalf("codex run = %+v", status.Run)
	}
}

// The run checks again before it runs anything, and a stale offer never
// reaches the updater, which installs whatever its channel holds.
func TestARunConfirmsTheOfferBeforeRunningTheUpdater(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*rig)
		error  string
		from   string
	}{
		{
			name:   "the channel moved to an older version",
			change: func(r *rig) { r.hosts.text(ClaudeChannelURL("latest"), http.StatusOK, "2.1.200") },
			error:  "2.1.200 is no longer newer than the installed 2.1.300, so nothing was updated",
			from:   "2.1.300",
		},
		{
			name:   "the installed binary caught up",
			change: func(r *rig) { r.setInstalled(ProviderClaude, "2.1.325") },
			error:  "2.1.325 is no longer newer than the installed 2.1.325, so nothing was updated",
			from:   "2.1.325",
		},
		{
			name:   "the confirming check failed",
			change: func(r *rig) { r.hosts.text(ClaudeChannelURL("latest"), http.StatusBadGateway, "down") },
			error:  "could not confirm the update before running it: ",
			from:   "2.1.300",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.check()
			tc.change(r)
			mark := r.mark()
			if err := r.svc.Run(ProviderClaude); err != nil {
				t.Fatal(err)
			}
			status := r.waitRunSettled(mark, ProviderClaude)
			if status.Run.Phase != PhaseFailed || !strings.Contains(status.Run.Error, tc.error) || status.Run.FromVersion != tc.from {
				t.Fatalf("run = %+v, want failed from %s with %q", status.Run, tc.from, tc.error)
			}
			if r.runner.ran("update") != 0 {
				t.Fatal("the updater ran on a stale offer")
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.afterRuns != 0 {
				t.Error("AfterRun ran with nothing run")
			}
		})
	}
}

// The run is judged on what its own check read, not on the offer.
func TestARunUsesTheVersionsItsCheckRead(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	r.setInstalled(ProviderClaude, "2.1.310") // updated outside the app, still behind
	r.runner.on("update", r.updateTo("2.1.325", Output{}))
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	status := r.waitRunSettled(mark, ProviderClaude)
	if want := (Run{Phase: PhaseSucceeded, FromVersion: "2.1.310", ToVersion: "2.1.325"}); status.Run != want {
		t.Fatalf("run = %+v, want %+v", status.Run, want)
	}
}

func TestRunIsOneAtATimeAndChecksStillAnswer(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	release, unblock := gate(t)
	started := make(chan struct{})
	r.runner.on("update", func(context.Context) (Output, error) {
		close(started)
		<-release
		r.setInstalled(ProviderClaude, "2.1.325")
		return Output{}, nil
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-started
	if err := r.svc.Run(ProviderClaude); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run while running = %v, want ErrRunning", err)
	}
	// Codex is a separate provider and is not held by Claude's run.
	r.runner.on(npmCodex.Launcher+" update", func(context.Context) (Output, error) { return Output{ExitCode: 1}, nil })
	if err := r.svc.Run(ProviderCodex); err != nil {
		t.Fatalf("codex Run during claude's: %v", err)
	}
	claude, _ := r.check()
	if claude.Run.Phase != PhaseRunning || !claude.Available {
		t.Errorf("a check during the run = %+v, want the run kept and the update still offered", claude)
	}
	unblock()
	if status := r.waitRunSettled(mark, ProviderClaude); status.Run.Phase != PhaseSucceeded {
		t.Fatalf("run = %+v", status.Run)
	}
	if err := r.svc.Run("gemini"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("Run(gemini) = %v", err)
	}
}

func TestStopCancelsARunningUpdateAndRefusesMore(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	started := make(chan struct{})
	r.runner.on("update", func(ctx context.Context) (Output, error) {
		close(started)
		<-ctx.Done()
		return Output{Text: "partial"}, ctx.Err()
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-started
	r.svc.Stop()
	// Stop joined the run, so its outcome is already recorded.
	claude := statusOf(r.svc.Snapshot(), ProviderClaude)
	if claude.Run.Phase != PhaseFailed || !strings.Contains(claude.Run.Error, "shutting down") || claude.Run.Output != "partial" {
		t.Fatalf("run after Stop = %+v", claude.Run)
	}
	if got := r.allFrames()[mark:]; statusOf(got[len(got)-1], ProviderClaude).Run.Phase != PhaseFailed {
		t.Errorf("the outcome was not emitted")
	}
	if err := r.svc.Run(ProviderCodex); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Run after Stop = %v, want ErrShuttingDown", err)
	}
	doctors := r.runner.ran("doctor")
	r.svc.Check()
	if r.runner.ran("doctor") != doctors {
		t.Error("Check after Stop looked a provider up")
	}
	r.svc.Stop() // repeated Stop is a no-op
}

func TestOutputTailIsBoundedOnARuneBoundary(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", runOutputLimit) + "\nthe end"
	tail := outputTail(long)
	if len(tail) > runOutputLimit || !utf8.ValidString(tail) || !strings.HasSuffix(tail, "the end") {
		t.Fatalf("tail is %d bytes, valid=%v, ends %q", len(tail), utf8.ValidString(tail), tail[max(0, len(tail)-10):])
	}
}

func TestTheLoopChecksAtStartAndOnEachTick(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	doctors := make(chan struct{}, 4)
	r.runner.on("doctor", func(ctx context.Context) (Output, error) {
		doctors <- struct{}{}
		return doctorSays("latest")(ctx)
	})
	expectCheck := func(what string) {
		t.Helper()
		select {
		case <-doctors:
		case <-time.After(10 * time.Second):
			t.Fatalf("no check %s", what)
		}
	}
	tick := func() {
		t.Helper()
		select {
		case r.ticks <- time.Now():
		case <-time.After(10 * time.Second):
			t.Fatal("the loop did not take its tick")
		}
	}
	r.svc.Start()
	r.svc.Start() // a second Start adds no loop
	expectCheck("at start")
	tick()
	expectCheck("on the first tick")
	tick()
	expectCheck("on the second tick")
	if got := r.runner.ran("doctor"); got != 3 {
		t.Fatalf("doctor ran %d times, want one per check", got)
	}

	r.svc.Stop()
	select {
	case r.ticks <- time.Now():
		t.Fatal("a tick was taken after Stop returned")
	default:
	}
	r.svc.Start()
	select {
	case r.ticks <- time.Now():
		t.Fatal("Start after Stop started a loop")
	case <-time.After(50 * time.Millisecond):
	}
}
