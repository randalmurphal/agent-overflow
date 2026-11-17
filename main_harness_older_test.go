package main

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// TestOlderComputerSurfaceIsV0019s: the harness's older computer leaves
// out exactly the methods the frontend's tests answer as v0.0.19 does
// (frontend/src/test/helpers/olderComputer.ts), each a method this build
// binds, and answers null for exactly the ones the frontend's tests do,
// each served and answering a value in this build.
func TestOlderComputerSurfaceIsV0019s(t *testing.T) {
	absent, resultless := olderComputerSurface()
	source, err := os.ReadFile("frontend/src/test/helpers/olderComputer.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []struct {
		name    string
		harness map[string]bool
	}{{"METHODS_AFTER_V0_0_19", absent}, {"RESULTLESS_AT_V0_0_19", resultless}} {
		frontend := frontendNames(t, source, list.name)
		harness := make([]string, 0, len(list.harness))
		for name := range list.harness {
			harness = append(harness, name)
		}
		sort.Strings(harness)
		if strings.Join(frontend, ",") != strings.Join(harness, ",") {
			t.Errorf("older computer's %s is %v, the frontend's is %v", list.name, harness, frontend)
		}
	}

	bound := transport.NewMethodAllowList()
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(&App{}, transport.RegisterOptions{
		Package: "main", TypeName: "App", AllowList: bound, Skip: absent, Resultless: resultless,
	}); err != nil {
		t.Fatalf("register App as the older computer: %v", err)
	}
	for name := range absent {
		if !bound[name] {
			t.Errorf("%s is no method this build binds", name)
		}
		if _, ok := dispatcher.LookupName(name); ok {
			t.Errorf("the older computer serves %s", name)
		}
	}
	app := reflect.TypeOf(&App{})
	for name := range resultless {
		if _, ok := dispatcher.LookupName(name); !ok || absent[name] {
			t.Errorf("the older computer does not serve %s", name)
		}
		if method, ok := app.MethodByName(name); !ok || method.Type.NumOut() < 2 {
			t.Errorf("%s answers no value in this build", name)
		}
	}
}

// frontendNames reads the sorted quoted names of the array const `name`.
func frontendNames(t *testing.T, source []byte, name string) []string {
	t.Helper()
	list := regexp.MustCompile(`(?s)` + name + ` = \[(.*?)\]`).FindSubmatch(source)
	if list == nil {
		t.Fatalf("frontend/src/test/helpers/olderComputer.ts names no %s", name)
	}
	var names []string
	for _, match := range regexp.MustCompile(`'(\w+)'`).FindAllSubmatch(list[1], -1) {
		names = append(names, string(match[1]))
	}
	sort.Strings(names)
	return names
}
