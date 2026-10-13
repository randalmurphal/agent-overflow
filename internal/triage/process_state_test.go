package triage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"go.opentelemetry.io/otel/trace"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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
	fill := func(v any) {
		for _, missing := range fillNonZero(reflect.ValueOf(v).Elem(), map[reflect.Type]bool{}) {
			t.Errorf("no filler for %s: add one to interfaceFillers so a clear of it shows", missing)
		}
	}
	listState, listIdentity := &threadState{}, &threadIdentity{}
	fill(listState)
	fill(listIdentity)
	for _, entry := range processBoundState {
		if entry.clear != nil {
			entry.clear(listState, listIdentity)
		}
	}

	r := NewRouter(nil, func(eventchan.Channel, any) {})
	commitState, commitIdentity := &threadState{}, &threadIdentity{}
	fill(commitState)
	fill(commitIdentity)
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
			if !sameFill(pair[0].Field(i), pair[1].Field(i)) {
				t.Errorf("%s.%s: MarkThreadActive and processBoundState's clears leave it different; clear it through the list", pair[0].Type().Name(), pair[0].Type().Field(i).Name)
			}
		}
	}
}

// interfaceFillers holds a concrete value for each interface type reachable
// from threadState and threadIdentity. fillNonZero reports an interface
// type missing here, so a new interface field cannot go unseen.
var interfaceFillers = map[reflect.Type]reflect.Value{
	reflect.TypeFor[trace.Span]():                reflect.ValueOf(trace.SpanFromContext(context.Background())),
	reflect.TypeFor[error]():                     reflect.ValueOf(errors.New("filled")),
	reflect.TypeFor[provider.TurnCompleteMeta](): reflect.ValueOf(&provider.WireTurnCompleteMeta{}),
}

// fillNonZero sets every field reachable from v to a non-zero value, so a
// clear of any of them shows, and returns the interface types it had no
// filler for. A skippedType is left zero and a pointer to one is set to a
// fresh zero value, since resetting a lock or atomic is not a clear. A
// struct whose type is already on the path (a back-reference such as a
// stream block's owner) is also left zero, which bounds the fill by the
// type graph; a pointer to one is still set, so its clear shows. The result
// is a tree, so sameFill needs no bound.
func fillNonZero(v reflect.Value, path map[reflect.Type]bool) (missing []string) {
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
	case reflect.Func:
		v.Set(reflect.MakeFunc(v.Type(), func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, v.Type().NumOut())
			for i := range out {
				out[i] = reflect.Zero(v.Type().Out(i))
			}
			return out
		}))
	case reflect.Chan:
		v.Set(reflect.MakeChan(v.Type(), 0))
	case reflect.Interface:
		filler, ok := interfaceFillers[v.Type()]
		if !ok {
			return []string{v.Type().String()}
		}
		v.Set(filler)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		if !skippedType(v.Type().Elem()) {
			missing = fillNonZero(p.Elem(), path)
		}
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		missing = fillNonZero(s.Index(0), path)
		v.Set(s)
	case reflect.Map:
		key, elem := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		missing = append(fillNonZero(key, path), fillNonZero(elem, path)...)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, elem)
		v.Set(m)
	case reflect.Array:
		for i := range v.Len() {
			missing = append(missing, fillNonZero(v.Index(i), path)...)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Unix(7, 0)))
			return nil
		}
		if skippedType(v.Type()) || path[v.Type()] {
			return nil
		}
		path[v.Type()] = true
		for i := range v.NumField() {
			missing = append(missing, fillNonZero(v.Field(i), path)...)
		}
		delete(path, v.Type())
	}
	return missing
}

// sameFill compares two values filled by fillNonZero. Funcs and channels
// compare by whether they are set, since two fills never share one.
func sameFill(a, b reflect.Value) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case reflect.Bool:
		return a.Bool() == b.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() == b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return a.Uint() == b.Uint()
	case reflect.Float32, reflect.Float64:
		return a.Float() == b.Float()
	case reflect.String:
		return a.String() == b.String()
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return a.IsNil() == b.IsNil()
	case reflect.Interface, reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return sameFill(a.Elem(), b.Elem())
	case reflect.Slice, reflect.Array:
		if a.Kind() == reflect.Slice && a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !sameFill(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		// fillNonZero writes one entry with equal keys in both copies.
		ak, bk := a.MapKeys(), b.MapKeys()
		for i := range ak {
			if !sameFill(ak[i], bk[i]) || !sameFill(a.MapIndex(ak[i]), b.MapIndex(bk[i])) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := range a.NumField() {
			if !sameFill(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	}
	return true
}

// skippedType reports a type fillNonZero leaves zero: locks, atomics and
// timers, and the store, a shared handle whose internals are not thread
// state.
func skippedType(t reflect.Type) bool {
	switch t.PkgPath() {
	case "sync", "sync/atomic", "time":
		return true
	}
	return t == reflect.TypeFor[store.Store]()
}

// exported makes an unexported field settable.
func exported(v reflect.Value) reflect.Value {
	if v.CanSet() || !v.CanAddr() {
		return v
	}
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}
