package transport

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/computerroute"
)

func TestBootstrapAdvertisesCurrentComputerRoutesOnlyAfterAdmission(t *testing.T) {
	var calls atomic.Int32
	routes := []computerroute.Route{{Endpoint: "https://gpu.test.ts.net"}, {Endpoint: "https://bad/?credential=secret"}}
	var current atomic.Value
	current.Store(routes)
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.BackendIdentity = func() (string, string) { return "backend-1", "gen-1" }
		cfg.ComputerRoutes = func() []computerroute.Route { calls.Add(1); return current.Load().([]computerroute.Route) }
	})
	response, err := http.Get("http://" + f.srv.Addr() + "/bootstrap.json")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound || calls.Load() != 0 {
		t.Fatal("an unadmitted page caused route discovery")
	}
	for _, want := range [][]computerroute.Route{{routes[0]}, nil} {
		response := getBootstrap(t, f.srv.Addr())
		var manifest Bootstrap
		err := json.NewDecoder(response.Body).Decode(&manifest)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(manifest.Routes, want) {
			t.Fatalf("routes = %v, want %v", manifest.Routes, want)
		}
		current.Store([]computerroute.Route(nil))
	}
}

func TestHelloResnapshotsComputerRoutesForEveryAuthenticatedConnection(t *testing.T) {
	var current atomic.Value
	current.Store([]computerroute.Route{{Endpoint: "https://initial.test.ts.net"}})
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.BackendIdentity = func() (string, string) { return "backend-1", "gen-1" }
		cfg.ComputerRoutes = func() []computerroute.Route { return current.Load().([]computerroute.Route) }
	})
	for _, routes := range [][]computerroute.Route{{{Endpoint: "https://later.test.ts.net"}}, nil} {
		current.Store(routes)
		conn := f.dial(t)
		var hello helloFrame
		if err := json.Unmarshal(readFirstFrame(t, conn), &hello); err != nil {
			t.Fatal(err)
		}
		conn.CloseNow()
		if !reflect.DeepEqual(hello.Routes, routes) {
			t.Fatalf("hello reused stale route snapshot: %v want %v", hello.Routes, routes)
		}
	}
}
