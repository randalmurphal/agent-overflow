//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/supervise"
)

var phaseLineRE = regexp.MustCompile(`: phase=(\S+) duration=(\S+)`)

// phaseCounts counts the phase lines in text by phase, and those that
// failed, failing the test on a duration that does not parse.
func phaseCounts(t *testing.T, text string) (timed, failed map[string]int) {
	t.Helper()
	timed, failed = map[string]int{}, map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		m := phaseLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, err := time.ParseDuration(m[2]); err != nil {
			t.Errorf("phase line %q: %v", line, err)
		}
		timed[m[1]]++
		if strings.Contains(line, " failed: ") {
			failed[m[1]]++
		}
	}
	return timed, failed
}

func wantPhasesOnce(t *testing.T, text string, names ...string) {
	t.Helper()
	timed, _ := phaseCounts(t, text)
	for _, name := range names {
		if timed[name] != 1 {
			t.Errorf("phase %s logged %d times, want once\nlog:\n%s", name, timed[name], text)
		}
	}
}

// The boot that follows an update, committed or not, ends its phase once:
// with success, with the startup error, or with the error a fatal exit
// names, whichever comes first. A boot that follows no update says
// nothing. Serial: the boot fatalf ends is package state.
func TestUpdateBootEndsOnceOnEveryExit(t *testing.T) {
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	lines := func() string {
		text := strings.Join(logged, "\n")
		logged = nil
		return text
	}

	none := beginUpdateBoot(logf, "", true, processStarted)
	none.end(nil)
	endActiveUpdateBoot(errors.New("exiting"))
	if len(logged) != 0 {
		t.Fatalf("a boot that follows no update logged %q", logged)
	}

	for _, tc := range []struct {
		name      string
		committed bool
		exit      func(b *updateBoot)
		failed    bool
		after     string
	}{
		{"ready after a commit", true, func(b *updateBoot) { b.end(nil) }, false, "committed"},
		{"ready after a rollback", false, func(b *updateBoot) { b.end(nil) }, false, "unsuccessful"},
		{"start failed", true, func(b *updateBoot) { b.end(errors.New("disk I/O error")) }, true, "committed"},
		{"fatal exit", false, func(*updateBoot) { endActiveUpdateBoot(errors.New("transport: start server: in use")) }, true, "unsuccessful"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := beginUpdateBoot(logf, "2.0.0", tc.committed, processStarted)
			tc.exit(boot)
			// Every later exit path finds the phase ended.
			boot.end(nil)
			boot.end(errors.New("the app closed"))
			endActiveUpdateBoot(errors.New("exiting"))
			text := lines()
			timed, failed := phaseCounts(t, text)
			if timed["first-boot"] != 1 || (failed["first-boot"] == 1) != tc.failed {
				t.Fatalf("logged %q; want first-boot once, failed=%t", text, tc.failed)
			}
			if !strings.HasPrefix(text, "updater: update to 2.0.0: phase=first-boot duration=") ||
				!strings.Contains(text, " after="+tc.after) {
				t.Fatalf("logged %q; want the update's first-boot after=%s", text, tc.after)
			}
		})
	}
}

// The WSL backend's boot follows the update the launcher's record names,
// and otherwise the one its install marker names, which is how a boot after
// the direct swap, or after a swap that did not apply, is timed.
func TestWSLBootFollowsTheRecordThenTheMarker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		updatingTo string
		failure    appupdate.LauncherFailure
		markerTo   string
		applied    bool
		to         string
		committed  bool
	}{
		{"committed trial", "2.0.0", appupdate.LauncherFailure{}, "2.0.0", true, "2.0.0", true},
		{"rolled-back trial", "", appupdate.LauncherFailure{To: "2.0.0", Reason: "x"}, "2.0.0", false, "2.0.0", false},
		{"direct swap", "", appupdate.LauncherFailure{}, "1.0.0", true, "1.0.0", true},
		{"swap that did not apply", "", appupdate.LauncherFailure{}, "2.0.0", false, "2.0.0", false},
		{"no update", "", appupdate.LauncherFailure{}, "", false, "", false},
	} {
		if to, committed := followedUpdate(tc.updatingTo, tc.failure, tc.markerTo, tc.applied); to != tc.to || committed != tc.committed {
			t.Errorf("%s: followedUpdate = %q, %t; want %q, %t", tc.name, to, committed, tc.to, tc.committed)
		}
	}
}

// A supervised serve boot follows the settled update its supervisor names,
// the previous version's after a rollback; a trial's boot is the
// supervisor's trial.boot, and a boot with no outcome follows none.
func TestServeBootFollowsTheSettledUpdate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sup       *serveSupervisor
		to        string
		committed bool
	}{
		{"no supervisor", nil, "", false},
		{"no outcome", &serveSupervisor{}, "", false},
		{"trial", &serveSupervisor{trial: true, target: "2.0.0"}, "", false},
		{"rolled back", &serveSupervisor{outcome: string(supervise.UpdateRolledBack), target: "2.0.0"}, "2.0.0", false},
		{"failed", &serveSupervisor{outcome: string(supervise.UpdateFailed), target: "2.0.0"}, "2.0.0", false},
		{"committed", &serveSupervisor{outcome: string(supervise.UpdateCommitted), target: "2.0.0"}, "2.0.0", true},
	} {
		if to, committed := tc.sup.followedUpdate(); to != tc.to || committed != tc.committed {
			t.Errorf("%s: followedUpdate = %q, %t; want %q, %t", tc.name, to, committed, tc.to, tc.committed)
		}
	}
}

// Every boot that can follow an update ends its boot phase where Start
// returns, on success and on failure, and fatalf ends it on every other
// exit.
func TestBootsEndTheUpdateBootOnEveryExit(t *testing.T) {
	text := readRootSource(t, "main.go")
	fatal := text[strings.Index(text, "func fatalf("):]
	if !strings.Contains(fatal[:strings.Index(fatal, "\n}")], "endActiveUpdateBoot(") {
		t.Fatal("fatalf does not end the boot that follows an update")
	}
	for _, path := range []string{"main.go", "main_serve.go"} {
		text := readRootSource(t, path)
		begin := strings.Index(text, "beginUpdateBoot(")
		bind := strings.Index(text, "srv := bootTransport(")
		failed := strings.Index(text, "boot.end(startErr)")
		start := strings.Index(text, "startErr := appService.Start(bootCtx)")
		ready := strings.Index(text, "srv.MarkReady()")
		ended := strings.Index(text, "boot.end(nil)")
		ordered := begin < bind && bind < start && start < failed && failed < ready && ready < ended
		if begin < 0 || !ordered {
			t.Fatalf("%s: begin=%d bootTransport=%d Start=%d end(startErr)=%d MarkReady=%d end(nil)=%d; want the boot begun before the transport and ended on both of Start's outcomes",
				path, begin, bind, start, failed, ready, ended)
		}
	}
	desktop := readRootSource(t, "main_desktop.go")
	hook := desktop[strings.Index(desktop, "appservice.SetStartDone(appService.App, func(err error) {"):]
	hook = hook[:strings.Index(hook, "\n\t\t\t})")]
	for _, want := range []string{"boot.end(nil)", "boot.end(err)"} {
		if !strings.Contains(hook, want) {
			t.Fatalf("the desktop's SetStartDone hook lacks %q", want)
		}
	}
	after := desktop[strings.Index(desktop, "runErr := shell.run()"):]
	if end := strings.Index(after, "boot.end("); end < 0 || end > strings.Index(after, "fatalf(") {
		t.Fatal("runDesktop does not end the boot once its window closes, before it exits")
	}
}

// The relaunched desktop's boot begins where its wait for the helper ends,
// by the wait's own clock, so first-boot never counts wait-helper. A boot
// no helper started begins with the process.
func TestDesktopBootBeginsWhenTheHelperWaitEnds(t *testing.T) {
	exited := exec.Command("sleep", "30")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	helper, err := supervise.ProcessRefOf(exited.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := exited.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = exited.Wait()
	base := time.Unix(1000, 0)
	ticks := 0
	now := func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * 10 * time.Second)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	started := waitForUpdateHelper(helper, 10*time.Second, logf, now)
	if len(logged) != 1 || !strings.Contains(logged[0], "phase=wait-helper duration=10s") {
		t.Fatalf("logged %q; want the wait from the first tick to the second", logged)
	}
	if want := base.Add(20 * time.Second); !started.Equal(want) {
		t.Fatalf("the boot begins at %v, want %v, where the wait ended", started, want)
	}

	logged = nil
	if started := waitForUpdateHelper(supervise.ProcessRef{}, 10*time.Second, logf, now); !started.Equal(processStarted) || len(logged) != 0 {
		t.Fatalf("with no helper the boot begins at %v and logged %q; want the process's start and nothing", started, logged)
	}
}

// The relaunched app waits for the helper that started it; the wait is
// timed, and one that runs out says so.
func TestWaitForUpdateHelperLogsTheWait(t *testing.T) {
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	helper, err := supervise.ProcessRefOf(sleeper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	waitForUpdateHelper(helper, 100*time.Millisecond, logf, nil)
	if timed, failed := phaseCounts(t, strings.Join(logged, "\n")); timed["wait-helper"] != 1 || failed["wait-helper"] != 1 {
		t.Fatalf("logged %q; want the wait logged once as failed", logged)
	}
	if err := sleeper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = sleeper.Wait()
	logged = nil
	waitForUpdateHelper(helper, 10*time.Second, logf, nil)
	if timed, failed := phaseCounts(t, strings.Join(logged, "\n")); timed["wait-helper"] != 1 || failed["wait-helper"] != 0 {
		t.Fatalf("logged %q; want the wait logged once", logged)
	}
}

// A desktop's shutdown after its app loop ends a framework swap's shutdown
// with what failed there: the transport's shutdown and the loop's error.
// A clean one ends it swapped.
func TestShutDownAfterRunEndsTheSwapWithItsFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runErr   error
		shutdown func(context.Context) error
		failure  string
	}{
		{"clean", nil, func(context.Context) error { return nil }, ""},
		{"no transport", nil, nil, ""},
		{"the transport fails to shut down", nil, func(context.Context) error { return errors.New("drain: boom") }, "transport shutdown: drain: boom"},
		{"the app loop fails", errors.New("wails: boom"), func(context.Context) error { return nil }, "wails: boom"},
		{"both fail", errors.New("wails: boom"), func(context.Context) error { return errors.New("drain: boom") },
			"wails: boom; transport shutdown: drain: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged []string
			logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
			swap := supervise.NewSwapTiming(logf, "2.0.0", nil)
			swap.Quitting()
			shutDownAfterRun(tc.runErr, "transport", tc.shutdown, swap.End)
			text := strings.Join(logged, "\n")
			timed, failed := phaseCounts(t, text)
			if timed["shutdown"] != 1 || len(logged) != 2 || !strings.Contains(logged[1], " summary ") {
				t.Fatalf("logged:\n%s\nwant the shutdown once, then the summary", text)
			}
			if tc.failure == "" {
				if failed["shutdown"] != 0 || !strings.HasSuffix(logged[1], " outcome=swapped") {
					t.Fatalf("logged:\n%s\nwant the shutdown swapped", text)
				}
				return
			}
			if !strings.HasSuffix(logged[0], " failed: "+tc.failure) || !strings.HasSuffix(logged[1], " outcome=error") {
				t.Fatalf("logged:\n%s\nwant the shutdown failed: %s and the outcome error", text, tc.failure)
			}
		})
	}
}
