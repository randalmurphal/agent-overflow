package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/providertest"
	"agent-overflow/internal/testutil/mockexec"
)

func TestParseResetCreditsReadsEveryShapeTheProtocolAllows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result string
		want   *ResetCredits
	}{
		{name: "absent", result: `{"rateLimits":{}}`, want: nil},
		{name: "null", result: `{"rateLimitResetCredits":null}`, want: nil},
		{name: "count only", result: `{"rateLimitResetCredits":{"availableCount":3,"credits":null}}`,
			want: &ResetCredits{AvailableCount: 3}},
		{name: "fetched and empty", result: `{"rateLimitResetCredits":{"availableCount":0,"credits":[]}}`,
			want: &ResetCredits{Credits: []ResetCredit{}}},
		{name: "capped details", result: `{"rateLimitResetCredits":{"availableCount":5,"credits":[
			{"id":"RateLimitResetCredit_1","resetType":"codexRateLimits","status":"available","grantedAt":1781654400,"expiresAt":1784246400,"title":"Full reset (Weekly + 5 hr)","description":"Ready to redeem"},
			{"id":"RateLimitResetCredit_2","resetType":"unknown","status":"redeeming","grantedAt":1781654400,"expiresAt":null,"title":null,"description":null},
			{"resetType":"codexRateLimits","status":"available"},
			"not a credit"
		]}}`, want: &ResetCredits{AvailableCount: 5, Credits: []ResetCredit{
			{ID: "RateLimitResetCredit_1", ResetType: "codexRateLimits", Status: "available", GrantedAt: 1781654400,
				ExpiresAt: 1784246400, Title: "Full reset (Weekly + 5 hr)", Description: "Ready to redeem"},
			{ID: "RateLimitResetCredit_2", ResetType: "unknown", Status: "redeeming", GrantedAt: 1781654400},
		}}},
		{name: "unreadable summary", result: `{"rateLimitResetCredits":{"credits":[]}}`, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResetCredits(json.RawMessage(tc.result))
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("parseResetCredits = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// writeSpendScript is an app-server that records every line it is sent,
// answers account/read after accountDelay, answers the consume with
// consumeFrame, then answers the rate-limit read. An empty consumeFrame exits
// once the consume arrives, as a process that dies before answering does.
func writeSpendScript(t *testing.T, consumeFrame string, accountDelay time.Duration) (binary, transcript string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "mock-codex")
	transcript = filepath.Join(dir, "stdin.log")
	record := "IFS= read -r line || exit 0; printf '%s\\n' \"$line\" >> \"$log\"\n"
	script := "#!/bin/bash\n" +
		`log="` + transcript + `"` + "\n" +
		record +
		`printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":` + providertest.CodexInitializeResult + `}'` + "\n" +
		record + record
	if accountDelay > 0 {
		script += "sleep " + strings.TrimSuffix(accountDelay.String(), "s") + "\n"
	}
	script += `printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"chatgpt","email":"person@example.com","planType":"pro"}}}'` + "\n" +
		record
	if consumeFrame == "" {
		script += "exit 0\n"
	} else {
		script += `printf '%s\n' '{"jsonrpc":"2.0","method":"some/notification","params":{}}'` + "\n" +
			`printf '%s\n' '` + consumeFrame + `'` + "\n" +
			record +
			`printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"planType":"pro"},"rateLimitResetCredits":{"availableCount":1,"credits":[{"id":"RateLimitResetCredit_2","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":2}]}}}'` + "\n"
	}
	mockexec.Write(t, binary, script)
	return binary, transcript
}

// spendConfig is a spend of key and credit, admitted for every login, with a
// cutoff a minute away.
func spendConfig(binary, key, credit string) ProbeConfig {
	return ProbeConfig{
		Binary: binary, Reaper: procutiltest.Reaper(), WorkDir: probeWorkDir,
		SendBy: time.Now().Add(time.Minute),
		BeforeConsume: func(provider.AccountInfo) (ResetSpend, error) {
			return ResetSpend{IdempotencyKey: key, CreditID: credit}, nil
		},
	}
}

func sentMethods(t *testing.T, transcript string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	var frames []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("sent a line that is not JSON: %q", line)
		}
		frames = append(frames, frame)
	}
	return frames
}

func consumeSent(t *testing.T, transcript string) bool {
	t.Helper()
	for _, frame := range sentMethods(t, transcript) {
		if frame["method"] == "account/rateLimitResetCredit/consume" {
			return true
		}
	}
	return false
}

func TestSpendResetConsumesBeforeItReadsAndReportsTheReadAfter(t *testing.T) {
	t.Parallel()
	binary, transcript := writeSpendScript(t, `{"jsonrpc":"2.0","id":4,"result":{"outcome":"reset"}}`, 0)
	var credits *ResetCredits
	read := false
	// The hooks in the order they ran.
	var hooks []string
	cfg := spendConfig(binary, "key-1", "RateLimitResetCredit_1")
	cfg.OnConsumeWritten = func() { hooks = append(hooks, "written") }
	cfg.OnConsumeAnswer = func(outcome string) { hooks = append(hooks, "answer "+outcome) }
	cfg.OnResetCredits = func(got *ResetCredits) { credits, read = got, true; hooks = append(hooks, "credits") }
	outcome, info, err := SpendReset(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SpendReset: %v", err)
	}
	if outcome != ResetOutcomeReset {
		t.Fatalf("outcome = %q, want reset", outcome)
	}
	if strings.Join(hooks, ", ") != "written, answer reset, credits" {
		t.Fatalf("hooks ran as %q, want the write, then the answer, then the reading after it", hooks)
	}
	if info.Email != "person@example.com" {
		t.Fatalf("the read after the spend reported %+v", info)
	}
	if !read || credits == nil || len(credits.Credits) != 1 || credits.Credits[0].ID != "RateLimitResetCredit_2" {
		t.Fatalf("credits after the spend = %+v (read=%v)", credits, read)
	}
	frames := sentMethods(t, transcript)
	methods := make([]any, len(frames))
	for i, frame := range frames {
		methods[i] = frame["method"]
	}
	want := []any{"initialize", "initialized", "account/read", "account/rateLimitResetCredit/consume", "account/rateLimits/read"}
	if len(methods) != len(want) {
		t.Fatalf("methods sent = %v, want %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("methods sent = %v, want %v", methods, want)
		}
	}
	params := frames[3]["params"].(map[string]any)
	if params["idempotencyKey"] != "key-1" || params["creditId"] != "RateLimitResetCredit_1" {
		t.Fatalf("consume params = %v", params)
	}
}

// Every consume names a credit: an admission without one sends nothing.
func TestSpendResetSendsNothingForASpendThatNamesNoCredit(t *testing.T) {
	t.Parallel()
	binary, transcript := writeSpendScript(t, `{"jsonrpc":"2.0","id":4,"result":{"outcome":"noCredit"}}`, 0)
	outcome, info, err := SpendReset(context.Background(), spendConfig(binary, "key-2", ""))
	if !errors.Is(err, ErrResetSpendInvalid) || outcome != "" {
		t.Fatalf("SpendReset = %q, %v; want ErrResetSpendInvalid", outcome, err)
	}
	if info.Email != "person@example.com" {
		t.Fatalf("the refusal dropped the account/read answer: %+v", info)
	}
	if consumeSent(t, transcript) {
		t.Fatal("a spend naming no credit was sent")
	}
}

func TestSpendResetClassifiesEveryUnsettledAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		frame       string
		refused     bool
		unconfirmed bool
	}{
		{name: "not a chatgpt login", frame: `{"jsonrpc":"2.0","id":4,"error":{"code":-32600,"message":"chatgpt authentication required for rate limit reset credits"}}`, refused: true},
		{name: "older cli", frame: `{"jsonrpc":"2.0","id":4,"error":{"code":-32601,"message":"Method not found"}}`, refused: true},
		{name: "app-server timeout", frame: `{"jsonrpc":"2.0","id":4,"error":{"code":-32603,"message":"rate limit reset consume timed out"}}`, unconfirmed: true},
		{name: "unknown outcome", frame: `{"jsonrpc":"2.0","id":4,"result":{"outcome":"maybe"}}`, unconfirmed: true},
		{name: "process ends first", frame: "", unconfirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary, _ := writeSpendScript(t, tc.frame, 0)
			outcome, info, err := SpendReset(context.Background(), spendConfig(binary, "key-3", "RateLimitResetCredit_1"))
			if outcome != "" {
				t.Fatalf("outcome = %q, want none", outcome)
			}
			var refused *ResetRefusedError
			var unconfirmed *ResetUnconfirmedError
			if errors.As(err, &refused) != tc.refused || errors.As(err, &unconfirmed) != tc.unconfirmed {
				t.Fatalf("err = %v; refused=%v unconfirmed=%v", err, tc.refused, tc.unconfirmed)
			}
			// The caller attributes what the app-server rotated by this login.
			if info.Email != "person@example.com" {
				t.Fatalf("an unsettled answer dropped the account/read answer: %+v", info)
			}
		})
	}
}

func TestSpendResetNeedsAnAdmissionAndACutoff(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]ProbeConfig{
		"no admission": {SendBy: time.Now().Add(time.Minute)},
		"no cutoff": {BeforeConsume: func(provider.AccountInfo) (ResetSpend, error) {
			return ResetSpend{IdempotencyKey: "k", CreditID: "c"}, nil
		}},
	} {
		cfg.Reaper, cfg.WorkDir = procutiltest.Reaper(), probeWorkDir
		if _, _, err := SpendReset(context.Background(), cfg); !errors.Is(err, ErrResetSpendInvalid) {
			t.Fatalf("%s: err = %v, want ErrResetSpendInvalid", name, err)
		}
	}
}

// The consume goes out only with what the caller admitted for the login it
// saw; a refused admission sends nothing.
func TestSpendResetSendsNothingUnderALoginTheCallerRefuses(t *testing.T) {
	t.Parallel()
	binary, transcript := writeSpendScript(t, `{"jsonrpc":"2.0","id":4,"result":{"outcome":"reset"}}`, 0)
	refusal := errors.New("someone else's login")
	var seen provider.AccountInfo
	cfg := spendConfig(binary, "key-4", "c")
	cfg.BeforeConsume = func(info provider.AccountInfo) (ResetSpend, error) { seen = info; return ResetSpend{}, refusal }
	_, info, err := SpendReset(context.Background(), cfg)
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the caller's refusal", err)
	}
	if seen.Email != "person@example.com" || info.Email != "person@example.com" {
		t.Fatalf("BeforeConsume saw %+v and SpendReset returned %+v, want the account/read answer", seen, info)
	}
	if consumeSent(t, transcript) {
		t.Fatal("the consume was sent under a refused login")
	}
}

// Every step before the consume ends by SendBy: an app-server that answers
// account/read after it gets no consume, and the caller is not asked to admit
// one.
func TestSpendResetStopsAtTheCutoffBeforeTheConsume(t *testing.T) {
	t.Parallel()
	binary, transcript := writeSpendScript(t, `{"jsonrpc":"2.0","id":4,"result":{"outcome":"reset"}}`, 2*time.Second)
	cfg := spendConfig(binary, "key-5", "c")
	cfg.SendBy = time.Now().Add(300 * time.Millisecond)
	admitted := false
	admit := cfg.BeforeConsume
	cfg.BeforeConsume = func(info provider.AccountInfo) (ResetSpend, error) {
		admitted = true
		return admit(info)
	}
	outcome, _, err := SpendReset(context.Background(), cfg)
	if err == nil || outcome != "" {
		t.Fatalf("SpendReset = %q, %v; want the cutoff's error", outcome, err)
	}
	var unconfirmed *ResetUnconfirmedError
	if errors.As(err, &unconfirmed) {
		t.Fatalf("a spend stopped before its consume reads as unconfirmed: %v", err)
	}
	if admitted {
		t.Fatal("the caller was asked to admit a spend past its cutoff")
	}
	if consumeSent(t, transcript) {
		t.Fatal("the consume was sent after the cutoff")
	}
}

// A probe whose rate-limit read fails still says whose login it ran.
func TestProbeAccountReturnsTheAccountReadWhenTheRateLimitReadFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "mock-codex")
	mockexec.Write(t, path, "#!/bin/bash\n"+
		probeInitReply+"read -r _ || true\nread -r _ || true\nread -r _ || true\n"+
		`printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"chatgpt","email":"person@example.com","planType":"pro"}}}'`+"\n"+
		`printf '%s\n' '{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"usage unavailable"}}'`+"\n")
	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: path, Reaper: procutiltest.Reaper(), WorkDir: probeWorkDir})
	if err == nil {
		t.Fatal("a failed rate-limit read reported success")
	}
	if info.Email != "person@example.com" {
		t.Fatalf("info = %+v, want the account/read answer", info)
	}
}

func TestProbeAccountReportsAnAbsentResetSummaryAsNone(t *testing.T) {
	t.Parallel()
	binary := writeMockCodexAppServerScript(t, t.TempDir(), `{"rateLimits":{"planType":"pro"}}`, "")
	called := false
	credits := &ResetCredits{}
	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary: binary, Reaper: procutiltest.Reaper(), WorkDir: probeWorkDir,
		OnResetCredits: func(got *ResetCredits) { called, credits = true, got },
	}); err != nil {
		t.Fatal(err)
	}
	if !called || credits != nil {
		t.Fatalf("OnResetCredits called=%v with %+v, want once with nil", called, credits)
	}
}

// The cutoff is checked last before the consume's write: an admission that
// ran past it sends nothing, and the write hook is not called.
func TestSpendResetSendsNothingWhenTheAdmissionRunsPastTheCutoff(t *testing.T) {
	t.Parallel()
	binary, transcript := writeSpendScript(t, `{"jsonrpc":"2.0","id":4,"result":{"outcome":"reset"}}`, 0)
	cfg := spendConfig(binary, "key-6", "c")
	cfg.SendBy = time.Now().Add(2 * time.Second)
	admit := cfg.BeforeConsume
	cfg.BeforeConsume = func(info provider.AccountInfo) (ResetSpend, error) {
		time.Sleep(time.Until(cfg.SendBy) + 50*time.Millisecond)
		return admit(info)
	}
	written := false
	cfg.OnConsumeWritten = func() { written = true }
	outcome, info, err := SpendReset(context.Background(), cfg)
	if !errors.Is(err, ErrResetLate) || outcome != "" {
		t.Fatalf("SpendReset = %q, %v; want ErrResetLate", outcome, err)
	}
	if info.Email != "person@example.com" {
		t.Fatalf("the late refusal dropped the account/read answer: %+v", info)
	}
	if written {
		t.Fatal("the write hook ran for a consume that was not written")
	}
	if consumeSent(t, transcript) {
		t.Fatal("the consume was sent after the cutoff")
	}
}
