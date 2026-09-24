package supervise

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLauncherRecordPathSeparatesLaunchersAndDataRoots: dev and production
// launchers, isolated profiles and distributions each get their own record;
// names that differ only in case are one distribution; no name leaves the
// record directory.
func TestLauncherRecordPathSeparatesLaunchersAndDataRoots(t *testing.T) {
	const dir = "/config"
	seen := map[string]string{}
	for _, c := range []struct{ mode, distro string }{
		{"prod", "Ubuntu"},
		{"dev", "Ubuntu"},
		{"harness", "Ubuntu"},
		{"prod", "Debian"},
		{"prod", "Ubuntu-22.04"},
		{"prod", "Ubuntu-22%2E04"},
		{"prod", "Ubuntu.22-04"},
		{"prod", "../../x"},
		{"prod", "a/b"},
		{"prod", "a%2Fb"},
		{"prod", ""},
	} {
		path := LauncherRecordPath(dir, c.mode, c.distro)
		if filepath.Dir(path) != filepath.Join(dir, LauncherRecordDir) {
			t.Errorf("LauncherRecordPath(%q, %q) = %q, outside the record directory", c.mode, c.distro, path)
		}
		// Windows compares file names without case.
		key, name := c.mode+"/"+c.distro, strings.ToLower(path)
		if other, ok := seen[name]; ok {
			t.Errorf("%s and %s share %q", other, key, path)
		}
		seen[name] = key
	}
	if a, b := LauncherRecordPath(dir, "prod", "Ubuntu"), LauncherRecordPath(dir, "PROD", "ubuntu"); a != b {
		t.Errorf("case variants got %q and %q; Windows would treat them as one file", a, b)
	}
}

// TestLauncherMigrationRecord: a migration runs the version already
// installed from itself to itself, stages nothing, runs its trial through
// the stable payload and is never reported to the backend as a failed
// update; an update still needs everything it stages.
func TestLauncherMigrationRecord(t *testing.T) {
	base, err := Adopt("2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	migration, err := base.BeginMigration("m1", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if u := migration.Update; u.From != "2.0.0" || u.To != "2.0.0" || u.State != UpdatePending || u.Attempts != 0 {
		t.Fatalf("migration = %+v", u)
	}
	record := LauncherRecord{State: migration, Distro: "Ubuntu", StablePayload: "/bin/agent-overflow"}
	if err := record.Validate(); err != nil {
		t.Fatalf("a migration record does not validate: %v", err)
	}
	if !record.Migration() || record.TrialPayload() != "/bin/agent-overflow" {
		t.Fatalf("Migration() = %v, TrialPayload() = %q", record.Migration(), record.TrialPayload())
	}
	staged := record
	staged.StagedPayload = "/bin/agent-overflow.update-m1"
	if err := staged.Validate(); err == nil || !strings.Contains(err.Error(), "stagedPayload") {
		t.Fatalf("a migration that stages a payload = %v", err)
	}
	if _, err := migration.BeginMigration("m2", time.UnixMilli(2)); err == nil {
		t.Fatal("a second migration began while one is pending")
	}
	if _, err := base.BeginMigration(" ", time.UnixMilli(1)); err == nil {
		t.Fatal("a migration began without an id")
	}
	settled, err := migration.Settle(UpdateRolledBack, "trial failed", time.UnixMilli(3))
	if err != nil {
		t.Fatal(err)
	}
	record.State = settled
	if _, _, ok := record.UnsuccessfulUpdate("2.0.0"); ok {
		t.Fatal("a rolled-back migration reads as an unsuccessful update")
	}

	update, err := base.Begin("u1", "3.0.0", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	record = LauncherRecord{State: update, Distro: "Ubuntu", StablePayload: "/bin/agent-overflow"}
	if record.Migration() {
		t.Fatal("an update reads as a migration")
	}
	if err := record.Validate(); err == nil || !strings.Contains(err.Error(), "stagedPayload") {
		t.Fatalf("an update without its staged payload = %v", err)
	}
	record.StagedPayload, record.StagedLauncher, record.InstallPath, record.TargetFingerprint =
		"/bin/agent-overflow.update-u1", `C:\x\update.exe`, `C:\x\agent-overflow.exe`, "target"
	if err := record.Validate(); err != nil || record.TrialPayload() != "/bin/agent-overflow.update-u1" {
		t.Fatalf("update: Validate = %v, TrialPayload = %q", err, record.TrialPayload())
	}
}
