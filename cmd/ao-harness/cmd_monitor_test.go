package main

import (
	"bytes"
	"strings"
	"testing"

	"agent-overflow/internal/harnessclient"
)

func TestMonitorDescriptorsExposeCompleteTypedSurface(t *testing.T) {
	want := []string{"list", "start", "heartbeat", "overlap", "status", "collect", "stop", "cleanup", "last"}
	got := commandNames(monitorCommandDescriptors())
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("monitor commands = %v, want %v", got, want)
	}
}

func TestMonitorDispatchRejectsUnknownOperationBeforeAttach(t *testing.T) {
	code, _, stderr := run(t, "monitor", "observe", "--registry-dir", t.TempDir())
	if code != exitUsage || !strings.Contains(stderr, "unknown monitor subcommand") {
		t.Fatalf("dispatch result = %d, %q", code, stderr)
	}
}

func TestMonitorOverlapRequiresTwoDistinctRunIDs(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	for _, args := range [][]string{
		{"one"},
		{"one", "--with-run-id", "one"},
		{"--run-id", "one", "--with-run-id", "bad id"},
	} {
		if err := monitorOverlap(e, args); err == nil || !strings.Contains(err.Error(), "run ID") {
			t.Errorf("args %v accepted: %v", args, err)
		}
	}
}

func TestSelectMonitorPageRefusesAmbiguousAndLatePages(t *testing.T) {
	pages := []harnessclient.HarnessPageIdentity{
		{PageID: "page-a", Marker: "marker", Origin: "http://127.0.0.1:1"},
		{PageID: "page-b", Marker: "marker", Origin: "http://127.0.0.1:1"},
	}
	if _, err := selectMonitorPage(pages, "", "/tmp/harness"); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("ambiguous page was accepted: %v", err)
	}
	if _, err := selectMonitorPage(pages, "page-gone", "/tmp/harness"); err == nil || !strings.Contains(err.Error(), "not attached") || !strings.Contains(err.Error(), "page-a") {
		t.Fatalf("late page error = %v", err)
	}
	if got, err := selectMonitorPage(pages, "page-b", "/tmp/harness"); err != nil || got != "page-b" {
		t.Fatalf("selected page = %q, err = %v", got, err)
	}
}

func TestSelectMonitorPageRejectsIncompleteOwnership(t *testing.T) {
	_, err := selectMonitorPage([]harnessclient.HarnessPageIdentity{{PageID: "page-a", Marker: ""}}, "page-a", "/tmp/harness")
	if err == nil || !strings.Contains(err.Error(), "incomplete ownership") {
		t.Fatalf("incomplete page was accepted: %v", err)
	}
}

func TestMonitorOptionParserAcceptsRepeatableAndPositionalIDs(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	opts, flags := bindMonitorFlags(e, "monitor start", true)
	rest, err := e.parse(flags, []string{"--monitor", "frame-pacing", "--run-id", "run-1", "skipped-frames", "--at-ms", "0", "--heartbeat-timeout-ms", "2500"})
	if err != nil {
		t.Fatal(err)
	}
	noteMonitorFlags(opts, flags)
	positionals, err := opts.finish(e, rest)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range positionals {
		opts.monitor = append(opts.monitor, id)
	}
	if !opts.hasAtMs || !opts.hasTimeout || opts.runID != "run-1" || strings.Join(opts.monitor, ",") != "frame-pacing,skipped-frames" {
		t.Fatalf("parsed options = %+v, positionals = %v", opts, positionals)
	}
}

func TestMonitorOptionParserRejectsUnsafeInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"bad run", []string{"--run-id", " bad"}, "identifier"},
		{"bad timestamp", []string{"--run-id", "run", "--at-ms", "NaN"}, "finite"},
		{"bad timeout", []string{"--run-id", "run", "--heartbeat-timeout-ms", "0"}, "positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := testEnv(t.TempDir())
			opts, flags := bindMonitorFlags(e, "monitor start", true)
			rest, err := e.parse(flags, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			noteMonitorFlags(opts, flags)
			_, err = opts.finish(e, rest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestMonitorTextResultRefusesUnboundedOutput(t *testing.T) {
	e, stdout, _ := testEnv(t.TempDir())
	e.format = "text"
	budget := &queryOutputBudget{maxBytes: 8}
	raw := []byte(`{"v":1,"runId":"run","status":"complete","monitors":[]}`)
	if err := e.writeMonitorResult(raw, budget, "stop"); err == nil || !strings.Contains(err.Error(), "--max-bytes") {
		t.Fatalf("oversized monitor result was accepted: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("refusal wrote output: %q", stdout.String())
	}
}

func TestMonitorTextResultSummarizesWithoutDumpingObservations(t *testing.T) {
	e, stdout, _ := testEnv(t.TempDir())
	raw := []byte(`{"v":1,"runId":"run","status":"partial","partial":true,"monitors":[{"observations":[{"value":"secret"}]}]}`)
	if err := e.writeMonitorResult(raw, &queryOutputBudget{maxBytes: 4096}, "stop"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "secret") || !strings.Contains(stdout.String(), "partial=true") {
		t.Fatalf("text output leaked detail or omitted status: %q", stdout.String())
	}
}

func TestMonitorJSONOutputPreservesTypedPayload(t *testing.T) {
	e, stdout, _ := testEnv(t.TempDir())
	e.format = "json"
	raw := []byte(`{"v":1,"runId":"run","heartbeats":2}`)
	if err := e.writeMonitorResult(raw, &queryOutputBudget{maxBytes: 4096}, "heartbeat"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"heartbeats": 2`)) {
		t.Fatalf("json output = %q", stdout.String())
	}
}

func TestMonitorResultRejectsMalformedOrFuturePayloads(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	for _, raw := range [][]byte{
		[]byte(`{"v":2,"runId":"run","monitors":[]}`),
		[]byte(`{"v":1,"monitors":[]}`),
		[]byte(`{"v":1,"runId":"run","monitors":[]}`),
	} {
		if err := e.writeMonitorResult(raw, &queryOutputBudget{maxBytes: 4096}, "start"); err == nil {
			t.Fatalf("malformed monitor payload was accepted: %s", raw)
		}
	}
}
