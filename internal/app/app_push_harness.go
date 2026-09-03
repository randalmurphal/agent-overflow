package app

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"

	"agent-overflow/internal/harnessrpc"
	"agent-overflow/internal/push"
)

// app_push_harness.go is the ONE substitution the push path takes for a
// test: the thing that would have talked to Google.
//
// Everything above it stays production. The mapping decides what a moment
// is, the fan-out decides which phones are woken, `push.MessageFor` builds
// the payload and enforces what it may say, and the preference gate runs
// unchanged — so a spec asserting on what this recorded is asserting on
// the real composition, not on a re-description of it.
//
// Only the LAST hop is faked, because it is the only hop that needs a
// Firebase project and a device holding a registration token. That
// remains true of the shipping app: `mobile/AGENTS.md` § google-services
// names it as the one thing no gate can prove.

// harnessPushSender records what would have been sent, in order.
//
// A ring is deliberately absent. The harness is a fixture with a lifetime
// of one spec file, the payloads are a handful of short strings each, and
// a bound would make "the spec asserted on a message that had already been
// evicted" a failure mode nobody would diagnose quickly. `HarnessReset`
// clears it, which is the same lifetime every other harness ledger has.
type harnessPushSender struct {
	mu   sync.Mutex
	sent []harnessrpc.PushMessage
}

func (s *harnessPushSender) Send(_ context.Context, message push.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The map is copied rather than retained: `push.MessageFor` builds a
	// fresh one per message today, but a reader holding somebody else's
	// map is the kind of aliasing that only shows up once the caller is
	// optimized.
	data := make(map[string]string, len(message.Data))
	for key, value := range message.Data {
		data[key] = value
	}
	s.sent = append(s.sent, harnessrpc.PushMessage{
		Token: message.Token,
		Tag:   message.Tag,
		Data:  data,
	})
	return nil
}

// snapshot answers everything recorded so far, oldest first.
func (s *harnessPushSender) snapshot() []harnessrpc.PushMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]harnessrpc.PushMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *harnessPushSender) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = nil
}

// InstallHarnessPushSender gives a harness boot a sender that records
// instead of sending, so `make e2e` can prove the fan-out end to end.
//
// A package-level function rather than a method on *App, and that is
// structural rather than stylistic: every exported method on *App becomes
// a wire RPC (`methodgen` requires an `//ao:scope` and a route for one),
// and a way to swap the push sender is not something any session should be
// able to reach. The harness boot path calls this directly, in-process.
//
// A backend that already holds a REAL credential is left alone. A harness
// data dir is isolated and will not have one, but the rule is what makes
// this safe to call unconditionally: it can never take a working sender
// away from an owner who pasted one into an instance the harness reused.
func InstallHarnessPushSender(a *App) {
	if a == nil || a.store == nil {
		return
	}
	if _, err := a.store.GetPushSender(); err == nil {
		log.Printf("harness: push sender left as configured; the recorder is not installed")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("harness: read the push sender: %v", err)
		return
	}
	recorder := &harnessPushSender{}
	a.harnessPush.Store(recorder)
	a.installPushSender(recorder, "harness-project", "harness@agent-overflow.invalid")
}

// harnessPushMessages answers what this boot's recorder has been handed.
// An instance with no recorder answers nothing rather than an error: a
// spec asking "what was pushed" on a boot with no push configured has its
// answer, and it is the empty list.
func (a *App) harnessPushMessages() []harnessrpc.PushMessage {
	recorder := a.harnessPush.Load()
	if recorder == nil {
		return nil
	}
	return recorder.snapshot()
}

// harnessPushForget drops the ledger, so one spec's sends cannot be read
// by the next. Wired into the same reset every other harness ledger is.
func (a *App) harnessPushForget() {
	if recorder := a.harnessPush.Load(); recorder != nil {
		recorder.clear()
	}
}
