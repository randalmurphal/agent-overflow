//go:build unix

package trackerapp

import (
	"database/sql"
	"errors"
	"os"
	"testing"
)

// A key file that cannot be removed stops the removal with both rows
// kept: the Jira connection's own key file is gone and its row stays, the
// order that never leaves a key file with no row. Removing again, once
// the file can go, finishes it.
func TestRemoveKeepsTheRowsWhenAKeyFileCannotBeRemoved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	tmp := f.addTempo(jira.ID)
	// keyfile refuses to remove a folder standing at the key file's name.
	if err := os.Remove(tmp.KeyFilePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp.KeyFilePath, 0o700); err != nil {
		t.Fatal(err)
	}
	before := f.changes.Load()
	if err := f.svc.Remove(jira.ID); err == nil {
		t.Fatal("Remove succeeded with a key file it could not remove")
	}
	for _, c := range []Connection{jira, tmp} {
		if _, err := f.st.GetTrackerConnection(c.ID); err != nil {
			t.Fatalf("the %s row went: %v", c.Kind, err)
		}
	}
	if _, err := os.Stat(jira.KeyFilePath); !os.IsNotExist(err) {
		t.Fatalf("the Jira key file: %v", err)
	}
	if f.changes.Load() != before+1 {
		t.Fatal("a removal that changed the key files told nobody")
	}
	list, err := f.svc.Connections()
	if err != nil || len(list) != 2 {
		t.Fatalf("Connections after the failed removal = %+v, %v", list, err)
	}

	if err := os.Remove(tmp.KeyFilePath); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Remove(jira.ID); err != nil {
		t.Fatalf("removing again = %v", err)
	}
	for _, c := range []Connection{jira, tmp} {
		if _, err := f.st.GetTrackerConnection(c.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("the %s row after removing again: %v", c.Kind, err)
		}
	}
}
