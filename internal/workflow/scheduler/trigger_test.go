package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseTriggerAccepts(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Trigger
		summary string
	}{
		{
			name:    "cron",
			raw:     `{"kind":"cron","expr":"0 3 * * *"}`,
			want:    Trigger{Kind: KindCron, Expr: "0 3 * * *"},
			summary: "cron 0 3 * * *",
		},
		{
			name:    "event without a workflow filter",
			raw:     `{"kind":"event","on":"item-done"}`,
			want:    Trigger{Kind: KindEvent, On: EventItemDone},
			summary: "event item-done",
		},
		{
			name:    "event narrowed to one workflow",
			raw:     `{"kind":"event","on":"item-failed","workflowId":"nightly-audit"}`,
			want:    Trigger{Kind: KindEvent, On: EventItemFailed, WorkflowID: "nightly-audit"},
			summary: "event item-failed on nightly-audit",
		},
		{
			name:    "needs-human is in the closed set",
			raw:     `{"kind":"event","on":"item-needs-human"}`,
			want:    Trigger{Kind: KindEvent, On: EventItemNeedsHuman},
			summary: "event item-needs-human",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			trigger, err := ParseTrigger(json.RawMessage(testCase.raw))
			if err != nil {
				t.Fatalf("ParseTrigger() error = %v", err)
			}
			if trigger.Kind != testCase.want.Kind || trigger.Expr != testCase.want.Expr ||
				trigger.On != testCase.want.On || trigger.WorkflowID != testCase.want.WorkflowID {
				t.Fatalf("trigger = %#v, want %#v", trigger, testCase.want)
			}
			if got := trigger.Summary(); got != testCase.summary {
				t.Fatalf("Summary() = %q, want %q", got, testCase.summary)
			}
		})
	}
}

func TestParseTriggerRefuses(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: ``, want: "trigger is required"},
		{name: "no kind", raw: `{"expr":"0 3 * * *"}`, want: "trigger kind is required"},
		{name: "unknown kind", raw: `{"kind":"webhook"}`, want: "must be cron or event"},
		{
			// A typo must be visible when it is written, not silently disable a
			// schedule at the next boot.
			name: "unknown field",
			raw:  `{"kind":"cron","expression":"0 3 * * *"}`,
			want: "unknown field",
		},
		{name: "cron without expr", raw: `{"kind":"cron"}`, want: "requires expr"},
		{name: "cron with an event field", raw: `{"kind":"cron","expr":"0 3 * * *","on":"item-done"}`, want: "declares expr only"},
		{name: "six-field cron", raw: `{"kind":"cron","expr":"0 0 3 * * *"}`, want: "has 6 fields"},
		{name: "descriptor cron", raw: `{"kind":"cron","expr":"@daily"}`, want: "has 1 fields"},
		{name: "every-descriptor cron", raw: `{"kind":"cron","expr":"@every 5s"}`, want: "has 2 fields"},
		{name: "invalid cron field", raw: `{"kind":"cron","expr":"99 * * * *"}`, want: "is invalid"},
		{name: "event without on", raw: `{"kind":"event"}`, want: "event trigger on must be one of"},
		{name: "event outside the closed set", raw: `{"kind":"event","on":"phase-done"}`, want: "event trigger on must be one of"},
		{name: "event with a cron expr", raw: `{"kind":"event","on":"item-done","expr":"0 3 * * *"}`, want: "declares on"},
		{name: "trailing content", raw: `{"kind":"event","on":"item-done"}{}`, want: "one JSON object"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseTrigger(json.RawMessage(testCase.raw))
			if err == nil {
				t.Fatal("ParseTrigger() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("ParseTrigger() error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

func TestTriggerNext(t *testing.T) {
	now := time.Date(2026, 7, 24, 2, 30, 0, 0, time.UTC)
	cron, err := ParseTrigger(json.RawMessage(`{"kind":"cron","expr":"0 3 * * *"}`))
	if err != nil {
		t.Fatalf("ParseTrigger() error = %v", err)
	}
	next, ok := cron.Next(now)
	if !ok || !next.Equal(time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("Next() = (%v, %v)", next, ok)
	}
	// Strictly after: standing on an occurrence answers the following one, which
	// is what stops a fire from repeating within its own minute.
	following, ok := cron.Next(next)
	if !ok || !following.Equal(time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("Next(next) = (%v, %v)", following, ok)
	}

	event, err := ParseTrigger(json.RawMessage(`{"kind":"event","on":"item-done"}`))
	if err != nil {
		t.Fatalf("ParseTrigger() error = %v", err)
	}
	if _, ok := event.Next(now); ok {
		t.Fatal("an event trigger answered a next fire time")
	}
	// A Trigger that never went through ParseTrigger carries no schedule and
	// must not pretend to: the zero value cannot silently mean "every minute".
	if _, ok := (Trigger{Kind: KindCron, Expr: "* * * * *"}).Next(now); ok {
		t.Fatal("an uncompiled trigger answered a next fire time")
	}
}

func TestEventKindForState(t *testing.T) {
	for state, want := range map[string]ItemEventKind{
		"done":        EventItemDone,
		"failed":      EventItemFailed,
		"needs-human": EventItemNeedsHuman,
	} {
		got, ok := EventKindForState(state)
		if !ok || got != want {
			t.Fatalf("EventKindForState(%q) = (%q, %v), want %q", state, got, ok, want)
		}
	}
	for _, state := range []string{"cancelled", "running", ""} {
		if _, ok := EventKindForState(state); ok {
			t.Fatalf("EventKindForState(%q) is inside the closed set", state)
		}
	}
}

func TestReservedSeedNames(t *testing.T) {
	// Both names must be legal `def` identifiers, or a workflow could neither
	// declare them as inputs nor reference them from a prompt.
	for _, name := range []string{TriggerVariable, JobNotesVariable} {
		if !ReservedSeed(name) {
			t.Fatalf("ReservedSeed(%q) = false", name)
		}
		for _, char := range name {
			if (char < 'a' || char > 'z') && char != '-' {
				t.Fatalf("reserved seed %q is not a valid def identifier", name)
			}
		}
	}
	if ReservedSeed("goal") {
		t.Fatal("ReservedSeed(\"goal\") = true")
	}
}

func TestParseCondition(t *testing.T) {
	predicate, present, err := ParseCondition(json.RawMessage(`{"eq":{"ref":"trigger.kind","value":"cron"}}`))
	if err != nil || !present || predicate.Eq == nil || predicate.Eq.Ref != "trigger.kind" {
		t.Fatalf("ParseCondition() = (%#v, %v, %v)", predicate, present, err)
	}
	for _, raw := range []string{``, `null`, `  `} {
		if _, present, err := ParseCondition(json.RawMessage(raw)); err != nil || present {
			t.Fatalf("ParseCondition(%q) = (%v, %v), want absent", raw, present, err)
		}
	}
	for name, raw := range map[string]string{
		"unknown operator":  `{"matches":{"ref":"trigger.kind"}}`,
		"two operators":     `{"exists":"trigger","eq":{"ref":"trigger.kind","value":"cron"}}`,
		"empty all":         `{"all":[]}`,
		"not an object":     `"trigger"`,
		"trailing content":  `{"exists":"trigger"}{}`,
		"malformed nesting": `{"any":[{"exists":"trigger"},{}]}`,
	} {
		if _, _, err := ParseCondition(json.RawMessage(raw)); err == nil {
			t.Fatalf("ParseCondition(%s) error = nil, want an error", name)
		}
	}
}
