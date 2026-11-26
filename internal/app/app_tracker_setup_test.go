package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/dispatchrefusal"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

func TestTrackerSetupMethodsCarryTheirScopeAndRoute(t *testing.T) {
	t.Parallel()
	for name, scope := range map[string]transport.Scope{
		"ProjectTrackerSetup":      transport.ScopeTrackerRead,
		"ProjectTrackerStatuses":   transport.ScopeTrackerRead,
		"ProjectTrackerSiteFields": transport.ScopeTrackerRead,
		"SetProjectTrackerSetup":   transport.ScopeTrackerOperate,
	} {
		i := slices.IndexFunc(transport.GeneratedMethods, func(m transport.MethodMeta) bool { return m.Name == name })
		if i < 0 {
			t.Errorf("%s is not a generated method", name)
			continue
		}
		// The setup is a file of the project's computer.
		if m := transport.GeneratedMethods[i]; m.Scope != scope || m.Route != transport.RouteProject || m.CallerBound || m.Budget != 0 {
			t.Errorf("%s = %+v, want scope %s, the project's route and the default budget", name, m, scope)
		}
	}
}

// setupPath is the fixture project's tracker setup file.
func (f *trackerBoardFixture) setupPath(t *testing.T) string {
	t.Helper()
	p, err := f.a.store.GetProject(f.project)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(project.ConfigDir(f.a.configDir, p.Slug), "tracker.json")
}

// waitChannels waits until an event of each of channels has arrived,
// skipping every other.
func (f *trackerBoardFixture) waitChannels(t *testing.T, channels ...eventchan.Channel) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for len(channels) > 0 {
		select {
		case e := <-f.events.Events():
			channels = slices.DeleteFunc(channels, func(c eventchan.Channel) bool { return string(c) == e.Channel })
		case <-deadline:
			t.Fatalf("no event of %v", channels)
		}
	}
}

// A project's status groups reach its client end to end: a save through
// the bound method is told on tracker:setup_updated, reads the open board
// again, and the board's totals leave out what it leaves out; a page and a
// patch read before the save are refused as stale.
func TestATrackerSetupReachesItsClient(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub := f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return e.ReadAgain && e.Seq > sub.Seq })
	sprint := []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}}
	before, err := f.a.TrackerBoard(ctx, sub.ID, sprint)
	if err != nil || before.Sprints[0].Total.Rows != 2 || before.Groups != "" {
		t.Fatalf("TrackerBoard before a setup = %+v, %v", before, err)
	}
	stored, err := f.a.ProjectTrackerSetup(ctx, f.project)
	if err != nil || stored.Revision != "" {
		t.Fatalf("ProjectTrackerSetup of a project with none = %+v, %v", stored, err)
	}
	// WEB-1 is In Progress (10001), which the project leaves out.
	setup := trackerapp.Setup{StatusGroups: []trackerapp.SiteStatusGroups{{Tracker: store.TrackerKindJira, Site: f.conn.Site, Statuses: map[string]string{"10001": "left_out"}}}}
	save, err := f.a.SetProjectTrackerSetup(ctx, f.project, "", setup)
	if err != nil || !save.Saved || save.Stored.Revision == "" {
		t.Fatalf("SetProjectTrackerSetup = %+v, %v", save, err)
	}
	f.waitChannels(t, eventchan.TrackerBoardUpdated, eventchan.TrackerSetupUpdated)
	after, err := f.a.TrackerBoard(ctx, sub.ID, sprint)
	if err != nil || len(after.Sections[0].Rows) != 2 || after.Sprints[0].Total.Rows != 1 || after.Groups == "" {
		t.Fatalf("TrackerBoard after the save = %+v, %v", after, err)
	}
	old := &trackerapp.SectionCursor{Order: "0", Issue: "10101", Lifetime: before.Lifetime, Revision: before.Revision, Groups: before.Groups}
	if _, err := f.a.TrackerSectionRows(ctx, sub.ID, sprint[0], old); !hasCode(err, "tracker_page_stale") {
		t.Fatalf("a page continued across the save = %v", err)
	}
	held := trackerapp.TotalsAt{Lifetime: before.Lifetime, Revision: before.TotalsRevision, Groups: before.Groups}
	if _, err := f.a.TrackerBoardPatch(ctx, sub.ID, sprint, []string{"10101"}, held); !hasCode(err, "tracker_page_stale") {
		t.Fatalf("a patch of sections read before the save = %v", err)
	}
	// A save over the revision before it is answered, not refused as an
	// error: the answer carries the sentence and what is stored.
	stale, err := f.a.SetProjectTrackerSetup(ctx, f.project, "", trackerapp.Setup{})
	if err != nil || stale.Saved || stale.Refused == "" || stale.Stored.Revision != save.Stored.Revision {
		t.Fatalf("a save over a moved revision = %+v, %v", stale, err)
	}
}

// Each refusal of a setup carries its code, a sentence of what to do,
// and the service's error as its cause; an unreadable file is named.
func TestTrackerSetupRefusalsSayWhatToDo(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	_, err := f.a.SetProjectTrackerSetup(ctx, f.project, "", trackerapp.Setup{StatusGroups: []trackerapp.SiteStatusGroups{
		{Tracker: store.TrackerKindJira, Site: f.conn.Site, Statuses: map[string]string{"10001": "blocked"}},
	}})
	if code, message, _ := errorsx.PublicDetails(err); code != "tracker_setup_refused" || !errors.Is(err, trackerapp.ErrInvalidSetup) ||
		!strings.Contains(message, `group "blocked"`) {
		t.Fatalf("an unknown group = %v (code %q, %q)", err, code, message)
	}
	if _, err := f.a.ProjectTrackerSetup(ctx, "no-such-project"); !hasCode(err, "tracker_project_missing") {
		t.Fatalf("the setup of no project = %v", err)
	}
	if _, err := f.a.ProjectTrackerStatuses(ctx, "no-such-project"); !hasCode(err, "tracker_project_missing") {
		t.Fatalf("the statuses of no project = %v", err)
	}
	path := f.setupPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file that cannot be read is never held, so mending it is read at
	// the next use.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = f.a.ProjectTrackerSetup(ctx, f.project)
	if code, message, _ := errorsx.PublicDetails(err); code != "tracker_setup_unreadable" || !errors.Is(err, trackerapp.ErrSetupUnreadable) ||
		!strings.Contains(message, path) {
		t.Fatalf("an unreadable setup = %v (code %q, %q)", err, code, message)
	}
	if _, err := f.a.ProjectTrackerStatuses(ctx, f.project); !hasCode(err, "tracker_setup_unreadable") {
		t.Fatalf("the statuses of a project with an unreadable setup = %v", err)
	}
	if _, err := f.a.SubscribeTrackerBoard(ctx, f.project, f.conn.ID, "1", nil, true); !hasCode(err, "tracker_setup_unreadable") {
		t.Fatalf("a subscribe of a project with an unreadable setup = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"status_groups": {"version": 2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	newer, err := f.a.ProjectTrackerSetup(ctx, f.project)
	if err != nil || !slices.Equal(newer.Newer, []string{"status_groups"}) {
		t.Fatalf("a setup a newer version saved = %+v, %v", newer, err)
	}
	_, err = f.a.SetProjectTrackerSetup(ctx, f.project, newer.Revision, trackerapp.Setup{StatusGroups: []trackerapp.SiteStatusGroups{
		{Tracker: store.TrackerKindJira, Site: f.conn.Site, Statuses: map[string]string{"10001": "done"}},
	}})
	if !hasCode(err, "tracker_setup_newer") || !errors.Is(err, trackerapp.ErrSetupNewer) {
		t.Fatalf("a save of a section a newer version saved = %v", err)
	}
	// Every sentence differs from the board refusals'.
	sentences := map[string]bool{}
	for _, err := range []error{
		&trackerapp.SetupFileError{Path: path, Reason: "x"}, trackerapp.ErrSetupNewer, &trackerapp.SetupError{Reason: "x"}, trackerapp.ErrStalePage,
		&trackerapp.SetupWriteError{Path: path, Err: errors.New("x")},
		trackerapp.ErrInvalidBoards, trackerapp.ErrBoardsChanged, trackerapp.ErrProjectNotFound,
	} {
		_, message := trackerRefusal(err)
		if sentences[message] {
			t.Errorf("%v shares the sentence %q", err, message)
		}
		sentences[message] = true
	}
}

// A save whose write may or may not have happened says so, names the
// file and why, and says to read the setup again before changing it
// again: the read answers what the file holds.
func TestATrackerSetupWriteThatMayHaveHappenedSaysSo(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	groups := func(group string) trackerapp.Setup {
		return trackerapp.Setup{StatusGroups: []trackerapp.SiteStatusGroups{
			{Tracker: store.TrackerKindJira, Site: f.conn.Site, Statuses: map[string]string{"10001": group}},
		}}
	}
	save, err := f.a.SetProjectTrackerSetup(ctx, f.project, "", groups("done"))
	if err != nil || !save.Saved {
		t.Fatalf("the first save = %+v, %v", save, err)
	}
	path := f.setupPath(t)
	testutil.UnsyncableDir(t, filepath.Dir(path))
	_, err = f.a.SetProjectTrackerSetup(ctx, f.project, save.Stored.Revision, groups("left_out"))
	code, message, _ := errorsx.PublicDetails(err)
	if code != "tracker_setup_unsure" || !errors.Is(err, trackerapp.ErrSetupWriteUnsure) || !strings.Contains(message, "may or may not be stored") ||
		!strings.Contains(message, path) || !strings.Contains(message, "read the project's tracker setup again before you change it again") ||
		strings.Contains(message, "shown") {
		t.Fatalf("a save whose sync failed = %v (code %q, %q)", err, code, message)
	}
	got, err := f.a.ProjectTrackerSetup(ctx, f.project)
	if err != nil || len(got.Setup.StatusGroups) != 1 || got.Setup.StatusGroups[0].Statuses["10001"] != "left_out" {
		t.Fatalf("the setup after = %+v, %v; want the file's, which holds the change", got, err)
	}
}

// A setup argument this computer cannot read whole is refused before the
// method runs, as any argument it cannot read is: null, anything but an
// object, a section this build does not read (a newer app's), and a field
// it does not know. Nothing is written, and the stored setup is as it
// was; {} is the empty setup, which removes the file.
func TestASetupArgumentThisComputerCannotReadIsRefused(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	rpc := newAppDispatch(t, f.a)
	path := f.setupPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A newer app saved card_marks, which this build keeps as it is.
	file := `{"card_marks": {"version": 1, "marks": []}, "status_groups": {"version": 1, "sites": [{"tracker": "jira", "site": "` +
		f.conn.Site + `", "statuses": {"10001": "done"}}]}}`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	stored, err := f.a.ProjectTrackerSetup(ctx, f.project)
	if err != nil {
		t.Fatal(err)
	}
	site := `{"tracker": "jira", "site": "` + f.conn.Site + `", "statuses": {"10001": "todo"}`
	for name, arg := range map[string]string{
		"null":                     `null`,
		"a list":                   `[]`,
		"a string":                 `"status_groups"`,
		"a section of a newer app": `{"card_marks": {"version": 1, "marks": ["x"]}, "status_groups": [` + site + `}]}`,
		"an unknown field":         `{"status_groups": [` + site + `, "colour": "red"}]}`,
	} {
		_, frame := rpc.call("SetProjectTrackerSetup", f.project, stored.Revision, json.RawMessage(arg))
		if frame == nil || frame.Code != transport.ErrCodeBadParams || frame.Message != dispatchrefusal.Unreadable {
			t.Errorf("%s: answered %+v; want the refusal of an argument this computer cannot read", name, frame)
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != file {
		t.Fatalf("the refused saves changed the file: %q, %v", data, err)
	}
	result, frame := rpc.call("SetProjectTrackerSetup", f.project, stored.Revision, json.RawMessage(`{}`))
	var save trackerapp.SetupSave
	if frame != nil || json.Unmarshal(result, &save) != nil || !save.Saved || len(save.Stored.Setup.StatusGroups) != 0 ||
		!slices.Equal(save.Stored.Newer, []string{"card_marks"}) {
		t.Fatalf("an empty setup = %s, %+v; want saved, keeping the newer app's section", result, frame)
	}
}

// A site the setup refuses is never repeated: a pasted address can hold a
// credential, so neither the refusal of a save nor that of a file written
// by hand carries it, in its sentence, its detail or the log. Not
// parallel: it reads the process's log.
func TestARefusedSiteIsNeverRepeated(t *testing.T) {
	logs := testutil.CaptureLogOutput(t)
	f := newTrackerBoardFixture(t)
	rpc := newAppDispatch(t, f.a)
	const marker = "tok3n-MARKER"
	told := func(what string, frame *transport.FrameError, code string) {
		t.Helper()
		if frame == nil || frame.Code != code {
			t.Fatalf("%s = %+v; want %s", what, frame, code)
		}
		text := frame.Message
		if frame.Detail != nil {
			text += strings.Join(frame.Detail.Chain, "\n")
		}
		if strings.Contains(text, marker) {
			t.Errorf("%s repeats the site: %s", what, text)
		}
	}
	for _, site := range []string{"https://user1:" + marker + "@a.example.com", "a.example.com/?token=" + marker} {
		setup := `{"status_groups": [{"tracker": "jira", "site": "` + site + `", "statuses": {"10001": "done"}}]}`
		_, frame := rpc.call("SetProjectTrackerSetup", f.project, "", json.RawMessage(setup))
		told("a save of "+site, frame, "tracker_setup_refused")

		path := f.setupPath(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		file := `{"status_groups": {"version": 1, "sites": [{"tracker": "jira", "site": "` + site + `", "statuses": {"10001": "done"}}]}}`
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		_, frame = rpc.call("ProjectTrackerSetup", f.project)
		told("a read of a file naming "+site, frame, "tracker_setup_unreadable")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(logs.String(), marker) {
		t.Errorf("the log repeats a refused site:\n%s", logs.String())
	}
}

// The setup a read answers is the value a save takes: a client that sends
// back the read's setup, changed or not, with the read's revision, saves
// it, as a settings screen would.
func TestTheSetupAReadAnswersIsSavedBack(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	rpc := newAppDispatch(t, f.a)
	setup := `{"status_groups": [{"tracker": "jira", "site": "` + f.conn.Site + `", "statuses": {"10001": "left_out"}}], "ticket_pins": []}`
	if _, frame := rpc.call("SetProjectTrackerSetup", f.project, "", json.RawMessage(setup)); frame != nil {
		t.Fatalf("the first save: %+v", frame)
	}
	result, frame := rpc.call("ProjectTrackerSetup", f.project)
	var read struct {
		Revision string          `json:"revision"`
		Setup    json.RawMessage `json:"setup"`
	}
	if frame != nil || json.Unmarshal(result, &read) != nil {
		t.Fatalf("the read = %s, %+v", result, frame)
	}
	result, frame = rpc.call("SetProjectTrackerSetup", f.project, read.Revision, read.Setup)
	var save trackerapp.SetupSave
	if frame != nil || json.Unmarshal(result, &save) != nil || !save.Saved || save.Stored.Revision != read.Revision {
		t.Fatalf("the read's setup saved back = %s, %+v; want it saved as it was (read %s)", result, frame, read.Setup)
	}
}

// A site's fields to pin, on the fake Jira site, are the site's fields
// a ticket's detail lists: none of the card's, the description, the
// sprint or the rank. A site no connection reaches is refused with the
// sentence that says so.
func TestProjectTrackerSiteFieldsListWhatATicketCanPin(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	got, err := f.a.ProjectTrackerSiteFields(t.Context(), f.project, store.TrackerKindJira, f.conn.Site)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, field := range got {
		ids[field.ID] = true
	}
	for _, id := range []string{"summary", "status", "assignee", "description", "customfield_10020", "customfield_10019"} {
		if ids[id] {
			t.Errorf("%s is offered to pin", id)
		}
	}
	for _, id := range []string{"reporter", "duedate", "components"} {
		if !ids[id] {
			t.Errorf("%s is not offered to pin: %+v", id, got)
		}
	}
	other := "https://other.example.atlassian.net"
	_, err = f.a.ProjectTrackerSiteFields(t.Context(), f.project, store.TrackerKindJira, other)
	if code, message, _ := errorsx.PublicDetails(err); code != "tracker_site_unreached" ||
		message != "no tracker connection of this computer reaches "+other+"; add one in Settings > Issues > Connections to find its fields, or take its pins off here" {
		t.Fatalf("a site no connection reaches = %v (code %q, %q)", err, code, message)
	}
}
