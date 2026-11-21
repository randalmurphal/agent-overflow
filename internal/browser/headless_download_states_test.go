package browser

import (
	"context"
	"sync"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// stateRig holds download G of a tracker in one state while an event is
// applied: an offer of G's start can be held at the Manager (offerGate),
// G's end report can be held at the Manager (endGate), and the report of a
// second download, B, can be held so G's reports queue behind it (bGate).
// Every Manager answer to G is answer.
type stateRig struct {
	t *testing.T
	*downloadTracker
	files *fakeDownloadFiles

	mu      sync.Mutex
	answer  downloadAdmission
	offers  int
	cancels int
	seen    downloadState
	// A gate, when set, holds its report until closed; reaching it
	// signals in.
	offerGate, endGate, bGate chan struct{}
	in                        chan struct{}
	// held is the goroutine holding a report at a gate, done once it
	// returns.
	held chan struct{}
	// endReported is the EndReported of G's last offered start.
	endReported func() bool
}

const guidG, guidB = "G", "B"

func newStateRig(t *testing.T) *stateRig {
	r := &stateRig{t: t, files: newFakeDownloadFiles(), answer: downloadUnowned, in: make(chan struct{}, 1)}
	events := engineEvents{
		DownloadStarted: func(start downloadStart) downloadAdmission {
			if start.ID == guidB {
				return downloadAdmitted
			}
			r.mu.Lock()
			r.offers++
			r.endReported = start.EndReported
			gate := r.offerGate
			r.mu.Unlock()
			r.pass(gate)
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.answer
		},
		DownloadProgress: func(progress downloadProgress) {
			if progress.ID == guidB {
				r.mu.Lock()
				gate := r.bGate
				r.mu.Unlock()
				r.pass(gate)
				return
			}
			r.mu.Lock()
			gate := r.endGate
			if progress.State.running() {
				gate = nil
			}
			r.mu.Unlock()
			r.pass(gate)
			r.mu.Lock()
			r.seen = progress.State
			r.mu.Unlock()
		},
	}
	r.downloadTracker = newDownloadTracker(events, func(id downloadID) {
		if id == guidG {
			r.mu.Lock()
			r.cancels++
			r.mu.Unlock()
		}
	}, r.files)
	lifetime, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	// The wait's own clock never fires: the test is its every turn.
	r.lifetime, r.ownerWait, r.ownerPoll = lifetime, time.Hour, time.Hour
	return r
}

// pass waits at gate, when there is one, after signaling that it did.
func (r *stateRig) pass(gate chan struct{}) {
	if gate == nil {
		return
	}
	r.in <- struct{}{}
	<-gate
}

// hold runs step on a goroutine of its own until it reaches the gate set
// for it.
func (r *stateRig) hold(set func(gate chan struct{}), step func()) {
	gate := make(chan struct{})
	r.mu.Lock()
	set(gate)
	r.mu.Unlock()
	r.held = make(chan struct{})
	go func() {
		defer close(r.held)
		step()
	}()
	select {
	case <-r.in:
	case <-time.After(5 * time.Second):
		r.t.Fatal("the held report never reached the Manager")
	}
}

// release lets every held report go and waits for it to return.
func (r *stateRig) release() {
	r.mu.Lock()
	for _, gate := range []*chan struct{}{&r.offerGate, &r.endGate, &r.bGate} {
		if *gate != nil {
			close(*gate)
			*gate = nil
		}
	}
	r.mu.Unlock()
	if r.held != nil {
		<-r.held
		r.held = nil
	}
}

func (r *stateRig) begin(guid string) {
	r.willBegin(cdpbrowser.EventDownloadWillBegin{GUID: guid, FrameID: "F1"})
}

func (r *stateRig) report(guid string, state cdpbrowser.DownloadProgressState) {
	r.progress(cdpbrowser.EventDownloadProgress{GUID: guid, ReceivedBytes: 10, State: state})
}

// state is G's state, or "gone" once it is forgotten.
func (r *stateRig) state() string {
	r.downloadTracker.mu.Lock()
	defer r.downloadTracker.mu.Unlock()
	if d := r.downloads[guidG]; d != nil {
		return d.state.String()
	}
	return "gone"
}

func (r *stateRig) setAnswer(answer downloadAdmission) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = answer
}

// counts are G's offers and the tracker's cancels of it since the last
// call, and the last state of G the Manager saw.
func (r *stateRig) counts() (offers, cancels int, seen string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	offers, cancels = r.offers, r.cancels
	r.offers, r.cancels = 0, 0
	if r.seen != 0 {
		seen = r.seen.String()
	}
	return offers, cancels, seen
}

// The setups put G in each state, its owner known from then on (the
// Manager answers admitted); any report they hold stays held for the
// event.
var downloadStateSetups = map[string]func(r *stateRig){
	"awaiting": func(r *stateRig) { r.begin(guidG) },
	"offered": func(r *stateRig) {
		r.begin(guidG)
		r.setAnswer(downloadAdmitted)
		r.begin(guidB)
		r.hold(func(gate chan struct{}) { r.bGate = gate }, func() { r.report(guidB, cdpbrowser.DownloadProgressStateInProgress) })
		r.offerAgain(cdpbrowser.EventDownloadWillBegin{GUID: guidG, FrameID: "F1"}, waitGoing)
	},
	"offering": func(r *stateRig) {
		r.begin(guidG)
		r.setAnswer(downloadAdmitted)
		r.hold(func(gate chan struct{}) { r.offerGate = gate }, func() {
			r.offerAgain(cdpbrowser.EventDownloadWillBegin{GUID: guidG, FrameID: "F1"}, waitGoing)
		})
	},
	"admitted":  func(r *stateRig) { r.setAnswer(downloadAdmitted); r.begin(guidG) },
	"canceling": func(r *stateRig) { r.setAnswer(downloadAdmitted); r.begin(guidG); r.canceling(guidG) },
	"refused":   func(r *stateRig) { r.setAnswer(downloadRefused); r.begin(guidG) },
	"ending": func(r *stateRig) {
		r.setAnswer(downloadAdmitted)
		r.begin(guidG)
		r.hold(func(gate chan struct{}) { r.endGate = gate }, func() { r.report(guidG, cdpbrowser.DownloadProgressStateCompleted) })
	},
}

// The events, each one input of the tracker's.
var downloadStateEvents = map[string]func(r *stateRig){
	"retry": func(r *stateRig) {
		r.offerAgain(cdpbrowser.EventDownloadWillBegin{GUID: guidG, FrameID: "F1"}, waitGoing)
	},
	"expiry": func(r *stateRig) {
		r.offerAgain(cdpbrowser.EventDownloadWillBegin{GUID: guidG, FrameID: "F1"}, waitExpired)
	},
	"lost": func(r *stateRig) {
		r.offerAgain(cdpbrowser.EventDownloadWillBegin{GUID: guidG, FrameID: "F1"}, waitLost)
	},
	// cancel is a cancel Chromium accepted, refusal one it refused.
	"cancel": func(r *stateRig) { r.canceling(guidG) },
	"refusal": func(r *stateRig) {
		undo, _ := r.canceling(guidG)
		undo()
	},
	// unowned has the Manager answer G's offers unowned from here.
	"unowned":   func(r *stateRig) { r.setAnswer(downloadUnowned) },
	"progress":  func(r *stateRig) { r.report(guidG, cdpbrowser.DownloadProgressStateInProgress) },
	"completed": func(r *stateRig) { r.report(guidG, cdpbrowser.DownloadProgressStateCompleted) },
	"canceled":  func(r *stateRig) { r.report(guidG, cdpbrowser.DownloadProgressStateCanceled) },
	"exit":      func(r *stateRig) { r.stop() },
	// held is G's completion, its report held at the Manager.
	"held": func(r *stateRig) {
		r.hold(func(gate chan struct{}) { r.endGate = gate }, func() { r.report(guidG, cdpbrowser.DownloadProgressStateCompleted) })
	},
	// close is its tab's close as the Manager makes it (pageLeftLocked):
	// a download whose start says its end was received is left to that
	// end, and any other is canceled.
	"close": func(r *stateRig) {
		r.mu.Lock()
		ended := r.endReported
		r.mu.Unlock()
		if ended == nil || !ended() {
			r.canceling(guidG)
		}
	},
	// answered lets what the setup holds go: the Manager answers.
	"answered": func(r *stateRig) { r.release() },
}

// Every state of a download crossed with every event the tracker takes.
// After the event, whatever it holds is let go, with the owner known, and
// a download still awaiting is offered once more. A row names G's state
// right after the event and once settled, the offers of G the Manager
// took and the cancels the tracker sent after the setup, the last report
// of G the Manager saw, what was done with G's files and how many times
// the directory was closed.
func TestDownloadTrackerStates(t *testing.T) {
	type want struct {
		after, final    string
		offers, cancels int
		seen, files     string
		closes          int
	}
	const in, done, failed, canceled = "inProgress", "completed", "failed", "canceled"
	rows := []struct {
		state  string
		events []string
		want   want
	}{
		{"awaiting", []string{"retry"}, want{"admitted", "admitted", 1, 0, "", "", 0}},
		{"awaiting", []string{"expiry"}, want{"refused", "refused", 0, 1, "", "", 0}},
		{"awaiting", []string{"lost"}, want{"refused", "refused", 0, 0, "", "", 0}},
		{"awaiting", []string{"cancel"}, want{"awaiting", "canceling", 1, 0, "", "", 0}},
		// A cancel Chromium refused leaves no mark for the admission.
		{"awaiting", []string{"refusal"}, want{"awaiting", "admitted", 1, 0, "", "", 0}},
		{"awaiting", []string{"progress"}, want{"awaiting", "admitted", 1, 0, in, "", 0}},
		{"awaiting", []string{"completed"}, want{"awaiting", "gone", 1, 0, done, "handOver", 0}},
		{"awaiting", []string{"canceled"}, want{"awaiting", "gone", 1, 0, failed, "discard", 0}},
		{"awaiting", []string{"exit"}, want{"gone", "gone", 0, 0, "", "", 1}},

		{"offered", []string{"retry"}, want{"offered", "admitted", 1, 0, "", "", 0}},
		{"offered", []string{"expiry"}, want{"refused", "refused", 0, 1, "", "", 0}},
		{"offered", []string{"lost"}, want{"refused", "refused", 0, 0, "", "", 0}},
		{"offered", []string{"cancel"}, want{"offered", "canceling", 1, 0, "", "", 0}},
		{"offered", []string{"progress"}, want{"offered", "admitted", 1, 0, in, "", 0}},
		{"offered", []string{"completed"}, want{"offered", "gone", 1, 0, done, "handOver", 0}},
		{"offered", []string{"canceled"}, want{"offered", "gone", 1, 0, failed, "discard", 0}},
		{"offered", []string{"exit"}, want{"gone", "gone", 0, 0, "", "", 1}},
		// A retry queued behind another download's report whose wait
		// expires before its turn is never offered: the Manager never
		// admits what the tracker refused, and its completion's file goes.
		{"offered", []string{"expiry", "completed"}, want{"gone", "gone", 0, 1, "", "discard", 0}},

		{"offering", []string{"retry"}, want{"offering", "admitted", 0, 0, "", "", 0}},
		// A wait that expires while the Manager answers leaves the answer
		// to decide: an admission stands, running and never canceled, and
		// a later end of Chromium's own is a failure.
		{"offering", []string{"expiry"}, want{"offering", "admitted", 0, 0, "", "", 0}},
		{"offering", []string{"expiry", "answered", "canceled"}, want{"gone", "gone", 0, 0, failed, "discard", 0}},
		// An answer of unowned meets the expired wait: refused and canceled.
		{"offering", []string{"unowned", "expiry"}, want{"offering", "refused", 0, 1, "", "", 0}},
		// One that ended by itself meanwhile is a failure, with nothing
		// left to cancel.
		{"offering", []string{"expiry", "canceled"}, want{"offering", "gone", 0, 0, failed, "discard", 0}},
		{"offering", []string{"lost"}, want{"offering", "admitted", 0, 0, "", "", 0}},
		{"offering", []string{"cancel"}, want{"offering", "canceling", 0, 0, "", "", 0}},
		// A cancel Chromium accepted while the Manager answered is listed
		// as the cancel it was.
		{"offering", []string{"cancel", "canceled"}, want{"offering", "gone", 0, 0, canceled, "discard", 0}},
		// One that finished all the same is the Manager's as far as its
		// report took it; what is left of its file is withdrawn.
		{"offering", []string{"cancel", "completed"}, want{"offering", "gone", 0, 0, done, "handOver withdraw", 0}},
		// A refused cancel leaves no mark: a later end of Chromium's own is
		// a failure.
		{"offering", []string{"refusal", "answered", "canceled"}, want{"gone", "gone", 0, 0, failed, "discard", 0}},
		{"offering", []string{"progress"}, want{"offering", "admitted", 0, 0, in, "", 0}},
		{"offering", []string{"completed"}, want{"offering", "gone", 0, 0, done, "handOver", 0}},
		{"offering", []string{"canceled"}, want{"offering", "gone", 0, 0, failed, "discard", 0}},
		{"offering", []string{"exit"}, want{"gone", "gone", 0, 0, "", "", 1}},

		{"admitted", []string{"retry"}, want{"admitted", "admitted", 0, 0, "", "", 0}},
		{"admitted", []string{"expiry"}, want{"admitted", "admitted", 0, 0, "", "", 0}},
		{"admitted", []string{"lost"}, want{"admitted", "admitted", 0, 0, "", "", 0}},
		{"admitted", []string{"cancel"}, want{"canceling", "canceling", 0, 0, "", "", 0}},
		{"admitted", []string{"refusal"}, want{"admitted", "admitted", 0, 0, "", "", 0}},
		{"admitted", []string{"refusal", "canceled"}, want{"gone", "gone", 0, 0, failed, "discard", 0}},
		{"admitted", []string{"progress"}, want{"admitted", "admitted", 0, 0, in, "", 0}},
		{"admitted", []string{"completed"}, want{"gone", "gone", 0, 0, done, "handOver", 0}},
		{"admitted", []string{"canceled"}, want{"gone", "gone", 0, 0, failed, "discard", 0}},
		{"admitted", []string{"exit"}, want{"gone", "gone", 0, 0, "", "", 1}},

		{"canceling", []string{"retry"}, want{"canceling", "canceling", 0, 0, "", "", 0}},
		{"canceling", []string{"expiry"}, want{"canceling", "canceling", 0, 0, "", "", 0}},
		{"canceling", []string{"lost"}, want{"canceling", "canceling", 0, 0, "", "", 0}},
		{"canceling", []string{"cancel"}, want{"canceling", "canceling", 0, 0, "", "", 0}},
		// A second cancel's refusal withdraws only its own, which it never
		// made.
		{"canceling", []string{"refusal"}, want{"canceling", "canceling", 0, 0, "", "", 0}},
		{"canceling", []string{"progress"}, want{"canceling", "canceling", 0, 0, in, "", 0}},
		{"canceling", []string{"completed"}, want{"gone", "gone", 0, 0, done, "handOver withdraw", 0}},
		{"canceling", []string{"canceled"}, want{"gone", "gone", 0, 0, canceled, "discard", 0}},
		{"canceling", []string{"exit"}, want{"gone", "gone", 0, 0, "", "", 1}},

		{"refused", []string{"retry"}, want{"refused", "refused", 0, 0, "", "", 0}},
		{"refused", []string{"expiry"}, want{"refused", "refused", 0, 0, "", "", 0}},
		{"refused", []string{"lost"}, want{"refused", "refused", 0, 0, "", "", 0}},
		{"refused", []string{"cancel"}, want{"refused", "refused", 0, 0, "", "", 0}},
		{"refused", []string{"progress"}, want{"refused", "refused", 0, 0, "", "", 0}},
		{"refused", []string{"completed"}, want{"gone", "gone", 0, 0, "", "discard", 0}},
		{"refused", []string{"canceled"}, want{"gone", "gone", 0, 0, "", "discard", 0}},
		{"refused", []string{"exit"}, want{"gone", "gone", 0, 0, "", "", 1}},

		{"ending", []string{"retry"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		{"ending", []string{"expiry"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		{"ending", []string{"lost"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		// A cancel of an end being reported has nothing to send; what the
		// report leaves of the file is withdrawn.
		{"ending", []string{"cancel"}, want{"ending", "gone", 0, 0, done, "handOver withdraw", 0}},
		{"ending", []string{"progress"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		{"ending", []string{"completed"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		{"ending", []string{"canceled"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		// The browser's exit keeps the file of an end being reported, and
		// the directory goes once that report is done.
		{"ending", []string{"exit"}, want{"ending", "gone", 0, 0, done, "handOver kept", 1}},

		// An end received in any state is the Manager's from its receipt: a
		// tab closed before its report leaves the row to that report, so
		// nothing is canceled and the finished file is not withdrawn.
		{"awaiting", []string{"completed", "close"}, want{"awaiting", "gone", 1, 0, done, "handOver", 0}},
		{"awaiting", []string{"canceled", "close"}, want{"awaiting", "gone", 1, 0, failed, "discard", 0}},
		{"offered", []string{"completed", "close"}, want{"offered", "gone", 1, 0, done, "handOver", 0}},
		{"offered", []string{"canceled", "close"}, want{"offered", "gone", 1, 0, failed, "discard", 0}},
		{"offering", []string{"completed", "close"}, want{"offering", "gone", 0, 0, done, "handOver", 0}},
		{"offering", []string{"canceled", "close"}, want{"offering", "gone", 0, 0, failed, "discard", 0}},
		{"admitted", []string{"held", "close"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		{"canceling", []string{"held", "close"}, want{"ending", "gone", 0, 0, done, "handOver withdraw", 0}},
		{"refused", []string{"completed", "close"}, want{"gone", "gone", 0, 0, "", "discard", 0}},
		{"ending", []string{"close"}, want{"ending", "gone", 0, 0, done, "handOver", 0}},
		// A cancel after Chromium's own end was received has nothing to
		// cancel: that end is listed as the failure it was.
		{"awaiting", []string{"canceled", "cancel"}, want{"awaiting", "gone", 1, 0, failed, "discard", 0}},
		{"offering", []string{"canceled", "cancel"}, want{"offering", "gone", 0, 0, failed, "discard", 0}},
		// The browser's exit keeps an end held for an admission in flight,
		// which reports it if the answer admits it and otherwise removes
		// its file; the directory goes after either.
		{"offering", []string{"completed", "close", "exit"}, want{"offering", "gone", 0, 0, done, "kept handOver", 1}},
		{"offering", []string{"unowned", "completed", "exit"}, want{"offering", "gone", 0, 0, "", "kept discard", 1}},
		// A tab closed while its download runs cancels it.
		{"admitted", []string{"close", "canceled"}, want{"gone", "gone", 0, 0, canceled, "discard", 0}},
	}
	for _, row := range rows {
		name := row.state
		for _, event := range row.events {
			name += "/" + event
		}
		t.Run(name, func(t *testing.T) {
			r := newStateRig(t)
			downloadStateSetups[row.state](r)
			if got := r.state(); got != row.state {
				t.Fatalf("the setup left G %s", got)
			}
			r.setAnswer(downloadAdmitted)
			r.counts()
			for _, event := range row.events {
				downloadStateEvents[event](r)
			}
			after := r.state()
			r.release()
			if r.state() == "awaiting" {
				downloadStateEvents["retry"](r)
			}
			offers, cancels, seen := r.counts()
			got := want{after, r.state(), offers, cancels, seen, r.files.of(guidG), r.files.closes()}
			if got != row.want {
				t.Fatalf("got  %+v\nwant %+v", got, row.want)
			}
		})
	}
}
