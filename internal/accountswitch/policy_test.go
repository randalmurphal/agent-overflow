package accountswitch

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.FixedZone("test", -5*3600))

func at(d time.Duration) int64 { return testNow.Add(d).Unix() }

func session(pct float64, reset int64) provider.RateLimitEntry {
	return provider.RateLimitEntry{LimitID: "session", LimitName: "Current session", UsedPercent: pct, WindowMins: 300, ResetsAt: reset}
}

func weekly(pct float64, reset int64) provider.RateLimitEntry {
	return provider.RateLimitEntry{LimitID: "weekly_all", LimitName: "All models", UsedPercent: pct, WindowMins: 10080, ResetsAt: reset}
}

func fable(pct float64, reset int64) provider.RateLimitEntry {
	return provider.RateLimitEntry{LimitID: "weekly_scoped:fable", LimitName: "Fable", UsedPercent: pct, WindowMins: 10080, ResetsAt: reset}
}

const (
	opusModel  = "claude-opus-4-8[1m]"
	fableModel = "claude-fable-5-2"
)

func TestProviderWide(t *testing.T) {
	cases := []struct {
		prov, id string
		want     bool
	}{
		{"claude", "session", true},
		{"claude", "weekly_all", true},
		{"claude", "FIVE_HOUR", true},
		{"claude", "seven_day", true},
		{"claude", "weekly_scoped:fable", false},
		{"claude", "codex", false},
		{"codex", "Codex", true},
		{"codex", "spark", false},
		{"codex", "session", false},
	}
	for _, tc := range cases {
		if got := providerWide(tc.prov, provider.RateLimitEntry{LimitID: tc.id}); got != tc.want {
			t.Errorf("providerWide(%s, %s) = %v, want %v", tc.prov, tc.id, got, tc.want)
		}
	}
}

func TestLimitApplies(t *testing.T) {
	cases := []struct {
		name  string
		prov  string
		entry provider.RateLimitEntry
		model string
		want  bool
	}{
		{"claude scoped names family", "claude", fable(0, 0), fableModel, true},
		{"claude scoped other family", "claude", fable(0, 0), opusModel, false},
		{"claude id form with duplicate suffix and 1m model", "claude", provider.RateLimitEntry{LimitID: "weekly_scoped:claude-opus:2"}, opusModel, true},
		{"claude name only", "claude", provider.RateLimitEntry{LimitID: "weekly_scoped", LimitName: "Fable"}, fableModel, true},
		{"claude unknown bucket", "claude", provider.RateLimitEntry{LimitID: "weekly_scoped:design", LimitName: "Design"}, fableModel, false},
		{"claude fallback name names no family", "claude", provider.RateLimitEntry{LimitID: "weekly_scoped", LimitName: "Scoped weekly"}, fableModel, false},
		{"claude provider-wide applies to every model", "claude", session(0, 0), fableModel, true},
		{"empty model has provider-wide limits", "claude", weekly(0, 0), "", true},
		{"empty model has no scoped limits", "claude", fable(0, 0), "", false},
		{"codex scoped by name", "codex", provider.RateLimitEntry{LimitID: "codex_bengalfox", LimitName: "GPT-5.3-Codex-Spark"}, "gpt-5.3-codex-spark", true},
		{"codex id is not matched", "codex", provider.RateLimitEntry{LimitID: "gpt-5.3-codex-spark", LimitName: "Spark"}, "gpt-5.3-codex-spark", false},
		{"codex scoped other model", "codex", provider.RateLimitEntry{LimitID: "spark", LimitName: "GPT-5.3-Codex-Spark"}, "gpt-5.5", false},
		{"codex provider-wide", "codex", provider.RateLimitEntry{LimitID: "codex"}, "gpt-5.5", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			family := modelFamily(tc.prov, modelKey(tc.prov, tc.model))
			if got := applies(tc.prov, tc.entry, family); got != tc.want {
				t.Fatalf("applies = %v, want %v (family %q)", got, tc.want, family)
			}
		})
	}
}

func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"claude-fable-5-2":    "fable",
		"claude-opus-4-8[1m]": "opus",
		"Claude-Sonnet-4-6":   "sonnet",
		"":                    "",
	}
	for model, want := range cases {
		if got := modelFamily("claude", modelKey("claude", model)); got != want {
			t.Errorf("family(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestAssessExpiry(t *testing.T) {
	cases := []struct {
		name  string
		entry provider.RateLimitEntry
		want  state
	}{
		{"reset passed counts as zero", session(100, at(-time.Second)), stateClear},
		{"reset at now counts as zero", session(100, at(0)), stateClear},
		{"future reset keeps percent", session(100, at(time.Minute)), stateExhausted},
		{"no reset time keeps percent", session(100, 0), stateExhausted},
		{"threshold", session(95, at(time.Hour)), statePastThreshold},
		{"below threshold", session(94.9, at(time.Hour)), stateClear},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assess("claude", "opus", []provider.RateLimitEntry{tc.entry}, nil, 95, testNow, nil).state
			if got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

func cand(id string, limits ...provider.RateLimitEntry) candidate {
	return candidate{Account: Account{ID: id, Label: id, InPool: true, Usable: true}, limits: limits, usable: true}
}

func TestDecide(t *testing.T) {
	week := at(72 * time.Hour)
	type tc struct {
		name       string
		accounts   []candidate
		models     []string
		strategy   string
		allowOrder bool
		pinned     bool
		wantTo     string
		wantReason Reason
		wantModel  string
	}
	marked := cand("a")
	marked.marks = []mark{{family: "fable", until: testNow.Add(time.Hour)}}
	outside := cand("a", session(100, week))
	outside.InPool = false
	unusable := cand("b")
	unusable.usable = false
	cases := []tc{
		{name: "past threshold to past threshold stays",
			accounts: []candidate{cand("a", session(96, week)), cand("b", session(97, week))}, models: []string{opusModel}},
		{name: "past threshold leaves for clear",
			accounts: []candidate{cand("a", session(96, week)), cand("b", session(97, week)), cand("c", session(10, week))}, models: []string{opusModel},
			wantTo: "c", wantReason: ReasonThreshold, wantModel: "claude-opus-4-8"},
		{name: "exhausted leaves for past threshold",
			accounts: []candidate{cand("a", session(100, week)), cand("b", session(97, week))}, models: []string{opusModel},
			wantTo: "b", wantReason: ReasonExhausted},
		{name: "scoped limit ignored when its model is not demanded",
			accounts: []candidate{cand("a", fable(100, week)), cand("b")}, models: []string{opusModel}},
		{name: "scoped limit counts when its model is demanded",
			accounts: []candidate{cand("a", fable(100, week)), cand("b")}, models: []string{opusModel, fableModel},
			wantTo: "b", wantReason: ReasonExhausted, wantModel: "claude-fable-5-2"},
		{name: "account serving most models is chosen",
			accounts: []candidate{cand("a", session(100, week)), cand("b", fable(100, week)), cand("c", fable(100, week))}, models: []string{opusModel, fableModel},
			wantTo: "b", wantReason: ReasonExhausted},
		{name: "no switch between accounts serving equally many",
			accounts: []candidate{cand("a", fable(100, week)), cand("b", fable(100, week)), cand("c", session(100, week))}, models: []string{opusModel, fableModel}},
		{name: "refusal mark forces a switch",
			accounts: []candidate{marked, cand("b")}, models: []string{fableModel},
			wantTo: "b", wantReason: ReasonRefused, wantModel: "claude-fable-5-2"},
		{name: "refusal mark of another family does not",
			accounts: []candidate{marked, cand("b")}, models: []string{opusModel}},
		{name: "selected outside the pool does nothing",
			accounts: []candidate{outside, cand("b")}, models: []string{opusModel}},
		{name: "no other usable account does nothing",
			accounts: []candidate{cand("a", session(100, week)), unusable}, models: []string{opusModel}},
		{name: "order switch to an earlier clear account",
			accounts: []candidate{cand("b"), cand("a")}, models: []string{opusModel}, strategy: StrategyAccountOrder, allowOrder: true,
			wantTo: "b", wantReason: ReasonOrder},
		{name: "order switch not allowed",
			accounts: []candidate{cand("b"), cand("a")}, models: []string{opusModel}, strategy: StrategyAccountOrder},
		{name: "order switch blocked by pin",
			accounts: []candidate{cand("b"), cand("a")}, models: []string{opusModel}, strategy: StrategyAccountOrder, allowOrder: true, pinned: true},
		{name: "forced ignores pin",
			accounts: []candidate{cand("a", session(100, week)), cand("b")}, models: []string{opusModel}, allowOrder: true, pinned: true,
			wantTo: "b", wantReason: ReasonExhausted},
		{name: "order switch needs a clear target",
			accounts: []candidate{cand("b", session(96, week)), cand("a")}, models: []string{opusModel}, strategy: StrategyAccountOrder, allowOrder: true},
		{name: "order switch needs a clear selection",
			accounts: []candidate{cand("b", session(96, week)), cand("a", fable(96, week))}, models: []string{fableModel}, strategy: StrategyAccountOrder, allowOrder: true},
		{name: "order switch never moves to a later account",
			accounts: []candidate{cand("a"), cand("b")}, models: []string{opusModel}, strategy: StrategyAccountOrder, allowOrder: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			strategy := c.strategy
			if strategy == "" {
				strategy = StrategyAccountOrder
			}
			d := decide(decideInput{
				provider: "claude", selectedID: "a", pinned: c.pinned, accounts: c.accounts,
				demand: buildDemand("claude", c.models), strategy: strategy, threshold: 95,
				allowOrder: c.allowOrder, now: testNow,
			})
			if d.to.ID != c.wantTo {
				t.Fatalf("target = %q, want %q", d.to.ID, c.wantTo)
			}
			if c.wantReason != "" && d.reason != c.wantReason {
				t.Fatalf("reason = %q, want %q", d.reason, c.wantReason)
			}
			if c.wantModel != "" && d.model != c.wantModel {
				t.Fatalf("model = %q, want %q", d.model, c.wantModel)
			}
		})
	}
}

func TestDecideCause(t *testing.T) {
	d := decide(decideInput{
		provider: "claude", selectedID: "a",
		accounts: []candidate{cand("a", session(96.4, at(time.Hour)), weekly(40, at(48*time.Hour))), cand("b")},
		demand:   buildDemand("claude", []string{opusModel}), strategy: StrategyAccountOrder, threshold: 95, now: testNow,
	})
	if !d.hasLimit || d.limit.LimitID != "session" || d.percent != 96.4 {
		t.Fatalf("cause = %+v %v", d.limit, d.percent)
	}
	want := "Switched Claude account from a to b: a is at 96% of its Current session limit."
	if got := switchNotice("claude", d); got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
}

func TestPoolOrder(t *testing.T) {
	accounts := []candidate{
		cand("later", weekly(10, at(120*time.Hour))),
		cand("unstarted", weekly(0, 0)),
		cand("sooner", weekly(10, at(48*time.Hour)), session(10, at(time.Hour))),
		cand("reset", weekly(50, at(-time.Hour))),
		cand("nothing"),
		// The session window resets soonest but the weekly window orders it.
		cand("session-soon", session(10, at(10*time.Minute)), weekly(10, at(96*time.Hour))),
		cand("tie", weekly(10, at(48*time.Hour))),
	}
	outside := cand("outside", weekly(10, at(time.Hour)))
	outside.InPool = false
	accounts = append(accounts, outside)

	ids := func(order []int) []string {
		out := make([]string, len(order))
		for i, idx := range order {
			out[i] = accounts[idx].ID
		}
		return out
	}
	got := ids(poolOrder("claude", StrategySoonestReset, accounts, testNow))
	want := []string{"sooner", "tie", "session-soon", "later", "unstarted", "reset", "nothing"}
	if !equal(got, want) {
		t.Fatalf("soonest-reset order = %v, want %v", got, want)
	}
	got = ids(poolOrder("claude", StrategyAccountOrder, accounts, testNow))
	want = []string{"later", "unstarted", "sooner", "reset", "nothing", "session-soon", "tie"}
	if !equal(got, want) {
		t.Fatalf("account-order = %v, want %v", got, want)
	}
}

func TestSoonestResetDrivesTarget(t *testing.T) {
	d := decide(decideInput{
		provider: "claude", selectedID: "a",
		accounts: []candidate{
			cand("a", session(100, at(time.Hour))),
			cand("b", weekly(10, at(120*time.Hour))),
			cand("c", weekly(10, at(24*time.Hour))),
		},
		demand: buildDemand("claude", []string{opusModel}), strategy: StrategySoonestReset, threshold: 95, now: testNow,
	})
	if d.to.ID != "c" {
		t.Fatalf("target = %q, want c", d.to.ID)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewMark(t *testing.T) {
	cases := []struct {
		name      string
		family    string
		limits    []provider.RateLimitEntry
		wantUntil time.Time
		wantAll   bool
	}{
		{"no reading lasts an hour", "fable", nil, testNow.Add(time.Hour), false},
		{"most used applying limit", "fable",
			[]provider.RateLimitEntry{session(30, at(2*time.Hour)), fable(100, at(50*time.Hour)), weekly(60, at(70*time.Hour))},
			testNow.Add(50 * time.Hour), false},
		{"other family's limit does not apply", "opus",
			[]provider.RateLimitEntry{session(30, at(2*time.Hour)), fable(100, at(50*time.Hour))},
			testNow.Add(2 * time.Hour), false},
		{"provider-wide past threshold covers every model", "opus",
			[]provider.RateLimitEntry{session(97, at(2*time.Hour)), weekly(40, at(70*time.Hour))},
			testNow.Add(2 * time.Hour), true},
		{"provider-wide below threshold keeps the family", "opus",
			[]provider.RateLimitEntry{session(80, at(2*time.Hour))},
			testNow.Add(2 * time.Hour), false},
		{"most used of any kind when none applies", "opus",
			[]provider.RateLimitEntry{fable(100, at(30*time.Hour)), {LimitID: "weekly_scoped:design", LimitName: "Design", UsedPercent: 20, ResetsAt: at(5 * time.Hour)}},
			testNow.Add(30 * time.Hour), false},
		{"limit without a reset time lasts an hour", "opus",
			[]provider.RateLimitEntry{session(100, 0)},
			testNow.Add(time.Hour), true},
		{"expired limit counts as zero", "opus",
			[]provider.RateLimitEntry{session(100, at(-time.Minute)), weekly(20, at(70*time.Hour))},
			testNow.Add(70 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMark("claude", tc.family, tc.limits, 95, testNow)
			if !m.until.Equal(tc.wantUntil) || m.allModels != tc.wantAll || m.family != tc.family {
				t.Fatalf("mark = until %v all %v family %q, want until %v all %v", m.until, m.allModels, m.family, tc.wantUntil, tc.wantAll)
			}
		})
	}
}

func TestMarkCoversFamily(t *testing.T) {
	m := mark{family: "fable", until: testNow.Add(time.Hour)}
	if assess("claude", "fable", nil, []mark{m}, 95, testNow, nil).state != stateExhausted {
		t.Fatal("mark does not exhaust its family")
	}
	if assess("claude", "opus", nil, []mark{m}, 95, testNow, nil).state != stateClear {
		t.Fatal("family mark exhausts another family")
	}
	m.allModels = true
	if assess("claude", "opus", nil, []mark{m}, 95, testNow, nil).state != stateExhausted {
		t.Fatal("widened mark does not cover every model")
	}
	if assess("claude", "opus", nil, []mark{m}, 95, testNow.Add(time.Hour), nil).state != stateClear {
		t.Fatal("expired mark still counts")
	}
}

func TestEarliestServe(t *testing.T) {
	accounts := []candidate{
		cand("a", session(100, at(3*time.Hour))),
		cand("b", fable(100, at(30*time.Hour)), session(100, at(2*time.Hour))),
		cand("c", session(100, 0)),
	}
	when, who, ok := earliestServe("claude", "fable", accounts, 95, testNow)
	if !ok || who.ID != "a" || !when.Equal(testNow.Add(3*time.Hour)) {
		t.Fatalf("earliest = %v %q %v", when, who.ID, ok)
	}
}
