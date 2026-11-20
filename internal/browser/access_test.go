package browser

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Every Manager method that takes an Access refuses the zero Access, which
// no bind made, with errUnbound, whatever else it is passed, and none
// panics. The thread has a live tab, so a method that checked only the page
// would find one. A new entry point is in this test the day it is written.
func TestEveryAccessEntryRefusesTheZeroAccess(t *testing.T) {
	m, _, ids := newTabsManager(t, addr("a"))
	page := reflect.ValueOf(PageID(ids[0]))
	managerType := reflect.TypeOf(m)
	accessType := reflect.TypeFor[Access]()
	contextType := reflect.TypeFor[context.Context]()
	errorType := reflect.TypeFor[error]()
	arguments := map[reflect.Type]reflect.Value{
		contextType:                reflect.ValueOf(t.Context()),
		reflect.TypeFor[PageID]():  page,
		reflect.TypeFor[MountID](): reflect.ValueOf(MountID("mount")),
		reflect.TypeFor[string]():  reflect.ValueOf("x"),
		reflect.TypeFor[uint64]():  reflect.ValueOf(uint64(1)),
		reflect.TypeFor[int]():     reflect.ValueOf(1),
		// A switch that needs no consent, so the refusal is the Access's.
		reflect.TypeFor[JarConsent](): reflect.ValueOf(JarSwitchConfirmed),
	}
	checked := 0
	for i := range managerType.NumMethod() {
		method := managerType.Method(i)
		takes := false
		args := []reflect.Value{reflect.ValueOf(m)}
		for j := 1; j < method.Type.NumIn(); j++ {
			in := method.Type.In(j)
			switch value, ok := arguments[in]; {
			case in == accessType:
				takes = true
				args = append(args, reflect.Zero(in))
			case ok:
				args = append(args, value)
			case in.Kind() == reflect.Pointer:
				args = append(args, reflect.New(in.Elem()))
			default:
				args = append(args, reflect.Zero(in))
			}
		}
		if !takes {
			continue
		}
		checked++
		t.Run(method.Name, func(t *testing.T) {
			out := method.Func.Call(args)
			if len(out) == 0 || out[len(out)-1].Type() != errorType {
				t.Fatalf("%s takes an Access and answers no error", method.Name)
			}
			err, _ := out[len(out)-1].Interface().(error)
			if !errors.Is(err, errUnbound) {
				t.Fatalf("the zero Access answered %v, want errUnbound", err)
			}
		})
	}
	if checked == 0 {
		t.Fatal("no method takes an Access")
	}
	if pages := m.CompanionState(Caller{ThreadID: "thread"}).Pages; len(pages) != 1 || pages[0].ID != ids[0] {
		t.Fatalf("the refused calls changed the thread's tabs: %+v", pages)
	}
}
