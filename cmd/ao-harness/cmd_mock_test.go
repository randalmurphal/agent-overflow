package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/harness/control"
)

func mockRows() []mockRow {
	return []mockRow{
		{MockID: "mock-1", OpenGate: "first-delta"},
		{MockID: "mock-2", Exited: true},
	}
}

// `mock advance first-delta` with one live mock, and `mock advance mock-1
// first-delta` with several, are both what they look like.
func TestAdvanceArgsPositionalGrammar(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mocks    []mockRow
		rest     []string
		flag     string
		wantMock string
		wantGate string
	}{
		{name: "gate only, one live mock", mocks: mockRows(), rest: []string{"first-delta"}, wantMock: "mock-1", wantGate: "first-delta"},
		{name: "mock and gate", mocks: mockRows(), rest: []string{"mock-1", "second"}, wantMock: "mock-1", wantGate: "second"},
		{name: "mock only", mocks: mockRows(), rest: []string{"mock-1"}, wantMock: "mock-1"},
		{name: "no positionals", mocks: mockRows(), wantMock: "mock-1"},
		{name: "flag form", mocks: mockRows(), flag: "first-delta", wantMock: "mock-1", wantGate: "first-delta"},
		// A registered id wins over the gate reading, which is what makes a
		// gate NAMED like a mock resolve by the registry rather than by shape.
		{
			name:     "a gate named like a mock",
			mocks:    []mockRow{{MockID: "mock-1"}},
			rest:     []string{"mock-2"},
			wantMock: "mock-1",
			wantGate: "mock-2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockID, gate, err := advanceArgs(tc.mocks, tc.rest, tc.flag)
			if err != nil {
				t.Fatalf("advanceArgs: %v", err)
			}
			if mockID != tc.wantMock || gate != tc.wantGate {
				t.Fatalf("got (%q, %q), want (%q, %q)", mockID, gate, tc.wantMock, tc.wantGate)
			}
		})
	}
}

func TestAdvanceArgsRefusesTwoDifferentGateNames(t *testing.T) {
	_, _, err := advanceArgs(mockRows(), []string{"mock-1", "second"}, "first-delta")
	if err == nil {
		t.Fatal("two different gate names were accepted")
	}
	if code := exitCodeOf(t, err); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}

func TestAdvanceArgsNeedsAMockIDWhenSeveralAreLive(t *testing.T) {
	mocks := []mockRow{{MockID: "mock-1"}, {MockID: "mock-2"}}
	_, _, err := advanceArgs(mocks, []string{"first-delta"}, "")
	if err == nil {
		t.Fatal("advance picked between two live mocks")
	}
	for _, want := range []string{"mock-1", "mock-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits candidate %s: %v", want, err)
		}
	}
}

func TestAdvanceArgsWithNoLiveMockSaysSo(t *testing.T) {
	_, _, err := advanceArgs([]mockRow{{MockID: "mock-2", Exited: true}}, nil, "")
	if err == nil {
		t.Fatal("advance resolved against an exited mock")
	}
	if !strings.Contains(err.Error(), "no live mock") {
		t.Fatalf("error = %v", err)
	}
}

// The gate column is the only place `mock advance`'s correct argument is
// visible: the name lives in the scenario document, and "which one is
// open right now" is the question worth asking before releasing one.
func TestGateCellRendersWhereAMockIsParked(t *testing.T) {
	for _, tc := range []struct {
		mock mockRow
		want string
	}{
		{mockRow{}, "-"},
		{mockRow{OpenGate: "first-delta"}, "first-delta"},
		{mockRow{OpenGate: "first-delta", PendingAdvances: []string{"second"}}, "first-delta (+1 buffered)"},
		{mockRow{PendingAdvances: []string{"second", "third"}}, "- (+2 buffered)"},
	} {
		if got := gateCell(tc.mock); got != tc.want {
			t.Errorf("gateCell(%+v) = %q, want %q", tc.mock, got, tc.want)
		}
	}
}

// A name that matches no open gate is BUFFERED by the mock, silently. The
// old fire-and-forget line was true of the RPC and said nothing about the
// gate, so a caller's next move was to conclude the harness was wedged.
func TestAdvanceOutcomeLinesNameWhatActuallyHappened(t *testing.T) {
	for _, tc := range []struct {
		report mockReport
		want   string
	}{
		{mockReport{Kind: "advance_released", Gate: "first-delta"}, `released gate "first-delta" on mock-1`},
		{mockReport{Kind: "advance_buffered", Gate: "second", OpenGate: "first-delta"}, `buffered "second" (open gate is "first-delta")`},
		{mockReport{Kind: "fixture_error", Detail: "no such fixture"}, "advance -> mock-1 failed: no such fixture"},
		{mockReport{}, "no report arrived"},
	} {
		e, stdout, _ := testEnv("")
		if err := e.printAdvanceOutcome("mock-1", control.Command{Type: control.CommandAdvance}, tc.report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), tc.want) {
			t.Errorf("outcome for %q = %q, want it to contain %q", tc.report.Kind, stdout.String(), tc.want)
		}
	}
}
