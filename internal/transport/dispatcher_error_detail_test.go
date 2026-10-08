package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/logging"
)

// detailApp returns one error of each kind the dispatcher answers with a
// diagnostic record, and the refusals it answers without one.
type detailApp struct{}

func (*detailApp) Wrapped() error {
	return fmt.Errorf("revert: %w", fmt.Errorf("slice: %w", errors.New("uuid u1 missing from /home/a/s.jsonl")))
}

func (*detailApp) Reviewed() error {
	return errorsx.Public("thing_refused", "Could not do the thing.", fmt.Errorf("step: %w", errors.New("root cause")))
}

func (*detailApp) Handled() error { return fmt.Errorf("answer: %w", ErrAlreadyHandled) }

func (*detailApp) Panics() error { panic("boom") }

func (*detailApp) TakesCount(int) error { return nil }

func (*detailApp) Unmarshalable() (func(), error) { return func() {}, nil }

// captureLog routes the standard logger through the shells' Output for the
// test, so a reference can be looked up the way an error report does.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(logging.Output(&buf))
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

func invokeDetail(t *testing.T, name string, loopback bool, params ...json.RawMessage) *FrameError {
	t.Helper()
	d := NewDispatcher()
	if _, err := d.Register(&detailApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, fe := resolveLoopback(d, 0, name)
	if fe != nil {
		t.Fatalf("resolve %s: %s", name, fe.Message)
	}
	_, fe = d.InvokeForOrigin(context.Background(), m, params, loopback)
	if fe == nil {
		t.Fatalf("%s returned no error frame", name)
	}
	return fe
}

func TestMethodErrorDetailCarriesTheChainToLoopbackOnly(t *testing.T) {
	captureLog(t)
	local := invokeDetail(t, "Wrapped", true)
	want := []string{"revert", "slice", "uuid u1 missing from /home/a/s.jsonl"}
	if local.Detail == nil || !slices.Equal(local.Detail.Chain, want) {
		t.Fatalf("loopback detail = %+v, want chain %q", local.Detail, want)
	}
	if local.Detail.Method != "Wrapped" || local.Detail.At == 0 || local.Detail.Ref == "" {
		t.Fatalf("loopback detail identity = %+v", local.Detail)
	}

	remote := invokeDetail(t, "Wrapped", false)
	if remote.Detail == nil || remote.Detail.Ref == "" || remote.Detail.Method != "Wrapped" {
		t.Fatalf("off-host detail = %+v, want ref and method", remote.Detail)
	}
	if remote.Detail.Chain != nil {
		t.Fatalf("off-host detail leaked the chain: %q", remote.Detail.Chain)
	}
	if !strings.Contains(remote.Message, remote.Detail.Ref) {
		t.Fatalf("off-host message %q does not name ref %q", remote.Message, remote.Detail.Ref)
	}
}

func TestMethodErrorRefNamesItsRetainedLogLine(t *testing.T) {
	captureLog(t)
	fe := invokeDetail(t, "Wrapped", true)
	lines, found := logging.RecentLogLinesBefore("(id: "+fe.Detail.Ref+")", 5)
	if !found || !strings.Contains(lines[len(lines)-1], "uuid u1 missing") {
		t.Fatalf("log lines for ref %s = %q found=%v", fe.Detail.Ref, lines, found)
	}
}

func TestReviewedErrorKeepsItsMessageAndGainsADetail(t *testing.T) {
	captureLog(t)
	fe := invokeDetail(t, "Reviewed", true)
	if fe.Code != "thing_refused" || fe.Message != "Could not do the thing." {
		t.Fatalf("frame = %s %q, want the reviewed code and message", fe.Code, fe.Message)
	}
	want := []string{"Could not do the thing.", "step", "root cause"}
	if fe.Detail == nil || !slices.Equal(fe.Detail.Chain, want) {
		t.Fatalf("detail = %+v, want chain %q", fe.Detail, want)
	}
	if _, found := logging.RecentLogLinesBefore("(id: "+fe.Detail.Ref+")", 1); !found {
		t.Fatal("a reviewed error was not logged under its ref")
	}
	if remote := invokeDetail(t, "Reviewed", false); remote.Detail.Chain != nil || remote.Message != "Could not do the thing." {
		t.Fatalf("off-host reviewed frame = %q %+v", remote.Message, remote.Detail)
	}
}

func TestAlreadyHandledCarriesNoDetail(t *testing.T) {
	captureLog(t)
	if fe := invokeDetail(t, "Handled", true); fe.Code != ErrCodeAlreadyHandled || fe.Detail != nil {
		t.Fatalf("frame = %s %+v, want already_handled without detail", fe.Code, fe.Detail)
	}
}

func TestPanicDetailNamesItsLoggedStack(t *testing.T) {
	captureLog(t)
	fe := invokeDetail(t, "Panics", true)
	if fe.Code != ErrCodeInternal || fe.Detail == nil || fe.Detail.Ref == "" {
		t.Fatalf("panic frame = %s %+v", fe.Code, fe.Detail)
	}
	lines, found := logging.RecentLogLinesBefore("(id: "+fe.Detail.Ref+")", 1)
	if !found || !strings.Contains(lines[0], "boom") {
		t.Fatalf("panic log line = %q found=%v", lines, found)
	}
}

func TestDecodeAndMarshalFailuresNameTheirLogLine(t *testing.T) {
	captureLog(t)
	for _, tc := range []struct {
		name, method string
		code         string
		params       []json.RawMessage
		logged       string
	}{
		{"bad parameter", "TakesCount", ErrCodeBadParams, []json.RawMessage{json.RawMessage(`"seven"`)}, "TakesCount param 0"},
		{"unmarshalable result", "Unmarshalable", ErrCodeInternal, nil, "marshal result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := invokeDetail(t, tc.method, true, tc.params...)
			if fe.Code != tc.code || fe.Detail == nil || len(fe.Detail.Chain) == 0 {
				t.Fatalf("frame = %s %+v, want %s with a chain", fe.Code, fe.Detail, tc.code)
			}
			lines, found := logging.RecentLogLinesBefore("(id: "+fe.Detail.Ref+")", 1)
			if !found || !strings.Contains(lines[0], tc.logged) {
				t.Fatalf("log line = %q found=%v, want %q", lines, found, tc.logged)
			}
			if remote := invokeDetail(t, tc.method, false, tc.params...); remote.Detail == nil || remote.Detail.Chain != nil {
				t.Fatalf("off-host detail = %+v, want a ref without the chain", remote.Detail)
			}
		})
	}
}
