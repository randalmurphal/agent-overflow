package harnessrpc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claude/claudefake"
)

func answersUsage(t *testing.T, usage *claudefake.Usage, email string) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, claude.UsageURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+claudefake.AccessToken(email, "org", "1"))
	resp, ok, err := usage.Serve(req)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		_ = resp.Body.Close()
	}
	return ok
}

func TestHarnessSetClaudeUsageSetsClearsAndReadsTheSelectedAccount(t *testing.T) {
	h, host := newHarnessTestHost(t)
	ctx := context.Background()
	const email = "usage@agent-overflow.test"

	if err := h.HarnessSetClaudeUsage(ctx, HarnessClaudeUsageSpec{Email: email, Limits: []claudefake.UsageLimit{{Kind: "weekly_all", Group: "weekly", Percent: 40}}}); err == nil {
		t.Fatal("a boot with no usage endpoint accepted a reading")
	}
	if host.usageReadings != 0 {
		t.Fatal("a refused set read the usage")
	}

	host.claudeUsage = claudefake.NewUsage()
	if err := h.HarnessSetClaudeUsage(ctx, HarnessClaudeUsageSpec{Email: email, Limits: []claudefake.UsageLimit{{Kind: "weekly_all", Group: "weekly", Percent: 40}}}); err != nil {
		t.Fatal(err)
	}
	if !answersUsage(t, host.claudeUsage, email) || host.usageReadings != 1 {
		t.Fatalf("after set: answered=%v readings=%d", answersUsage(t, host.claudeUsage, email), host.usageReadings)
	}
	if err := h.HarnessSetClaudeUsage(ctx, HarnessClaudeUsageSpec{Email: email}); err != nil {
		t.Fatal(err)
	}
	if answersUsage(t, host.claudeUsage, email) || host.usageReadings != 2 {
		t.Fatalf("after clear: readings=%d", host.usageReadings)
	}
	if err := h.HarnessSetClaudeUsage(ctx, HarnessClaudeUsageSpec{}); err == nil {
		t.Fatal("a clear naming no identity was accepted")
	}

	// A read that fails is the call's error, with the answer already set.
	host.usageReadErr = errors.New("the usage endpoint refused the selected account")
	err := h.HarnessSetClaudeUsage(ctx, HarnessClaudeUsageSpec{Email: email, Limits: []claudefake.UsageLimit{{Kind: "session", Group: "session", Percent: 5}}})
	if !errors.Is(err, host.usageReadErr) || !answersUsage(t, host.claudeUsage, email) {
		t.Fatalf("set with a failing read = %v, answered = %v; want the read's error with the answer set",
			err, answersUsage(t, host.claudeUsage, email))
	}
	host.usageReadErr = nil

	// A read and decision that cannot settle end within the bound, before
	// the e2e client stops waiting, with the host's account of what did not
	// settle as the answer.
	unsettled := fmt.Errorf("auto-switch did not settle: the worker is still deciding: %w", context.DeadlineExceeded)
	host.usageReadErr = unsettled
	err = h.HarnessSetClaudeUsage(ctx, HarnessClaudeUsageSpec{Email: email, Limits: []claudefake.UsageLimit{{Kind: "session", Group: "session", Percent: 6}}})
	returned := time.Now()
	if !errors.Is(err, unsettled) {
		t.Fatalf("set with an unsettled read = %v, want the host's error", err)
	}
	const clientWait = 15 * time.Second // e2e/src/harness-app.ts RPC_TIMEOUT_MS
	if !host.usageReadBounded || host.usageReadDeadline.Sub(returned) > harnessUsageReadTimeout || harnessUsageReadTimeout >= clientWait {
		t.Fatalf("read deadline = %v after the call returned (bounded %v), bound %v; want a bound below the client's %v",
			host.usageReadDeadline.Sub(returned), host.usageReadBounded, harnessUsageReadTimeout, clientWait)
	}
	host.usageReadErr = nil

	if err := h.HarnessReset(); err != nil {
		t.Fatal(err)
	}
	if answersUsage(t, host.claudeUsage, email) {
		t.Fatal("reset kept a usage answer for the next test")
	}
}
