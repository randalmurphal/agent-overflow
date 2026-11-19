package main

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// TestOlderComputerSurfaceIsV0019s: the harness's older computer leaves
// out exactly the methods the frontend's tests answer as v0.0.19 does
// (frontend/src/test/helpers/olderComputer.ts), each a method this build
// binds; answers null for exactly the ones the frontend's tests do, each
// served and answering a value in this build; and answers every settings
// snapshot without exactly the keys the frontend's tests leave out, each a
// key this build always sends.
func TestOlderComputerSurfaceIsV0019s(t *testing.T) {
	absent, reshape := olderComputerSurface()
	source, err := os.ReadFile("frontend/src/test/helpers/olderComputer.ts")
	if err != nil {
		t.Fatal(err)
	}
	var resultless []string
	for name, rewrite := range reshape {
		if answer, err := rewrite(json.RawMessage(`{"value":1}`)); err == nil && string(answer) == `null` {
			resultless = append(resultless, name)
		}
	}
	for _, list := range []struct {
		name    string
		harness []string
	}{
		{"METHODS_AFTER_V0_0_19", keysOf(absent)},
		{"RESULTLESS_AT_V0_0_19", resultless},
		{"SETTINGS_AFTER_V0_0_19", slices.Clone(settingsAfterV0019)},
	} {
		frontend := frontendNames(t, source, list.name)
		sort.Strings(list.harness)
		if strings.Join(frontend, ",") != strings.Join(list.harness, ",") {
			t.Errorf("older computer's %s is %v, the frontend's is %v", list.name, list.harness, frontend)
		}
	}

	bound := transport.NewMethodAllowList()
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(&App{}, transport.RegisterOptions{
		Package: "main", TypeName: "App", AllowList: bound, Skip: absent, Reshape: reshape,
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
	for name := range reshape {
		if _, ok := dispatcher.LookupName(name); !ok || absent[name] {
			t.Errorf("the older computer does not serve %s", name)
		}
		if method, ok := app.MethodByName(name); !ok || method.Type.NumOut() < 2 {
			t.Errorf("%s answers no value in this build", name)
		}
	}

	// Every bound method answering a settings snapshot answers it as v0.0.19.
	snapshot := reflect.TypeFor[settings.Settings]()
	for i := 0; i < app.NumMethod(); i++ {
		method := app.Method(i)
		if bound[method.Name] && method.Type.NumOut() > 0 && method.Type.Out(0) == snapshot && reshape[method.Name] == nil {
			t.Errorf("%s answers a settings snapshot the older computer does not reshape", method.Name)
		}
	}
	encoded, err := json.Marshal(settings.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	var full, older map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &full); err != nil {
		t.Fatal(err)
	}
	reshaped, err := settingsWithoutNewerKeys(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reshaped, &older); err != nil {
		t.Fatal(err)
	}
	for _, key := range settingsAfterV0019 {
		if _, sent := full[key]; !sent {
			t.Errorf("%s is no key this build always sends", key)
		}
		if _, kept := older[key]; kept {
			t.Errorf("the older computer's settings name %s", key)
		}
	}
	if len(full)-len(older) != len(settingsAfterV0019) {
		t.Errorf("the older computer's settings drop %d keys, want %d", len(full)-len(older), len(settingsAfterV0019))
	}
}

func keysOf(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
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
