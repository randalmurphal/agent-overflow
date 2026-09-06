package frontendclient

import (
	"context"
	"reflect"
	"testing"

	"agent-overflow/internal/app"
)

// The local controller reuses App wire IDs. Its parameter and result shapes
// must remain identical when a future edit changes the primary bound method.
// Reflection sees types only: this test never constructs an App or a provider.
func TestControllerMethodsMatchTheAppWire(t *testing.T) {
	controller, host := reflect.TypeFor[*service](), reflect.TypeFor[*app.App]()
	ctxType, errType := reflect.TypeFor[context.Context](), reflect.TypeFor[error]()
	inputs := func(method reflect.Method) []reflect.Type {
		var out []reflect.Type
		for i := 1; i < method.Type.NumIn(); i++ {
			typ := method.Type.In(i)
			if typ != ctxType {
				out = append(out, typ)
			}
		}
		return out
	}
	outputs := func(method reflect.Method) []reflect.Type {
		var out []reflect.Type
		for i := range method.Type.NumOut() {
			typ := method.Type.Out(i)
			if typ != errType {
				out = append(out, typ)
			}
		}
		return out
	}
	for i := range controller.NumMethod() {
		method := controller.Method(i)
		primary, found := host.MethodByName(method.Name)
		if !found {
			t.Errorf("controller method %s has no App wire ID", method.Name)
			continue
		}
		if !reflect.DeepEqual(inputs(method), inputs(primary)) || !reflect.DeepEqual(outputs(method), outputs(primary)) {
			t.Errorf("controller %s drifted from the App wire: %s / %s", method.Name, method.Type, primary.Type)
		}
	}
}
