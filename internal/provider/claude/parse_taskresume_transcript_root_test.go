package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// The §E6 transcript root: a resume rebinds the task LIFECYCLE onto the
// resuming tool's own call, but the agent's sidechain rows keep naming
// the ORIGINAL launch as their parent_tool_use_id. These tests pin the
// two facts the parser is responsible for carrying across that rebind —
// the `transcript_root_id` stamp and the resume prompt row — plus the
// negative case that a fresh launch produces neither.

func findRebindStart(t *testing.T, events []provider.ProviderEvent, itemID string) map[string]any {
	t.Helper()
	for _, evt := range events {
		if evt.Kind != provider.EventToolStart || evt.ItemID != itemID || evt.ItemType != "" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("rebind meta unmarshal: %v", err)
		}
		return meta
	}
	t.Fatalf("no meta-only EventToolStart for %s in %+v", itemID, events)
	return nil
}

// TestReplay_LocalAgentAsyncResume_StampsTranscriptRoot pins the stamp
// that names where a resumed agent's rows actually live. FAILS pre-fix:
// nothing carried the original launch id past the rebind, so triage read
// the carrier as the resumed round's transcript scope.
func TestReplay_LocalAgentAsyncResume_StampsTranscriptRoot(t *testing.T) {
	events := replayFixture(t, fixtureLocalAgentAsyncResume)

	const (
		originalLaunchID = "toolu_01EHRwHNH98jqRKdFVcpmLtH"
		carrierID        = "toolu_01HNzp6MQbMMcTmoY7Yy1wdw"
	)

	rebindMeta := findRebindStart(t, events, carrierID)
	if rebindMeta["transcript_root_id"] != originalLaunchID {
		t.Fatalf("rebind meta.transcript_root_id = %v, want %s (the original launch)",
			rebindMeta["transcript_root_id"], originalLaunchID)
	}
	// The two stamps answer different questions and must both survive:
	// resumes_tool_use_id is the PREVIOUS binding (a round-3 carrier
	// names round 2's), transcript_root_id is the launch every round's
	// rows are parented to.
	if rebindMeta["resumes_tool_use_id"] != originalLaunchID {
		t.Fatalf("rebind meta.resumes_tool_use_id = %v, want %s",
			rebindMeta["resumes_tool_use_id"], originalLaunchID)
	}

	// Round 1's own meta-only task_started is not a rebind and must stay
	// unadorned — a launch that stamped its own id as its transcript
	// root would make every launch look like a carrier.
	round1Meta := findRebindStart(t, events, originalLaunchID)
	if v, ok := round1Meta["transcript_root_id"]; ok {
		t.Fatalf("round-1 launch stamped transcript_root_id = %v", v)
	}
}

// TestReplay_LocalAgentAsyncResume_EmitsResumePrompt pins the row that
// says WHAT the model asked the resumed agent to do. The rebind
// `task_started` is the only envelope carrying it (`prompt` == the
// SendMessage `message` text). FAILS pre-fix: no row represented the
// resume message at all, so the resumed round opened with the agent's
// answer and no question.
func TestReplay_LocalAgentAsyncResume_EmitsResumePrompt(t *testing.T) {
	events := replayFixture(t, fixtureLocalAgentAsyncResume)

	const (
		carrierID  = "toolu_01HNzp6MQbMMcTmoY7Yy1wdw"
		wantPrompt = "Apply the reviewer-synthesized single-forward-pass rework to subagentGrouping.ts and report back."
	)

	prompts := filterKinds(events, provider.EventUserText)
	if len(prompts) != 1 {
		t.Fatalf("expected exactly 1 EventUserText (the resume prompt), got %d: %+v", len(prompts), prompts)
	}
	prompt := prompts[0]
	if want := provider.SubagentOpeningPromptItemID(carrierID); prompt.ItemID != want {
		t.Fatalf("resume prompt ItemID = %q, want %q (the CARRIER's scope, so it cannot collide with round 1's opening prompt)",
			prompt.ItemID, want)
	}
	if prompt.ParentToolUseID != carrierID {
		t.Fatalf("resume prompt ParentToolUseID = %q, want %q", prompt.ParentToolUseID, carrierID)
	}
	if prompt.Content != wantPrompt {
		t.Fatalf("resume prompt Content = %q, want %q", prompt.Content, wantPrompt)
	}
	if !prompt.ContentPresent || prompt.Role != "user" {
		t.Fatalf("resume prompt must be a present user row, got role=%q present=%v", prompt.Role, prompt.ContentPresent)
	}

	var meta map[string]any
	if err := json.Unmarshal(prompt.Meta, &meta); err != nil {
		t.Fatalf("resume prompt meta unmarshal: %v", err)
	}
	for key, want := range map[string]any{
		"wire_only":                   true,
		"subagent_resume_prompt":      true,
		"subagent_prompt_provisional": true,
		"resume_carrier_id":           carrierID,
		// The root rides the row so triage places it without a store
		// read — and without depending on the carrier's own row having
		// been written first.
		"transcript_root_id": "toolu_01EHRwHNH98jqRKdFVcpmLtH",
	} {
		if meta[key] != want {
			t.Fatalf("resume prompt meta[%q] = %v, want %v", key, meta[key], want)
		}
	}
	// It is NOT the agent's opening prompt: that row belongs to the
	// original launch and the two must stay distinguishable.
	if v, ok := meta[provider.MetaSubagentOpeningPromptKey]; ok {
		t.Fatalf("resume prompt must not be marked as the opening prompt, got %v", v)
	}
}

// A fresh launch emits neither stamp nor prompt row, however much its
// task_started envelope resembles a rebind's.
func TestParseTaskStarted_FreshLaunchEmitsNoRootStampOrResumePrompt(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-agent-fresh","name":"Agent","input":{"description":"Inline review"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-fresh-1","tool_use_id":"tool-agent-fresh","task_type":"local_agent","description":"Inline review","prompt":"do the thing"}`))
	if err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("fresh launch must emit exactly the meta-only EventToolStart, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("start meta unmarshal: %v", err)
	}
	if v, ok := meta["transcript_root_id"]; ok {
		t.Fatalf("fresh launch stamped transcript_root_id = %v", v)
	}
}

// An empty `prompt` on the rebind produces no row rather than an empty
// user bubble under the agent.
func TestParseTaskStarted_ResumeWithEmptyPromptEmitsNoRow(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"launch-1","name":"Agent","input":{"description":"review"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"launch-1","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("round-1 task_started: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"carrier-1","task_type":"local_agent","prompt":"   "}`))
	if err != nil {
		t.Fatalf("rebind task_started: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("a blank resume prompt must emit no row, got %d events: %+v", len(events), events)
	}
}

// The root map is write-once per task_id: a second and third resume must
// keep naming the ORIGINAL launch, never the carrier that preceded them.
func TestParseTaskStarted_TranscriptRootSurvivesRepeatedResumes(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"launch-1","name":"Agent","input":{"description":"review"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"launch-1","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("round-1 task_started: %v", err)
	}
	for _, carrier := range []string{"carrier-2", "carrier-3"} {
		events, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"`+carrier+`","task_type":"local_agent","prompt":"keep going"}`))
		if err != nil {
			t.Fatalf("%s task_started: %v", carrier, err)
		}
		meta := findRebindStart(t, events, carrier)
		if meta["transcript_root_id"] != "launch-1" {
			t.Fatalf("%s meta.transcript_root_id = %v, want launch-1 (the root never moves)", carrier, meta["transcript_root_id"])
		}
		// resumes_tool_use_id follows the CHAIN, which is exactly why it
		// cannot be read as the root: carrier-3 names carrier-2.
		if carrier == "carrier-3" && meta["resumes_tool_use_id"] != "carrier-2" {
			t.Fatalf("carrier-3 meta.resumes_tool_use_id = %v, want carrier-2 (the previous binding)", meta["resumes_tool_use_id"])
		}
		if got := parser.taskTranscriptRoot("agent-1"); got != "launch-1" {
			t.Fatalf("taskTranscriptRoot after %s = %q, want launch-1", carrier, got)
		}
	}
}

// The map is bounded like every other parser correlation map: a
// wholesale reset at the cap, and Close releases it.
func TestTaskTranscriptRootsAreBoundedAndReleased(t *testing.T) {
	parser := NewParser()
	for i := range parserTaskMapCap {
		parser.rememberTaskTranscriptRoot(fmt.Sprintf("task-%d", i), fmt.Sprintf("launch-%d", i))
	}
	if len(parser.taskTranscriptRoots) != parserTaskMapCap {
		t.Fatalf("map size before overflow = %d, want %d", len(parser.taskTranscriptRoots), parserTaskMapCap)
	}
	parser.rememberTaskTranscriptRoot("task-overflow", "launch-overflow")
	if len(parser.taskTranscriptRoots) != 1 {
		t.Fatalf("map size after overflow = %d, want 1 (wholesale reset)", len(parser.taskTranscriptRoots))
	}
	if got := parser.taskTranscriptRoot("task-overflow"); got != "launch-overflow" {
		t.Fatalf("taskTranscriptRoot after reset = %q, want launch-overflow", got)
	}
	parser.Close()
	if parser.taskTranscriptRoots != nil {
		t.Fatal("Close must release taskTranscriptRoots")
	}
}

// The reconnect edge, from a live thread (2026-09-22). The original launch
// toolu_0189m ran in a process that died, so this parser never saw it. The
// recovery carrier toolu_01HSTnk rebinds the task with no prior binding,
// toolu_011HU resumes it, the CLI then announces toolu_011HU again while
// the task is bound to it (the round parked and was woken; the envelope
// carried the waking shell's notification), and toolu_01Gv4d9h resumes
// it once more. The parser never learned the root, so no carrier may name
// one: FAILS pre-fix, where the re-announce recorded toolu_011HU as the
// task's root and toolu_01Gv4d9h and its resume prompt named that
// carrier.
func TestParseTaskStarted_ReannouncedCarrierBindingNamesNoRoot(t *testing.T) {
	const (
		task     = "aeb391c5ca059d78f"
		recovery = "toolu_01HSTnkfWfX3M54n5e4Bje3K"
		round2   = "toolu_011HUFrpGE5W7yVa9L6XoFzE"
		round3   = "toolu_01Gv4d9hMYaVLopAXfd26Vvm"
	)
	parser := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		return events
	}
	resume := func(carrier, prompt string) []provider.ProviderEvent {
		t.Helper()
		parse(`{"type":"assistant","message":{"id":"msg-` + carrier + `","role":"assistant","content":[{"type":"tool_use","id":"` + carrier + `","name":"SendMessage","input":{"to":"` + task + `","message":"` + prompt + `"}}]}}`)
		events := parse(`{"type":"system","subtype":"task_started","task_id":"` + task + `","tool_use_id":"` + carrier + `","description":"L3 fail-closed tenant session routing","subagent_type":"general-purpose","task_type":"local_agent","prompt":"` + prompt + `"}`)
		parse(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"` + carrier + `","type":"tool_result","content":[{"type":"text","text":"{\"success\":true,\"message\":\"resumed from transcript in the background with your message.\",\"resumedAgentId\":\"` + task + `\"}"}]}]},"parent_tool_use_id":null}`)
		return events
	}
	stop := func(bound, summary string) {
		t.Helper()
		parse(`{"type":"system","subtype":"task_updated","task_id":"` + task + `","patch":{"status":"completed","end_time":1790114536078}}`)
		parse(`{"type":"system","subtype":"task_notification","task_id":"` + task + `","tool_use_id":"` + bound + `","status":"completed","summary":"` + summary + `"}`)
	}

	resume(recovery, "Recovery: the app crashed and killed you mid-task.")
	stop(recovery, "Round 1 report")
	resume(round2, "L3 lead review of C5.")

	var reannounced []provider.ProviderEvent
	out := captureLog(t, func() {
		reannounced = parse(`{"type":"system","subtype":"task_started","task_id":"` + task + `","tool_use_id":"` + round2 + `","description":"L3 fail-closed tenant session routing","subagent_type":"general-purpose","task_type":"local_agent","is_backgrounded":true,"prompt":"<task-notification>\n<task-id>bflfd5w3i</task-id>\n<tool-use-id>toolu_01PCRUekAgYhLs7cCSLKdHga</tool-use-id>\n<status>completed</status>\n<summary>Background command \"gate\" completed (exit code 0)</summary>\n</task-notification>"}`)
	})
	if got := parser.taskTranscriptRoot(task); got != "" {
		t.Fatalf("taskTranscriptRoot after the re-announce = %q, want none (the bound call is a carrier)", got)
	}
	for _, evt := range reannounced {
		if evt.Kind == provider.EventUserText {
			t.Fatalf("the re-announce is unmodeled and writes no wake row, got %+v", evt)
		}
	}
	for _, want := range []string{"unmodeled wake shape", round2, task, testThread, "toolu_01PCRUekAgYhLs7cCSLKdHga"} {
		if !strings.Contains(out, want) {
			t.Fatalf("re-announce diagnostic %q does not name %q", out, want)
		}
	}
	stop(round2, "Round 2 report")

	events := resume(round3, "L3 round 3.")
	meta := findRebindStart(t, events, round3)
	if meta["resumes_tool_use_id"] != round2 {
		t.Fatalf("round-3 meta.resumes_tool_use_id = %v, want %s", meta["resumes_tool_use_id"], round2)
	}
	if v, ok := meta["transcript_root_id"]; ok {
		t.Fatalf("round-3 meta.transcript_root_id = %v, want none: the parser never saw the launch", v)
	}
	for _, evt := range events {
		if evt.Kind != provider.EventUserText {
			continue
		}
		var promptMeta map[string]any
		if err := json.Unmarshal(evt.Meta, &promptMeta); err != nil {
			t.Fatalf("resume prompt meta: %v", err)
		}
		if v, ok := promptMeta["transcript_root_id"]; ok {
			t.Fatalf("round-3 resume prompt names transcript_root_id = %v, want none", v)
		}
	}
}

// A re-announce without a notification prompt is not a wake candidate and
// logs nothing.
func TestParseTaskStarted_ReannounceWithoutNotificationIsSilent(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"launch-1","name":"Agent","input":{"description":"review"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	line := []byte(`{"type":"system","subtype":"task_started","task_id":"agent-1","tool_use_id":"launch-1","task_type":"local_agent","prompt":"review"}`)
	if _, err := parser.ParseLine(testThread, line); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	out := captureLog(t, func() {
		if _, err := parser.ParseLine(testThread, line); err != nil {
			t.Fatalf("repeated task_started: %v", err)
		}
	})
	if strings.Contains(out, "unmodeled wake shape") {
		t.Fatalf("a re-announce without a notification logged %q", out)
	}
	if got := parser.taskTranscriptRoot("agent-1"); got != "launch-1" {
		t.Fatalf("taskTranscriptRoot = %q, want launch-1 (the first binding)", got)
	}
}
