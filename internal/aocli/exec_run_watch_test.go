package aocli

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// `agent-overflow run watch` and `agent-overflow run amend`. Both are exercised
// against the same fake backend as every other execution command; what is
// specific here is that watch is a CONVERSATION — the queue in exec_test.go
// answers successive polls differently, which is the only way to assert that a
// watch streams, advances its cursor, and ends for the right reason.

// watchReply builds one long-poll answer.
func watchReply(cursor int64, run map[string]any, transitions ...map[string]any) map[string]any {
	return map[string]any{
		"itemId": run["itemId"], "cursor": cursor,
		"transitions": transitions, "run": run,
	}
}

func watchRunning(itemID, phase string) map[string]any {
	return map[string]any{
		"itemId": itemID, "workflowId": "flow", "state": "running",
		"phaseId": phase, "resting": false,
	}
}

func watchParked(itemID, phase, reason, repair string) map[string]any {
	return map[string]any{
		"itemId": itemID, "workflowId": "flow", "state": "needs-human",
		"reason": reason, "phaseId": phase, "resting": true, "repair": repair,
	}
}

func watchTransitionOf(itemID, phase string, attempt int, from, to, reason string) map[string]any {
	return map[string]any{
		"seq": 1, "at": 1754700000000, "itemId": itemID, "phaseId": phase,
		"attempt": attempt, "from": from, "to": to, "reason": reason,
	}
}

// The verb's whole point: transitions print AS THEY HAPPEN across several
// server-side holds, the cursor the app returned is what the next call carries,
// and the watch ends the moment the run rests — carrying the app's own repair
// sentence, because a supervisor reading the last line needs the next verb.
func TestRunWatchStreamsTransitionsAcrossPollsAndEndsOnTheRestingRun(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun",
		fakeReply{result: watchReply(4, watchRunning("run-1", "survey"))},
		fakeReply{result: watchReply(7, watchRunning("run-1", "review"),
			watchTransitionOf("run-1", "survey", 1, "running", "running", ""))},
		fakeReply{result: watchReply(9,
			watchParked("run-1", "review", "stuck", "run resume run-1 once the blocker is cleared"),
			map[string]any{
				"seq": 9, "at": 1754700005000, "itemId": "run-1", "phaseId": "review",
				"attempt": 2, "from": "running", "to": "needs-human",
				"reason": "stuck", "cause": "the branch would not cut", "resting": true,
			})},
	)

	code, stdout, stderr := runCLI([]string{"run", "watch", "run-1"}, backend.env())
	if code != exitFindings {
		t.Fatalf("exit = %d (%s), want %d for a run that rested somewhere other than done",
			code, stderr, exitFindings)
	}
	lines := nonEmptyLines(stdout)
	if len(lines) != 5 {
		t.Fatalf("watch printed %d lines, want the opening line, two transitions, and the two-line summary:\n%s",
			len(lines), stdout)
	}
	if !strings.HasPrefix(lines[0], "watching run=run-1") {
		t.Fatalf("first line = %q, want it to name what is being watched", lines[0])
	}
	if !strings.Contains(lines[1], "phase=survey#1") || !strings.Contains(lines[1], "running->running") {
		t.Fatalf("transition line = %q", lines[1])
	}
	for _, want := range []string{"phase=review#2", "running->needs-human", "reason=stuck", "cause="} {
		if !strings.Contains(lines[2], want) {
			t.Fatalf("resting transition line = %q, want %q", lines[2], want)
		}
	}
	if !strings.Contains(lines[3], "state=needs-human") || !strings.Contains(lines[4], "run resume run-1") {
		t.Fatalf("summary = %q, want the resting state and the app's repair sentence verbatim",
			strings.Join(lines[3:], "\n"))
	}

	// Every call after the first carries the cursor the previous one returned, so
	// nothing between two holds can be missed.
	calls := backend.recorded("WorkflowAgentWatchRun")
	if len(calls) != 3 {
		t.Fatalf("watch made %d calls, want one per hold", len(calls))
	}
	for index, want := range []int64{0, 4, 7} {
		if got := watchSent(t, calls[index]).Cursor; got != want {
			t.Fatalf("call %d cursor = %d, want %d", index, got, want)
		}
	}
	if watchSent(t, calls[0]).Tree {
		t.Fatal("watch asked for the tree without being told to")
	}
}

// A run that reaches `done` is the one outcome that exits 0: a watch's exit code
// is what a supervising script branches on.
func TestRunWatchExitsZeroOnADoneRun(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun", fakeReply{result: watchReply(3, map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "done", "resting": true,
	})})
	code, stdout, stderr := runCLI([]string{"run", "watch", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "state=done") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// `--tree` is the campaign case: the transitions a caller needs are its called
// runs', and the flag is what asks the app for them.
func TestRunWatchTreeAsksForDescendantsAndPrintsTheirTransitions(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun", fakeReply{result: watchReply(2,
		map[string]any{"itemId": "root", "workflowId": "flow", "state": "done", "resting": true},
		watchTransitionOf("wave-3", "build", 1, "running", "needs-human", "unit-failed"))})

	code, stdout, stderr := runCLI([]string{"run", "watch", "root", "--tree"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "run=wave-3") {
		t.Fatalf("stdout = %q, want the descendant's transition", stdout)
	}
	if !watchSent(t, backend.recorded("WorkflowAgentWatchRun")[0]).Tree {
		t.Fatal("--tree did not reach the app")
	}
}

// A timeout is a distinct verdict, not a failure and not a resting run: the
// caller is told the run is still going and given the verb to look again.
func TestRunWatchTimesOutWithItsOwnExitCode(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun",
		fakeReply{hold: true, result: watchReply(1, watchRunning("run-1", "survey"))})

	code, stdout, stderr := runCLI([]string{"run", "watch", "run-1", "--timeout", "60ms"}, backend.env())
	if code != exitWatchTimeout {
		t.Fatalf("exit = %d (%s), want %d", code, stderr, exitWatchTimeout)
	}
	if !strings.Contains(stdout, "watch timed out after 60ms") || !strings.Contains(stdout, "run status run-1") {
		t.Fatalf("stdout = %q, want the timeout to say what to do next", stdout)
	}
	// The CLI asked the app to hold for its remaining budget rather than sleeping
	// on its own: the whole point of the verb is that it does not poll.
	sent := watchSent(t, backend.recorded("WorkflowAgentWatchRun")[0])
	if sent.WaitMillis <= 0 || sent.WaitMillis > 61 {
		t.Fatalf("waitMillis = %d, want the caller's remaining budget", sent.WaitMillis)
	}
}

// The failure this verb exists to prevent: a monitor that stops without saying
// so. A dropped connection is retried once — a torn localhost hop is not a dead
// app — and a second drop ends the watch loudly, on a code no run state uses.
func TestRunWatchReportsADeadAppInsteadOfHanging(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun",
		fakeReply{result: watchReply(2, watchRunning("run-1", "survey"))},
		fakeReply{drop: true},
		fakeReply{drop: true},
	)
	code, stdout, stderr := runCLI([]string{"run", "watch", "run-1"}, backend.env())
	if code != exitWatchDisconnected {
		t.Fatalf("exit = %d (%s), want %d", code, stderr, exitWatchDisconnected)
	}
	if !strings.Contains(stdout, "watch ended:") || !strings.Contains(stdout, "the run itself is unaffected") {
		t.Fatalf("stdout = %q, want the watch to say why it stopped", stdout)
	}
	if !strings.Contains(stdout, "cursor=2") {
		t.Fatalf("stdout = %q, want the cursor the watch got to", stdout)
	}
	// The cause travels with the line: an exit code says which outcome, and the
	// transport error says why. Losing it would leave a disconnect undiagnosable.
	if !strings.Contains(stdout, "WorkflowAgentWatchRun") {
		t.Fatalf("stdout = %q, want the underlying transport failure named", stdout)
	}
	if len(backend.recorded("WorkflowAgentWatchRun")) != 3 {
		t.Fatalf("calls = %d, want the first poll plus one retried drop", len(backend.recorded("WorkflowAgentWatchRun")))
	}
}

// Every line a --json watch prints is parseable, the CLI's own events included:
// a consumer reads the stream line by line and cannot special-case prose.
func TestRunWatchJSONReportsADisconnectAsAnObject(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun", fakeReply{drop: true}, fakeReply{drop: true})
	code, stdout, _ := runCLI([]string{"run", "watch", "run-1", "--json"}, backend.env())
	if code != exitWatchDisconnected {
		t.Fatalf("exit = %d, want %d", code, exitWatchDisconnected)
	}
	lines := nonEmptyLines(stdout)
	if len(lines) != 1 {
		t.Fatalf("--json printed %d lines:\n%s", len(lines), stdout)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("disconnect line is not JSON: %v (%q)", err, lines[0])
	}
	if event["watch"] != "disconnected" || event["error"] == "" {
		t.Fatalf("event = %#v", event)
	}
}

// One torn connection is not a dead app: the retry re-establishes from the same
// cursor and the watch carries on to its real verdict.
func TestRunWatchRetriesASingleTornConnection(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun",
		fakeReply{drop: true},
		fakeReply{result: watchReply(5, map[string]any{
			"itemId": "run-1", "workflowId": "flow", "state": "done", "resting": true,
		})},
	)
	code, stdout, stderr := runCLI([]string{"run", "watch", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "state=done") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// A credential revoked mid-watch terminates it with the message that names the
// cause. It is NOT the disconnect code: the app answered, so the caller has a
// verdict rather than a gap.
func TestRunWatchEndsWhenTheTokenIsRevokedMidWatch(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun",
		fakeReply{result: watchReply(2, watchRunning("run-1", "survey"))},
		fakeReply{status: 401},
	)
	code, _, stderr := runCLI([]string{"run", "watch", "run-1"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d, want %d for a refusal the app expressed", code, exitError)
	}
	if !strings.Contains(stderr, "no longer valid") {
		t.Fatalf("stderr = %q", stderr)
	}
	// A refusal is never retried: the app is alive and said no.
	if calls := len(backend.recorded("WorkflowAgentWatchRun")); calls != 2 {
		t.Fatalf("calls = %d, want the refusal to end it immediately", calls)
	}
}

// A phase without the grant is refused by the transport, and the refusal reaches
// the caller naming what to add.
func TestRunWatchAndAmendCarryTheGrantRefusal(t *testing.T) {
	for _, testCase := range []struct {
		method string
		args   []string
	}{
		{"WorkflowAgentWatchRun", []string{"run", "watch", "run-1"}},
		{"WorkflowAgentAmendSeeds", []string{"run", "amend", "run-1", "--seed", "budget=4"}},
	} {
		backend := newFakeBackend(t)
		backend.refuse(testCase.method, transport.ErrCodeGrantRequired,
			`this phase was not granted "introspect"; add "introspect" to the phase's grants: to allow it`)
		code, stdout, stderr := runCLI(testCase.args, backend.env())
		if code != exitError {
			t.Fatalf("%v exit = %d", testCase.args, code)
		}
		if stdout != "" {
			t.Fatalf("%v printed a refusal to stdout: %q", testCase.args, stdout)
		}
		if !strings.Contains(stderr, `not granted "introspect"`) {
			t.Fatalf("%v stderr = %q", testCase.args, stderr)
		}
	}
}

// A gap is the app saying "I cannot tell you what happened between these two
// cursors". It must be visible: a watch that silently skipped transitions is a
// monitor lying about a run it is supposed to be reporting on.
func TestRunWatchPrintsAGapItCannotFillIn(t *testing.T) {
	backend := newFakeBackend(t)
	first := watchReply(4, watchRunning("run-1", "survey"))
	second := watchReply(2, map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "done", "resting": true,
	})
	second["gap"] = true
	backend.queue("WorkflowAgentWatchRun", fakeReply{result: first}, fakeReply{result: second})

	code, stdout, stderr := runCLI([]string{"run", "watch", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "watch gap: transitions between 4 and 2") {
		t.Fatalf("stdout = %q, want the gap stated", stdout)
	}
}

// --json is NDJSON: one line per transition, forwarding the app's own objects,
// and the run document as the last line. A stream cannot be one document, and a
// CLI-invented shape for it would be a second definition of the wire.
func TestRunWatchJSONIsNDJSONOfTheAppsOwnObjects(t *testing.T) {
	backend := newFakeBackend(t)
	backend.queue("WorkflowAgentWatchRun", fakeReply{result: watchReply(6,
		watchParked("run-1", "review", "question", "run answer run-1 <text>"),
		watchTransitionOf("run-1", "review", 1, "running", "needs-human", "question"))})

	code, stdout, stderr := runCLI([]string{"run", "watch", "run-1", "--json"}, backend.env())
	if code != exitFindings {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	lines := nonEmptyLines(stdout)
	if len(lines) != 2 {
		t.Fatalf("--json printed %d lines, want one transition and the run:\n%s", len(lines), stdout)
	}
	var transition map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &transition); err != nil {
		t.Fatalf("first line is not JSON: %v", err)
	}
	if transition["reason"] != "question" || transition["phaseId"] != "review" {
		t.Fatalf("transition = %#v, want the app's own object", transition)
	}
	var run map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &run); err != nil {
		t.Fatalf("last line is not JSON: %v", err)
	}
	if run["repair"] != "run answer run-1 <text>" {
		t.Fatalf("run = %#v, want the app's document verbatim", run)
	}
	// No human line leaked into the stream: a consumer parses every line.
	if strings.Contains(stdout, "watching run=") {
		t.Fatalf("--json printed a human line:\n%s", stdout)
	}
}

func TestRunWatchRejectsANegativeTimeout(t *testing.T) {
	backend := newFakeBackend(t)
	code, _, stderr := runCLI([]string{"run", "watch", "run-1", "--timeout", "-1s"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "--timeout cannot be negative") {
		t.Fatalf("stderr = %q", stderr)
	}
	if calls := backend.recorded("WorkflowAgentWatchRun"); len(calls) != 0 {
		t.Fatalf("a usage error still reached the app: %#v", calls)
	}
}

// `run amend` sends the seeds parsed exactly as `run start` parses them, and
// prints what changed, the run's whole seed object, and the app's own sentence
// about when the run will read it — the fact the verb exists to deliver.
func TestRunAmendSendsParsedSeedsAndPrintsWhenTheyApply(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentAmendSeeds", map[string]any{
		"itemId": "run-1", "names": []string{"fix-budget"},
		"seeds":  map[string]any{"fix-budget": 4, "label": "first"},
		"effect": "next-attempt",
		"appliesNote": "the next attempt this run starts renders the new values, " +
			"so `agent-overflow run resume run-1` reads them",
		"callerNote": "run-1 was called by root-1",
	})

	code, stdout, stderr := runCLI([]string{
		"run", "amend", "run-1", "--seed", "fix-budget=4", "--seed", "label=first",
	}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	for _, want := range []string{
		"run=run-1", "amended=fix-budget", "effect=next-attempt",
		"seed fix-budget=4", `seed label="first"`,
		"when: the next attempt", "note: run-1 was called by root-1",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}

	calls := backend.recorded("WorkflowAgentAmendSeeds")
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	var sent struct {
		ItemID string         `json:"itemId"`
		Seeds  map[string]any `json:"seeds"`
	}
	if err := json.Unmarshal(calls[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ItemID != "run-1" {
		t.Fatalf("sent = %#v", sent)
	}
	// The same parse `run start --seed` uses: a JSON value is a JSON value, and a
	// bare word is a string.
	if sent.Seeds["fix-budget"] != float64(4) {
		t.Fatalf("fix-budget = %#v, want the number 4", sent.Seeds["fix-budget"])
	}
	if sent.Seeds["label"] != "first" {
		t.Fatalf("label = %#v", sent.Seeds["label"])
	}
}

// An amendment naming nothing is a usage error, refused before the app is
// called: a verb whose whole content is its flags cannot be given none.
func TestRunAmendRefusesAnEmptyOrMalformedChange(t *testing.T) {
	backend := newFakeBackend(t)
	for _, args := range [][]string{
		{"run", "amend"},
		{"run", "amend", "run-1"},
		{"run", "amend", "run-1", "--seed", "novalue"},
		{"run", "amend", "run-1", "--seed", "k=1", "--seed", "k=2"},
		{"run", "amend", "run-1", "run-2", "--seed", "k=1"},
	} {
		code, _, stderr := runCLI(args, backend.env())
		if code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
		if stderr == "" {
			t.Fatalf("%v refused silently", args)
		}
	}
	if calls := backend.recorded("WorkflowAgentAmendSeeds"); len(calls) != 0 {
		t.Fatalf("a usage error still reached the app: %#v", calls)
	}
}

// The app's refusals — a running run, an undeclared key — reach the caller
// whole. They name the fix, and the CLI is not entitled to reword them.
func TestRunAmendForwardsTheAppsRefusal(t *testing.T) {
	backend := newFakeBackend(t)
	backend.refuse("WorkflowAgentAmendSeeds", transport.ErrCodeInternal,
		`amend seeds "run-1": "fixbudget" is not an input of workflow "flow"; it declares fix-budget, label`)
	code, stdout, stderr := runCLI([]string{
		"run", "amend", "run-1", "--seed", "fixbudget=4",
	}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "" {
		t.Fatalf("a refusal printed to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "it declares fix-budget, label") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunAmendJSONForwardsTheAppsDocument(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentAmendSeeds", map[string]any{
		"itemId": "run-1", "names": []string{"fix-budget"},
		"seeds": map[string]any{"fix-budget": 4}, "effect": "fresh-phase-entry",
	})
	code, stdout, stderr := runCLI([]string{
		"run", "amend", "run-1", "--seed", "fix-budget=4", "--json",
	}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("--json did not print the app's document: %v (%q)", err, stdout)
	}
	if document["effect"] != "fresh-phase-entry" {
		t.Fatalf("document = %#v", document)
	}
}

// watchSent decodes the one input a watch call carries.
func watchSent(t *testing.T, call transport.ClientFrame) struct {
	ItemID     string `json:"itemId"`
	Cursor     int64  `json:"cursor"`
	Tree       bool   `json:"tree"`
	WaitMillis int64  `json:"waitMillis"`
} {
	t.Helper()
	var sent struct {
		ItemID     string `json:"itemId"`
		Cursor     int64  `json:"cursor"`
		Tree       bool   `json:"tree"`
		WaitMillis int64  `json:"waitMillis"`
	}
	if len(call.Params) != 1 {
		t.Fatalf("watch call params = %#v, want exactly one input", call.Params)
	}
	if err := json.Unmarshal(call.Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	return sent
}

func nonEmptyLines(output string) []string {
	lines := make([]string, 0)
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
