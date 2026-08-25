package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/provider"
)

// sessionScopedStateGroups are the Session field groups whose contents describe
// THIS process's run — turns, child threads, quarantined child frames, the
// resume traversal, raw tool calls — and which Close therefore zeroes whole.
//
// The test below populates every field of every group reflectively, so a field
// added to one of them later cannot be forgotten by Close: it is filled here
// without anyone touching this file, and the post-Close assertion fails until
// Close drops it. That only holds while Close keeps assigning the ZERO GROUP
// (s.turn = sessionTurnState{}) rather than clearing field by field.
var sessionScopedStateGroups = []string{
	"turn",
	"collab",
	"childRouting",
	"collabHistory",
	"rawCalls",
}

// Deliberately NOT in the list above, each for its own reason:
//
//   - origins — turn-origin tracking. Bounded by maxTrackedTurnOrigins and
//     dropped per turn on completion; Close has never cleared it and clearing
//     it is a behavior change, not a rename.
//   - turnConfig / settings — what the next turn would ASK FOR and what Codex
//     last reported it is running. Both are fixed-size scalar blocks, neither
//     grows, and a closed session's config is still the honest answer to "what
//     was this thread running", which the app layer reads while tearing down.
//   - review — already its own sub-struct (*reviewRun). Close drops the
//     pointer, and the assertion for that is inline below.
//   - childLifecycleRevision — child state, but guarded by childLifecycleMu
//     rather than mu, so it cannot join a group that is assigned under mu.
//     Close drops it under its own lock; asserted inline below.
//   - pending — deliberately left as an empty map so a late sendRequest does
//     not panic writing to a nil map. See Close.
func TestCloseReleasesSessionScopedState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// `cat` is a stand-in process, never a provider CLI: Close needs a real
	// *provider.Process to close and this test must not spawn codex.
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		cancel:   cancel,
	}

	session := reflect.ValueOf(s).Elem()
	for _, name := range sessionScopedStateGroups {
		group := session.FieldByName(name)
		if !group.IsValid() || group.Kind() != reflect.Struct {
			t.Fatalf("Session has no struct field %q — update sessionScopedStateGroups", name)
		}
		for i := range group.NumField() {
			field := group.Type().Field(i)
			setUnexported(group.Field(i), nonZeroSample(t, field.Type))
			if group.Field(i).IsZero() {
				t.Fatalf("%s.%s: sample value was still zero", name, field.Name)
			}
		}
	}
	s.review = &reviewRun{turnIndex: 1}
	s.childLifecycleRevision = map[string]uint64{"child-thread": 1}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, name := range sessionScopedStateGroups {
		if group := session.FieldByName(name); !group.IsZero() {
			t.Errorf("Close left %s populated: %+v", name, group)
		}
	}
	if s.review != nil {
		t.Errorf("Close left review populated: %+v", s.review)
	}
	if s.childLifecycleRevision != nil {
		t.Errorf("Close left childLifecycleRevision populated: %+v", s.childLifecycleRevision)
	}
}

// setUnexported writes through an unexported field. Same package, so the field
// is readable; reflect still refuses the write without the address hop.
func setUnexported(field reflect.Value, value reflect.Value) {
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(value)
}

// nonZeroSample builds a value that is not the zero value for typ. It fails the
// test on a kind it does not know rather than returning something zero-valued:
// a silently-zero sample would make the post-Close assertion pass for a field
// Close never dropped.
func nonZeroSample(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	if typ == reflect.TypeFor[*time.Timer]() {
		// Close stops every deadline timer it drops, and Stop panics on a
		// hand-built zero Timer, so this one has to be a real timer.
		return reflect.ValueOf(time.NewTimer(time.Hour))
	}
	switch typ.Kind() {
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(typ)
	case reflect.String:
		return reflect.ValueOf("close-test").Convert(typ)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflect.ValueOf(int64(1)).Convert(typ)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflect.ValueOf(uint64(1)).Convert(typ)
	case reflect.Map:
		m := reflect.MakeMap(typ)
		m.SetMapIndex(mapSample(t, typ.Key()), mapSample(t, typ.Elem()))
		return m
	case reflect.Slice:
		return reflect.MakeSlice(typ, 1, 1)
	case reflect.Pointer:
		return reflect.New(typ.Elem())
	case reflect.Chan:
		return reflect.MakeChan(typ, 1)
	case reflect.Func:
		return reflect.MakeFunc(typ, func([]reflect.Value) []reflect.Value {
			t.Fatalf("sample func for %s was called", typ)
			return nil
		})
	}
	t.Fatalf("no non-zero sample for %s — teach nonZeroSample about it", typ)
	return reflect.Value{}
}

// mapSample supplies one key/value pair for a sampled map. A struct key or
// value falls back to its zero value: the map being non-nil is what makes the
// field non-zero, and Session's map keys include comparable structs
// (subagentNotificationDedupKey) with no meaningful "non-zero" form here.
func mapSample(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	if typ.Kind() == reflect.Struct {
		return reflect.New(typ).Elem()
	}
	return nonZeroSample(t, typ)
}
