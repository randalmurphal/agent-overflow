package agy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

func TestParseAgyTranscriptModel(t *testing.T) {
	cases := []struct {
		raw        string
		wantModel  string
		wantEffort string
	}{
		{"gemini-4-hydrogen-tiered-high", "gemini-4-hydrogen", "high"},
		{"gemini-4-neon-tiered-xhigh", "gemini-4-neon", "xhigh"},
		{"gemini-4-hydrogen-minimal", "gemini-4-hydrogen", "minimal"},
		{"claude-sonnet-4-6-medium", "claude-sonnet-4-6", "medium"},
		{"claude-opus-4-6-thinking-low", "claude-opus-4-6-thinking", "low"},
		{"gpt-oss-120b-medium", "gpt-oss-120b-medium", ""},
		{"gemini-4-neon", "gemini-4-neon", ""},
		{"  ", "", ""},
	}
	for _, tc := range cases {
		gotModel, gotEffort := ParseAgyTranscriptModel(tc.raw)
		if gotModel != tc.wantModel || gotEffort != tc.wantEffort {
			t.Errorf("ParseAgyTranscriptModel(%q) = (%q, %q), want (%q, %q)",
				tc.raw, gotModel, gotEffort, tc.wantModel, tc.wantEffort)
		}
	}
}

func writeJSONL(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func TestWatchSubagentLiveTranscriptEndToEnd(t *testing.T) {
	appDataDir := t.TempDir()
	parentID := "parent-sess-1"
	childID := "child-sess-1"
	parentLog := transcriptPathForSession(appDataDir, parentID)
	childLog := transcriptPathForSession(appDataDir, childID)

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 2 * time.Second
	flavor.subagentSettleGrace = 30 * time.Millisecond

	var mu sync.Mutex
	var events []provider.ProviderEvent
	emit := func(evt provider.ProviderEvent) {
		mu.Lock()
		events = append(events, evt)
		mu.Unlock()
	}

	rawIn := json.RawMessage(`{"Model":"flash","Role":"Note Reader","TypeName":"research","Prompt":"Read /tmp/note.txt"}`)
	started := flavor.NormalizeToolStart(&acp.ToolCallUpdate{
		ToolCallID: "tool-sub-1",
		Name:       "invoke_subagent",
		RawInput:   rawIn,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
			SessionID:  parentID,
			ThreadID:   "thread-1",
			TurnID:     "turn-1",
			ToolCallID: "tool-sub-1",
			RawInput:   rawIn,
			Started:    started,
			Emit:       emit,
		})
	}()

	// Write parent transcript spawn + child transcript steps incrementally.
	createdPayload, _ := json.Marshal(map[string]any{
		"step_index": 2,
		"source":     "MODEL",
		"type":       "GENERIC",
		"status":     "DONE",
		"content": "Created the following subagent:\n{\n" +
			`  "conversationId": "` + childID + `",` + "\n" +
			`  "logAbsoluteUri": "file://` + filepath.ToSlash(childLog) + `"` + "\n" +
			"}",
	})
	writeJSONL(t, parentLog,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"invoke_subagent","args":{"Model":"\"flash\"","Role":"\"Note Reader\"","TypeName":"\"research\"","Prompt":"\"Read /tmp/note.txt\""}}]}`,
		string(createdPayload),
	)

	time.Sleep(25 * time.Millisecond)

	// Child step 1: thinking + read_file tool call + model & tokens.
	writeJSONL(t, childLog,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T18:22:57Z","model":"gemini-4-hydrogen-tiered-high","input_tokens":5543,"cache_read_tokens":0,"output_tokens":269,"thinking":"Let me read note.txt","tool_calls":[{"name":"read_file","args":{"Path":"\"/tmp/note.txt\""}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-22T18:22:58Z","content":"1: SECRET_VALUE=42"}`,
	)

	time.Sleep(25 * time.Millisecond)

	// Child step 3: send_message + final response; parent receives SYSTEM_MESSAGE.
	writeJSONL(t, childLog,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T18:23:00Z","model":"gemini-4-hydrogen-tiered-high","input_tokens":2441,"cache_read_tokens":3494,"output_tokens":231,"tool_calls":[{"name":"send_message","args":{"Recipient":"\"parent-sess-1\"","Message":"\"SECRET_VALUE is 42.\""}}]}`,
		`{"step_index":4,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-22T18:23:01Z","content":"Message sent."}`,
		`{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T18:23:02Z","model":"gemini-4-hydrogen-tiered-high","input_tokens":100,"cache_read_tokens":3800,"output_tokens":50,"content":"Sent the result back to parent."}`,
	)
	writeJSONL(t, parentLog,
		`{"step_index":4,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-22T18:23:01Z sender=child-sess-1 priority=MESSAGE_PRIORITY_HIGH content=SECRET_VALUE is 42.\n</SYSTEM_MESSAGE>"}`,
	)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WatchSubagent did not finish in time")
	}

	mu.Lock()
	defer mu.Unlock()

	var (
		gotModel       string
		gotEffort      string
		gotTaskID      string
		gotThinking    bool
		gotChildRead   bool
		gotChildReadOK bool
		gotChildText   bool
		gotProgress    provider.SubagentProgressMeta
		gotNotif       *provider.ProviderEvent
	)
	for i := range events {
		evt := events[i]
		switch evt.Kind {
		case provider.EventToolStart:
			if evt.ItemID == "tool-sub-1" && evt.ParentToolUseID == "" {
				var m map[string]any
				_ = json.Unmarshal(evt.Meta, &m)
				if v, _ := m["subagent_model"].(string); v != "" {
					gotModel = v
				}
				if v, _ := m["run_effort"].(string); v != "" {
					gotEffort = v
				}
				if v, _ := m["task_id"].(string); v != "" {
					gotTaskID = v
				}
			} else if evt.ParentToolUseID == "tool-sub-1" && evt.ItemType == "Read" {
				gotChildRead = true
			}
		case provider.EventToolComplete:
			if evt.ParentToolUseID == "tool-sub-1" && evt.ItemType == "Read" && strings.Contains(evt.Content, "SECRET_VALUE=42") {
				gotChildReadOK = true
			}
		case provider.EventThinking:
			if evt.ParentToolUseID == "tool-sub-1" && strings.Contains(evt.Content, "read note.txt") {
				gotThinking = true
			}
		case provider.EventTextDelta:
			if evt.ParentToolUseID == "tool-sub-1" && strings.Contains(evt.Content, "Sent the result") {
				gotChildText = true
			}
		case provider.EventSubagentProgress:
			if evt.ItemID == "tool-sub-1" {
				_ = json.Unmarshal(evt.Meta, &gotProgress)
			}
		case provider.EventBackgroundTaskNotification:
			copyEvt := evt
			gotNotif = &copyEvt
		}
	}

	if gotTaskID != childID {
		t.Errorf("task_id = %q, want %q", gotTaskID, childID)
	}
	if gotModel != "gemini-4-hydrogen" || gotEffort != "high" {
		t.Errorf("subagent model/effort = (%q, %q), want (gemini-4-hydrogen, high)", gotModel, gotEffort)
	}
	if !gotThinking || !gotChildRead || !gotChildReadOK || !gotChildText {
		t.Errorf("child events missing: thinking=%v read=%v readComplete=%v text=%v",
			gotThinking, gotChildRead, gotChildReadOK, gotChildText)
	}
	// Fresh spend = (5543+269) + (2441+231) + (100+50) = 8634 (excludes cache_read_tokens!).
	const wantTokens int64 = (5543 + 269) + (2441 + 231) + (100 + 50)
	if gotProgress.TotalTokens != wantTokens {
		t.Errorf("TotalTokens = %d, want %d", gotProgress.TotalTokens, wantTokens)
	}
	if gotProgress.ToolUses != 2 {
		t.Errorf("ToolUses = %d, want 2", gotProgress.ToolUses)
	}
	if gotNotif == nil {
		t.Fatal("missing EventBackgroundTaskNotification")
	}
	if gotNotif.Content != "SECRET_VALUE is 42." {
		t.Errorf("notification report = %q, want %q", gotNotif.Content, "SECRET_VALUE is 42.")
	}
	var notifMeta struct {
		TaskID string                        `json:"task_id"`
		Status string                        `json:"status"`
		Usage  provider.SubagentProgressMeta `json:"usage"`
	}
	if err := json.Unmarshal(gotNotif.Meta, &notifMeta); err != nil {
		t.Fatalf("unmarshal notif meta: %v", err)
	}
	if notifMeta.TaskID != childID || notifMeta.Status != "completed" || notifMeta.Usage.TotalTokens != wantTokens {
		t.Errorf("notifMeta = %+v, want task=%s status=completed tokens=%d", notifMeta, childID, wantTokens)
	}
}

func TestWatchSubagentParallelAndResumedSession(t *testing.T) {
	appDataDir := t.TempDir()
	parentID := "parent-par"
	parentLog := transcriptPathForSession(appDataDir, parentID)

	// Historical subagent from an earlier turn before session resume.
	writeJSONL(t, parentLog,
		`{"step_index":1,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created the following subagent:\n{\"conversationId\":\"old-child-0\"}"}`,
	)

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 2 * time.Second
	flavor.subagentSettleGrace = 20 * time.Millisecond

	// BeforePrompt seeds pre-existing subagents so old-child-0 is skipped.
	flavor.BeforePrompt(context.Background(), parentID)

	// New turn spawns two parallel subagents.
	childA := "child-A"
	childB := "child-B"
	writeJSONL(t, parentLog,
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"invoke_subagent","args":{"Role":"\"Alpha\"","TypeName":"\"research\"","Prompt":"\"Task A\""}},{"name":"invoke_subagent","args":{"Role":"\"Beta\"","TypeName":"\"research\"","Prompt":"\"Task B\""}}]}`,
		`{"step_index":3,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created the following subagent:\n{\"conversationId\":\"child-A\"}"}`,
		`{"step_index":4,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created the following subagent:\n{\"conversationId\":\"child-B\"}"}`,
	)
	writeJSONL(t, transcriptPathForSession(appDataDir, childA),
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","model":"gemini-4-neon-tiered-high","input_tokens":100,"output_tokens":20,"content":"Alpha done"}`,
	)
	writeJSONL(t, transcriptPathForSession(appDataDir, childB),
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","model":"gemini-4-hydrogen-tiered-low","input_tokens":50,"output_tokens":10,"content":"Beta done"}`,
	)

	var mu sync.Mutex
	taskByLaunch := map[string]string{}
	recordEmit := func(evt provider.ProviderEvent) {
		if evt.Kind != provider.EventBackgroundTaskNotification {
			return
		}
		var m struct {
			TaskID string `json:"task_id"`
		}
		_ = json.Unmarshal(evt.Meta, &m)
		mu.Lock()
		taskByLaunch[evt.ItemID] = m.TaskID
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for _, item := range []struct {
		id   string
		role string
	}{
		{"launch-A", "Alpha"},
		{"launch-B", "Beta"},
	} {
		rawIn, _ := json.Marshal(map[string]string{"Role": item.role, "TypeName": "research"})
		wg.Add(1)
		go func(id string, in json.RawMessage) {
			defer wg.Done()
			flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
				SessionID:  parentID,
				ThreadID:   "t1",
				TurnID:     "turn-2",
				ToolCallID: id,
				RawInput:   in,
				Emit:       recordEmit,
			})
		}(item.id, rawIn)
	}
	wg.Wait()

	if taskByLaunch["launch-A"] != childA || taskByLaunch["launch-B"] != childB {
		t.Fatalf("parallel correlation = %+v, want launch-A->%s launch-B->%s", taskByLaunch, childA, childB)
	}
}

func TestWatchSubagentDiscoveryTimeoutSettlesAsFailed(t *testing.T) {
	flavor := NewFlavor(t.TempDir())
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 40 * time.Millisecond

	var gotStatus string
	flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
		SessionID:  "missing-session",
		ThreadID:   "t1",
		TurnID:     "turn-1",
		ToolCallID: "launch-timeout",
		Emit: func(evt provider.ProviderEvent) {
			if evt.Kind == provider.EventBackgroundTaskNotification && evt.ItemID == "launch-timeout" {
				var m struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(evt.Meta, &m)
				gotStatus = m.Status
			}
		},
	})
	if gotStatus != "failed" {
		t.Fatalf("discovery timeout status = %q, want failed", gotStatus)
	}
}

func TestSubagentPathContainmentAndClaimRoleMismatch(t *testing.T) {
	// Traversal conversationId is rejected outright.
	if isSafeConversationID("../other-conv") || isSafeConversationID("a/b") || isSafeConversationID("") {
		t.Fatal("expected unsafe conversation IDs to be rejected")
	}
	if convID, _ := parseCreatedSubagentResult(`{"conversationId":"../escape","logAbsoluteUri":"file:///etc/passwd"}`, t.TempDir()); convID != "" {
		t.Fatalf("unsafe conversationId accepted: %q", convID)
	}

	// Symlink escaping child brain dir (both file symlink and redirected child
	// brain/<id> directory symlink) is refused by openContainedTranscript.
	appDataDir := t.TempDir()
	brainDir := filepath.Join(appDataDir, "brain")
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.jsonl")
	if err := os.WriteFile(outsideFile, []byte(`{"step_index":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	childRoot := filepath.Join(brainDir, "child-sym")
	childLog := filepath.Join(childRoot, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(childLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, childLog); err == nil {
		var off int64
		var rem string
		if lines := readNewJSONLLinesContained(brainDir, "child-sym", childLog, &off, &rem); len(lines) != 0 {
			t.Fatalf("symlink file escape read lines: %v", lines)
		}
	}
	outsideLog := filepath.Join(outsideDir, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(outsideLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideLog, []byte(`{"step_index":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(brainDir, "child-dir-sym")); err == nil {
		var off int64
		var rem string
		redirectedLog := filepath.Join(brainDir, "child-dir-sym", ".system_generated", "logs", "transcript.jsonl")
		if lines := readNewJSONLLinesContained(brainDir, "child-dir-sym", redirectedLog, &off, &rem); len(lines) != 0 {
			t.Fatalf("redirected brain/<id> symlink escape read lines: %v", lines)
		}
	}

	// Parallel claim must not steal a child whose role OR prompt conflicts,
	// and an exact prompt match must beat an earlier shorter prefix match.
	coord := &parentSubagentCoordinator{
		discovered: []*discoveredSubagent{
			{conversationID: "child-prefix", role: "Alpha", typeName: "research", prompt: "Inspect"},
			{conversationID: "child-alpha", role: "Alpha", typeName: "research", prompt: "Inspect billing"},
		},
	}
	if stolen := coord.claim("Beta", "research", "Task B"); stolen != nil {
		t.Fatalf("watcher Beta stole child with conflicting role Alpha: %+v", stolen)
	}
	if stolenPrompt := coord.claim("Alpha", "research", "Different prompt"); stolenPrompt != nil {
		t.Fatalf("watcher with conflicting prompt stole child: %+v", stolenPrompt)
	}
	if matched := coord.claim("Alpha", "research", "Inspect billing"); matched == nil || matched.conversationID != "child-alpha" {
		t.Fatalf("exact prompt match lost to prefix match: got %+v, want child-alpha", matched)
	}

	// Unrelated GENERIC JSON mentioning conversationId without subagent signal
	// must be ignored even while an invoke_subagent launch is pending.
	if cid, _ := parseCreatedSubagentResult(`{"conversationId":"unrelated-conv-99","status":"ok"}`, appDataDir); cid != "" {
		t.Fatalf("unrelated GENERIC conversationId was admitted: %q", cid)
	}

	// GENERIC output with a conversationId when NO invoke_subagent is pending must not be admitted.
	parentLog := filepath.Join(brainDir, "parent-no-spawn", ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(parentLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parentLog, []byte(`{"step_index":1,"type":"GENERIC","content":"{\"conversationId\":\"other-conv-123\"}"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	noSpawnCoord := &parentSubagentCoordinator{
		path:       parentLog,
		seenConvID: map[string]bool{},
		reports:    map[string]string{},
	}
	noSpawnCoord.refresh(appDataDir)
	if len(noSpawnCoord.discovered) != 0 {
		t.Fatalf("GENERIC without pending invoke_subagent admitted foreign conversation: %+v", noSpawnCoord.discovered)
	}
}

func TestFlavorMultiReplaceGenerateImageAndBuiltins(t *testing.T) {
	f := NewFlavor("")

	// 1. multi_replace_file_content produces normalized input + fallback diff hunks.
	multiIn := json.RawMessage(`{
		"TargetFile": "/repo/app.go",
		"ReplacementChunks": [
			{"TargetContent": "oldA", "ReplacementContent": "newA"},
			{"TargetContent": "oldB", "ReplacementContent": "newB"}
		]
	}`)
	u := &acp.ToolCallUpdate{
		ToolCallID: "edit-multi",
		Name:       "multi_replace_file_content",
		Status:     "completed",
		RawInput:   multiIn,
	}
	st := f.NormalizeToolStart(u)
	if st.ToolName != "Edit" || st.ItemType != "file_change" {
		t.Fatalf("multi_replace start = %+v", st)
	}
	done := f.NormalizeToolComplete(u, st)
	if len(done.DiffChanges) != 1 || !strings.Contains(done.DiffChanges[0].Diff, "-oldA") || !strings.Contains(done.DiffChanges[0].Diff, "+newB") {
		t.Fatalf("multi_replace diff changes = %+v", done.DiffChanges)
	}

	// 2. generate_image extracts saved path from output text when input only had ImageName.
	imgU := &acp.ToolCallUpdate{
		ToolCallID: "gen-img-1",
		Name:       "generate_image",
		Status:     "completed",
		RawInput:   json.RawMessage(`{"Prompt":"A glowing nebula","ImageName":"nebula"}`),
		Content: []acp.ToolContentItem{{
			Type:    "content",
			Content: &acp.ContentBlock{Type: "text", Text: "Saved generated image to /home/u/.gemini/antigravity-cli/brain/s1/nebula.png"},
		}},
	}
	imgStart := f.NormalizeToolStart(imgU)
	imgDone := f.NormalizeToolComplete(imgU, imgStart)
	inputMap, _ := imgDone.ExtraMeta["input"].(map[string]any)
	if inputMap["path"] != "/home/u/.gemini/antigravity-cli/brain/s1/nebula.png" || inputMap["prompt"] != "A glowing nebula" {
		t.Fatalf("generate_image complete ExtraMeta[input] = %+v", inputMap)
	}

	// 3. Cancelled ACP tool maps to killed status (never completed).
	cancelDone := f.NormalizeToolComplete(&acp.ToolCallUpdate{
		ToolCallID: "bash-1",
		Name:       "run_command",
		Status:     "cancelled",
	}, acp.NormalizedToolStart{ToolName: "Bash", ItemType: "command_execution"})
	if cancelDone.ItemStatus != "killed" {
		t.Fatalf("cancelled ItemStatus = %q, want killed", cancelDone.ItemStatus)
	}

	// 4. Built-in tool summaries populate description cleanly.
	schedStart := f.NormalizeToolStart(&acp.ToolCallUpdate{
		ToolCallID: "sched-1",
		Name:       "schedule",
		RawInput:   json.RawMessage(`{"DurationSeconds":60,"Prompt":"Check CI status"}`),
	})
	if !strings.Contains(string(schedStart.Input), "60s Check CI status") {
		t.Fatalf("schedule normalized input = %s", string(schedStart.Input))
	}
}
