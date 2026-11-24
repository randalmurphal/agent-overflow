package providerupdate

import (
	"context"
	"maps"
	"strings"
	"testing"
)

func (r *rig) setEnv(name string, env map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.env == nil {
		r.env = map[string]map[string]string{}
	}
	r.env[name] = env
}

// duringNextDoctor runs change inside the next `claude doctor`, which is
// mid-check, and answers the latest channel.
func (r *rig) duringNextDoctor(change func()) {
	r.runner.on("doctor", func(ctx context.Context) (Output, error) {
		r.runner.on("doctor", doctorSays("latest"))
		change()
		return doctorSays("latest")(ctx)
	})
}

// A binary setting switched while the run's confirming check ran doctor:
// the check describes neither install, so nothing runs and nothing succeeds.
func TestABinarySwitchedDuringTheConfirmingCheckRunsNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	r.runner.on("update", r.updateTo("2.1.330", Output{}))
	r.duringNextDoctor(func() {
		r.setBinary(ProviderClaude, Binary{Path: "/opt/other-claude", ResolvedPath: "/opt/other/2.1.330"})
		r.setInstalled(ProviderClaude, "2.1.330")
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	status := r.waitRunSettled(mark, ProviderClaude)
	if status.Run.Phase != PhaseFailed || !strings.Contains(status.Run.Error, "changed during the check") {
		t.Fatalf("run = %+v, want failed because the install changed", status.Run)
	}
	if r.runner.ran("update") != 0 {
		t.Fatalf("the updater ran against %+v", r.runner.lastOf("update"))
	}
}

// The installed version changing in place mid-check fails the check.
func TestAnInstalledVersionChangedDuringTheCheckFailsIt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.duringNextDoctor(func() { r.setInstalled(ProviderClaude, "2.1.310") })
	claude, codex := r.check()
	if claude.Available || claude.LatestVersion != "" || !strings.Contains(claude.CheckError, "the installed claude changed during the check") {
		t.Fatalf("claude = %+v, want the check failed", claude)
	}
	if !codex.Available {
		t.Errorf("codex = %+v", codex)
	}
	// The next check sees one install and answers.
	if claude, _ := r.check(); !claude.Available || claude.InstalledVersion != "2.1.310" {
		t.Errorf("recheck = %+v", claude)
	}
}

// The updater runs with the environment its confirming check used, even
// when the setting changed after that check read it.
func TestTheUpdaterRunsWithTheCheckedEnvironment(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	r.runner.on("update", r.updateTo("2.1.325", Output{}))
	r.duringNextDoctor(func() { r.setEnv(ProviderClaude, map[string]string{"PROXY_FOR": "changed"}) })
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	if status := r.waitRunSettled(mark, ProviderClaude); status.Run.Phase != PhaseSucceeded {
		t.Fatalf("run = %+v", status.Run)
	}
	if env := r.runner.lastOf("update").Env.Custom; !maps.Equal(env, map[string]string{"PROXY_FOR": ProviderClaude}) {
		t.Errorf("the updater ran with %v, want the checked environment", env)
	}
}

// A binary setting changed while the updater ran: the installed version
// read afterwards belongs to another install, so the run is not a success.
func TestABinaryChangedDuringTheUpdateFailsTheRun(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	r.runner.on("update", func(context.Context) (Output, error) {
		r.setBinary(ProviderClaude, Binary{Path: "/opt/other-claude", ResolvedPath: "/opt/other/2.1.330"})
		r.setInstalled(ProviderClaude, "2.1.330")
		return Output{Text: "updated"}, nil
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	status := r.waitRunSettled(mark, ProviderClaude)
	if status.Run.Phase != PhaseFailed || !strings.Contains(status.Run.Error, "binary changed during the update") ||
		status.Run.FromVersion != "2.1.300" || status.Run.Output != "updated" {
		t.Fatalf("run = %+v, want failed because the binary changed", status.Run)
	}
}

// An update moves the file the binary resolves to (Claude's versions
// symlink); that is the update, not a changed install.
func TestAnUpdateThatMovesTheResolvedPathSucceeds(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	r.runner.on("update", func(context.Context) (Output, error) {
		r.setBinary(ProviderClaude, Binary{Path: "/opt/claude", ResolvedPath: "/home/u/.local/share/claude/versions/2.1.325"})
		r.setInstalled(ProviderClaude, "2.1.325")
		return Output{}, nil
	})
	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	status := r.waitRunSettled(mark, ProviderClaude)
	if want := (Run{Phase: PhaseSucceeded, FromVersion: "2.1.300", ToVersion: "2.1.325"}); status.Run != want {
		t.Fatalf("run = %+v, want %+v", status.Run, want)
	}
	if status.Available || status.CheckError != "" {
		t.Errorf("status = %+v", status)
	}
}
