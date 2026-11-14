package harnessrpc

import (
	"net/http"
	"testing"

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

func TestHarnessSetClaudeUsageSetsClearsAndRequestsAReading(t *testing.T) {
	h, host := newHarnessTestHost(t)
	const email = "usage@agent-overflow.test"

	if err := h.HarnessSetClaudeUsage(HarnessClaudeUsageSpec{Email: email, Limits: []claudefake.UsageLimit{{Kind: "weekly_all", Group: "weekly", Percent: 40}}}); err == nil {
		t.Fatal("a boot with no usage endpoint accepted a reading")
	}
	if host.usageReadings != 0 {
		t.Fatal("a refused set requested a reading")
	}

	host.claudeUsage = claudefake.NewUsage()
	if err := h.HarnessSetClaudeUsage(HarnessClaudeUsageSpec{Email: email, Limits: []claudefake.UsageLimit{{Kind: "weekly_all", Group: "weekly", Percent: 40}}}); err != nil {
		t.Fatal(err)
	}
	if !answersUsage(t, host.claudeUsage, email) || host.usageReadings != 1 {
		t.Fatalf("after set: answered=%v readings=%d", answersUsage(t, host.claudeUsage, email), host.usageReadings)
	}
	if err := h.HarnessSetClaudeUsage(HarnessClaudeUsageSpec{Email: email}); err != nil {
		t.Fatal(err)
	}
	if answersUsage(t, host.claudeUsage, email) || host.usageReadings != 2 {
		t.Fatalf("after clear: readings=%d", host.usageReadings)
	}
	if err := h.HarnessSetClaudeUsage(HarnessClaudeUsageSpec{}); err == nil {
		t.Fatal("a clear naming no identity was accepted")
	}

	if err := h.HarnessSetClaudeUsage(HarnessClaudeUsageSpec{Email: email, Limits: []claudefake.UsageLimit{{Kind: "session", Group: "session", Percent: 5}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.HarnessReset(); err != nil {
		t.Fatal(err)
	}
	if answersUsage(t, host.claudeUsage, email) {
		t.Fatal("reset kept a usage answer for the next test")
	}
}
