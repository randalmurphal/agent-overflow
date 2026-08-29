package main

import (
	"encoding/json"
	"slices"
	"testing"

	"agent-overflow/internal/transport"
)

// TestHarnessListMethodsReportsTheWholeWireSurface drives the real
// dispatcher registration bootTransport performs and hands the result to
// the receiver through the same anonymous-interface sink, so a rename on
// either side fails here rather than at runtime in a harness nobody is
// watching.
func TestHarnessListMethodsReportsTheWholeWireSurface(t *testing.T) {
	h, app := newHarnessTestApp(t)

	dispatcher := transport.NewDispatcher()
	appMethods, err := dispatcher.Register(app, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "App",
		AllowList: transport.NewMethodAllowList(),
	})
	if err != nil {
		t.Fatalf("register App: %v", err)
	}
	harnessMethods, err := dispatcher.Register(h, transport.RegisterOptions{
		Package:   "main",
		TypeName:  "Harness",
		LocalOnly: true,
	})
	if err != nil {
		t.Fatalf("register Harness: %v", err)
	}

	// The exact assertion bootTransport makes. A signature change on
	// setWireMethods would make that assertion silently fail and leave
	// HarnessListMethods answering [] forever.
	sink, ok := any(h).(interface{ setWireMethods([]string) })
	if !ok {
		t.Fatal("*Harness no longer satisfies bootTransport's setWireMethods sink")
	}
	var names []string
	for _, m := range append(append([]*transport.Method{}, appMethods...), harnessMethods...) {
		names = append(names, m.Name)
	}
	sink.setWireMethods(names)

	got := h.HarnessListMethods()
	if !slices.IsSorted(got) {
		t.Fatalf("HarnessListMethods is not sorted: %v", got)
	}
	if len(got) != len(names) {
		t.Fatalf("HarnessListMethods returned %d names, dispatcher registered %d", len(got), len(names))
	}
	// Both halves of the surface, and the method describing itself.
	for _, want := range []string{"HarnessListMethods", "HarnessCapabilities", "HarnessInfo", "HarnessReset", "ListThreads"} {
		if !slices.Contains(got, want) {
			t.Errorf("HarnessListMethods omits %q", want)
		}
	}
	// Bare wire names, not FQNs: that is what a caller puts on the frame.
	for _, name := range got {
		if name == "" || name[0] == 'm' && len(name) > 5 && name[:5] == "main." {
			t.Fatalf("method name %q is not a bare wire name", name)
		}
	}

	// A mutating caller must not be able to edit the receiver's record.
	got[0] = "clobbered"
	if again := h.HarnessListMethods(); slices.Contains(again, "clobbered") {
		t.Fatal("HarnessListMethods handed out its backing array")
	}
}

// TestHarnessListMethodsIsAJSONArray: the result shape is the contract —
// ao-harness decodes it as a plain array of strings, and a nil slice
// would encode as null.
func TestHarnessListMethodsIsAJSONArray(t *testing.T) {
	h, _ := newHarnessTestApp(t)

	raw, err := json.Marshal(h.HarnessListMethods())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("an unpopulated list encodes as %s, want []", raw)
	}

	h.setWireMethods([]string{"Zeta", "Alpha", "Alpha"})
	raw, err = json.Marshal(h.HarnessListMethods())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `["Alpha","Zeta"]` {
		t.Fatalf("encoded %s, want sorted and deduped", raw)
	}
}

func TestHarnessCapabilitiesCatalogIsVersionedAndDefensive(t *testing.T) {
	h, _ := newHarnessTestApp(t)
	h.setWireMethods([]string{"HarnessCapabilities", "SendMessage"})
	caps, err := h.HarnessCapabilities()
	if err != nil {
		t.Fatalf("HarnessCapabilities: %v", err)
	}
	if caps.ProtocolRevision != harnessProtocolRevision {
		t.Fatalf("protocol revision = %d, want %d", caps.ProtocolRevision, harnessProtocolRevision)
	}
	for _, name := range []string{"HarnessCapabilities", "SendMessage"} {
		if !slices.Contains(caps.Methods, name) {
			t.Errorf("methods omit %q: %v", name, caps.Methods)
		}
	}
	for _, name := range []string{"frames", "busy", "dom"} {
		if !slices.Contains(caps.Meters, name) {
			t.Errorf("meters omit %q: %v", name, caps.Meters)
		}
	}
	for _, name := range []string{"open", "reload"} {
		if !slices.Contains(caps.Actions, name) {
			t.Errorf("actions omit %q: %v", name, caps.Actions)
		}
	}
	for _, name := range []string{"viewport", "element", "globals", "monitor", "perf", "open", "reload"} {
		if !slices.Contains(caps.Queries, name) {
			t.Errorf("queries omit %q: %v", name, caps.Queries)
		}
	}
	for _, name := range []string{"burst-stream", "active-multi-pane", "many-threads"} {
		if !slices.Contains(caps.Workloads, name) {
			t.Errorf("workloads omit %q: %v", name, caps.Workloads)
		}
	}
	if caps.Build.Version == "" {
		t.Fatal("build version is empty")
	}
	caps.Methods[0] = "clobbered"
	caps.Meters[0] = "clobbered"
	again, err := h.HarnessCapabilities()
	if err != nil {
		t.Fatalf("HarnessCapabilities second call: %v", err)
	}
	if slices.Contains(again.Methods, "clobbered") || slices.Contains(again.Meters, "clobbered") {
		t.Fatal("capability catalog handed out mutable backing storage")
	}
}
