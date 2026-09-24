package wsllauncher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

func handoffRecord(dir string) supervise.LauncherRecord {
	return supervise.LauncherRecord{
		Distro: "Ubuntu", StablePayload: testStable, StagedPayload: testStaged,
		StagedLauncher: filepath.Join(dir, "staged.exe"), InstallPath: filepath.Join(dir, "agent-overflow.exe"),
		TargetFingerprint: "target",
	}
}

func TestBeginLauncherUpdate(t *testing.T) {
	dir := t.TempDir()
	path := supervise.LauncherRecordPath(dir, "prod", "Ubuntu")
	now := time.UnixMilli(5)
	record, err := BeginLauncherUpdate(path, handoffRecord(dir), "1.0.0", "2.0.0", "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if record.Update.State != supervise.UpdatePending || record.Update.From != "1.0.0" || record.Update.To != "2.0.0" {
		t.Fatalf("record = %+v", record.Update)
	}
	if _, err := BeginLauncherUpdate(path, handoffRecord(dir), "1.0.0", "3.0.0", "u2", now); err == nil ||
		!strings.Contains(err.Error(), "still in progress") {
		t.Fatalf("a second update began over a pending one: %v", err)
	}
	if err := SettleLauncherUpdate(path, "u1", supervise.UpdateCommitted, "", now); err != nil {
		t.Fatal(err)
	}
	// The running version, not what the settled record selected, is where
	// the next update starts.
	record, err = BeginLauncherUpdate(path, handoffRecord(dir), "2.0.1", "3.0.0", "u2", now)
	if err != nil {
		t.Fatal(err)
	}
	if record.Update.ID != "u2" || record.ActiveVersion != "2.0.1" {
		t.Fatalf("record = %+v", record)
	}
	if _, err := BeginLauncherUpdate(path, handoffRecord(dir), "3.0.0", "3.0.0", "u3", now); err == nil {
		t.Fatal("an update to the running version began")
	}
}

func TestSettleLauncherUpdateIgnoresOtherUpdates(t *testing.T) {
	dir := t.TempDir()
	path := supervise.LauncherRecordPath(dir, "prod", "Ubuntu")
	if err := SettleLauncherUpdate(path, "u1", supervise.UpdateFailed, "x", time.Now()); err != nil {
		t.Fatalf("no record: %v", err)
	}
	if _, err := BeginLauncherUpdate(path, handoffRecord(dir), "1.0.0", "2.0.0", "u1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := SettleLauncherUpdate(path, "u9", supervise.UpdateFailed, "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	record, _, err := supervise.LoadLauncherRecord(path)
	if err != nil || record.Update.State != supervise.UpdatePending {
		t.Fatalf("another update's settle changed the record: %+v, %v", record.Update, err)
	}
	if err := SettleLauncherUpdate(path, "u1", supervise.UpdateFailed, "the new launcher did not start", time.Now()); err != nil {
		t.Fatal(err)
	}
	record, _, err = supervise.LoadLauncherRecord(path)
	if err != nil || record.Update.State != supervise.UpdateFailed || record.Update.Reason != "the new launcher did not start" {
		t.Fatalf("record = %+v, %v", record.Update, err)
	}
}

func TestPreflightAnswerRoundTrip(t *testing.T) {
	path := PreflightAnswerPath(t.TempDir(), "0123456789abcdef")
	if _, found, err := ReadPreflightAnswer(path); found || err != nil {
		t.Fatalf("missing answer: found=%v err=%v", found, err)
	}
	want := PreflightAnswer{OK: true, Version: "2.0.0", Fingerprint: "f", StagedPayload: testStaged}
	if err := WritePreflightAnswer(path, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := ReadPreflightAnswer(path)
	if err != nil || !found || got != want {
		t.Fatalf("answer = %+v found=%v err=%v", got, found, err)
	}
	if err := WritePreflightAnswer(path, PreflightAnswer{OK: true, Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadPreflightAnswer(path); err == nil {
		t.Fatal("an incomplete answer was accepted")
	}
}

func TestUpdateIDs(t *testing.T) {
	id, err := NewUpdateID()
	if err != nil || !ValidUpdateID(id) {
		t.Fatalf("id %q err %v", id, err)
	}
	for _, bad := range []string{"", "u1", "0123456789ABCDEF", "0123456789abcde/", "..\\..\\0123456789"} {
		if ValidUpdateID(bad) {
			t.Fatalf("%q was accepted", bad)
		}
	}
	if got := StagedPayloadPath(testStable, id); got != testStable+".update-"+id {
		t.Fatalf("staged payload = %q", got)
	}
}

func TestPublishLauncherFile(t *testing.T) {
	setup := func(t *testing.T) (dir, staged, install string) {
		dir = t.TempDir()
		staged = filepath.Join(dir, "staged.exe")
		install = filepath.Join(dir, "agent-overflow.exe")
		if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(install, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir, staged, install
	}
	read := func(t *testing.T, p string) string {
		t.Helper()
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("replace", func(t *testing.T) {
		_, staged, install := setup(t)
		if err := PublishLauncherFile(staged, install, "u1", os.Rename); err != nil {
			t.Fatal(err)
		}
		if read(t, install) != "new" || read(t, staged) != "new" {
			t.Fatal("the install path does not hold the staged launcher")
		}
	})
	t.Run("a running launcher is moved aside", func(t *testing.T) {
		_, staged, install := setup(t)
		busy := true
		replace := func(from, to string) error {
			// Windows refuses to replace a running executable, not to
			// rename it.
			if to == install && busy {
				busy = false
				return errors.New("access is denied")
			}
			return os.Rename(from, to)
		}
		if err := PublishLauncherFile(staged, install, "u1", replace); err != nil {
			t.Fatal(err)
		}
		if read(t, install) != "new" || read(t, install+".old-u1") != "old" {
			t.Fatal("the running launcher was not moved aside")
		}
		record := supervise.LauncherRecord{StagedLauncher: staged, InstallPath: install}
		if err := RemoveLauncherFiles(record); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{staged, install + ".old-u1", install + ".new"} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s remains: %v", p, err)
			}
		}
		if read(t, install) != "new" {
			t.Fatal("residue removal touched the install path")
		}
	})
	t.Run("a failure keeps the previous launcher", func(t *testing.T) {
		_, staged, install := setup(t)
		calls := 0
		replace := func(from, to string) error {
			calls++
			if from == install+".new" {
				return errors.New("sharing violation")
			}
			return os.Rename(from, to)
		}
		if err := PublishLauncherFile(staged, install, "u1", replace); err == nil {
			t.Fatal("publish reported success")
		}
		if read(t, install) != "old" {
			t.Fatal("the previous launcher is not at the install path")
		}
		if _, err := os.Stat(install + ".new"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the copy remains: %v", err)
		}
		if calls != 4 {
			t.Fatalf("replace calls = %d, want replace, aside, replace, restore", calls)
		}
	})
}

// fakePreflightHost answers the new launcher's preflight steps and records
// them in order.
type fakePreflightHost struct {
	installErr   error
	preflight    supervise.Preflight
	preflightErr error
	space        supervise.UpdateEvent
	spaceErr     error
	hostFree     *uint64
	calls        []string
}

func (h *fakePreflightHost) InstallEmbeddedPayload(_ context.Context, distro, staged string) error {
	h.calls = append(h.calls, "install "+distro+" "+staged)
	return h.installErr
}

func (h *fakePreflightHost) Preflight(_ context.Context, _, payload string) (supervise.Preflight, error) {
	h.calls = append(h.calls, "preflight "+payload)
	return h.preflight, h.preflightErr
}

func (h *fakePreflightHost) RunCommand(_ context.Context, _, payload, command string, args []string, _ func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	h.calls = append(h.calls, command+"@"+payload+" "+strings.Join(args, " "))
	return h.space, h.spaceErr
}

func (h *fakePreflightHost) RemoveStagedPayload(_ context.Context, record supervise.LauncherRecord) error {
	h.calls = append(h.calls, "remove "+record.StagedPayload)
	return nil
}

func (h *fakePreflightHost) HostFreeBytes(string) (uint64, bool) {
	if h.hostFree == nil {
		return 0, false
	}
	return *h.hostFree, true
}

func TestPreflightStagedPayloadAsksTheTargetWhetherItsSnapshotFits(t *testing.T) {
	const id = "0123456789abcdef"
	staged := StagedPayloadPath(testStable, id)
	request := PreflightRequest{ID: id, Distro: "Ubuntu", Stable: testStable, Version: "2.0.0", Fingerprint: "fp"}
	ok := supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomeOK}
	short := "An update backs up the database first and needs 2.0 GB free. Free at least 1.0 GB and try again."

	t.Run("fits", func(t *testing.T) {
		free := uint64(5 << 30)
		host := &fakePreflightHost{preflight: supervise.Preflight{Version: "2.0.0"}, space: ok, hostFree: &free}
		answer := PreflightStagedPayload(t.Context(), host, request)
		want := PreflightAnswer{OK: true, Version: "2.0.0", Fingerprint: "fp", StagedPayload: staged}
		if answer != want || answer.Err() != nil {
			t.Fatalf("answer = %+v, want %+v", answer, want)
		}
		wantCalls := []string{
			"install Ubuntu " + staged,
			"preflight " + staged,
			supervise.UpdateSpaceCommand + "@" + staged + " --id " + id + " --host-free 5368709120",
		}
		if strings.Join(host.calls, "\n") != strings.Join(wantCalls, "\n") {
			t.Fatalf("calls = %q, want %q", host.calls, wantCalls)
		}
	})
	t.Run("does not fit", func(t *testing.T) {
		host := &fakePreflightHost{preflight: supervise.Preflight{Version: "2.0.0"},
			space: supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomeRefused, Reason: short}}
		answer := PreflightStagedPayload(t.Context(), host, request)
		if answer.OK || !answer.Refused || answer.Reason != short {
			t.Fatalf("answer = %+v, want the target's refusal", answer)
		}
		// The old launcher reports the target's words as they are.
		if err := answer.Err(); err == nil || err.Error() != short {
			t.Fatalf("Err = %v, want %q", err, short)
		}
		if last := host.calls[len(host.calls)-1]; last != "remove "+staged {
			t.Fatalf("calls = %q, want the staged payload removed", host.calls)
		}
		if strings.Contains(strings.Join(host.calls, "\n"), "--host-free") {
			t.Fatalf("calls = %q, want no host bound when it is unknown", host.calls)
		}
	})
	for _, c := range []struct {
		name string
		host *fakePreflightHost
		want string
	}{
		{"install fails", &fakePreflightHost{installErr: errors.New("disk full")}, "install the new backend"},
		{"preflight fails", &fakePreflightHost{preflightErr: errors.New("exec format error")}, "did not start"},
		{"wrong version", &fakePreflightHost{preflight: supervise.Preflight{Version: "1.9.0"}}, "reports version 1.9.0"},
		{"space check fails", &fakePreflightHost{preflight: supervise.Preflight{Version: "2.0.0"}, spaceErr: errors.New("wsl.exe exited 1")}, "could not check the free space"},
		{"space check errs", &fakePreflightHost{preflight: supervise.Preflight{Version: "2.0.0"},
			space: supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomeFailed, Reason: "statfs: EIO"}}, "statfs: EIO"},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := PreflightStagedPayload(t.Context(), c.host, request)
			if answer.OK || answer.Refused || !strings.Contains(answer.Reason, c.want) {
				t.Fatalf("answer = %+v, want a failure naming %q", answer, c.want)
			}
			if err := answer.Err(); err == nil || !strings.HasPrefix(err.Error(), "the new version could not start inside WSL: ") {
				t.Fatalf("Err = %v", err)
			}
			installed := c.host.installErr == nil
			removed := c.host.calls[len(c.host.calls)-1] == "remove "+staged
			if installed != removed {
				t.Fatalf("calls = %q: an installed payload must be removed, and only then", c.host.calls)
			}
		})
	}
	t.Run("bad request", func(t *testing.T) {
		host := &fakePreflightHost{}
		bad := request
		bad.ID = "../x"
		if answer := PreflightStagedPayload(t.Context(), host, bad); answer.OK || len(host.calls) != 0 {
			t.Fatalf("answer = %+v, calls %q", answer, host.calls)
		}
	})
}
