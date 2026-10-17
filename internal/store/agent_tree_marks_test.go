package store

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
)

// agentTreeMarksRun drives one random sequence of the ledger's writers over
// a random tree of spawns: opens, settles (finished, cancelled, expired,
// the receipt path's), wake arming, remote spawns and their reports, a
// transfer move out and back, New Thread, Same Assistant with its receipt
// retarget, thread deletes at every level (a holder retire included),
// single request deletes and the retention sweep's lineage conversion.
// After each write, check reports what the marks hold against the walk.
type agentTreeMarksRun struct {
	t      *testing.T
	s      *Store
	random *rand.Rand
	// threads are the live threads; spawned maps each spawned thread to
	// the token of the spawn that reached it.
	threads []string
	spawned map[string]string
	open    []string
	settled []string
	remote  []string
	root    string
	next    int
	clock   int64
}

func (r *agentTreeMarksRun) name(prefix string) string {
	r.next++
	return fmt.Sprintf("%s-%03d", prefix, r.next)
}

func (r *agentTreeMarksRun) now() int64 {
	r.clock += 10
	return r.clock
}

func (r *agentTreeMarksRun) pick(from []string) string {
	return from[r.random.IntN(len(from))]
}

func (r *agentTreeMarksRun) drop(from []string, token string) []string {
	return slices.DeleteFunc(from, func(x string) bool { return x == token })
}

// spawn opens a local spawn of a new thread from caller, through the
// receipt and NameLocalThreadRequestTarget as thread_spawn does, or with
// its target named at insert.
func (r *agentTreeMarksRun) spawn(caller string) string {
	t, s := r.t, r.s
	child, token := r.name("thread"), r.name("spawn")
	mustCreateThread(t, s, child)
	at := r.now()
	if r.random.IntN(2) == 0 {
		seedAgentRequest(t, s, token, caller, ThreadRequestSpawn, child, false, at)
	} else {
		if err := s.InsertThreadRequest(ThreadRequest{Token: token, CallerThreadID: caller, Kind: ThreadRequestSpawn,
			State: ThreadRequestUnconfirmed, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: token, OwnerDeviceID: "local",
			SourceThreadID: caller, Kind: ThreadRequestSpawn, State: ThreadReceiptAccepted, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := s.NameLocalThreadRequestTarget(token, child); err != nil {
			t.Fatal(err)
		}
	}
	r.threads = append(r.threads, child)
	r.spawned[child] = token
	r.open = append(r.open, token)
	return child
}

func (r *agentTreeMarksRun) step(check func(what string)) {
	t, s := r.t, r.s
	switch op := r.random.IntN(16); {
	case op < 4:
		r.spawn(r.pick(r.threads))
		check("spawn")
	case op < 6:
		token := r.name("send")
		kind := []string{ThreadRequestSend, ThreadRequestAsk}[r.random.IntN(2)]
		seedAgentRequest(t, s, token, r.pick(r.threads), kind, r.pick(r.threads), r.random.IntN(3) > 0, r.now())
		r.open = append(r.open, token)
		check("send")
	case op < 9 && len(r.open) > 0:
		token := r.pick(r.open)
		state := []string{ThreadRequestFinished, ThreadRequestCancelled, ThreadRequestExpired}[r.random.IntN(3)]
		var settled bool
		var err error
		row := rawThreadRequest(t, s, token)
		var local bool
		if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM threads WHERE id = ?)`, row.TargetThreadID).Scan(&local); err != nil {
			t.Fatal(err)
		}
		if local && row.TargetComputerID == "" && r.random.IntN(3) == 0 {
			var local LocalSettlement
			if _, _, err = s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: token, OwnerDeviceID: "local",
				SourceThreadID: row.CallerThreadID, Kind: row.Kind, TargetThreadID: row.TargetThreadID,
				State: ThreadReceiptRunning, CreatedAt: 1, UpdatedAt: 1}); err != nil {
				t.Fatal(err)
			}
			local, err = s.SettleLocalThreadRequest(token, []string{ThreadReceiptAccepted, ThreadReceiptRunning},
				ThreadRequestSettlement{State: ThreadReceiptFinished, Answer: []byte("a"), AnswerKind: ThreadAnswerFinal, SettledAt: r.now()}, state)
			settled = local.Collected
		} else {
			settled, err = s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
				State: state, Answer: []byte("a"), AnswerKind: ThreadAnswerFinal, SettledAt: r.now()})
		}
		if err != nil {
			t.Fatal(err)
		}
		r.open = r.drop(r.open, token)
		if settled {
			r.settled = append(r.settled, token)
		}
		check("settle " + state)
	case op < 10 && len(r.open) > 0:
		token := r.pick(r.open)
		if _, err := s.SetThreadRequestNotify(token, r.random.IntN(2) == 0); err != nil {
			t.Fatal(err)
		}
		check("notify")
	case op == 10:
		token := r.name("remote")
		seedAgentRequest(t, s, token, r.pick(r.threads), ThreadRequestSpawn, "", false, r.now())
		if _, err := s.SetThreadRequestTarget(token, "pc", r.name("far"), "Far", ""); err != nil {
			t.Fatal(err)
		}
		settleAgentRequest(t, s, token, r.now())
		r.remote = append(r.remote, token)
		check("remote spawn")
	case op == 11 && len(r.remote) > 0:
		if r.random.IntN(4) == 0 {
			if _, err := s.ClearThreadRequestsNestedOpenForComputer("pc"); err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.SetThreadRequestNestedOpen(r.pick(r.remote), r.random.IntN(3) > 0); err != nil {
			t.Fatal(err)
		}
		check("nested open")
	case op == 12 && len(r.spawned) > 0:
		// A conversation moved to another computer and back.
		child := r.pick(slices.Sorted(func(yield func(string) bool) {
			for c := range r.spawned {
				if !yield(c) {
					return
				}
			}
		}))
		token := r.spawned[child]
		if _, err := s.SetThreadRequestTarget(token, "pc", child, "Moved", ""); err != nil {
			t.Fatal(err)
		}
		check("transfer out")
		if _, err := s.SetThreadRequestTarget(token, "", child, "", ""); err != nil {
			t.Fatal(err)
		}
		check("transfer back")
	case op == 13:
		r.handOff(check)
	case op == 14 && len(r.threads) > 1:
		victim := r.pick(r.threads[1:])
		if victim == r.root {
			return
		}
		if r.random.IntN(4) == 0 {
			if err := s.CreatePointerFork(makeThread(r.name("fork"), "claude"), victim, ForkCut{}, testInterruptedSummary, r.now()); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.DeleteThread(victim); err != nil {
			t.Fatal(err)
		}
		r.forget(victim)
		check("delete " + victim)
	case op == 15:
		if len(r.settled) > 0 && r.random.IntN(2) == 0 {
			token := r.pick(r.settled)
			if _, err := s.DeleteThreadRequest(token); err != nil {
				t.Fatal(err)
			}
			r.settled = r.drop(r.settled, token)
			check("delete request")
			return
		}
		if _, err := s.DeleteThreadRequestsBefore(r.clock - 50); err != nil {
			t.Fatal(err)
		}
		check("retention sweep")
	}
}

// handOff gives the root, an assistant, a new thread, and the spawn that
// reached a spawned assistant its successor, as New Thread, Same
// Assistant and its receipt retarget do.
func (r *agentTreeMarksRun) handOff(check func(what string)) {
	t, s := r.t, r.s
	old := r.root
	if len(r.spawned) > 0 && r.random.IntN(2) == 0 {
		children := slices.Sorted(func(yield func(string) bool) {
			for c := range r.spawned {
				if !yield(c) {
					return
				}
			}
		})
		old = r.pick(children)
		if _, found, err := s.GetStandingAgent(old); err != nil {
			t.Fatal(err)
		} else if !found {
			mustCreateAssistant(t, s, old, "/h/"+old)
		}
	}
	successor := r.name("successor")
	mustCreateThread(t, s, successor)
	token, spawned := r.spawned[old]
	if spawned {
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: token, OwnerDeviceID: "local",
			SourceThreadID: "x", Kind: ThreadRequestSpawn, TargetThreadID: old, State: ThreadReceiptAccepted, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.HandOffAssistant(old, successor, r.now()); err != nil {
		t.Fatal(err)
	}
	check("hand off " + old)
	if spawned {
		if _, err := s.RetargetThreadReceipt(token, old, successor); err != nil {
			t.Fatal(err)
		}
		delete(r.spawned, old)
		r.spawned[successor] = token
		check("retarget " + old)
	}
	r.threads = append(r.threads, successor)
	if old == r.root {
		r.root = successor
	}
}

// forget drops a deleted thread and the requests its delete took.
func (r *agentTreeMarksRun) forget(thread string) {
	r.threads = r.drop(r.threads, thread)
	delete(r.spawned, thread)
	var gone []string
	rows, err := r.s.db.Query(`SELECT token FROM thread_requests`)
	if err != nil {
		r.t.Fatal(err)
	}
	live := map[string]bool{}
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			r.t.Fatal(err)
		}
		live[token] = true
	}
	if err := rows.Close(); err != nil {
		r.t.Fatal(err)
	}
	for _, list := range []*[]string{&r.open, &r.settled, &r.remote} {
		*list = slices.DeleteFunc(*list, func(token string) bool { return !live[token] })
	}
	for child, token := range r.spawned {
		if !live[token] {
			gone = append(gone, child)
		}
	}
	for _, child := range gone {
		delete(r.spawned, child)
	}
}

// newAgentTreeMarksRun seeds a random tree under an assistant root: up to
// seven levels of spawns, some with open sends.
func newAgentTreeMarksRun(t *testing.T, seed uint64) *agentTreeMarksRun {
	r := &agentTreeMarksRun{t: t, s: newTestStore(t), random: rand.New(rand.NewPCG(seed, 157)), spawned: map[string]string{}, root: "root"}
	mustCreateThread(t, r.s, "root")
	mustCreateAssistant(t, r.s, "root", "/h/root")
	r.threads = []string{"root"}
	level := []string{"root"}
	for range 7 {
		var next []string
		for _, caller := range level {
			for range r.random.IntN(3) {
				next = append(next, r.spawn(caller))
			}
		}
		if len(next) == 0 {
			next = []string{r.spawn(r.pick(level))}
		}
		level = next
	}
	return r
}

// Every writer of the ledger keeps the marks the walk finds, in its own
// transaction: random sequences of writes over random trees, each write
// checked against the walk.
func TestAgentTreeMarksFollowEveryWriter(t *testing.T) {
	t.Parallel()
	for seed := range uint64(24) {
		r := newAgentTreeMarksRun(t, seed)
		assertAgentTreeMarks(t, r.s.db, fmt.Sprintf("seed %d tree", seed))
		for op := range 80 {
			r.step(func(what string) { assertAgentTreeMarks(t, r.s.db, fmt.Sprintf("seed %d op %d %s", seed, op, what)) })
		}
	}
}

// The check catches a writer the triggers miss: with the delete trigger
// dropped, a sequence that deletes requests leaves marks the walk does not
// find.
func TestAgentTreeMarksCheckFindsAMissedWriter(t *testing.T) {
	t.Parallel()
	r := newAgentTreeMarksRun(t, 3)
	mustExec(t, r.s.db, `DROP TRIGGER trg_thread_requests_open_below_delete`)
	missed := false
	for op := 0; op < 400 && !missed; op++ {
		r.step(func(string) { missed = missed || agentTreeMarksDiff(t, r.s.db) != "" })
	}
	if !missed {
		t.Fatal("400 writes without the delete trigger left the marks as the walk finds them; the check proves nothing")
	}
}

// A spawn that names a thread with open work below it already carries that
// work to every spawn above it, each count at its own depth: inserted with
// its target, and named after its insert. Three spawns stand above it, so
// each of the carry's six counts lands on one of them. The thread four
// spawns down has one open send, the one below it two, then three, then
// four, so a count moved to the wrong depth is seen.
func TestAgentTreeMarksCarryANamedSubtreeUpTheChain(t *testing.T) {
	t.Parallel()
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named after insert %v", named), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			for _, id := range []string{"top", "root", "a", "b", "x", "y", "z", "w", "peer"} {
				mustCreateThread(t, s, id)
			}
			seedAgentRequest(t, s, "top-root", "top", ThreadRequestSpawn, "root", false, 0)
			settleAgentRequest(t, s, "top-root", 1)
			seedAgentRequest(t, s, "root-a", "root", ThreadRequestSpawn, "a", false, 1)
			settleAgentRequest(t, s, "root-a", 2)
			seedAgentRequest(t, s, "a-b", "a", ThreadRequestSpawn, "b", false, 3)
			settleAgentRequest(t, s, "a-b", 4)
			for i, link := range [][2]string{{"x", "y"}, {"y", "z"}, {"z", "w"}} {
				token := link[0] + "-" + link[1]
				seedAgentRequest(t, s, token, link[0], ThreadRequestSpawn, link[1], false, int64(10+i))
				settleAgentRequest(t, s, token, int64(20+i))
			}
			for depth, thread := range []string{"x", "y", "z", "w"} {
				for i := range depth + 1 {
					seedAgentRequest(t, s, fmt.Sprintf("%s-open-%d", thread, i), thread, ThreadRequestSend, "peer", true, int64(30+i))
				}
			}
			if named {
				seedAgentRequest(t, s, "b-x", "b", ThreadRequestSpawn, "", false, 40)
				if _, err := s.SetThreadRequestTarget("b-x", "", "x", "", ""); err != nil {
					t.Fatal(err)
				}
			} else {
				seedAgentRequest(t, s, "b-x", "b", ThreadRequestSpawn, "x", false, 40)
			}
			settleAgentRequest(t, s, "b-x", 41)
			want := map[string][agentOpenDepth]int64{
				"b-x":      {1, 2, 3, 4},
				"a-b":      {0, 1, 2, 3},
				"root-a":   {0, 0, 1, 2},
				"top-root": {0, 0, 0, 1},
				"x-y":      {2, 3, 4, 0},
				"y-z":      {3, 4, 0, 0},
				"z-w":      {4, 0, 0, 0},
			}
			if got := agentTreeMarks(t, s.db); !maps.Equal(got, want) {
				t.Fatalf("marks %v; want %v", got, want)
			}
			assertAgentTreeMarks(t, s.db, "after the spawn named x")
		})
	}
}
