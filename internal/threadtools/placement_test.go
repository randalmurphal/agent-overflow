package threadtools

import (
	"strings"
	"testing"
)

// TestSpawnResolvesItsPlacement: the placement a call names reaches the
// app as named and an omitted one reaches it empty, for the destination to
// resolve; a contradiction is refused before the app hears of it.
func TestSpawnResolvesItsPlacement(t *testing.T) {
	app := spawnApp()
	server := New(app)
	for args, want := range map[string]string{
		`{"prompt":"go"}`:                                       "",
		`{"prompt":"go","placement":"nested"}`:                  PlacementNested,
		`{"prompt":"go","placement":"standalone"}`:              PlacementStandalone,
		`{"prompt":"go","group":"Sweep"}`:                       "",
		`{"prompt":"go","group":"Sweep","placement":"grouped"}`: PlacementGrouped,
	} {
		before := len(app.spawns)
		call(t, server, localCaller(), "thread_spawn", args)
		if len(app.spawns) != before+1 || app.spawns[before].Placement != want {
			t.Errorf("%s: spawn = %+v, want placement %s", args, app.spawns[len(app.spawns)-1], want)
		}
	}
	for args, fragment := range map[string]string{
		`{"prompt":"go","placement":"grouped"}`:                    "needs group",
		`{"prompt":"go","group":"Sweep","placement":"nested"}`:     "cannot also be nested",
		`{"prompt":"go","group":"Sweep","placement":"standalone"}`: "cannot also be standalone",
		`{"prompt":"go","placement":"floating"}`:                   "nested, grouped or standalone",
	} {
		before := len(app.spawns)
		message := callErr(t, server, localCaller(), "thread_spawn", args, CodeInvalidRequest)
		if !strings.Contains(message, fragment) {
			t.Errorf("%s: refusal = %q, want %q", args, message, fragment)
		}
		if len(app.spawns) != before {
			t.Errorf("%s: a refused spawn reached the app", args)
		}
	}
}

// TestUpdateCarriesPlacement: placement alone is a patch, it travels to the
// computer that holds the thread, and a contradiction with group refuses
// the whole call.
func TestUpdateCarriesPlacement(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})

	call(t, p.server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"placement":"standalone"}`)
	for name, updates := range map[string][]UpdateCall{"local": p.local.updates, "remote": p.remote.updates} {
		if len(updates) != 1 || updates[0].Placement == nil || *updates[0].Placement != PlacementStandalone {
			t.Fatalf("%s patch = %+v, want placement standalone", name, updates)
		}
	}
	// Grouped alone is left to the thread, which knows whether it has a group.
	call(t, p.server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"placement":"grouped"}`)
	// An ungroup with nested is one patch: out of the group, under the agent.
	call(t, p.server, localCaller(), "thread_update", `{"thread_ids":["`+localThreadID+`"],"group":null,"placement":"nested"}`)
	last := p.local.updates[len(p.local.updates)-1]
	if last.Group == nil || *last.Group != "" || last.Placement == nil || *last.Placement != PlacementNested {
		t.Fatalf("ungroup and nest = %+v", last)
	}
	before := len(p.local.updates)
	for _, args := range []string{
		`{"thread_ids":["` + localThreadID + `"],"group":"Release","placement":"standalone"}`,
		`{"thread_ids":["` + localThreadID + `"],"group":"Release","placement":"nested"}`,
		`{"thread_ids":["` + localThreadID + `"],"group":null,"placement":"grouped"}`,
		`{"thread_ids":["` + localThreadID + `"],"placement":"  "}`,
		`{"thread_ids":["` + localThreadID + `"],"placement":"floating"}`,
	} {
		callErr(t, p.server, localCaller(), "thread_update", args, CodeInvalidRequest)
	}
	if len(p.local.updates) != before {
		t.Fatal("a contradictory patch reached the app")
	}
}
