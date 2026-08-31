package app

import (
	"context"
	"go/ast"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// draftBroadcasts captures the `draft:updated` frames one write produced.
type draftBroadcasts struct {
	t      *testing.T
	events []DraftUpdatedEvent
}

func captureDraftBroadcasts(t *testing.T, app *App) *draftBroadcasts {
	t.Helper()
	recorder := &draftBroadcasts{t: t}
	app.emitEventFn = func(name string, data any) {
		if name != "draft:updated" {
			return
		}
		evt, ok := data.(DraftUpdatedEvent)
		if !ok {
			t.Errorf("draft:updated payload type = %T, want app.DraftUpdatedEvent", data)
			return
		}
		recorder.events = append(recorder.events, evt)
	}
	return recorder
}

func (r *draftBroadcasts) reset() { r.events = nil }

func (r *draftBroadcasts) expectOne(what string) DraftUpdatedEvent {
	r.t.Helper()
	if len(r.events) != 1 {
		r.t.Fatalf("%s emitted %d draft:updated events, want 1: %+v", what, len(r.events), r.events)
	}
	return r.events[0]
}

func (r *draftBroadcasts) expectSilence(what string) {
	r.t.Helper()
	if len(r.events) != 0 {
		r.t.Fatalf("%s emitted %d draft:updated events, want 0: %+v", what, len(r.events), r.events)
	}
}

// ctxFromClient builds the context a bound method sees when the named screen
// issued the call, the same way the transport assembles it at upgrade.
func ctxFromClient(client transport.ClientIdentity) context.Context {
	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{Client: client})
	return ctx
}

// draftTestApp is an app with the named threads already present, since a draft
// row hangs off a thread by foreign key.
func draftTestApp(t *testing.T, threadIDs ...string) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	for _, id := range threadIDs {
		if err := app.store.CreateThread(testThread(id)); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}
	return app
}

func TestDraftWritesBroadcastAndCarryTheWriter(t *testing.T) {
	phone := transport.ClientIdentity{DeviceID: "device-phone", ConnectionID: "conn-phone-1"}

	t.Run("a save announces the thread and the writer", func(t *testing.T) {
		app := draftTestApp(t, "thr-1")
		broadcasts := captureDraftBroadcasts(t, app)

		if err := app.SaveDraft(ctxFromClient(phone), "thr-1", "hello", nil, nil, nil); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		evt := broadcasts.expectOne("a first save")
		if evt.ThreadID != "thr-1" {
			t.Errorf("frame = %+v, want a frame for thr-1", evt)
		}
		if evt.DeviceID != phone.DeviceID || evt.ConnectionID != phone.ConnectionID {
			t.Errorf("frame = %+v, want the writing screen's identity %+v", evt, phone)
		}
		if evt.UpdatedAt <= 0 {
			t.Errorf("frame carried updatedAt %d, want the persisted timestamp", evt.UpdatedAt)
		}
	})

	// The composer autosaves. A save of a buffer nobody touched must not wake
	// every attached client on a timer.
	t.Run("re-saving identical content says nothing", func(t *testing.T) {
		app := draftTestApp(t, "thr-1")
		if err := app.SaveDraft(ctxFromClient(phone), "thr-1", "hello", nil, nil, nil); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		broadcasts := captureDraftBroadcasts(t, app)
		if err := app.SaveDraft(ctxFromClient(phone), "thr-1", "hello", nil, nil, nil); err != nil {
			t.Fatalf("repeat SaveDraft: %v", err)
		}
		broadcasts.expectSilence("an autosave of unchanged content")
	})

	t.Run("a clear announces the thread, and clearing nothing says nothing", func(t *testing.T) {
		app := draftTestApp(t, "thr-1")
		if err := app.SaveDraft(ctxFromClient(phone), "thr-1", "hello", nil, nil, nil); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		broadcasts := captureDraftBroadcasts(t, app)

		if err := app.ClearDraft(ctxFromClient(phone), "thr-1"); err != nil {
			t.Fatalf("ClearDraft: %v", err)
		}
		// A delete carries the moment it happened rather than the vanished
		// row's stored timestamp, so the frame announcing it is still dateable.
		if evt := broadcasts.expectOne("a clear"); evt.ThreadID != "thr-1" || evt.UpdatedAt <= 0 {
			t.Errorf("frame = %+v, want a dated frame for thr-1", evt)
		}

		broadcasts.reset()
		if err := app.ClearDraft(ctxFromClient(phone), "thr-1"); err != nil {
			t.Fatalf("repeat ClearDraft: %v", err)
		}
		broadcasts.expectSilence("clearing a thread with no draft")
	})

	// A call with no connection behind it — an in-process binding, a test, a
	// background saga — is anonymous rather than an error, and every client
	// applies the frame it produces.
	t.Run("a write with no screen behind it carries no identity", func(t *testing.T) {
		app := draftTestApp(t, "thr-1")
		broadcasts := captureDraftBroadcasts(t, app)

		if err := app.SaveDraft(context.Background(), "thr-1", "hello", nil, nil, nil); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		if evt := broadcasts.expectOne("an unattributed save"); evt.DeviceID != "" || evt.ConnectionID != "" {
			t.Errorf("frame = %+v, want no identity", evt)
		}
	})

	// Content never rides the channel: GetDraft is loopback-only for the
	// disclosure reason, and a push carrying the text would be the way around
	// that. Receivers re-read instead.
	t.Run("the frame carries no draft text", func(t *testing.T) {
		app := draftTestApp(t, "thr-1")
		broadcasts := captureDraftBroadcasts(t, app)
		const typed = "the-secret-sentence-the-user-typed"
		if err := app.SaveDraft(ctxFromClient(phone), "thr-1", typed, nil, nil, nil); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		evt := broadcasts.expectOne("a save")
		for _, field := range []string{evt.ThreadID, evt.DeviceID, evt.ConnectionID} {
			if strings.Contains(field, typed) {
				t.Fatalf("frame carried the draft text: %+v", evt)
			}
		}
	})
}

// Every persisted draft write goes through one pair of helpers, because the
// draft row is written from a dozen places — a send consuming it, a queue
// dispatch, a fork seeding one, two saga paths parking and restoring one — and
// a per-call-site emit would have converged some of them and silently not the
// rest.
func TestOnlyTheDraftHelpersWriteDrafts(t *testing.T) {
	const (
		upsert = "UpsertThreadDraft"
		remove = "DeleteThreadDraft"
	)
	allowed := map[string]string{
		upsert: "writeThreadDraft",
		remove: "removeThreadDraft",
	}
	for name, file := range parsePackageFiles(t, appPackageDir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				wanted, guarded := allowed[sel.Sel.Name]
				if !guarded || fn.Name.Name == wanted {
					return true
				}
				t.Errorf("%s: %s calls store.%s directly; route it through %s so the write is announced",
					name, fn.Name.Name, sel.Sel.Name, wanted)
				return true
			})
		}
	}
}

// The two helpers must stay the only emit sites for the channel, so that
// "persisted" and "announced" cannot come apart.
func TestDraftBroadcastHasOneEmitSite(t *testing.T) {
	var emitters []string
	for _, file := range parsePackageFiles(t, appPackageDir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "broadcastDraft" {
					emitters = append(emitters, fn.Name.Name)
				}
				return true
			})
		}
	}
	slices.Sort(emitters)
	emitters = slices.Compact(emitters)
	if !slices.Equal(emitters, []string{"removeThreadDraft", "writeThreadDraft"}) {
		t.Fatalf("broadcastDraft callers = %v, want exactly [removeThreadDraft writeThreadDraft]", emitters)
	}
}

// A draft the backend wrote on the user's behalf still converges every client:
// the saga has no screen to credit, which is what the empty identity means.
func TestBackendWrittenDraftsBroadcastAnonymously(t *testing.T) {
	app := draftTestApp(t, "thr-saga")
	broadcasts := captureDraftBroadcasts(t, app)

	if err := app.writeThreadDraft(transport.ClientIdentity{}, store.ThreadDraft{
		ThreadID:  "thr-saga",
		Content:   "restored by a saga",
		UpdatedAt: 42,
	}); err != nil {
		t.Fatalf("writeThreadDraft: %v", err)
	}
	evt := broadcasts.expectOne("a saga write")
	if evt.ThreadID != "thr-saga" || evt.UpdatedAt != 42 {
		t.Fatalf("frame = %+v, want thr-saga at 42", evt)
	}
	if evt.DeviceID != "" || evt.ConnectionID != "" {
		t.Fatalf("frame = %+v, want no identity for a backend write", evt)
	}
}
