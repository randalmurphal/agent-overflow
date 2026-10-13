package triage

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/eventchan"
)

// MarkThreadActive clears process-bound state only through
// processBoundState, so the list's writer cases cover everything a commit
// resets. Two copies of a thread's state with every field set are cleared,
// one by the list's clears and one by MarkThreadActive; the commit may
// change nothing else, apart from the identity fields that record the
// commit itself.
func TestMarkThreadActiveClearsOnlyThroughProcessBoundState(t *testing.T) {
	t.Parallel()
	const threadID = "thread-commit-clears"
	listState, listIdentity := &threadState{}, &threadIdentity{}
	fillNonZero(reflect.ValueOf(listState).Elem(), 0)
	fillNonZero(reflect.ValueOf(listIdentity).Elem(), 0)
	for _, entry := range processBoundState {
		if entry.clear != nil {
			entry.clear(listState, listIdentity)
		}
	}

	r := NewRouter(nil, func(eventchan.Channel, any) {})
	commitState, commitIdentity := &threadState{}, &threadIdentity{}
	fillNonZero(reflect.ValueOf(commitState).Elem(), 0)
	fillNonZero(reflect.ValueOf(commitIdentity).Elem(), 0)
	r.threads[threadID] = commitState
	r.identities[threadID] = commitIdentity
	r.MarkThreadActive(threadID, "session-b")
	commitIdentity.stopped, commitIdentity.sessionToken = listIdentity.stopped, listIdentity.sessionToken
	commitIdentity.epoch, commitIdentity.effectiveModelRevision = listIdentity.epoch, listIdentity.effectiveModelRevision

	for _, pair := range [][2]reflect.Value{
		{reflect.ValueOf(listState).Elem(), reflect.ValueOf(commitState).Elem()},
		{reflect.ValueOf(listIdentity).Elem(), reflect.ValueOf(commitIdentity).Elem()},
	} {
		for i := range pair[0].NumField() {
			if !reflect.DeepEqual(exported(pair[0].Field(i)).Interface(), exported(pair[1].Field(i)).Interface()) {
				t.Errorf("%s.%s: MarkThreadActive and processBoundState's clears leave it different; clear it through the list", pair[0].Type().Name(), pair[0].Type().Field(i).Name)
			}
		}
	}
}

// fillNonZero sets every field reachable from v to a non-zero value, so a
// clear of any of them shows. Locks, atomics, timers, funcs, channels and
// interfaces stay zero: no clear resets them.
func fillNonZero(v reflect.Value, depth int) {
	v = exported(v)
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.String:
		v.SetString("x")
	case reflect.Pointer:
		if depth > 3 || skippedType(v.Type().Elem()) {
			return
		}
		p := reflect.New(v.Type().Elem())
		fillNonZero(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		key, elem := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillNonZero(key, depth+1)
		fillNonZero(elem, depth+1)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, elem)
		v.Set(m)
	case reflect.Array:
		for i := range v.Len() {
			fillNonZero(v.Index(i), depth+1)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Unix(7, 0)))
			return
		}
		if skippedType(v.Type()) {
			return
		}
		for i := range v.NumField() {
			fillNonZero(v.Field(i), depth+1)
		}
	}
}

func skippedType(t reflect.Type) bool {
	switch t.PkgPath() {
	case "sync", "sync/atomic", "time":
		return true
	}
	return false
}

// exported makes an unexported field settable and readable.
func exported(v reflect.Value) reflect.Value {
	if v.CanSet() || !v.CanAddr() {
		return v
	}
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}
