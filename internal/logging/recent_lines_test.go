package logging

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestRecentLinesReturnsTheLinesBeforeAMarker(t *testing.T) {
	var ring recentLines
	ring.add([]byte("a\n"))
	ring.add([]byte("b\nc (id: ref1)\n"))
	ring.add([]byte("d\n"))
	got, found := ring.before("(id: ref1)", 2)
	if !found || !slices.Equal(got, []string{"b", "c (id: ref1)"}) {
		t.Fatalf("before = %q found=%v, want [b, c (id: ref1)]", got, found)
	}
	if _, found := ring.before("(id: missing)", 5); found {
		t.Fatal("found a marker that was never logged")
	}
}

func TestRecentLinesEvictsPastTheBudget(t *testing.T) {
	var ring recentLines
	line := strings.Repeat("x", 1000)
	ring.add([]byte("first (id: old)\n"))
	for i := range recentLogBudget/1000 + 10 {
		ring.add(fmt.Appendf(nil, "%s %d\n", line, i))
	}
	if _, found := ring.before("(id: old)", 5); found {
		t.Fatal("a line past the byte budget is still retained")
	}
	if ring.bytes > recentLogBudget {
		t.Fatalf("retained %d bytes, budget %d", ring.bytes, recentLogBudget)
	}
	if live := len(ring.lines) - ring.head; cap(ring.lines) > 4*live+64 {
		t.Fatalf("backing array %d for %d live lines", cap(ring.lines), live)
	}
}

func TestRecentLinesCutsAnOversizedLine(t *testing.T) {
	var ring recentLines
	ring.add([]byte(strings.Repeat("y", recentLogLineMax*2) + " (id: big)\n"))
	if _, found := ring.before("(id: big)", 1); found {
		t.Fatal("the cut tail of an oversized line is searchable")
	}
	got, found := ring.before("yyy", 1)
	if !found || len(got[0]) > recentLogLineMax+len("…") {
		t.Fatalf("oversized line kept %d bytes", len(got[0]))
	}
}

func TestOutputRetainsAndForwardsLines(t *testing.T) {
	var out bytes.Buffer
	w := Output(&out)
	if Output(w) != w {
		t.Fatal("Output wrapped its own writer twice")
	}
	if _, err := w.Write([]byte("hello (id: fwd)\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if out.String() != "hello (id: fwd)\n" {
		t.Fatalf("forwarded %q", out.String())
	}
	if got, found := RecentLogLinesBefore("(id: fwd)", 1); !found || got[0] != "hello (id: fwd)" {
		t.Fatalf("retained %q found=%v", got, found)
	}
}
