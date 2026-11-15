package logging_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/testutil"
)

type refusal struct{}

func (refusal) Error() string { return "refused" }
func (refusal) Refused() bool { return true }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestLogSlowStepWritesOneLineOnlyPastTheThreshold(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 50*time.Millisecond)
	logs := testutil.CaptureLogOutput(t)

	logging.LogSlowStep("transport", "renew", time.Now().Add(-10*time.Millisecond), "ok")
	if got := logs.String(); got != "" {
		t.Fatalf("a step inside the threshold logged %q", got)
	}
	logging.LogSlowStep("transport", "renew", time.Now().Add(-120*time.Millisecond), "refused")
	steps := testutil.CheckedSlowSteps(t, logs.String(), "transport")
	testutil.ExpectOneSlowStep(t, steps, "renew", "refused", 120*time.Millisecond)
}

func TestSlowStepThresholdRestores(t *testing.T) {
	logs := testutil.CaptureLogOutput(t)
	restore := logging.SetSlowStepThreshold(time.Millisecond)
	restore()
	logging.LogSlowStep("transport", "renew", time.Now().Add(-time.Second), "ok")
	if got := logs.String(); got != "" {
		t.Fatalf("a one-second step logged %q under the restored two-second threshold", got)
	}
}

func TestErrorOutcomeClassifiesHowAStepEnded(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{fmt.Errorf("wrapped: %w", refusal{}), "refused"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "canceled"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{fmt.Errorf("wrapped: %w", timeoutErr{}), "timeout"},
		{errors.New("connection refused"), "error"},
	} {
		if got := logging.ErrorOutcome(tc.err); got != tc.want {
			t.Errorf("ErrorOutcome(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestStatusOutcomeClassifiesAnAnswer(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusOK: "ok", http.StatusSwitchingProtocols: "ok", http.StatusNotFound: "refused",
		http.StatusTooManyRequests: "refused", http.StatusServiceUnavailable: "error",
	} {
		if got := logging.StatusOutcome(status); got != want {
			t.Errorf("StatusOutcome(%d) = %q, want %q", status, got, want)
		}
	}
}
