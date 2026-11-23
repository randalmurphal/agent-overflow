package app

import (
	"errors"
	"testing"
)

func TestNewerPeerReadWinsAndChangeDuringReadStaysOwed(t *testing.T) {
	var reads ownerReads
	var result string
	first := reads.start(t.Context(), "desk", []string{"token"})
	defer first.close()
	later := reads.start(t.Context(), "desk", []string{"token"})
	defer later.close()
	if err := later.apply(func() error { result = "new title"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := first.apply(func() error { result = "old title"; return nil }); !errors.Is(err, errReadSuperseded) || result != "new title" {
		t.Fatalf("older completion applied %q, %v", result, err)
	}
	current := reads.start(t.Context(), "desk", []string{"token"})
	defer current.close()
	reads.changed([]string{"token"})
	if err := current.apply(func() error { result = "previous state"; return nil }); !errors.Is(err, errReadSuperseded) {
		t.Fatalf("pre-change read returned %v", err)
	}
}

func (o *ownerReads) changed(keys []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.changedLocked(keys)
}
