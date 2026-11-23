package codexapp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/codexusage"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider/codex"
)

func TestBackgroundControlsDistinguishMissingMismatchAndForward(t *testing.T) {
	ctx := context.Background()
	missing := New(Deps{Reaper: procutiltest.Reaper(), Session: func(threadID string) (*codex.Session, bool) {
		return nil, threadID == "other-provider"
	}})
	if err := missing.CleanBackgroundTerminals(ctx, "missing", "root-provider-thread"); err == nil || !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("missing error = %v", err)
	}
	if _, err := missing.TerminateBackgroundTerminal(ctx, "other-provider", "root-provider-thread", "42"); err == nil || !strings.Contains(err.Error(), "not a Codex thread") {
		t.Fatalf("mismatch error = %v", err)
	}
	if _, err := missing.RootProcessScope("missing"); err == nil || !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("root scope of a missing session: err = %v", err)
	}

	type deadlineKey struct{}
	callCtx := context.WithValue(ctx, deadlineKey{}, "caller")
	var cleaned, terminated, listed []string
	sawCaller := func(ctx context.Context) {
		if ctx.Value(deadlineKey{}) != "caller" {
			t.Error("a call did not run under the caller's context")
		}
	}
	session := codex.NewBackgroundTerminalsTestSession(codex.BackgroundTerminalsTestWire{
		RootThreadID: "root-provider-thread",
		Clean: func(ctx context.Context, providerThreadID string) error {
			sawCaller(ctx)
			cleaned = append(cleaned, providerThreadID)
			return nil
		},
		Terminate: func(ctx context.Context, providerThreadID, processID string) (bool, error) {
			sawCaller(ctx)
			terminated = append(terminated, providerThreadID+"/"+processID)
			return true, nil
		},
		List: func(ctx context.Context, providerThreadID string) ([]string, error) {
			sawCaller(ctx)
			listed = append(listed, providerThreadID)
			return []string{"cmd-" + providerThreadID}, nil
		},
	})
	service := New(Deps{Reaper: procutiltest.Reaper(), Session: func(string) (*codex.Session, bool) { return session, true }})
	root, err := service.RootProcessScope("thread")
	if err != nil || root != "root-provider-thread" {
		t.Fatalf("RootProcessScope = (%q, %v), want root-provider-thread", root, err)
	}
	for _, scope := range []string{root, "child-provider-thread"} {
		if err := service.CleanBackgroundTerminals(callCtx, "thread", scope); err != nil {
			t.Fatalf("CleanBackgroundTerminals(%q): %v", scope, err)
		}
		if ok, err := service.TerminateBackgroundTerminal(callCtx, "thread", scope, "1734029"); err != nil || !ok {
			t.Fatalf("TerminateBackgroundTerminal(%q) = (%v, %v)", scope, ok, err)
		}
		terminals, err := service.ListBackgroundTerminals(callCtx, "thread", scope)
		if err != nil || len(terminals) != 1 || terminals[0].ItemID != "cmd-"+scope {
			t.Fatalf("ListBackgroundTerminals(%q) = (%+v, %v)", scope, terminals, err)
		}
	}
	want := []string{"root-provider-thread", "child-provider-thread"}
	if !slices.Equal(cleaned, want) || !slices.Equal(listed, want) {
		t.Fatalf("cleaned %v, listed %v, want %v", cleaned, listed, want)
	}
	if want := []string{"root-provider-thread/1734029", "child-provider-thread/1734029"}; !slices.Equal(terminated, want) {
		t.Fatalf("terminated = %v, want %v", terminated, want)
	}
}

func TestStopSubagentForwardsOwnedLaunchUnderCallerContext(t *testing.T) {
	var called atomic.Bool
	var sawDeadline bool
	fake := codex.NewInterruptSubagentTestSession("spawn-1", func(ctx context.Context, childThreadID, turnID string) error {
		called.Store(true)
		_, sawDeadline = ctx.Deadline()
		if childThreadID != "test-child-thread" || turnID != "" {
			t.Fatalf("child interrupt target = %q/%q", childThreadID, turnID)
		}
		return nil
	})
	service := New(Deps{Reaper: procutiltest.Reaper(), Session: func(string) (*codex.Session, bool) { return fake, true }})
	ctx, cancel := context.WithTimeout(context.Background(), BackgroundTerminalStopTimeout)
	defer cancel()
	stopped, err := service.StopSubagent(ctx, "codex-thread", "spawn-1")
	if err != nil || !stopped {
		t.Fatalf("StopSubagent = (%v, %v), want (true, nil)", stopped, err)
	}
	if !called.Load() || !sawDeadline {
		t.Fatalf("interrupt called=%v deadline=%v", called.Load(), sawDeadline)
	}
	stopped, err = service.StopSubagent(ctx, "codex-thread", "spawn-1")
	if err != nil || stopped {
		t.Fatalf("repeated StopSubagent = (%v, %v), want (false, nil)", stopped, err)
	}
}

func TestSkillsValidationProjectionAndReset(t *testing.T) {
	cache := codexskills.New()
	cwd := t.TempDir()
	key := codexskills.Key("codex-test", cwd)
	calls := 0
	fetch := func() (codexskills.CwdSkills, error) {
		calls++
		return codexskills.CwdSkills{Cwd: cwd}, nil
	}
	if _, err := cache.Get(context.Background(), key, fetch); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	service := New(Deps{Reaper: procutiltest.Reaper(), Binary: func() string { return "codex-test" }, SkillsCache: cache})
	got, err := service.Skills(context.Background(), cwd, false)
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}
	if got.Skills == nil || got.Errors == nil || calls != 1 {
		t.Fatalf("skills=%+v calls=%d", got, calls)
	}
	service.ResetSkills()
	_, err = cache.Get(context.Background(), key, fetch)
	if err != nil || calls != 2 {
		t.Fatalf("after reset: calls=%d err=%v", calls, err)
	}
	for _, path := range []string{"", "relative/path"} {
		if _, err := service.Skills(context.Background(), path, false); err == nil {
			t.Fatalf("Skills(%q) succeeded", path)
		}
	}
}

func TestSkillsEntryForCwdPreservesExactAndUnambiguousNormalisedAnswers(t *testing.T) {
	entries := []codexskills.CwdSkills{
		{Cwd: "/other", Skills: []codexskills.Skill{{Name: "wrong"}}},
		{Cwd: "/repo", Skills: []codexskills.Skill{{Name: "right"}}},
	}
	got, err := skillsEntryForCwd(entries, "/repo")
	if err != nil || len(got.Skills) != 1 || got.Skills[0].Name != "right" {
		t.Fatalf("exact answer = %+v, err=%v", got, err)
	}
	got, err = skillsEntryForCwd([]codexskills.CwdSkills{{Cwd: "/repo/"}}, "/repo")
	if err != nil || got.Cwd != "/repo/" {
		t.Fatalf("single normalised answer = %+v, err=%v", got, err)
	}
	if _, err := skillsEntryForCwd([]codexskills.CwdSkills{{Cwd: "/a"}, {Cwd: "/b"}}, "/repo"); err == nil {
		t.Fatal("ambiguous response must be refused")
	}
}

func TestAccountUsagePreservesAbsenceErrorsAndAccountIdentity(t *testing.T) {
	value := int64(42)
	cache := codexusage.New()
	selection := AccountSelection{ID: "account-1", Email: " user@example.com "}
	key := "codex-test\x00" + selection.ID
	if _, err := cache.Get(context.Background(), key, func(context.Context) (codex.AccountUsage, error) {
		return codex.AccountUsage{
			LifetimeTokens: &value,
			DailyBuckets:   []codex.AccountUsageDailyBucket{{StartDate: "2026-08-01", Tokens: 42}},
		}, nil
	}); err != nil {
		t.Fatalf("prime usage: %v", err)
	}
	service := New(Deps{
		Reaper:        procutiltest.Reaper(),
		Binary:        func() string { return "codex-test" },
		ActiveAccount: func() AccountSelection { return selection },
		UsageCache:    cache,
	})
	result, err := service.AccountUsage()
	if err != nil || result == nil || result.AccountEmail != "user@example.com" || result.Usage.LifetimeTokens == nil || *result.Usage.LifetimeTokens != 42 {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	for name, cachedErr := range map[string]error{
		"unavailable": codex.ErrAccountUsageUnavailable,
		"failure":     errors.New("spawn failed"),
	} {
		t.Run(name, func(t *testing.T) {
			failureCache := codexusage.New()
			_, _ = failureCache.Get(context.Background(), key, func(context.Context) (codex.AccountUsage, error) {
				return codex.AccountUsage{}, cachedErr
			})
			failureService := New(Deps{
				Reaper: procutiltest.Reaper(),
				Binary: func() string { return "codex-test" }, ActiveAccount: func() AccountSelection { return selection }, UsageCache: failureCache,
			})
			got, err := failureService.AccountUsage()
			if name == "unavailable" {
				if got != nil || err != nil {
					t.Fatalf("unavailable: got=%v err=%v", got, err)
				}
			} else if err == nil {
				t.Fatalf("failure became absence: got=%v", got)
			}
		})
	}
}

func TestAccountUsageTreatsAnEmptySuccessfulReportAsAbsence(t *testing.T) {
	cache := codexusage.New()
	if _, err := cache.Get(context.Background(), "codex-test\x00account-1", func(context.Context) (codex.AccountUsage, error) {
		return codex.AccountUsage{}, nil
	}); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	service := New(Deps{
		Reaper: procutiltest.Reaper(),
		Binary: func() string { return "codex-test" },
		ActiveAccount: func() AccountSelection {
			return AccountSelection{ID: "account-1"}
		},
		UsageCache: cache,
	})
	result, err := service.AccountUsage()
	if err != nil || result != nil {
		t.Fatalf("empty report = %+v, err=%v", result, err)
	}
}
