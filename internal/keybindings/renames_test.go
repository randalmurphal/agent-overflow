package keybindings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	oldFindWhen      = "hasActiveThread && !terminalFocus"
	oldInterruptWhen = "hasActiveThread && (turnActive || sendInFlight || hasPendingPrompt) && !anyModalOpen"
)

// loadOverrides writes user as the override file and loads it through Get.
func loadOverrides(t *testing.T, user []Keybinding) []Keybinding {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := svc.Get()
	if res.LoadError != "" {
		t.Fatalf("LoadError = %q", res.LoadError)
	}
	return res.Bindings
}

func rowsFor(bindings []Keybinding, command string) []Keybinding {
	var out []Keybinding
	for _, b := range bindings {
		if b.Command == command {
			out = append(out, b)
		}
	}
	return out
}

func defaultRow(t *testing.T, id string) Keybinding {
	t.Helper()
	for _, d := range Defaults {
		if d.DefaultID == id {
			return d
		}
	}
	t.Fatalf("no default %s", id)
	return Keybinding{}
}

// A choice stored for search.in-thread, in every override format, is the
// same choice for find.in-pane: a custom chord replaces the shipped mod+f
// and an explicit unbinding keeps it unbound.
func TestRenamedFindCommandKeepsStoredChoices(t *testing.T) {
	find := defaultRow(t, "find.in-pane")
	cases := []struct {
		name string
		user Keybinding
	}{
		{"default id", Keybinding{Key: "mod+alt+f", Command: "search.in-thread", When: oldFindWhen, DefaultID: "search.in-thread", DefaultKey: "mod+f"}},
		{"default key", Keybinding{Key: "mod+alt+f", Command: "search.in-thread", When: oldFindWhen, DefaultKey: "mod+f"}},
		{"command and when", Keybinding{Key: "mod+alt+f", Command: "search.in-thread", When: oldFindWhen}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := loadOverrides(t, []Keybinding{tc.user})
			if old := rowsFor(got, "search.in-thread"); len(old) != 0 {
				t.Fatalf("old command survived: %+v", old)
			}
			rows := rowsFor(got, "find.in-pane")
			want := Keybinding{Key: "mod+alt+f", Command: "find.in-pane", When: find.When, DefaultID: "find.in-pane", DefaultKey: "mod+f"}
			if len(rows) != 1 || rows[0] != want {
				t.Fatalf("find rows = %+v, want [%+v]", rows, want)
			}
		})
	}
	t.Run("unbound", func(t *testing.T) {
		got := loadOverrides(t, []Keybinding{{Key: Unbound, Command: "search.in-thread", When: oldFindWhen, DefaultID: "search.in-thread", DefaultKey: "mod+f"}})
		rows := rowsFor(got, "find.in-pane")
		if len(rows) != 1 || !IsUnbound(rows[0]) || rows[0].DefaultID != "find.in-pane" {
			t.Fatalf("find rows = %+v, want one unbound find.in-pane row", rows)
		}
	})
	t.Run("extra binding keeps its own context", func(t *testing.T) {
		got := loadOverrides(t, []Keybinding{{Key: "mod+g", Command: "search.in-thread", When: "hasActiveThread"}})
		rows := rowsFor(got, "find.in-pane")
		if len(rows) != 2 || rows[0].Key != "mod+f" || rows[1] != (Keybinding{Key: "mod+g", Command: "find.in-pane", When: "hasActiveThread"}) {
			t.Fatalf("find rows = %+v, want the shipped row and the extra mod+g", rows)
		}
	})
}

// The shipped interrupt context changed, so an override stored under the
// old one still names that row: it takes the new context with its chord.
func TestChangedInterruptContextKeepsStoredChoices(t *testing.T) {
	interrupt := defaultRow(t, "thread.interrupt")
	for _, user := range []Keybinding{
		{Key: "mod+.", Command: "thread.interrupt", When: oldInterruptWhen, DefaultID: "thread.interrupt", DefaultKey: "esc"},
		{Key: "mod+.", Command: "thread.interrupt", When: oldInterruptWhen, DefaultKey: "esc"},
		{Key: "mod+.", Command: "thread.interrupt", When: oldInterruptWhen},
	} {
		got := rowsFor(loadOverrides(t, []Keybinding{user}), "thread.interrupt")
		want := Keybinding{Key: "mod+.", Command: "thread.interrupt", When: interrupt.When, DefaultID: "thread.interrupt", DefaultKey: "esc"}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("override %+v: interrupt rows = %+v, want [%+v]", user, got, want)
		}
	}
}

// Escape closes an open find before it interrupts a turn, and never while a
// modal surface (settings, a dialog) owns the press.
func TestEscapeClosesFindBeforeInterrupt(t *testing.T) {
	findAt, interruptAt := -1, -1
	for i, d := range Defaults {
		switch d.DefaultID {
		case "find.close":
			findAt = i
		case "thread.interrupt":
			interruptAt = i
		}
	}
	if findAt < 0 || interruptAt < 0 {
		t.Fatalf("find.close at %d, thread.interrupt at %d", findAt, interruptAt)
	}
	find, interrupt := Defaults[findAt], Defaults[interruptAt]
	if find.Key != "esc" || find.When != "paneFindOpen && !anyModalOpen" {
		t.Fatalf("find.close = %+v", find)
	}
	// Dispatch walks the list in reverse: the later row is asked first.
	if findAt < interruptAt {
		t.Fatalf("find.close (%d) must follow thread.interrupt (%d)", findAt, interruptAt)
	}
	if interrupt.When != "hasActiveThread && (turnActive || sendInFlight || hasPendingPrompt) && !anyModalOpen && !paneFindOpen" {
		t.Fatalf("thread.interrupt when = %q", interrupt.When)
	}
}
