package browser

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// permutations answers every order of events.
func permutations(events []string) [][]string {
	if len(events) <= 1 {
		return [][]string{slices.Clone(events)}
	}
	var out [][]string
	for i, first := range events {
		rest := slices.Concat(events[:i], events[i+1:])
		for _, tail := range permutations(rest) {
			out = append(out, append([]string{first}, tail...))
		}
	}
	return out
}

// A popup target's created, info change and destroyed settle the same way
// in every order the three subscriptions deliver them: the change reaches
// the page once, unless the target went first; a destroyed target keeps
// no opener and no held info, only its tombstone; and a created after the
// destroyed reports nothing.
func TestCDPTargetsSettleInEveryOrder(t *testing.T) {
	for _, events := range [][]string{{"created", "changed"}, {"created", "changed", "destroyed"}} {
		for _, order := range permutations(events) {
			t.Run(fmt.Sprint(order), func(t *testing.T) {
				targets := newCDPTargets(nil)
				info := &target.Info{TargetID: "T-popup", Type: "page", OpenerID: "T-opener", URL: "https://example.test/", Title: "popup"}
				bound, delivered, reported := false, 0, false
				for _, event := range order {
					switch event {
					case "created":
						reported = targets.created(info, func(newest *target.Info, held bool, _ uint64) {
							// The engine binds the popup here.
							bound = true
							if held {
								delivered++
							}
						})
					case "changed":
						if _, ok := targets.changed(info, func() bool { return bound }); ok {
							delivered++
						}
					case "destroyed":
						// The engine unbinds the popup here.
						targets.destroyed(info.TargetID, func() { bound = false })
					}
				}
				at := func(event string) int {
					if i := slices.Index(order, event); i >= 0 {
						return i
					}
					return len(order)
				}
				goneBeforeCreated := at("destroyed") < at("created")
				if reported == goneBeforeCreated {
					t.Errorf("created reported %v", reported)
				}
				wantDelivered := 0
				if !goneBeforeCreated && at("changed") < at("destroyed") {
					wantDelivered = 1
				}
				if delivered != wantDelivered {
					t.Errorf("the change was delivered %d times, want %d", delivered, wantDelivered)
				}
				owners := targets.owners("T-popup")
				if slices.Contains(order, "destroyed") {
					if !slices.Equal(owners, []string{"T-popup"}) {
						t.Errorf("a destroyed target's owners are %v", owners)
					}
					if n := targets.retained(); n != 1 {
						t.Errorf("a destroyed target retains %d ids, want its tombstone", n)
					}
				} else if !slices.Equal(owners, []string{"T-popup", "T-opener"}) {
					t.Errorf("a live popup's owners are %v", owners)
				}
			})
		}
	}
}

// A target destroyed before its creation is handled leaves no opener
// behind for the navigation policy to follow, and nothing for good.
func TestBP4DestroyedBeforeCreatedLeavesNoLiveOpener(t *testing.T) {
	targets := newCDPTargets(nil)
	targets.destroyed("T-popup", nil)
	if targets.created(&target.Info{TargetID: "T-popup", Type: "page", OpenerID: "T-opener"}, func(*target.Info, bool, uint64) {
		t.Error("a gone target was reported")
	}) {
		t.Error("created answered true for a gone target")
	}
	if owners := targets.owners("T-popup"); !slices.Equal(owners, []string{"T-popup"}) {
		t.Errorf("owners %v", owners)
	}
	targets.mu.Lock()
	known, openers := len(targets.known), len(targets.openers)
	targets.mu.Unlock()
	if known != 0 || openers != 0 {
		t.Errorf("known %d, openers %d", known, openers)
	}
	// Tombstones and held info are bounded however many targets come and
	// go out of order.
	for i := range 10 * orderSlack {
		id := target.ID(fmt.Sprintf("T-%d", i))
		targets.changed(&target.Info{TargetID: id, Type: "page"}, func() bool { return false })
		targets.destroyed(id, nil)
		targets.changed(&target.Info{TargetID: target.ID(fmt.Sprintf("U-%d", i)), Type: "page"}, func() bool { return false })
	}
	if n := targets.retained(); n > 2*orderSlack {
		t.Errorf("the record retains %d ids", n)
	}
}

// A popup's owners are its openers, nearest first, as far as each is known,
// and a cycle ends.
func TestCDPTargetsOwnersFollowTheOpenerChain(t *testing.T) {
	targets := newCDPTargets(nil)
	targets.created(&target.Info{TargetID: "B", Type: "page", OpenerID: "A"}, nil)
	targets.created(&target.Info{TargetID: "C", Type: "page", OpenerID: "B"}, nil)
	// A worker is no popup.
	targets.created(&target.Info{TargetID: "W", Type: "worker", OpenerID: "C"}, nil)
	if owners := targets.owners("C"); !slices.Equal(owners, []string{"C", "B", "A"}) {
		t.Errorf("owners %v", owners)
	}
	if owners := targets.owners("W"); !slices.Equal(owners, []string{"W"}) {
		t.Errorf("a worker's owners %v", owners)
	}
	targets.created(&target.Info{TargetID: "A", Type: "page", OpenerID: "C"}, nil)
	if owners := targets.owners("C"); len(owners) > 4 {
		t.Errorf("a cycle's owners %v", owners)
	}
}

// Info changes are numbered as they are handled, held or not, so a held
// change delivered late is older than one handled after it.
func TestCDPTargetsNumberChangesAsTheyArrive(t *testing.T) {
	targets := newCDPTargets(nil)
	bound := false
	first, _ := targets.changed(&target.Info{TargetID: "T", Type: "page", Title: "held"}, func() bool { return bound })
	bound = true
	second, ok := targets.changed(&target.Info{TargetID: "T", Type: "page", Title: "newer"}, func() bool { return bound })
	if !ok {
		t.Fatal("a change of a bound target was held")
	}
	info, order, held := targets.takeHeld("T")
	if !held || info.Title != "held" || order != first || order >= second {
		t.Errorf("held %v %+v numbered %d, newer numbered %d", held, info, order, second)
	}
}

// The hosted engine's record of its targets is the connection's: a popup's
// opener it learned goes when the connection does.
func TestHostedTargetsGoWithTheirConnection(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	l.open(access)
	l.endpoint.send(map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{
		"targetId": "PT-9", "type": "page", "openerId": "T-opener", "url": "about:blank", "title": "",
		"attached": false, "canAccessOpener": false,
	}}})
	await := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(hostedTestDeadline); !done(); {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	await("the popup's opener was never recorded", func() bool {
		return slices.Equal(l.engine.currentTargets().owners("PT-9"), []string{"PT-9", "T-opener"})
	})
	l.endpoint.hangUp()
	await("the lost connection's targets stayed", func() bool {
		return l.engine.currentTargets().retained() == 0
	})
}
