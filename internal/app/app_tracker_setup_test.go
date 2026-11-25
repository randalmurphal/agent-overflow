package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

func TestTrackerSetupMethodsCarryTheirScopeAndRoute(t *testing.T) {
	t.Parallel()
	for name, scope := range map[string]transport.Scope{
		"ProjectTrackerSetup":    transport.ScopeTrackerRead,
		"SetProjectTrackerSetup": transport.ScopeTrackerOperate,
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
		trackerapp.ErrInvalidBoards, trackerapp.ErrBoardsChanged, trackerapp.ErrProjectNotFound,
	} {
		_, message := trackerRefusal(err)
		if sentences[message] {
			t.Errorf("%v shares the sentence %q", err, message)
		}
		sentences[message] = true
	}
}
