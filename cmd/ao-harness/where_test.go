package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseWhereRejectsMalformedClauses(t *testing.T) {
	for _, clause := range []string{"", "nofield", "=value", " =value", "a..b=1", ".a=1", "a.=1"} {
		if _, err := parseWhere(clause); err == nil {
			t.Errorf("parseWhere(%q) was accepted", clause)
		}
	}
}

// An empty value is a legitimate clause (`--where error=` matches an
// empty string), and a value may itself contain '='.
func TestParseWhereSplitsOnTheFirstEqualsOnly(t *testing.T) {
	m, err := parseWhere("a.b=x=y")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.path, ".") != "a.b" || m.value != "x=y" {
		t.Fatalf("matcher = %v / %q, want [a b] / \"x=y\"", m.path, m.value)
	}
	empty, err := parseWhere("error=")
	if err != nil {
		t.Fatal(err)
	}
	if empty.value != "" {
		t.Fatalf("value = %q, want empty", empty.value)
	}
}

func TestWhereMatchesScalarsByTheirText(t *testing.T) {
	payload := json.RawMessage(`{
		"threadId": "t1",
		"turn": 2,
		"done": true,
		"error": "",
		"nested": {"state": "ok"},
		"units": [{"state": "done"}, {"state": "failed"}],
		"cost": 1.5,
		"missing": null
	}`)
	cases := []struct {
		clause string
		want   bool
	}{
		{"threadId=t1", true},
		{"threadId=t2", false},
		// A shell has no types: the number 2 and the text "2" are the
		// same clause on purpose.
		{"turn=2", true},
		{"turn=3", false},
		{"done=true", true},
		{"done=false", false},
		{"error=", true},
		{"nested.state=ok", true},
		{"nested.state=bad", false},
		{"units.0.state=done", true},
		{"units.1.state=failed", true},
		{"units.2.state=done", false},
		{"cost=1.5", true},
		{"missing=null", true},
		{"absent=anything", false},
		// An object or array has no text a '=' comparison could mean.
		{"nested=whatever", false},
		{"units=whatever", false},
	}
	for _, c := range cases {
		m, err := parseWhere(c.clause)
		if err != nil {
			t.Fatalf("parseWhere(%q): %v", c.clause, err)
		}
		if got := m.match(payload); got != c.want {
			t.Errorf("%q matched = %v, want %v", c.clause, got, c.want)
		}
	}
}

// A number keeps the wire's own spelling rather than a float round-trip,
// so 2 stays "2" and never becomes "2e+00".
func TestWhereKeepsTheWiresNumberSpelling(t *testing.T) {
	m, err := parseWhere("n=1000000")
	if err != nil {
		t.Fatal(err)
	}
	if !m.match(json.RawMessage(`{"n":1000000}`)) {
		t.Fatal("a large integer did not match its own spelling")
	}
}

func TestWhereNeverMatchesANonObjectPayload(t *testing.T) {
	m, err := parseWhere("a=1")
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`"a string"`, `[1,2]`, `null`, ``} {
		if m.match(json.RawMessage(payload)) {
			t.Errorf("payload %q matched", payload)
		}
	}
}
