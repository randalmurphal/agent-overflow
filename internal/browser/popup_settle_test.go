package browser

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// Where a parked download of a popup never listed goes, against every
// state its opener can be in when the download settles and every point it
// settles at: a row is in the log of exactly the page its settlement
// decided, or, with no page to take it, nowhere. Each row runs a popup's
// download from its start to its settlement through the Manager, with the
// opener's departure placed where the state puts it.

// settleOpener is the opener's state at the settlement.
type settleOpener int

const (
	// openerLive: listed throughout.
	openerLive settleOpener = iota + 1
	// openerDeparting: its tab closes once the settlement has chosen the
	// page the download goes to, while it lists it.
	openerDeparting
	// openerCarries: its tab closed after the popup's end was known, so its
	// leave carries the popup (popupCarrier).
	openerCarries
	// openerLeftFirst: its tab closed before the popup's end was known, so
	// no leave carries the popup.
	openerLeftFirst
	// openerThreadClosed: its thread closed after the popup's end was
	// known.
	openerThreadClosed
)

func (s settleOpener) String() string {
	switch s {
	case openerLive:
		return "its opener live"
	case openerDeparting:
		return "its opener departing as the download is listed"
	case openerCarries:
		return "its opener departed carrying the popup"
	case openerLeftFirst:
		return "its opener departed before the popup ended"
	case openerThreadClosed:
		return "its opener's thread closed"
	}
	panic(fmt.Sprintf("settle opener %d", int(s)))
}

// settleRow is one row's Manager, engine and opener.
type settleRow struct {
	t        *testing.T
	manager  *Manager
	engine   *adoptionEngine
	access   Access
	opener   ids.PageHandle
	holder   *managedPage
	state    settleOpener
	departed chan struct{}
}

// depart takes the opener away as the row's state does: its tab closes, or
// its thread does.
func (r *settleRow) depart() {
	if r.state == openerThreadClosed {
		if err := r.manager.CloseThread(r.t.Context(), r.access.ThreadID()); err != nil {
			r.t.Errorf("close the opener's thread: %v", err)
		}
		return
	}
	if err := r.manager.CloseCompanionPage(r.t.Context(), r.access, r.holder.id); err != nil {
		r.t.Errorf("close the opener: %v", err)
	}
}

// beforeEnd is what the row's state does before the popup's end is known:
// the opener that leaves first leaves, and the departing one's close is
// armed to run once the settlement has chosen the download's page. It
// runs at once where nothing holds the Manager's lock, and otherwise as
// soon as the settlement lets it go.
func (r *settleRow) beforeEnd() {
	switch r.state {
	case openerLeftFirst:
		r.depart()
	case openerDeparting:
		r.departed = make(chan struct{})
		var once sync.Once
		r.manager.downloadAdmitting = func() {
			once.Do(func() {
				if r.manager.mu.TryLock() {
					r.manager.mu.Unlock()
					r.depart()
					close(r.departed)
					return
				}
				go func() {
					r.depart()
					close(r.departed)
				}()
			})
		}
	case openerLive, openerCarries, openerThreadClosed:
	}
}

// afterEnd is what the row's state does once the popup's end is known and
// before its settlement.
func (r *settleRow) afterEnd() {
	switch r.state {
	case openerCarries, openerThreadClosed:
		r.depart()
	case openerLive, openerDeparting, openerLeftFirst:
	}
}

// settled waits for a departure the settlement set off.
func (r *settleRow) settled() {
	if r.departed == nil {
		return
	}
	select {
	case <-r.departed:
	case <-time.After(hostedTestDeadline):
		r.t.Fatal("the opener's departure never finished")
	}
}

// receivedEnd records the popup's received close, naming its opener, as
// the hosted engine's receipt does (engineEvents.PageEndReceived).
func (r *settleRow) receivedEnd() {
	r.manager.pageEndReceived("popup", true, "", []ids.PageHandle{r.opener})
}

// parked starts the popup's download dl-1 and reports it running at 10
// bytes, before its fate.
func (r *settleRow) parked() {
	r.t.Helper()
	if !popupDownload(r.manager, r.engine, "popup", "dl-1", r.opener) {
		r.t.Fatal("the popup's download was refused")
	}
	r.manager.downloadProgress(progressOf("dl-1", downloadInProgress, 10))
}

// settlePoint drives a row from the popup's download to its settlement;
// bytes are the download's received bytes as its settlement finds them;
// closes is set for the browser's close, which a departing opener has no
// place in.
type settlePoint struct {
	name   string
	bytes  int64
	closes bool
	run    func(r *settleRow)
}

var settlePoints = []settlePoint{
	{name: "settled at the popup's processed close", bytes: 10, run: func(r *settleRow) {
		r.parked()
		r.beforeEnd()
		r.receivedEnd()
		r.afterEnd()
		r.manager.removeClosedPage("popup")
	}},
	{name: "settled at its refused adoption's end", bytes: 10, run: func(r *settleRow) {
		r.engine.failing["popup"] = true
		r.parked()
		r.beforeEnd()
		if r.state != openerCarries && r.state != openerThreadClosed {
			r.manager.adoptPopup(r.engine.popup(r.opener, "popup"))
			return
		}
		// The refusal records the end; the opener leaves during the
		// discard, before the adoption's end settles the download.
		discarding, release := r.engine.holdDiscards(r.t)
		adopted := make(chan struct{})
		go func() {
			r.manager.adoptPopup(r.engine.popup(r.opener, "popup"))
			close(adopted)
		}()
		select {
		case <-discarding:
		case <-time.After(hostedTestDeadline):
			r.t.Fatal("the popup was never discarded")
		}
		r.afterEnd()
		release()
		awaitDone(r.t, adopted, "the adoption")
	}},
	{name: "settled at the park timeout", bytes: 10, run: func(r *settleRow) {
		r.parked()
		r.beforeEnd()
		r.receivedEnd()
		r.afterEnd()
		r.manager.resolveParked("popup", nil)
	}},
	{name: "started after the popup's fate", bytes: 0, run: func(r *settleRow) {
		r.beforeEnd()
		r.receivedEnd()
		r.manager.resolveParked("popup", nil)
		r.afterEnd()
		popupDownload(r.manager, r.engine, "popup", "dl-1", r.opener)
	}},
	{name: "settled at the browser's close", bytes: 10, closes: true, run: func(r *settleRow) {
		r.parked()
		r.beforeEnd()
		r.receivedEnd()
		r.afterEnd()
		if err := r.manager.Close(); err != nil {
			r.t.Fatalf("close the browser: %v", err)
		}
	}},
}

// settledRow is where a row's download is listed and how: in is "opener"
// for the opener's log, "" for no log.
type settledRow struct {
	in    string
	state string
	cause string
	bytes int64
}

func wantSettled(state settleOpener, point settlePoint) settledRow {
	switch state {
	case openerLive:
		if point.closes {
			return settledRow{in: "opener", state: downloadStateCanceled, cause: "the browser closed", bytes: point.bytes}
		}
		return settledRow{in: "opener", state: downloadStateRunning, bytes: point.bytes}
	case openerDeparting, openerCarries, openerThreadClosed:
		return settledRow{in: "opener", state: downloadStateCanceled, cause: "page closed", bytes: point.bytes}
	case openerLeftFirst:
		return settledRow{}
	}
	panic(fmt.Sprintf("settle opener %d", int(state)))
}

// The cross of the opener's state and the point the popup's download
// settles at, on each way an engine saves downloads: 5 states and 5
// points, but for a departure as the browser's close lists the download,
// which that close's own departures already are (24 rows a mode).
func TestAParkedDownloadSettlesInTheOneLogItsSettlementChose(t *testing.T) {
	states := []settleOpener{openerLive, openerDeparting, openerCarries, openerLeftFirst, openerThreadClosed}
	for _, mode := range downloadModes {
		for _, point := range settlePoints {
			for _, state := range states {
				if point.closes && state == openerDeparting {
					continue
				}
				t.Run(fmt.Sprintf("%s, %s, %s", mode.name, point.name, state), func(t *testing.T) {
					manager, engine, access, opener := newAdoptionManagerSaving(t, mode.saving)
					r := &settleRow{t: t, manager: manager, engine: engine, access: access, opener: opener, holder: listedPage(t, manager, opener), state: state}
					point.run(r)
					r.settled()
					want := wantSettled(state, point)
					var got settledRow
					for _, row := range r.holder.downloads.list() {
						if row.ID == "dl-1" {
							got = settledRow{in: "opener", state: row.State, cause: row.Error, bytes: row.Bytes}
						}
					}
					if got != want {
						t.Fatalf("the download is listed as %+v, want %+v", got, want)
					}
					manager.parkMu.Lock()
					parked := len(manager.parked)
					manager.parkMu.Unlock()
					if parked != 0 {
						t.Fatalf("%d popups still park downloads", parked)
					}
					if state == openerThreadClosed || point.closes {
						return
					}
					history := manager.CompanionState(access.Caller()).Downloads
					listed := len(history) == 1 && history[0].State == want.state
					if listed != (want.in != "") {
						t.Fatalf("the thread's history is %+v, want the row %+v", history, want)
					}
				})
			}
		}
	}
}
