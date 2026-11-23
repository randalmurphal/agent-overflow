package orphanreaper

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
)

func newTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orphan-registry.json")
	return NewRegistry(path), path
}

// leaderCapture is what CaptureLeader returns for a leader with start.
func leaderCapture(pid int, start string) Capture {
	return Capture{leader: pid, identity: Identity{Boot: bootA, Start: start}, parent: 500, createUnix: recordedMs}
}

func TestRegistryAddLoadRoundTrip(t *testing.T) {
	reg, _ := newTestRegistry(t)
	if err := reg.Add("b", leaderCapture(200, "222")); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("a", leaderCapture(100, "111")); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by leader.
	want := []Record{
		{Owner: "a", Leader: 100, Identity: &Identity{Boot: bootA, Start: "111"}, CreateUnix: recordedMs, Parent: 500},
		{Owner: "b", Leader: 200, Identity: &Identity{Boot: bootA, Start: "222"}, CreateUnix: recordedMs, Parent: 500},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %+v, want %+v", got, want)
	}
}

func TestRegistryAddReplacesSameGroup(t *testing.T) {
	reg, _ := newTestRegistry(t)
	_ = reg.Add("old", leaderCapture(100, "111"))
	_ = reg.Add("new", leaderCapture(100, "999"))
	got, _ := reg.Load()
	if len(got) != 1 || got[0].Owner != "new" || got[0].Identity.Start != "999" {
		t.Fatalf("records = %+v, want only the replacement", got)
	}
}

func TestRegistryRemove(t *testing.T) {
	reg, _ := newTestRegistry(t)
	_ = reg.Add("a", leaderCapture(100, "1"))
	_ = reg.Add("b", leaderCapture(200, "2"))
	if err := reg.Remove(100); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Load()
	if len(got) != 1 || got[0].Leader != 200 {
		t.Fatalf("after remove = %+v, want only group 200", got)
	}
}

func TestRegistryReplaceWithNothingRemovesFile(t *testing.T) {
	reg, path := newTestRegistry(t)
	_ = reg.Add("a", leaderCapture(100, "1"))
	if err := reg.replace(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("registry file should be gone, stat err = %v", err)
	}
	got, err := reg.Load()
	if err != nil || len(got) != 0 {
		t.Errorf("Load after replace(nil) = %+v %v, want empty", got, err)
	}
}

func TestRegistryEmptyWhenAbsent(t *testing.T) {
	reg, _ := newTestRegistry(t)
	got, err := reg.Load()
	if err != nil {
		t.Fatalf("Load on absent file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Load = %+v, want empty for absent file", got)
	}
}

func TestRegistryAddResetsCorruptFile(t *testing.T) {
	reg, path := newTestRegistry(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Add must not fail on a corrupt file — it logs and starts fresh.
	if err := reg.Add("a", leaderCapture(300, "333")); err != nil {
		t.Fatalf("Add over corrupt file: %v", err)
	}
	got, err := reg.Load()
	if err != nil {
		t.Fatalf("Load after reset: %v", err)
	}
	if len(got) != 1 || got[0].Leader != 300 {
		t.Errorf("after corrupt-reset add = %+v, want just group 300", got)
	}
}

// A record carries its identity and, for earlier versions' readers, their
// start time; one an earlier version wrote reads with its create_unix and no
// identity, which the sweep compares the old way.
func TestRegistryRecordShapes(t *testing.T) {
	reg, path := newTestRegistry(t)
	if err := reg.Add("new", leaderCapture(100, "7")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"owner":"new","pid":100,"pgid":100,"identity":{"boot":"boot-a","start":"7"},"create_unix":1700000000000,"parent_pid":500}]`; string(data) != want {
		t.Fatalf("registry file %s, want %s", data, want)
	}
	if err := os.WriteFile(path, []byte(`[{"owner":"old","pid":5,"pgid":5,"create_unix":1700000000000,"parent_pid":9}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Record{{Owner: "old", Leader: 5, CreateUnix: recordedMs, Parent: 9}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("an earlier version's record reads %+v, want %+v", got, want)
	}
}

// An entry that cannot name its leader safely is rejected when the file is
// read, and the sweep never signals it: its pid and pgid differ, its leader
// is out of range, or it names no identity.
func TestRegistryRejectsEntriesThatCannotNameTheirLeader(t *testing.T) {
	reg, path := newTestRegistry(t)
	entries := `[
		{"owner":"split","pid":10,"pgid":11,"identity":{"boot":"boot-a","start":"6299260"},"parent_pid":500},
		{"owner":"init","pid":1,"pgid":1,"identity":{"boot":"boot-a","start":"6299260"},"parent_pid":500},
		{"owner":"huge","pid":99999999,"pgid":99999999,"identity":{"boot":"boot-a","start":"6299260"},"parent_pid":500},
		{"owner":"blank","pid":12,"pgid":12,"identity":{"boot":"boot-a","start":""},"parent_pid":500},
		{"owner":"none","pid":13,"pgid":13,"parent_pid":500},
		{"owner":"kept","pid":14,"pgid":14,"identity":{"boot":"boot-a","start":"6299260"},"parent_pid":500}
	]`
	if err := os.WriteFile(path, []byte(entries), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := leaders(t, reg); len(got) != 1 || got[0] != 14 {
		t.Fatalf("Load = %v, want only the valid entry", got)
	}
	everyone := map[int]instanceinfo.Process{}
	for _, pid := range []int{1, 10, 11, 12, 13, 14, 99999999} {
		everyone[pid] = running(1)
	}
	s := &stops{}
	var lines logLines
	if err := sweepWith(reg, fakeProc{procs: everyone, boot: bootA}, s.stop, probes(nil), lines.logf); err != nil {
		t.Fatal(err)
	}
	if len(s.stopped) != 1 || s.stopped[0] != 14 {
		t.Fatalf("the sweep stopped %v, want only the valid entry's group", s.stopped)
	}
	if got := leaders(t, reg); len(got) != 0 {
		t.Fatalf("after the sweep the registry holds %v, want the rejected entries gone", got)
	}
}
