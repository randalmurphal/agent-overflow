package accountswitch

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReportJSON(t *testing.T) {
	b, err := json.Marshal(Report{Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"provider":"claude"}` {
		t.Fatalf("json = %s", b)
	}
	b, _ = json.Marshal(Report{Provider: "codex", FromAccountID: "a", ToAccountID: "b", FromLabel: "x", ToLabel: "y", Reason: ReasonExhausted, LimitName: "n", UsedPercent: 100, ResetsAt: 5, Model: "m", Error: "e"})
	want := `{"provider":"codex","fromAccountId":"a","toAccountId":"b","fromLabel":"x","toLabel":"y","reason":"exhausted","limitName":"n","usedPercent":100,"resetsAt":5,"model":"m","error":"e"}`
	if string(b) != want {
		t.Fatalf("json = %s", b)
	}
}

func TestUnservableNotice(t *testing.T) {
	cases := []struct {
		name         string
		accounts     []string
		autoContinue bool
		want         string
	}{
		{name: "single account not waiting is silent", accounts: []string{"a"}},
		{name: "single account waiting", accounts: []string{"a"}, autoContinue: true,
			want: "This account is out of usage for this model until Fri 1:00 PM. This thread continues then."},
		{name: "pool not waiting", accounts: []string{"a", "b"},
			want: "No account in the auto-switch pool can run this model right now. The earliest reset is a@x.com at Fri 1:00 PM."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeApp("claude", "a", tc.accounts...)
			f.cfg.AutoContinue = tc.autoContinue
			for _, id := range tc.accounts {
				f.setReading("claude", id, testNow, session(100, at(time.Hour)))
			}
			c := New(f.deps())
			c.TurnRefused("claude", "t1", opusModel, "a")
			settle(c)
			_, _, notices, _ := f.snapshot()
			if tc.want == "" {
				if len(notices) != 0 {
					t.Fatalf("notices = %+v", notices)
				}
				return
			}
			if len(notices) != 1 || notices[0].text != tc.want {
				t.Fatalf("notices = %+v", notices)
			}
		})
	}
}

// One turn can be refused twice (a subagent, then its parent); the thread is
// told once, and again after its next send.
func TestUnservableNoticeIsNotRepeatedWithinATurn(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	for _, id := range []string{"a", "b"} {
		f.setReading("claude", id, testNow, session(100, at(time.Hour)))
	}
	c := New(f.deps())
	refuse := func() int {
		c.TurnRefused("claude", "t1", opusModel, "a")
		settle(c)
		_, _, notices, _ := f.snapshot()
		return len(notices)
	}
	if got := refuse(); got != 1 {
		t.Fatalf("notices after the first refusal = %d, want 1", got)
	}
	if got := refuse(); got != 1 {
		t.Fatalf("notices after a second refusal of the same turn = %d, want 1", got)
	}
	c.BeforeSend("claude", "t1", opusModel, false)
	if got := refuse(); got != 2 {
		t.Fatalf("notices after a refusal of the next turn = %d, want 2", got)
	}
}

// A thread that never sends again does not keep its notice memory forever.
func TestUnservableNoticeMemoryPruned(t *testing.T) {
	f := newFakeApp("claude", "a", "a", "b")
	f.setReading("claude", "a", testNow, session(100, at(48*time.Hour)))
	f.setReading("claude", "b", testNow, session(100, at(48*time.Hour)))
	c := New(f.deps())
	c.TurnRefused("claude", "t1", opusModel, "a")
	settle(c)
	f.advance(noticeMemoryAge + time.Minute)
	c.TurnRefused("claude", "t2", opusModel, "a")
	settle(c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.unservableNoticed["t1"]; ok || len(c.unservableNoticed) != 1 {
		t.Fatalf("notice memory = %v", c.unservableNoticed)
	}
}
