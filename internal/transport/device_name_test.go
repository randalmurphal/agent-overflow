package transport

import "testing"

func TestBackendNameGetterRemainsLive(t *testing.T) {
	name := "Studio"
	s := &Server{cfg: Config{BackendName: "fallback", BackendNameGetter: func() string { return name }}}
	if s.backendName() != "Studio" {
		t.Fatal("getter ignored")
	}
	name = "Workhorse"
	if s.backendName() != "Workhorse" {
		t.Fatal("name cached")
	}
	s.cfg.BackendNameGetter = nil
	if s.backendName() != "fallback" {
		t.Fatal("static compatibility fallback lost")
	}
}
