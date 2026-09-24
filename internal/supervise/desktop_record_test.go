//go:build !windows

package supervise

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDesktopInstallPathIsTheBundleOnMacOSAndTheExecutableElsewhere(t *testing.T) {
	bundleExe := "/Applications/Agent Overflow.app/Contents/MacOS/agent-overflow"
	for _, tc := range []struct {
		name, executable, goos, want string
	}{
		{"a macOS bundle", bundleExe, "darwin", "/Applications/Agent Overflow.app"},
		{"a macOS binary outside a bundle", "/usr/local/bin/agent-overflow", "darwin", "/usr/local/bin/agent-overflow"},
		{"a Linux binary", "/opt/ao/agent-overflow", "linux", "/opt/ao/agent-overflow"},
		{"a bundle layout on Linux", bundleExe, "linux", bundleExe},
	} {
		got, err := DesktopInstallPath(tc.executable, tc.goos)
		if err != nil || got != tc.want {
			t.Errorf("%s: DesktopInstallPath(%q) = %q, %v; want %q", tc.name, tc.executable, got, err, tc.want)
		}
	}
	if _, err := DesktopInstallPath("/Applications/Agent Overflow.app/Contents/MacOS/other", "darwin"); err == nil {
		t.Error("a bundle that runs another executable was accepted")
	}
	if _, err := DesktopInstallPath("agent-overflow", "linux"); err == nil {
		t.Error("a relative executable path was accepted")
	}
}

func TestDesktopStagedPathIsAHiddenSiblingOfTheSameKind(t *testing.T) {
	bundle := "/Applications/Agent Overflow.app"
	staged := DesktopStagedPath(bundle, "0123456789abcdef")
	if staged != "/Applications/.agent-overflow-update-0123456789abcdef.app" {
		t.Fatalf("staged bundle = %q", staged)
	}
	if got := DesktopExecutable(staged); got != staged+"/Contents/MacOS/agent-overflow" {
		t.Fatalf("staged bundle executable = %q", got)
	}
	record := DesktopRecord{InstallPath: bundle, State: State{Update: &UpdateRecord{ID: "0123456789abcdef"}}}
	if got := desktopPreviousPath(record); got != "/Applications/.agent-overflow-previous-0123456789abcdef.app" {
		t.Fatalf("previous bundle = %q", got)
	}
	if got := DesktopStagedPath("/opt/ao/agent-overflow", "0123456789abcdef"); got != "/opt/ao/.agent-overflow-update-0123456789abcdef" {
		t.Fatalf("staged binary = %q", got)
	}
	if got := DesktopExecutable("/opt/ao/agent-overflow"); got != "/opt/ao/agent-overflow" {
		t.Fatalf("binary executable = %q", got)
	}
}

func desktopUpdateRecord(t *testing.T, install, id string) DesktopRecord {
	t.Helper()
	base, err := Adopt("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	state, err := base.Begin(id, "2.0.0", time.UnixMilli(1000))
	if err != nil {
		t.Fatal(err)
	}
	return DesktopRecord{State: state, InstallPath: install, StagedPath: DesktopStagedPath(install, id), TargetDigest: "ab"}
}

func TestDesktopRecordValidatesWhereTheUpdateInstalls(t *testing.T) {
	layout := appLayout(t, t.TempDir())
	install := filepath.Join(t.TempDir(), "agent-overflow")
	record := desktopUpdateRecord(t, install, "0123456789abcdef")
	if err := SaveDesktopRecord(layout, record); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := LoadDesktopRecord(layout)
	if err != nil || !found || !reflect.DeepEqual(loaded, record) {
		t.Fatalf("LoadDesktopRecord = %+v, %t, %v; want %+v", loaded, found, err, record)
	}

	refuse := func(name string, change func(*DesktopRecord)) {
		t.Helper()
		bad := desktopUpdateRecord(t, install, "0123456789abcdef")
		change(&bad)
		if err := SaveDesktopRecord(layout, bad); err == nil {
			t.Errorf("%s: saved", name)
		}
	}
	refuse("a relative install path", func(r *DesktopRecord) { r.InstallPath = "agent-overflow" })
	refuse("no staged path", func(r *DesktopRecord) { r.StagedPath = "" })
	refuse("no digest", func(r *DesktopRecord) { r.TargetDigest = "" })
	refuse("a staged path elsewhere", func(r *DesktopRecord) { r.StagedPath = filepath.Join(t.TempDir(), "x") })
	refuse("the install path staged", func(r *DesktopRecord) { r.StagedPath = r.InstallPath })
	refuse("no update", func(r *DesktopRecord) { r.Update = nil })

	migration, err := Adopt("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	migration, err = migration.BeginMigration("0123456789abcdef", time.UnixMilli(1000))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveDesktopRecord(layout, DesktopRecord{State: migration, InstallPath: install}); err != nil {
		t.Fatalf("a migration record: %v", err)
	}
	staging := DesktopRecord{State: migration, InstallPath: install, StagedPath: DesktopStagedPath(install, "0123456789abcdef"), TargetDigest: "ab"}
	if err := SaveDesktopRecord(layout, staging); err == nil {
		t.Error("a migration record that stages a version was saved")
	}
}

func TestSettleDesktopUpdateSettlesOnlyThePendingUpdateItNames(t *testing.T) {
	dataDir := t.TempDir()
	layout := appLayout(t, dataDir)
	record := desktopUpdateRecord(t, filepath.Join(t.TempDir(), "agent-overflow"), "0123456789abcdef")
	if err := SaveDesktopRecord(layout, record); err != nil {
		t.Fatal(err)
	}
	if err := SettleDesktopUpdate(dataDir, "fedcba9876543210", UpdateFailed, "x", true, time.UnixMilli(2000)); err == nil {
		t.Fatal("another update was settled")
	}
	if err := SettleDesktopUpdate(dataDir, "0123456789abcdef", UpdateFailed, "no helper", true, time.UnixMilli(2000)); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadDesktopRecord(layout)
	if err != nil {
		t.Fatal(err)
	}
	if u := loaded.Update; u.State != UpdateFailed || u.Reason != "no helper" || !u.Reported {
		t.Fatalf("settled = %+v", u)
	}
	if err := SettleDesktopUpdate(dataDir, "0123456789abcdef", UpdateRolledBack, "again", false, time.UnixMilli(3000)); err == nil {
		t.Fatal("a settled update was settled again")
	}

	// One that started a trial may have a trial's database: its recovery
	// restores or resumes it, so it is not settled here.
	tried := desktopUpdateRecord(t, filepath.Join(t.TempDir(), "agent-overflow"), "fedcba9876543210")
	state, err := tried.State.Retry()
	if err != nil {
		t.Fatal(err)
	}
	tried.State = state
	if err := SaveDesktopRecord(layout, tried); err != nil {
		t.Fatal(err)
	}
	if err := SettleDesktopUpdate(dataDir, "fedcba9876543210", UpdateFailed, "no exit", false, time.UnixMilli(4000)); err == nil ||
		!strings.Contains(err.Error(), "started a trial") {
		t.Fatalf("settling an update that started a trial = %v", err)
	}
	if loaded, _, err := LoadDesktopRecord(layout); err != nil || loaded.Update.State != UpdatePending || loaded.Update.Attempts != 1 {
		t.Fatalf("the update that started a trial = %+v, %v; want it pending", loaded.Update, err)
	}
}

func TestDesktopArgvCarriesTheAppsArgumentsThroughTheHelper(t *testing.T) {
	original := []string{"--data-dir", "/data", "--dev", "positional"}
	self := ProcessRef{PID: 42, Start: "1700000000.25"}
	helper := DesktopHelperArgs([]string{"--id", "0123456789abcdef"}, self, "/data", original)
	want := []string{DesktopApplyCommand, "--id", "0123456789abcdef", "--wait-pid", "42", "--wait-start", "1700000000.25",
		"--data-dir", "/data", "--", "--data-dir", "/data", "--dev", "positional"}
	if !reflect.DeepEqual(helper, want) {
		t.Fatalf("helper argv = %q\nwant %q", helper, want)
	}
	if got := DesktopHelperArgs([]string{"--migrate", "7"}, self, "", nil); !reflect.DeepEqual(got,
		[]string{DesktopApplyCommand, "--migrate", "7", "--wait-pid", "42", "--wait-start", "1700000000.25", "--"}) {
		t.Fatalf("migration helper argv = %q", got)
	}

	app := DesktopAppArgs(ProcessRef{PID: 7, Start: "9"}, original)
	if !reflect.DeepEqual(app, append([]string{"--wait-pid", "7", "--wait-start", "9"}, original...)) {
		t.Fatalf("app argv = %q", app)
	}
	if got := DesktopRelaunchArgs(app); !reflect.DeepEqual(got, original) {
		t.Fatalf("relaunch of %q = %q, want the original %q", app, got, original)
	}
	for _, args := range [][]string{
		{"-wait-pid=7", "-wait-start=9", "--dev"},
		{"--wait-pid=7", "--dev", "--wait-start", "9"},
		{"-wait-pid", "7", "--dev", "-wait-start", "9"},
	} {
		if got := DesktopRelaunchArgs(args); !reflect.DeepEqual(got, []string{"--dev"}) {
			t.Errorf("DesktopRelaunchArgs(%q) = %q, want [--dev]", args, got)
		}
	}
	kept := []string{"--wait-pidx", "1", "wait-pid", "---wait-start"}
	if got := DesktopRelaunchArgs(kept); !reflect.DeepEqual(got, kept) {
		t.Fatalf("DesktopRelaunchArgs(%q) = %q, want it unchanged", kept, got)
	}
}

func TestDesktopLaunchCommandOpensABundleThroughLaunchServices(t *testing.T) {
	argv := func(cmd *exec.Cmd) string { return strings.Join(cmd.Args, " ") }
	bundle := "/Applications/Agent Overflow.app"
	if got := DesktopLaunchCommand(bundle, []string{"--wait-pid", "1"}, "darwin"); got.Path != "/usr/bin/open" ||
		argv(got) != "/usr/bin/open -n "+bundle+" --args --wait-pid 1" {
		t.Fatalf("bundle launch = %s %q", got.Path, got.Args)
	}
	if got := DesktopLaunchCommand("/usr/local/bin/agent-overflow", []string{"a"}, "darwin"); got.Path != "/usr/local/bin/agent-overflow" ||
		argv(got) != "/usr/local/bin/agent-overflow a" {
		t.Fatalf("macOS binary launch = %s %q", got.Path, got.Args)
	}
	if got := DesktopLaunchCommand("/opt/ao/agent-overflow", []string{"a"}, "linux"); got.Path != "/opt/ao/agent-overflow" ||
		argv(got) != "/opt/ao/agent-overflow a" {
		t.Fatalf("Linux launch = %s %q", got.Path, got.Args)
	}
}
