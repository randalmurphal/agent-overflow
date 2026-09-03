package supervise

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLayout(t *testing.T) Layout {
	t.Helper()
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return layout
}

// The selection table IS the feature. Every row here is a sentence in
// t3code's server-updates doc, and a supervisor that got one wrong would run
// a version nobody chose.
func TestSelectionSemantics(t *testing.T) {
	base := func(update *UpdateRecord) State {
		return State{Schema: StateSchema, ActiveVersion: "1.0.0", Update: update}
	}
	record := func(state UpdateState) *UpdateRecord {
		return &UpdateRecord{ID: "u1", State: state, From: "1.0.0", To: "2.0.0", Attempts: 1}
	}
	for _, test := range []struct {
		name      string
		state     State
		wantVer   string
		wantTrial bool
		wantOut   UpdateState
	}{
		{name: "no record runs the active version", state: base(nil), wantVer: "1.0.0"},
		{name: "pending selects the target as a trial", state: base(record(UpdatePending)),
			wantVer: "2.0.0", wantTrial: true},
		{name: "committed selects the target ordinarily", state: base(record(UpdateCommitted)),
			wantVer: "2.0.0", wantOut: UpdateCommitted},
		{name: "rolled-back selects the previous", state: base(record(UpdateRolledBack)),
			wantVer: "1.0.0", wantOut: UpdateRolledBack},
		{name: "failed selects the previous", state: base(record(UpdateFailed)),
			wantVer: "1.0.0", wantOut: UpdateFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection, err := test.state.Select()
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if selection.Version != test.wantVer {
				t.Errorf("version = %q, want %q", selection.Version, test.wantVer)
			}
			if selection.Trial != test.wantTrial {
				t.Errorf("trial = %t, want %t", selection.Trial, test.wantTrial)
			}
			if selection.Outcome != test.wantOut {
				t.Errorf("outcome = %q, want %q", selection.Outcome, test.wantOut)
			}
		})
	}
}

// Fail closed: every one of these is a file whose selection would be a guess,
// and a supervisor that guessed would run a version the operator did not pick.
func TestInvalidStateHasNoSelection(t *testing.T) {
	for _, test := range []struct {
		name  string
		state State
	}{
		{name: "an unknown schema", state: State{Schema: StateSchema + 1, ActiveVersion: "1.0.0"}},
		{name: "no active version", state: State{Schema: StateSchema}},
		{name: "an active version naming a parent directory",
			state: State{Schema: StateSchema, ActiveVersion: ".."}},
		{name: "an active version with a separator",
			state: State{Schema: StateSchema, ActiveVersion: "a/b"}},
		{name: "a record with no id", state: State{Schema: StateSchema, ActiveVersion: "1.0.0",
			Update: &UpdateRecord{State: UpdatePending, From: "1.0.0", To: "2.0.0"}}},
		{name: "a record in a state nobody defined", state: State{Schema: StateSchema, ActiveVersion: "1.0.0",
			Update: &UpdateRecord{ID: "u1", State: "half-done", From: "1.0.0", To: "2.0.0"}}},
		{name: "a record whose from disagrees with the active version",
			state: State{Schema: StateSchema, ActiveVersion: "1.0.0",
				Update: &UpdateRecord{ID: "u1", State: UpdatePending, From: "0.9.0", To: "2.0.0"}}},
		{name: "a record with a negative attempt count",
			state: State{Schema: StateSchema, ActiveVersion: "1.0.0",
				Update: &UpdateRecord{ID: "u1", State: UpdatePending, From: "1.0.0", To: "2.0.0", Attempts: -1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.state.Select(); err == nil {
				t.Fatal("Select answered a state it should have refused")
			}
			if err := SaveState(testLayout(t), test.state); err == nil {
				t.Fatal("SaveState wrote a state it would later refuse to read")
			}
		})
	}
}

// An unreadable state file is an ERROR, never "no state". A supervisor that
// treated one as fresh would silently adopt its own binary over a committed
// update.
func TestLoadStateFailsClosedOnAFileItCannotRead(t *testing.T) {
	layout := testLayout(t)
	if _, found, err := LoadState(layout); err != nil || found {
		t.Fatalf("LoadState on a fresh install = (found %t, %v), want (false, nil)", found, err)
	}
	writeFile(t, layout.StatePath(), "{ not json")
	if _, _, err := LoadState(layout); err == nil {
		t.Fatal("LoadState accepted a corrupt file")
	}
	writeFile(t, layout.StatePath(), `{"schema":1,"activeVersion":"../escape"}`)
	if _, _, err := LoadState(layout); err == nil {
		t.Fatal("LoadState accepted a version that escapes the versions directory")
	}
}

// Begin collapses the previous record so the file always holds exactly one
// transition, and "previous" always means one thing.
func TestBeginCollapsesTheSettledRecord(t *testing.T) {
	now := time.Unix(0, 0)
	state := State{Schema: StateSchema, ActiveVersion: "1.0.0",
		Update: &UpdateRecord{ID: "u1", State: UpdateCommitted, From: "1.0.0", To: "2.0.0"}}
	next, err := state.Begin("u2", "3.0.0", now)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if next.ActiveVersion != "2.0.0" {
		t.Errorf("active = %q, want the committed target 2.0.0", next.ActiveVersion)
	}
	if next.Update.From != "2.0.0" || next.Update.To != "3.0.0" {
		t.Errorf("record = %+v, want 2.0.0 -> 3.0.0", next.Update)
	}
	// Opening an update is not attempting one. Run counts the attempt at the
	// spawn, so a supervisor killed between the two burns nothing.
	if next.Update.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", next.Update.Attempts)
	}
	// And a rollback from here returns to the version that was actually
	// running, not to the one two updates ago.
	rolled, err := next.Settle(UpdateRolledBack, "trial crashed", now)
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	selection, err := rolled.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Version != "2.0.0" {
		t.Errorf("rollback selected %q, want 2.0.0", selection.Version)
	}
}

func TestBeginRefusesAnUpdateAlreadyInFlight(t *testing.T) {
	state := State{Schema: StateSchema, ActiveVersion: "1.0.0",
		Update: &UpdateRecord{ID: "u1", State: UpdatePending, From: "1.0.0", To: "2.0.0", Attempts: 1}}
	if _, err := state.Begin("u2", "3.0.0", time.Unix(0, 0)); err == nil {
		t.Fatal("Begin opened a second update while one was pending")
	}
	if _, err := state.Begin("u2", "1.0.0", time.Unix(0, 0)); err == nil {
		t.Fatal("Begin accepted a target that is already running")
	}
}

func TestStateRoundTripsThroughDisk(t *testing.T) {
	layout := testLayout(t)
	state, err := Adopt("1.0.0")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if err := SaveState(layout, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, found, err := LoadState(layout)
	if err != nil || !found {
		t.Fatalf("LoadState = (found %t, %v)", found, err)
	}
	if loaded.ActiveVersion != "1.0.0" || loaded.Update != nil {
		t.Fatalf("loaded = %+v", loaded)
	}
}

// A version is one path component. Everything else is a name that could reach
// out of the versions directory, and it arrives from three places only one of
// which this tree writes.
func TestValidVersionRefusesEverythingThatIsNotAComponent(t *testing.T) {
	for _, version := range []string{
		"", " ", "..", ".", "../escape", "a/b", "a" + string(filepath.Separator) + "b",
		"with\nnewline", "with\x00null", " leading", "trailing ",
	} {
		if err := ValidVersion(version); err == nil {
			t.Errorf("ValidVersion(%q) accepted it", version)
		}
	}
	for _, version := range []string{"1.0.0", "dev", "v2.3.4-rc1", "2026.09.01+build"} {
		if err := ValidVersion(version); err != nil {
			t.Errorf("ValidVersion(%q) = %v, want nil", version, err)
		}
	}
}

// The layout is where every path in this feature comes from, and the version
// directory must stay under the versions directory whatever it is handed.
func TestLayoutKeepsVersionsUnderTheVersionsDirectory(t *testing.T) {
	layout := testLayout(t)
	if _, err := layout.VersionDir("../../etc"); err == nil {
		t.Fatal("VersionDir accepted an escaping version")
	}
	dir, err := layout.VersionDir("1.0.0")
	if err != nil {
		t.Fatalf("VersionDir: %v", err)
	}
	if filepath.Dir(dir) != layout.VersionsDir() {
		t.Fatalf("VersionDir = %q, which is not under %q", dir, layout.VersionsDir())
	}
	if _, err := NewLayout("relative/path"); err == nil {
		t.Fatal("NewLayout accepted a relative data directory")
	}
}

// The protocol version is a safety boundary, not a courtesy: a target that
// needs a newer one than the installed supervisor is refused, and the refusal
// names the one command that fixes it.
func TestPreflightCompatibility(t *testing.T) {
	if err := CheckPreflight(Preflight{ProtocolVersion: ProtocolVersion, Version: "2.0.0"}); err != nil {
		t.Fatalf("an equal protocol was refused: %v", err)
	}
	if err := CheckPreflight(Preflight{ProtocolVersion: ProtocolVersion - 1}); err != nil {
		t.Fatalf("an older protocol was refused: %v", err)
	}
	err := CheckPreflight(Preflight{ProtocolVersion: ProtocolVersion + 1})
	if err == nil {
		t.Fatal("a newer protocol was accepted")
	}
	if !strings.Contains(err.Error(), "service update") {
		t.Fatalf("the refusal does not name the remedy: %v", err)
	}
}

func TestParsePreflightReadsTheLastLine(t *testing.T) {
	answer, err := ParsePreflight("some log line\n{\"protocolVersion\":1,\"version\":\"9.9.9\"}\n")
	if err != nil {
		t.Fatalf("ParsePreflight: %v", err)
	}
	if answer.Version != "9.9.9" || answer.ProtocolVersion != 1 {
		t.Fatalf("answer = %+v", answer)
	}
	for _, bad := range []string{"", "   \n", "not json", `{"version":"1.0.0"}`} {
		if _, err := ParsePreflight(bad); err == nil {
			t.Errorf("ParsePreflight(%q) accepted it", bad)
		}
	}
}

// A settled record's outcome is carried ONCE.
//
// The record itself rests in the file until the next update collapses it,
// which can be months of nightly reboots. Before the reported flag existed,
// every one of those boots re-announced the same verdict, so an admin client
// attaching for the first time in June was shown a rollback from March as if
// it had just happened, with no way to dismiss it.
func TestASettledOutcomeIsAnnouncedUntilItIsReported(t *testing.T) {
	for _, settled := range []UpdateState{UpdateCommitted, UpdateRolledBack, UpdateFailed} {
		t.Run(string(settled), func(t *testing.T) {
			state := State{
				Schema: StateSchema, ActiveVersion: "1.0.0",
				Update: &UpdateRecord{
					ID: "upd-1", State: settled, From: "1.0.0", To: "2.0.0",
					Reason: "because",
				},
			}
			first, err := state.Select()
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if first.Outcome != settled || first.UpdateID != "upd-1" || first.Reason != "because" {
				t.Fatalf("selection = %+v, want the outcome announced once", first)
			}
			// The target is what the update was AIMING at, which on a
			// rollback is not the version selected to run.
			if first.Target != "2.0.0" {
				t.Errorf("target = %q, want the version the update was aiming at", first.Target)
			}

			marked, changed, err := state.MarkReported()
			if err != nil || !changed {
				t.Fatalf("MarkReported = (%t, %v), want (true, nil)", changed, err)
			}
			second, err := marked.Select()
			if err != nil {
				t.Fatalf("Select after reporting: %v", err)
			}
			if second.Version != first.Version {
				t.Errorf("version moved after reporting: %q then %q", first.Version, second.Version)
			}
			if second.Outcome != "" || second.UpdateID != "" || second.Reason != "" || second.Target != "" {
				t.Errorf("selection = %+v, want nothing left to announce", second)
			}
			// Idempotent: a child that says hello twice writes nothing twice.
			if _, changed, err := marked.MarkReported(); err != nil || changed {
				t.Errorf("MarkReported twice = (%t, %v), want (false, nil)", changed, err)
			}
		})
	}
}

// A PENDING record is never "reported": it has no outcome yet, and the trial
// it selects must keep being selected until it settles.
func TestMarkReportedRefusesToSettleAPendingRecord(t *testing.T) {
	state := State{
		Schema: StateSchema, ActiveVersion: "1.0.0",
		Update: &UpdateRecord{ID: "upd-1", State: UpdatePending, From: "1.0.0", To: "2.0.0"},
	}
	next, changed, err := state.MarkReported()
	if err != nil || changed {
		t.Fatalf("MarkReported on a pending record = (%t, %v), want (false, nil)", changed, err)
	}
	selection, err := next.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !selection.Trial || selection.UpdateID != "upd-1" {
		t.Fatalf("selection = %+v, want the trial still selected", selection)
	}
}

// A state file written before the reported flag existed carries the same
// schema number, so it must decode as unreported and validate unchanged.
// Bumping the schema for an additive boolean would have made every existing
// install fail closed at boot for a field whose absence has one reading.
func TestAStateFileWithoutTheReportedFieldReadsAsUnreported(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	writeFile(t, layout.StatePath(), `{"schema":1,"activeVersion":"1.0.0",`+
		`"update":{"id":"upd-1","state":"rolled-back","from":"1.0.0","to":"2.0.0","reason":"crashed"}}`)

	state, found, err := LoadState(layout)
	if err != nil || !found {
		t.Fatalf("LoadState = (found %t, %v)", found, err)
	}
	if state.Update.Reported {
		t.Fatal("an absent reported field decoded as true, which would silence a real outcome")
	}
	selection, err := state.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Outcome != UpdateRolledBack || selection.Target != "2.0.0" {
		t.Fatalf("selection = %+v, want the outcome still announced", selection)
	}
}
