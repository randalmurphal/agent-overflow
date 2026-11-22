package trackerapp

import (
	"os"
	"testing"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// holdFixture is a mirror fixture that records each provider it builds
// and counts its store calls.
type holdFixture struct {
	*mirrorFixture
	cs *countingStore
	// builds are the connection rows providers were built for, and
	// whether each had a Tempo client.
	builds []build
}

type build struct {
	row   store.TrackerConnection
	tempo bool
}

func newHoldFixture(t *testing.T) *holdFixture {
	t.Helper()
	m := newMirrorFixture(t)
	h := &holdFixture{mirrorFixture: m, cs: newCountingStore(m.st)}
	m.svc.store = h.cs
	m.svc.makeProvider = func(row store.TrackerConnection, _ *clients, tempo *clients) (tracker.Provider, error) {
		h.builds = append(h.builds, build{row: row, tempo: tempo != nil})
		return m.p, nil
	}
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	return h
}

// tick runs a delta tick and returns its outcome and store calls.
func (h *holdFixture) tick(t *testing.T) (Outcome, map[string]int) {
	t.Helper()
	h.cs.take()
	out := must[Outcome](t)(h.svc.tickDelta(t.Context(), h.scope(), Background))
	return out, h.cs.take()
}

// lastBuild is the newest provider build.
func (h *holdFixture) lastBuild(t *testing.T) build {
	t.Helper()
	if len(h.builds) == 0 {
		t.Fatal("no provider was built")
	}
	return h.builds[len(h.builds)-1]
}

// held runs two ticks: the first builds the hold, the second reuses it,
// reading the sync state and nothing of the connection.
func (h *holdFixture) held(t *testing.T) {
	t.Helper()
	if out, _ := h.tick(t); out.Failure != nil || out.Ended {
		t.Fatalf("first tick %+v", out)
	}
	built := len(h.builds)
	out, calls := h.tick(t)
	if out.Failure != nil || out.Ended || len(h.builds) != built {
		t.Fatalf("second tick %+v, %d builds", out, len(h.builds))
	}
	for _, name := range []string{"GetTrackerConnection", "ListTrackerConnections", "GetOrCreateMirrorRoot"} {
		if calls[name] != 0 {
			t.Fatalf("a held tick read %s: %v", name, calls)
		}
	}
	if calls["GetMirrorSync"] != 1 {
		t.Fatalf("a held tick's store calls %v", calls)
	}
}

// Each change trackerapp makes to a connection drops the hold its ticks
// reuse: the tick after it reads the connection again and builds its
// provider from the changed row.
func TestTickAfterAChangeUsesTheChangedConnection(t *testing.T) {
	t.Parallel()
	rebuilt := func(t *testing.T, h *holdFixture, before int, calls map[string]int) build {
		t.Helper()
		if len(h.builds) != before+1 || calls["GetTrackerConnection"] != 1 {
			t.Fatalf("%d builds after %d, store calls %v", len(h.builds), before, calls)
		}
		return h.lastBuild(t)
	}
	cases := []struct {
		name   string
		setup  func(t *testing.T, h *holdFixture)
		change func(t *testing.T, h *holdFixture)
		check  func(t *testing.T, h *holdFixture, before int, out Outcome, calls map[string]int)
	}{
		{"rename", nil, func(t *testing.T, h *holdFixture) {
			must[Connection](t)(h.svc.Rename(h.conn.ID, "Renamed"))
		}, func(t *testing.T, h *holdFixture, before int, _ Outcome, calls map[string]int) {
			if b := rebuilt(t, h, before, calls); b.row.Label != "Renamed" {
				t.Fatalf("the tick used label %q", b.row.Label)
			}
		}},
		{"token expiry", nil, func(t *testing.T, h *holdFixture) {
			must[Connection](t)(h.svc.SetTokenExpiry(h.conn.ID, 1_900_000_000_000))
		}, func(t *testing.T, h *holdFixture, before int, _ Outcome, calls map[string]int) {
			if b := rebuilt(t, h, before, calls); b.row.TokenExpiresAt != 1_900_000_000_000 {
				t.Fatalf("the tick used token expiry %d", b.row.TokenExpiresAt)
			}
		}},
		{"key file made again", nil, func(t *testing.T, h *holdFixture) {
			if err := os.Remove(h.conn.KeyFilePath); err != nil {
				t.Fatal(err)
			}
			if c := must[Connection](t)(h.svc.PrepareKeyFile(h.conn.ID)); c.KeyFile.State != keyfile.StateEmpty {
				t.Fatalf("key file %+v", c.KeyFile)
			}
		}, func(t *testing.T, h *holdFixture, before int, _ Outcome, calls map[string]int) {
			rebuilt(t, h, before, calls)
		}},
		{"verify", func(t *testing.T, h *holdFixture) {
			h.jira.On("GET", myselfPath, myself(t, jiraToken))
			h.writeToken(h.conn, jiraToken)
		}, func(t *testing.T, h *holdFixture) {
			h.verify(h.conn.ID)
		}, func(t *testing.T, h *holdFixture, before int, _ Outcome, calls map[string]int) {
			if b := rebuilt(t, h, before, calls); b.row.VerifiedAt == 0 {
				t.Fatalf("the tick used an unverified row %+v", b.row)
			}
		}},
		{"tempo added", nil, func(t *testing.T, h *holdFixture) {
			h.addTempo(h.conn.ID)
		}, func(t *testing.T, h *holdFixture, before int, _ Outcome, calls map[string]int) {
			if b := rebuilt(t, h, before, calls); !b.tempo {
				t.Fatal("the tick's provider has no Tempo client")
			}
		}},
		{"tempo removed", func(t *testing.T, h *holdFixture) {
			h.addTempo(h.conn.ID)
		}, func(t *testing.T, h *holdFixture) {
			child, ok := must2[store.TrackerConnection, bool](t)(h.svc.tempoOf(h.conn.ID))
			if !ok || !h.lastBuild(t).tempo {
				t.Fatal("no Tempo connection to remove")
			}
			if err := h.svc.Remove(child.ID); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, h *holdFixture, before int, _ Outcome, calls map[string]int) {
			if b := rebuilt(t, h, before, calls); b.tempo {
				t.Fatal("the tick's provider kept the removed Tempo client")
			}
		}},
		{"tempo turned off", func(t *testing.T, h *holdFixture) {
			h.addTempo(h.conn.ID)
		}, func(t *testing.T, h *holdFixture) {
			child, _ := must2[store.TrackerConnection, bool](t)(h.svc.tempoOf(h.conn.ID))
			must[Connection](t)(h.svc.SetEnabled(child.ID, false))
		}, func(t *testing.T, h *holdFixture, before int, _ Outcome, calls map[string]int) {
			if b := rebuilt(t, h, before, calls); b.tempo {
				t.Fatal("the tick's provider kept the Tempo client turned off")
			}
		}},
		{"turned off", nil, func(t *testing.T, h *holdFixture) {
			must[Connection](t)(h.svc.SetEnabled(h.conn.ID, false))
		}, func(t *testing.T, h *holdFixture, before int, out Outcome, calls map[string]int) {
			if !out.Ended || len(h.builds) != before || calls["GetTrackerConnection"] != 1 {
				t.Fatalf("tick %+v, %d builds after %d, store calls %v", out, len(h.builds), before, calls)
			}
		}},
		{"turned off and on", nil, func(t *testing.T, h *holdFixture) {
			must[Connection](t)(h.svc.SetEnabled(h.conn.ID, false))
			must[Connection](t)(h.svc.SetEnabled(h.conn.ID, true))
		}, func(t *testing.T, h *holdFixture, before int, out Outcome, calls map[string]int) {
			rebuilt(t, h, before, calls)
			// The turn off detached the mirror: the tick made a new root and
			// read the board into it.
			if out.Ended || calls["GetOrCreateMirrorRoot"] != 1 || len(h.rowIDs(testBoard)) != 1 {
				t.Fatalf("tick %+v, store calls %v, rows %v", out, calls, h.rowIDs(testBoard))
			}
		}},
		{"removed", nil, func(t *testing.T, h *holdFixture) {
			if err := h.svc.Remove(h.conn.ID); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, h *holdFixture, before int, out Outcome, calls map[string]int) {
			if !out.Ended || len(h.builds) != before || calls["GetTrackerConnection"] != 1 {
				t.Fatalf("tick %+v, %d builds after %d, store calls %v", out, len(h.builds), before, calls)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHoldFixture(t)
			if c.setup != nil {
				c.setup(t, h)
			}
			h.held(t)
			c.change(t, h)
			before := len(h.builds)
			out, calls := h.tick(t)
			c.check(t, h, before, out, calls)
		})
	}
}

// A root the store detached under a held tick is looked up again once:
// the tick continues into the new root, and the next tick holds it.
func TestTickLooksAGoneRootUpAgain(t *testing.T) {
	t.Parallel()
	h := newHoldFixture(t)
	h.held(t)
	old := h.root()
	if _, err := h.st.DetachMirror(h.conn.ID); err != nil {
		t.Fatal(err)
	}
	built := len(h.builds)
	out, calls := h.tick(t)
	if out.Ended || out.Failure != nil || calls["GetMirrorSync"] != 2 || calls["GetOrCreateMirrorRoot"] != 1 || len(h.builds) != built {
		t.Fatalf("tick %+v, store calls %v, %d builds after %d", out, calls, len(h.builds), built)
	}
	if root := h.root(); root == old || len(h.rowIDs(testBoard)) != 1 {
		t.Fatalf("root %d after %d, rows %v", root, old, h.rowIDs(testBoard))
	}
	if _, calls = h.tick(t); calls["GetMirrorSync"] != 1 || calls["GetOrCreateMirrorRoot"] != 0 {
		t.Fatalf("the tick after the look up: store calls %v", calls)
	}
}

func must2[A, B any](t *testing.T) func(A, B, error) (A, B) {
	return func(a A, b B, err error) (A, B) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a, b
	}
}
