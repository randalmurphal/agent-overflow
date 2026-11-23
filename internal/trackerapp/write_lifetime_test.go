package trackerapp

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// holdCall holds the provider's first call of name until the returned
// release runs; arrived is closed once it is in.
func (f *writeFixture) holdCall(name string) (arrived <-chan struct{}, release func()) {
	in := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	f.p.set(func(p *scripted) {
		p.block = func(ctx context.Context, n string) error {
			if n != name {
				return nil
			}
			held := false
			once.Do(func() { held = true })
			if !held {
				return nil
			}
			close(in)
			select {
			case <-rel:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	return in, sync.OnceFunc(func() { close(rel) })
}

// editInBackground runs an edit of 10001 and returns its answer.
func (f *writeFixture) editInBackground(t *testing.T) <-chan writeAnswer {
	done := make(chan writeAnswer, 1)
	go func() {
		v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
		done <- writeAnswer{v, err}
	}()
	return done
}

type writeAnswer struct {
	v   WriteView
	err error
}

var appliedEdit = tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001"))}}

// A write to a connection that is off, removed, a Tempo connection's, or
// made after Stop is refused by name, and nothing is sent.
func TestWritesToAConnectionNotInUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		end  func(f *writeFixture) string
		want error
	}{
		{"off", func(f *writeFixture) string {
			must[Connection](f.t)(f.svc.SetEnabled(f.conn.ID, false))
			return f.conn.ID
		}, ErrDisabled},
		{"removed", func(f *writeFixture) string {
			if err := f.svc.Remove(f.conn.ID); err != nil {
				f.t.Fatal(err)
			}
			return f.conn.ID
		}, ErrNotFound},
		{"Tempo", func(f *writeFixture) string { return f.addTempo(f.conn.ID).ID }, ErrNoProvider},
		{"stopped", func(f *writeFixture) string { f.svc.Stop(); return f.conn.ID }, ErrStopped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newWriteFixture(t)
			f.answer(appliedEdit)
			id := c.end(f)
			before := len(f.p.callsInOrder())
			_, err := f.svc.Edit(t.Context(), id, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if got := f.callsSince(before); len(got) > 0 {
				t.Fatalf("calls %v", got)
			}
		})
	}
}

// A connection turned off or removed while the provider's write is in
// flight: a write that may have happened answers its parts with Ended and
// reads nothing, since the mirror went with the connection; one that did
// not happen is refused by name. The same holds of a Stop. On again, the
// new mirror holds nothing of the write.
func TestEndedDuringTheProviderCall(t *testing.T) {
	t.Parallel()
	notSent := tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: tracker.StepEdit, Outcome: tracker.NotSent(errors.New("the client is closed"))}}}
	off := func(f *writeFixture) { must[Connection](f.t)(f.svc.SetEnabled(f.conn.ID, false)) }
	// stop stops the service, which waits for the write.
	stop := func(f *writeFixture) {
		go f.svc.Stop()
		for {
			f.svc.mu.Lock()
			stopped := f.svc.stopped
			f.svc.mu.Unlock()
			if stopped {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	cases := []struct {
		name  string
		res   tracker.Result
		end   func(f *writeFixture)
		want  error
		again bool
	}{
		{"off, applied", appliedEdit, off, nil, false},
		{"off then on, applied", appliedEdit, off, nil, true},
		{"off, not sent", notSent, off, ErrDisabled, false},
		{"removed, not sent", notSent, func(f *writeFixture) {
			if err := f.svc.Remove(f.conn.ID); err != nil {
				f.t.Fatal(err)
			}
		}, ErrNotFound, false},
		{"stopped, applied", appliedEdit, stop, nil, false},
		{"stopped, not sent", notSent, stop, ErrStopped, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newWriteFixture(t)
			f.answer(c.res)
			f.state.set(testBoard, writeRow("10001", 1, "z"))
			arrived, release := f.holdCall("Edit")
			before := len(f.p.callsInOrder())
			done := f.editInBackground(t)
			<-arrived
			c.end(f)
			if c.again {
				must[Connection](t)(f.svc.SetEnabled(f.conn.ID, true))
			}
			release()
			a := <-done
			if c.want != nil {
				if !errors.Is(a.err, c.want) {
					t.Fatalf("got %+v, %v; want %v", a.v, a.err, c.want)
				}
			} else if a.err != nil || !a.v.Ended || !slices.Equal(states(a.v), []string{"10001 edit applied"}) {
				t.Fatalf("got %+v, %v", a.v, a.err)
			}
			if got := f.callsSince(before); !slices.Equal(got, []string{"Edit"}) {
				t.Fatalf("calls %v", got)
			}
			if f.problem() != "" {
				t.Fatalf("problem %q", f.problem())
			}
			if c.again {
				tickOK(t, f.svc, Scope{Connection: f.conn.ID, Board: "7"}, BoardDue{Structure: true})
				if got := f.orderOf(t, testBoard, "10001"); got != "" {
					t.Fatalf("the write's issue is in the new mirror, order %q", got)
				}
			}
		})
	}
}

// Off then on while the refresh reads: the rows it read land in no
// mirror, the reads after it are not made, and the parts answer Ended.
func TestTurnedOffAndOnDuringTheRefresh(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.answer(appliedEdit)
	f.state.set(testBoard, writeRow("10001", 1, "z"))
	old := f.root()
	arrived, release := f.holdCall("BoardRows")
	before := len(f.p.callsInOrder())
	done := f.editInBackground(t)
	<-arrived
	must[Connection](t)(f.svc.SetEnabled(f.conn.ID, false))
	must[Connection](t)(f.svc.SetEnabled(f.conn.ID, true))
	tickOK(t, f.svc, Scope{Connection: f.conn.ID, Board: "7"}, BoardDue{Structure: true})
	if f.root() == old || f.root() == 0 {
		t.Fatalf("on again, root %d", f.root())
	}
	release()
	a := <-done
	if a.err != nil || !a.v.Ended || len(a.v.Stale) > 0 {
		t.Fatalf("got %+v, %v", a.v, a.err)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Edit", "BoardRows", "Structure"}) {
		t.Fatalf("calls %v", got)
	}
	if cards := must[[]store.MirrorCard](t)(f.st.ListMirrorCards(f.root(), []string{"10001"})); len(cards) > 0 || f.orderOf(t, testBoard, "10001") != "" {
		t.Fatalf("the refresh landed in the new mirror: %v", cards)
	}
	if f.problem() != "" {
		t.Fatalf("problem %q", f.problem())
	}
}
