package trackerapp

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker/tempo"
)

func TestAddStoresTheConnectionAndAnEmptyKeyFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	if conn.Site != jiraSiteAt || conn.Account != jiraEmail || !conn.Enabled || conn.CreatedAt != 1_800_000_000_000 {
		t.Fatalf("added %+v", conn.TrackerConnection)
	}
	if want := filepath.Join(f.creds, "tracker-"+conn.ID); conn.KeyFilePath != want {
		t.Fatalf("key file %q, want %q", conn.KeyFilePath, want)
	}
	if conn.KeyFile.State != keyfile.StateEmpty || conn.Problem != nil {
		t.Fatalf("a new connection's key file is %+v, problem %v", conn.KeyFile, conn.Problem)
	}
	info, err := os.Stat(conn.KeyFilePath)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Fatalf("key file %v, %v", info, err)
	}
	if f.changes.Load() != 1 {
		t.Fatalf("Changed %d times, want 1", f.changes.Load())
	}
	list, err := f.svc.Connections()
	if err != nil || len(list) != 1 || list[0].ID != conn.ID || list[0].KeyFile.State != keyfile.StateEmpty {
		t.Fatalf("Connections = %+v, %v", list, err)
	}
}

func TestAddTwiceIsADuplicate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addJira()
	_, err := f.svc.Add(NewConnection{Kind: store.TrackerKindJira, Site: jiraSiteAt, Account: jiraEmail})
	if !errors.Is(err, store.ErrTrackerConnectionExists) {
		t.Fatalf("the second Add = %v, want a duplicate", err)
	}
	entries, err := os.ReadDir(f.creds)
	if err != nil || len(entries) != 1 {
		t.Fatalf("key files %v, %v; want the first connection's alone", entries, err)
	}
	if f.changes.Load() != 1 {
		t.Fatalf("a refused Add told Changed (%d)", f.changes.Load())
	}
}

func TestAddRefusesBeforeTheStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.svc.Add(NewConnection{Kind: store.TrackerKindJira, Site: "http://example.atlassian.net", Account: jiraEmail}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Add = %v, want ErrInvalid", err)
	}
	if rows, _ := f.st.ListTrackerConnections(); len(rows) != 0 {
		t.Fatalf("a refused Add stored %+v", rows)
	}
}

// A key file that cannot be made deletes the row the Add stored.
func TestAddDeletesTheRowWhenTheKeyFileCannotBeMade(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.WriteFile(f.creds, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Add(NewConnection{Kind: store.TrackerKindLinear, Site: "example"})
	if err == nil {
		t.Fatal("Add succeeded without a key file")
	}
	if rows, listErr := f.st.ListTrackerConnections(); listErr != nil || len(rows) != 0 {
		t.Fatalf("rows after the failed Add: %+v, %v", rows, listErr)
	}
	if f.changes.Load() != 0 {
		t.Fatalf("a failed Add told Changed (%d)", f.changes.Load())
	}
}

func TestTempoBelongsToOneJiraConnection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	lin := f.addLinear()
	if _, err := f.svc.Add(NewConnection{Kind: store.TrackerKindTempo, Site: tempo.EUBaseURL, ParentID: "missing"}); !errors.Is(err, store.ErrInvalidTrackerConnection) {
		t.Errorf("a Tempo connection of a missing parent = %v", err)
	}
	if _, err := f.svc.Add(NewConnection{Kind: store.TrackerKindTempo, Site: tempo.EUBaseURL, ParentID: lin.ID}); !errors.Is(err, store.ErrInvalidTrackerConnection) {
		t.Errorf("a Tempo connection of a Linear one = %v", err)
	}
	f.addTempo(jira.ID)
	if _, err := f.svc.Add(NewConnection{Kind: store.TrackerKindTempo, Site: tempo.USBaseURL, ParentID: jira.ID}); !errors.Is(err, store.ErrTrackerConnectionExists) {
		t.Errorf("a second Tempo connection of one Jira connection = %v", err)
	}
}

func TestRenameAndTokenExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	got, err := f.svc.Rename(conn.ID, "  Work  ")
	if err != nil || got.Label != "Work" || f.changes.Load() != 2 {
		t.Fatalf("Rename = %+v, %v (changes %d)", got.TrackerConnection, err, f.changes.Load())
	}
	if _, err := f.svc.Rename(conn.ID, "Work"); err != nil || f.changes.Load() != 2 {
		t.Fatalf("an unchanged Rename = %v, told Changed (%d)", err, f.changes.Load())
	}
	if _, err := f.svc.Rename(conn.ID, "a\x00b"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Rename to a control character = %v", err)
	}
	got, err = f.svc.SetTokenExpiry(conn.ID, 1_900_000_000_000)
	if err != nil || got.TokenExpiresAt != 1_900_000_000_000 || f.changes.Load() != 3 {
		t.Fatalf("SetTokenExpiry = %+v, %v", got.TrackerConnection, err)
	}
	got, err = f.svc.SetTokenExpiry(conn.ID, 0)
	if err != nil || got.TokenExpiresAt != 0 || f.changes.Load() != 4 {
		t.Fatalf("clearing the expiry = %+v, %v", got.TrackerConnection, err)
	}
	if _, err := f.svc.SetTokenExpiry(conn.ID, -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a negative expiry = %v", err)
	}
	for name, call := range map[string]func() error{
		"Rename":         func() error { _, err := f.svc.Rename("missing", "x"); return err },
		"SetTokenExpiry": func() error { _, err := f.svc.SetTokenExpiry("missing", 1); return err },
		"SetEnabled":     func() error { _, err := f.svc.SetEnabled("missing", false); return err },
		"Verify":         func() error { _, err := f.svc.Verify(t.Context(), "missing"); return err },
		"Remove":         func() error { return f.svc.Remove("missing") },
		"PrepareKeyFile": func() error { _, err := f.svc.PrepareKeyFile("missing"); return err },
		"TokenPage":      func() error { _, err := f.svc.TokenPage("missing"); return err },
		"Provider":       func() error { _, err := f.svc.Provider(t.Context(), "missing"); return err },
	} {
		if err := call(); !isNotFound(err) || !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s of a missing connection = %v, want ErrNotFound matching sql.ErrNoRows", name, err)
		}
	}
}

func TestDisableThenEnable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	f.jira.On("GET", myselfPath, myself(t, jiraToken))
	if v := f.verify(conn.ID); v.Problem != nil || v.VerifiedAt == 0 || !f.inUse(conn.ID) {
		t.Fatalf("Verify = %+v", v)
	}
	before := f.changes.Load()

	off, err := f.svc.SetEnabled(conn.ID, false)
	if err != nil || off.Enabled || f.inUse(conn.ID) || f.changes.Load() != before+1 {
		t.Fatalf("disable = %+v, %v (in use %v)", off.TrackerConnection, err, f.inUse(conn.ID))
	}
	if off.KeyFile.State != keyfile.StatePresent {
		t.Fatalf("disabling touched the key file: %+v", off.KeyFile)
	}
	if _, err := f.svc.SetEnabled(conn.ID, false); err != nil || f.changes.Load() != before+1 {
		t.Fatalf("disabling twice = %v, told Changed (%d)", err, f.changes.Load()-before)
	}
	if _, err := f.svc.Provider(t.Context(), conn.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Provider of a connection turned off = %v", err)
	}
	if _, err := f.svc.Verify(t.Context(), conn.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Verify of a connection turned off = %v", err)
	}
	if f.inUse(conn.ID) {
		t.Fatal("a refused call built a client for a connection turned off")
	}

	on, err := f.svc.SetEnabled(conn.ID, true)
	if err != nil || !on.Enabled || f.inUse(conn.ID) || f.changes.Load() != before+2 {
		t.Fatalf("enable = %+v, %v", on.TrackerConnection, err)
	}
	f.verify(conn.ID)
	if got := f.jira.Count("GET", myselfPath); got != 2 {
		t.Fatalf("the Jira fake saw %d sign-in checks, want one before and one after", got)
	}
}

// Disabling a connection forgets its problem; the next failure brings
// one back.
func TestDisableForgetsTheProblem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	if v := f.verify(conn.ID); v.Problem == nil || v.Problem.Kind != FailureSetup {
		t.Fatalf("verifying an empty key file = %+v", v.Problem)
	}
	before := f.changes.Load()
	off, err := f.svc.SetEnabled(conn.ID, false)
	if err != nil || off.Problem != nil || f.changes.Load() != before+1 {
		t.Fatalf("disable = %+v, %v", off.Problem, err)
	}
}

func TestRemoveTwice(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	f.writeToken(conn, linearKey)
	f.linear.Expect(lineartestViewer("example"))
	f.verify(conn.ID)
	if err := f.svc.Remove(conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(conn.KeyFilePath); !os.IsNotExist(err) {
		t.Fatalf("key file after Remove: %v", err)
	}
	if _, err := f.st.GetTrackerConnection(conn.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("row after Remove: %v", err)
	}
	if f.inUse(conn.ID) {
		t.Fatal("a removed connection keeps its client")
	}
	before := f.changes.Load()
	if err := f.svc.Remove(conn.ID); !isNotFound(err) || f.changes.Load() != before {
		t.Fatalf("the second Remove = %v (changes %d)", err, f.changes.Load()-before)
	}
}

func TestRemovingJiraRemovesItsTempo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	other := f.add(NewConnection{Kind: store.TrackerKindJira, Site: "other.atlassian.net", Account: jiraEmail})
	tmp := f.addTempo(jira.ID)
	f.writeToken(tmp, tempoToken)
	f.tempo.On("GET", settings, nil, "", 200, `{}`)
	f.verify(tmp.ID)
	if err := f.svc.Remove(jira.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Connection{jira, tmp} {
		if _, err := os.Stat(c.KeyFilePath); !os.IsNotExist(err) {
			t.Errorf("key file of %s after Remove: %v", c.Kind, err)
		}
		if _, err := f.st.GetTrackerConnection(c.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("row of %s after Remove: %v", c.Kind, err)
		}
	}
	if f.inUse(tmp.ID) {
		t.Fatal("the removed Tempo connection keeps its client")
	}
	if _, err := os.Stat(other.KeyFilePath); err != nil {
		t.Fatalf("another connection's key file went with it: %v", err)
	}
}

func TestPrepareKeyFileMakesAMissingFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	before := f.changes.Load()
	if got, err := f.svc.PrepareKeyFile(conn.ID); err != nil || got.KeyFile.State != keyfile.StateEmpty || f.changes.Load() != before {
		t.Fatalf("PrepareKeyFile of an existing file = %+v, %v (changes %d)", got.KeyFile, err, f.changes.Load()-before)
	}
	if err := os.Remove(conn.KeyFilePath); err != nil {
		t.Fatal(err)
	}
	if got, err := f.svc.PrepareKeyFile(conn.ID); err != nil || got.KeyFile.State != keyfile.StateEmpty || f.changes.Load() != before+1 {
		t.Fatalf("PrepareKeyFile of a missing file = %+v, %v", got.KeyFile, err)
	}
}

func TestTokenPageOfEachConnection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	tmp := f.addTempo(jira.ID)
	lin := f.addLinear()
	for _, tc := range []struct{ id, want string }{
		{jira.ID, jiraTokenPage},
		{tmp.ID, jiraSiteAt},
		{lin.ID, "https://linear.app/settings/account/security"},
	} {
		if got, err := f.svc.TokenPage(tc.id); err != nil || got != tc.want {
			t.Errorf("TokenPage = %q, %v; want %q", got, err, tc.want)
		}
	}
}
