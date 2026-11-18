package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// A prompt being answered is a state it passes through: what happened
// meanwhile, a lease that ran out, a hand-back, a link that ended, a
// handoff or a pause of its holder, is reconciled when the answer ends
// without landing, on both computers. And only a session that may answer a
// prompt reads or renews it.

// blockedAnswer is a hook that holds one answer until it is released and
// then fails it: entered is closed once the answer is held.
type blockedAnswer struct {
	entered, release chan struct{}
	once, freed      sync.Once
}

func newBlockedAnswer(t *testing.T) *blockedAnswer {
	b := &blockedAnswer{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(b.free)
	return b
}

func (b *blockedAnswer) free() { b.freed.Do(func() { close(b.release) }) }

// hold blocks the first call until free, and fails every call.
func (b *blockedAnswer) hold() error {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return errorsx.Public(threadtools.CodeInvalidRequest, "The worker's provider did not take the answer.", nil)
}

// answerBlocked starts the link's own agent_prompt_reply on the held
// prompt under token, which the app holds at blocked, and returns its
// result channel once it is held.
func (f *linkedWorkers) answerBlocked(t *testing.T, token string, blocked *blockedAnswer) <-chan error {
	t.Helper()
	f.app.workerPromptAnsweringForTest = func(string) error { return blocked.hold() }
	done := make(chan error, 1)
	go func() {
		done <- f.call(t, f.device, f.lead, threadAgentPeerPromptReply,
			threadPromptPeerArgs{Spawn: f.spawns[0], Token: token, Decision: threadtools.DecisionAllow})
	}()
	within(t, "the answer being written", blocked.entered)
	return done
}

// On the worker's computer, a held prompt whose answer fails goes on as
// what happened while it was answered says: its lease ran out, its link
// ended, or that computer gave it back.
func TestAHeldPromptWhoseAnswerFailsIsReconciled(t *testing.T) {
	t.Parallel()
	for name, meanwhile := range map[string]func(t *testing.T, f *linkedWorkers, token string){
		"the lease ran out": func(t *testing.T, f *linkedWorkers, token string) {
			// The check fires while the answer is written.
			f.app.workerPromptLeaseOverride = 50 * time.Millisecond
			f.app.expirePromptLease(token)
			time.Sleep(100 * time.Millisecond)
			f.app.expirePromptLease(token)
		},
		"the link ended": func(t *testing.T, f *linkedWorkers, token string) {
			revocation, err := f.app.store.RevokeDevice(f.device, time.Now().UnixMilli())
			if err != nil || !slices.Contains(revocation.ReleasedThreads, f.workers[0].ID) {
				t.Fatalf("revoke: %+v, %v", revocation, err)
			}
			f.app.announceReleasedWorkers(revocation.ReleasedThreads)
			f.drain(t)
		},
		"that computer gave it back": func(t *testing.T, f *linkedWorkers, token string) {
			// Its own call, while the answer is written: the binding admits
			// it, and the hand-back waits on the answer.
			f.app.handBackWorkerPrompt(token)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkedWorkers(t, 1)
			token := f.ask(t, 0)
			blocked := newBlockedAnswer(t)
			done := f.answerBlocked(t, token, blocked)
			meanwhile(t, f, token)
			if route := f.route(t, f.workers[0].ID); route == nil {
				t.Fatal("the prompt moved while its answer was being written")
			}
			blocked.free()
			if err := within(t, "the answer", done); err == nil {
				t.Fatal("the failed answer was taken")
			}
			waitUntil(t, 5*time.Second, func() bool {
				f.drain(t)
				return f.route(t, f.workers[0].ID) == nil && f.handedToPerson(f.workers[0].ID)
			})
		})
	}
}

// A held prompt whose answer fails inside its lease stays held, and its
// lease is checked again: it goes to the person when the lease runs out.
func TestAHeldPromptWhoseAnswerFailsKeepsItsLease(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 1)
	token := f.ask(t, 0)
	blocked := newBlockedAnswer(t)
	done := f.answerBlocked(t, token, blocked)
	// The registration's check fires while the answer is written, inside
	// the lease, and is dropped.
	f.app.workerPromptLeaseOverride = time.Second
	f.app.expirePromptLease(token)
	blocked.free()
	if err := within(t, "the answer", done); err == nil {
		t.Fatal("the failed answer was taken")
	}
	f.drain(t)
	if route := f.route(t, f.workers[0].ID); route == nil || route.ThreadID != f.lead {
		t.Fatalf("the prompt is routed to %+v after a failed answer inside its lease, want the lead", route)
	}
	waitUntil(t, 10*time.Second, func() bool {
		f.drain(t)
		return f.route(t, f.workers[0].ID) == nil && f.handedToPerson(f.workers[0].ID)
	})
}

// failRemoteAnswers holds the spawner's first answer at the worker's
// computer and refuses it there, before it reaches the app.
func failRemoteAnswers(t *testing.T, peer *ownerOpPeer) *blockedAnswer {
	t.Helper()
	blocked := newBlockedAnswer(t)
	hold := blocked.hold
	peer.replying.Store(&hold)
	return blocked
}

// On the spawner's computer, a prompt whose answer fails is reconciled with
// what happened to its holder meanwhile: a handoff moves it to the
// successor, assistants turned off and a wake its holder did not take give
// it back to the person there.
func TestARemotePromptWhoseAnswerFailsFollowsItsHolder(t *testing.T) {
	t.Parallel()
	type outcome func(t *testing.T, p *remotePromptPair, moved string)
	gaveBack := func(t *testing.T, p *remotePromptPair, _ string) {
		t.Helper()
		p.givenToThePerson(t)
		p.drain(t)
		if held := p.source.heldRemotePrompts(p.spawn); len(held) != 0 {
			t.Fatalf("this computer still holds %q", held)
		}
	}
	for name, tc := range map[string]struct {
		meanwhile func(t *testing.T, p *remotePromptPair, wake workerPromptWake) string
		then      outcome
	}{
		"handed off": {
			meanwhile: func(t *testing.T, p *remotePromptPair, _ workerPromptWake) string {
				return handOffOn(t, p.source, p.caller.ID).ID
			},
			then: func(t *testing.T, p *remotePromptPair, moved string) {
				t.Helper()
				wake := p.wakesFor(t, moved, 1)[0]
				if held := p.source.heldRemotePrompts(p.spawn); !slices.Equal(held, []string{wake.token}) {
					t.Fatalf("this computer holds %q, want the prompt for the successor", held)
				}
				if wakes := remoteWakes(t, p.source, p.caller.ID); len(wakes) != 0 {
					t.Fatalf("the thread handed off still holds %+v", wakes)
				}
			},
		},
		"assistants off": {
			meanwhile: func(t *testing.T, p *remotePromptPair, _ workerPromptWake) string {
				switchAssistants(t, p.source, false)
				p.source.convergeAssistants()
				return ""
			},
			then: gaveBack,
		},
		"its wake not taken": {
			meanwhile: func(t *testing.T, p *remotePromptPair, wake workerPromptWake) string {
				p.source.remotePromptNotTaken(p.keyOf(wake.token), wake.delivery)
				return ""
			},
			then: gaveBack,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, peer := promptOwnerOpPair(t, 0)
			testAssistant(t, p.source, p.caller.ID)
			p.openThere(t, 1)
			p.readOwnerState(t, p.source)
			wake := p.wakesFor(t, p.caller.ID, 1)[0]
			blocked := failRemoteAnswers(t, peer)
			done := make(chan error, 1)
			go func() {
				_, err := p.reply(t, p.callerIdentity(), threadtools.ReplyCall{Token: wake.token, Decision: threadtools.DecisionAllow})
				done <- err
			}()
			within(t, "the answer on its way", blocked.entered)
			moved := tc.meanwhile(t, p, wake)
			p.drain(t)
			blocked.free()
			if err := within(t, "the answer", done); err == nil {
				t.Fatal("the refused answer was taken")
			}
			p.drain(t)
			tc.then(t, p, moved)
		})
	}
}

// narrowSessionThere mints, on the pair's destination, a session of the
// spawner's device that may read threads and start work but not answer
// prompts, and returns the context a call on it carries.
func narrowSessionThere(t *testing.T, p *remotePromptPair) context.Context {
	t.Helper()
	device, err := p.dest.store.GetDevice(p.receipt(t, p.spawn.Token).OwnerDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := p.dest.identityState().sessions.Mint(identity.MintRequest{
		UserID: device.UserID, DeviceID: device.ID, BindingClass: identity.BindingDeviceBound,
		Scopes: []identity.Scope{identity.ScopeThreadsRead, identity.ScopeTerminalOperate}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := transport.WithConnState(t.Context(), transport.ConnPrincipal{SessionID: session.ID})
	return ctx
}

// A session of the link's device that may not answer prompts reads the
// worker's blocked state, and neither lists the prompt's content, renews
// it, answers it nor gives it back.
func TestOnlyASessionThatMayAnswerAPromptReadsIt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	waitUntil(t, 10*time.Second, func() bool {
		p.drain(t)
		return len(p.dest.workerPrompts.heldFor(p.spawn.Token, p.worker)) == 1
	})
	token := p.dest.workerPrompts.heldFor(p.spawn.Token, p.worker)[0].token
	ctx := narrowSessionThere(t, p)
	encoded, err := json.Marshal(threadAgentPeerArgs{ThreadID: p.worker, Spawn: p.spawn.Token, Held: []string{token}, Depth: 1, StateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := p.dest.ThreadToolQuery(ctx, ThreadPeerCall{Tool: threadAgentPeerRows, Args: encoded, Source: threadtools.Caller{ThreadID: p.caller.ID}})
	if err != nil {
		t.Fatalf("agent_rows on a session that may read threads: %v", err)
	}
	var listing ThreadAgentRows
	if err := json.Unmarshal(reply.Result, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.BlockedOn != "approval" || len(listing.Prompts) != 0 {
		t.Fatalf("the listing = blocked on %q with prompts %+v; want the blocked state and no prompt", listing.BlockedOn, listing.Prompts)
	}
	p.dest.workerPrompts.mu.Lock()
	renewed := !p.dest.workerPrompts.byToken[token].remote.heldAt.IsZero()
	p.dest.workerPrompts.mu.Unlock()
	if renewed {
		t.Fatal("a session that may not answer the prompt renewed it")
	}
	for _, tool := range []string{threadAgentPeerPromptReply, threadAgentPeerPromptReturn} {
		args, err := json.Marshal(threadPromptPeerArgs{Spawn: p.spawn.Token, Token: token, Decision: threadtools.DecisionAllow})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.dest.ThreadToolCall(ctx, ThreadPeerCall{Tool: tool, Args: args, Source: threadtools.Caller{ThreadID: p.caller.ID}})
		wantScopeRefusal(t, err, transport.ScopeApprovalsRespond)
	}
	if route, open := p.routeThere("req-allow"); !open || route == nil || route.ThreadID != p.caller.ID {
		t.Fatalf("the prompt is routed to %+v (open %v) after the refused calls, want the spawner", route, open)
	}
	// The pairing's own session reads it.
	listing, _, err = p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
	if err != nil || len(listing.Prompts) != 1 || listing.Prompts[0].Token != token {
		t.Fatalf("the pairing's own listing = %+v, %v; want the prompt", listing.Prompts, err)
	}
}

// The owner's authentic listing of a worker it gave a sidebar row or a
// group is its contact with the link: the retention pass keeps the link,
// and Remove from Sidebar makes the worker its spawner's again at its
// level.
func TestAListingOfAPlacedWorkerKeepsItsLink(t *testing.T) {
	t.Parallel()
	for name, place := range map[string]func(t *testing.T, f *linkedWorkers){
		"standalone": func(t *testing.T, f *linkedWorkers) {
			if _, err := f.app.SetThreadPlacement([]string{f.workers[0].ID}, store.PlacementStandalone); err != nil {
				t.Fatal(err)
			}
		},
		"grouped": func(t *testing.T, f *linkedWorkers) {
			group, err := f.app.CreateThreadGroup(f.workers[0].ProjectID, "Later")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.SetThreadGroup([]string{f.workers[0].ID}, group.ID); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkedWorkersAt(t, 1, 1)
			worker := f.workers[0].ID
			level, err := f.app.store.SubThreadLevel(worker)
			if err != nil {
				t.Fatal(err)
			}
			place(t, f)
			if row, err := f.app.store.GetThread(worker); err != nil || row.Placement == store.PlacementOwned {
				t.Fatalf("the placed worker = %+v, %v", row.Placement, err)
			}
			f.list(t, 0)
			f.app.expireThreadRequests(time.Now())
			if _, err := f.app.SetThreadPlacement([]string{worker}, store.PlacementOwned); err != nil {
				t.Fatalf("Remove from Sidebar after the retention pass: %v", err)
			}
			if owner, linked, err := f.app.store.SpawnOwnerOf(worker); err != nil || !linked || owner.Token != f.spawns[0] {
				t.Fatalf("the worker's owner = %+v, %v, %v; want its link", owner, linked, err)
			}
			if got, err := f.app.store.SubThreadLevel(worker); err != nil || got != level {
				t.Fatalf("the worker's level = %d, %v; want %d", got, err, level)
			}
		})
	}
}

// Forgetting a computer closes the prompts its workers put to threads
// here and withdraws their wakes, a wake queued before and one a listing
// recorded just before the forget alike; nothing is given back to it.
func TestForgettingAComputerClosesItsPrompts(t *testing.T) {
	t.Parallel()
	t.Run("a queued wake", func(t *testing.T) {
		t.Parallel()
		p, peer := promptOwnerOpPair(t, 0)
		p.openThere(t, 1)
		p.readOwnerState(t, p.source)
		wake := p.wakesFor(t, p.caller.ID, 1)[0]
		p.source.forgetPeerAgentRows(p.computer)
		p.wakesFor(t, p.caller.ID, 0)
		if held := p.source.heldRemotePrompts(p.spawn); len(held) != 0 {
			t.Fatalf("this computer still holds %q", held)
		}
		if _, err := p.reply(t, p.callerIdentity(), threadtools.ReplyCall{Token: wake.token, Decision: threadtools.DecisionAllow}); !errors.Is(err, errWorkerPromptClosed) {
			t.Fatalf("a reply after the forget: %v, want it refused as closed", err)
		}
		if returned := peer.returns.Load(); returned != 0 {
			t.Fatalf("%d prompts were given back to the forgotten computer", returned)
		}
	})
	t.Run("a listing recorded before the forget", func(t *testing.T) {
		t.Parallel()
		p, peer := promptOwnerOpPair(t, 0)
		p.openThere(t, 1)
		listing, origin, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
		if err != nil || len(listing.Prompts) != 1 {
			t.Fatalf("the listing there: %+v, %v", listing.Prompts, err)
		}
		// The queue is busy, so the listing's reconcile waits behind it, and
		// the forget behind that.
		busy, release := make(chan struct{}), make(chan struct{})
		p.source.workerPrompts.run(func() {
			close(busy)
			<-release
		})
		<-busy
		done := p.source.noteRemotePrompts(p.source.ownerReads.start(t.Context(), p.computer, nil), origin, p.spawn, listing, nil)
		if err := p.manager.Remove(p.computer); err != nil {
			close(release)
			t.Fatal(err)
		}
		close(release)
		if err := <-done; err == nil {
			t.Fatal("queued prompt applied after its carrier was forgotten")
		}
		p.drain(t)
		if wakes := remoteWakes(t, p.source, p.caller.ID); len(wakes) != 0 {
			t.Fatalf("the spawner holds %+v after its computer was forgotten", wakes)
		}
		if held := p.source.heldRemotePrompts(p.spawn); len(held) != 0 {
			t.Fatalf("this computer still holds %q", held)
		}
		if returned := peer.returns.Load(); returned != 0 {
			t.Fatalf("%d prompts were given back to the forgotten computer", returned)
		}
	})
}

// A send from a thread other than the worker's spawner, which wakes the
// worker into a prompt routed to that spawner, is not routed to the
// sender: its row names the thread that holds the prompt here, the one its
// wake went to, and the prompt is still picked up at the poll's pace
// through the spawn. After the spawner's handoff that is its successor, by
// the successor's name now, though the worker's computer still names the
// spawner the link came from.
func TestASiblingsSendNamesTheSpawnerItsPromptWentTo(t *testing.T) {
	t.Parallel()
	for name, handoff := range map[string]bool{"the spawner": false, "the spawner's successor": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			remotetest.Require(t)
			pair := newReachPair(t)
			routeWorkerPromptsOn(pair.dest, pair.destBus)
			pair.source.startSessionFn = func(string) error { return nil }
			installMockClaudeTurns(t, pair.dest, [][]string{
				{mockClaudeInitLine, `{"type":"result","subtype":"success","is_error":false}`},
				{remoteApproveLine},
			})
			ack := pair.spawnThere(t, "work over there", map[string]any{"runtime_mode": string(provider.RuntimeApprovalRequired)})
			worker := pair.receipt(t, ack.Token).TargetThreadID
			t.Cleanup(func() { _ = pair.dest.StopSession(worker) })
			waitUntil(t, 15*time.Second, func() bool {
				pair.poll(t)
				return threadRequestSettled(pair.request(t, ack.Token))
			})
			pair.source.retireThreadRequestPoll(ack.Token)
			startReconciledOwner(t, pair)
			holder := pair.caller
			if handoff {
				testAssistant(t, pair.source, pair.caller.ID)
				successor := handOffOn(t, pair.source, pair.caller.ID)
				if err := pair.source.RenameThread(successor.ID, "Successor"); err != nil {
					t.Fatal(err)
				}
				if holder, err := pair.source.store.GetThread(successor.ID); err != nil || holder.Title != "Successor" {
					t.Fatalf("the successor = %+v, %v", holder, err)
				}
				holder.ID, holder.Title = successor.ID, "Successor"
			}
			sibling, err := createTestThread(t, pair.source, pair.caller.Provider, t.TempDir(), pair.caller.Model, pair.caller.Mode)
			if err != nil {
				t.Fatal(err)
			}
			from := pair.callerIdentity()
			from.ThreadID, from.Title = sibling.ID, sibling.Title
			send, err := threadToolsApp{app: pair.source}.Send(t.Context(), from, threadtools.SendCall{
				ThreadID: worker, ComputerID: pair.computer, Message: "one more thing", Notify: true,
			})
			if err != nil {
				t.Fatalf("the sibling's send: %v", err)
			}
			waitUntil(t, 15*time.Second, func() bool {
				pending := pair.dest.triage.PendingInteractiveRequests(worker).Approvals
				return len(pending) == 1 && pending[0].RoutedTo != nil && pending[0].RoutedTo.ThreadID == pair.caller.ID
			})
			state, err := pair.dest.threadPeerRequestState(t.Context(), pair.receipt(t, ack.Token).OwnerDeviceID, send.Token)
			if err != nil || !state.Blocked || state.BlockedRoutedToCaller || state.BlockedRoutedToThread != pair.caller.ID {
				t.Fatalf("the send's state there = %+v, %v; want blocked, routed to the spawner and not to the sender", state, err)
			}
			waitUntil(t, 15*time.Second, func() bool {
				return len(remoteWakes(t, pair.source, holder.ID)) == 1
			})
			for _, other := range []string{sibling.ID, pair.caller.ID} {
				if wakes := remoteWakes(t, pair.source, other); other != holder.ID && len(wakes) != 0 {
					t.Fatalf("%s holds %+v; want the wake on %s alone", other, wakes, holder.ID)
				}
			}
			// The spawner's wake and the sibling's status are separate receipts.
			waitUntil(t, 15*time.Second, func() bool {
				rows, err := pair.source.ListThreadAgentRows(t.Context(), sibling.ID)
				if err != nil {
					t.Fatal(err)
				}
				row, found := agentRowByToken(rows.Rows, send.Token)
				return found && row.BlockedRoutedHere && row.BlockedRoutedTo == holder.Title
			})
		})
	}
}
