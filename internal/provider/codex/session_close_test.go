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
// resume recovery queue, raw tool calls — and which Close therefore zeroes whole.
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
	"rolloutTail",
}

// Every OTHER Session field must claim a disposition in
// sessionCloseFieldDispositions below — TestSessionFieldsHaveACloseDisposition
// fails on a new top-level field that never decided its Close story, which is
// how the guard-mutation review found five session-scoped maps sitting outside
// both the groups and the then-prose exclusions list (2026-08-25, finding 10).
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

// sessionCloseFieldDispositions names every Session field OUTSIDE the zeroed
// groups and states its Close story. The disposition strings are documentation
// with teeth: TestSessionFieldsHaveACloseDisposition cross-checks this map
// against the struct in both directions, so a new top-level field cannot land
// without deciding what Close does about it — the failure mode the
// guard-mutation review demonstrated with five undocumented session-scoped
// maps (2026-08-25, finding 10). A field holding UNBOUNDED session-scoped
// state belongs in a zeroed group instead; every map listed here must state
// its bound.
var sessionCloseFieldDispositions = map[string]string{
	// Cleared by Close, asserted inline in TestCloseReleasesSessionScopedState.
	"review":                 "cleared by Close (pointer drop); asserted inline",
	"childLifecycleRevision": "cleared by Close under childLifecycleMu; asserted inline",

	// Deliberately kept, bounded maps.
	"pending":                    "kept as an EMPTY map so a late sendRequest does not panic writing to nil; see Close",
	"unclaimedNotifications":     "bounded by maxUnclaimedNotificationMethods; kept — clearing is a behavior change",
	"reportedForeignSubmissions": "bounded by maxReportedForeignSubmissions; kept",
	"planBuffersByItemID":        "bounded by maxPlanDeltaBuffers; kept",
	"planBuffersByTurnID":        "bounded by maxPlanDeltaBuffers; kept",
	"mcpStartupStates":           "bounded by the configured server count; kept",

	// Deliberately kept, fixed-size state.
	"origins":                          "bounded by maxTrackedTurnOrigins, dropped per turn; Close never cleared it",
	"turnConfig":                       "fixed-size scalars; a closed session's config is the honest teardown answer",
	"settings":                         "fixed-size scalars + one small pointer; same teardown-read reason as turnConfig",
	"unclaimedNotificationsOverflowed": "bool rider on unclaimedNotifications",
	"queueListInflight":                "scalar flag",
	"queueListDirty":                   "scalar flag",
	"usageAcct":                        "fixed-size accounting scalars",
	"requestTimeoutOverride":           "scalar config",

	// Process/identity/infrastructure — lives exactly as long as the Session.
	"proc":     "process handle; Close closes it",
	"ctx":      "lifecycle context",
	"cancel":   "lifecycle cancel; Close calls it",
	"closing":  "the Close latch itself",
	"readDone": "read-loop join channel; Close waits on it",
	"threadID": "identity",
	"workDir":  "identity",
	"binary":   "identity",
	"nextID":   "request-id counter",
	"mu":       "the lock",
	"eventMu":  "the event-callback lock",

	// Handlers/callbacks installed at construction.
	"onEvent":                       "construction-time callback",
	"dynamicToolHandler":            "construction-time handler",
	"ownsQueuedClient":              "construction-time callback",
	"probeFn":                       "test seam",
	"resumeFn":                      "test seam",
	"cleanBackgroundTerminalsFn":    "test seam",
	"terminateBackgroundTerminalFn": "test seam",
	"interruptChildTurnFn":          "test seam",
	"mcpOAuthCompletedHandler":      "construction-time handler",
	"mcpStartupUpdateHandler":       "construction-time handler",
	"skillsChangedHandler":          "construction-time handler",

	// Join/serialization infrastructure Close waits on or latches.
	"approvals":           "provider.ApprovalRegistry; Close drains it via clearPendingApprovals",
	"childLifecycleMu":    "lock",
	"rolloutObserverWG":   "joined by Close",
	"collabMetadataReads": "serialization channel",
	"collabAsyncMu":       "lock",
	"collabAsyncClosing":  "the collab-async latch",
	"collabAsyncWG":       "joined by Close",

	// Atomic wire facts; honest answers after Close.
	"codexThreadID":     "identity, atomic",
	"appServerVersion":  "wire fact, atomic",
	"threadHistoryMode": "wire fact, atomic",
	"pendingRevert":     "atomic pointer, one small expectation",
	"threadQueueNative": "wire fact, atomic",
}

// TestSessionFieldsHaveACloseDisposition is the completeness half of the
// Close guard: every Session field is either in a zeroed group
// (sessionScopedStateGroups) or names its disposition above, and every
// disposition names a live field. Without this, a new top-level map on
// Session is invisible to TestCloseReleasesSessionScopedState.
func TestSessionFieldsHaveACloseDisposition(t *testing.T) {
	grouped := make(map[string]bool, len(sessionScopedStateGroups))
	for _, name := range sessionScopedStateGroups {
		grouped[name] = true
	}
	typ := reflect.TypeFor[Session]()
	seen := make(map[string]bool, typ.NumField())
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		seen[name] = true
		if grouped[name] {
			continue
		}
		if _, ok := sessionCloseFieldDispositions[name]; !ok {
			t.Errorf(
				"Session.%s has no Close disposition: add it to a zeroed group in Close (unbounded session-scoped state) or to sessionCloseFieldDispositions with its bound/reason",
				name,
			)
		}
	}
	for name := range sessionCloseFieldDispositions {
		if !seen[name] {
			t.Errorf("sessionCloseFieldDispositions names %q, which Session no longer has — delete the entry", name)
		}
		if grouped[name] {
			t.Errorf("sessionCloseFieldDispositions names %q, which is already in a zeroed group — delete the entry", name)
		}
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
