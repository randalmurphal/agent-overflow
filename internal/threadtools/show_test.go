package threadtools

import (
	"fmt"
	"strings"
	"testing"
)

func transcriptThread(app *fakeApp, threadID string) {
	app.addThread(Thread{ID: threadID, Title: "Parser work", Provider: "claude", Model: "opus-5"})
	app.addItems(threadID,
		Item{ID: "i1", Position: 1, Kind: "user_text", Role: "user", TurnID: "t1", Text: "run the tests", Size: 13},
		Item{ID: "i2", Position: 2, Kind: "assistant_text", Role: "assistant", TurnID: "t1", Text: "running them now", Size: 16},
		Item{ID: "i3", Position: 3, Kind: "tool_call", Role: "tool", TurnID: "t1", Name: "Bash", Size: 2_400_000},
		Item{ID: "i4", Position: 4, Kind: "user_text", Role: "user", TurnID: "t2", Text: "what failed?", Size: 12},
		Item{ID: "i5", Position: 5, Kind: "assistant_text", Role: "assistant", TurnID: "t2", Text: "two parser cases", Size: 16},
	)
}

// TestShowRendersRolesItemIdsAndTurnDelimiters: the transcript is plain
// text a model reads, so every row says who said it and which item it is,
// and turns are told apart.
func TestShowRendersRolesItemIdsAndTurnDelimiters(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`"}`)

	transcript, _ := result["transcript"].(string)
	for _, want := range []string{"--- turn t1 ---", "[user i1] run the tests", "[assistant i2] running them now", "--- turn t2 ---", "[user i4] what failed?"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript is missing %q:\n%s", want, transcript)
		}
	}
	if result["done"] != true {
		t.Errorf("done = %v, want true", result["done"])
	}
	if result["state"] != StateIdle || result["title"] != "Parser work" {
		t.Errorf("the result does not name the thread it read: %v", result)
	}
	if _, present := result["computer_id"]; present {
		t.Error("a single-computer result must carry no computer field")
	}
}

// TestShowFoldsARunOfHiddenRowsIntoOneLine: by default a run of tool
// calls and thinking inside one turn is one line that counts the calls by
// name and states what it left out, so a busy turn costs a line, not a
// line per call.
func TestShowFoldsARunOfHiddenRowsIntoOneLine(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Busy"})
	app.addItems(localThreadID,
		Item{ID: "u1", Position: 1, Kind: "user_text", Role: "user", TurnID: "t1", Text: "fix it", Size: 6},
		Item{ID: "b1", Position: 2, Kind: "tool_call", Role: "tool", TurnID: "t1", Name: "Bash", Label: "go test ./...", Size: 2048},
		Item{ID: "k1", Position: 3, Kind: "thinking", Role: "assistant", TurnID: "t1", Size: 1024},
		Item{ID: "r1", Position: 4, Kind: "tool_call", Role: "tool", TurnID: "t1", Name: "Read", Size: 1024},
		Item{ID: "b2", Position: 5, Kind: "tool_call", Role: "tool", TurnID: "t1", Name: "Bash", Size: 1024},
		Item{ID: "d1", Position: 6, Kind: "diff", Role: "tool", TurnID: "t1", Name: "Edit", Size: 1024},
		Item{ID: "a1", Position: 7, Kind: "assistant_text", Role: "assistant", TurnID: "t1", Text: "found it", Size: 8},
		Item{ID: "b3", Position: 8, Kind: "tool_call", Role: "tool", TurnID: "t1", Name: "Bash", Size: 512},
		// A run never crosses a turn: the next turn's call is its own line.
		Item{ID: "b4", Position: 9, Kind: "tool_call", Role: "tool", TurnID: "t2", Name: "Bash", Size: 512},
		Item{ID: "a2", Position: 10, Kind: "assistant_text", Role: "assistant", TurnID: "t2", Text: "done", Size: 4},
	)
	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`"}`)

	want := strings.Join([]string{
		"--- turn t1 ---",
		"[user u1] fix it",
		"(4 tool calls: Bash 2, Edit 1, Read 1; 1 thinking; 6.0 KB not shown)",
		"[assistant a1] found it",
		"(1 tool call: Bash 1; 512 B not shown)",
		"--- turn t2 ---",
		"(1 tool call: Bash 1; 512 B not shown)",
		"[assistant a2] done",
	}, "\n")
	if got := result["transcript"].(string); got != want {
		t.Fatalf("transcript:\n%s\nwant:\n%s", got, want)
	}
	if result["items"] != float64(10) || result["done"] != true {
		t.Errorf("items = %v done = %v, want every row counted", result["items"], result["done"])
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "include tool_calls") {
		t.Errorf("a folded page must say how to unfold it: %q", note)
	}
}

// TestShowListsEachToolCallWithIncludeToolCalls: tool_calls turns the folded
// run back into a line per call, naming its summary and size with no body,
// while thinking the read did not ask for still folds.
func TestShowListsEachToolCallWithIncludeToolCalls(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	app.addItems(localThreadID, Item{ID: "k9", Position: 6, Kind: "thinking", Role: "assistant", TurnID: "t2", Size: 2048})
	app.items[localThreadID][2].Label = "go test\n  ./internal/..."

	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","include":["tool_calls"]}`)
	transcript := result["transcript"].(string)
	if want := "[tool i3] Bash: go test ./internal/... (2.3 MB)"; !strings.Contains(transcript, want) {
		t.Fatalf("transcript is missing the call line %q:\n%s", want, transcript)
	}
	if !strings.Contains(transcript, "(1 thinking; 2.0 KB not shown)") {
		t.Errorf("thinking the read left out must still fold:\n%s", transcript)
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "thread_item") {
		t.Errorf("a listed call must say how to read its body: %q", note)
	}

	// tool_outputs puts the body under its call line.
	app.items[localThreadID][2].Text = "ok  parser"
	app.items[localThreadID][2].Size = 10
	withOutputs := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","include":["tool_outputs"]}`)
	if want := "[tool i3] Bash: go test ./internal/...\nok  parser"; !strings.Contains(withOutputs["transcript"].(string), want) {
		t.Fatalf("tool output did not follow its call line:\n%s", withOutputs["transcript"])
	}
}

// TestShowTailKeepsTheNewestRowsAndPagesBackwards: a tail window that does
// not fit ends at the thread's end and drops its oldest rows, and the
// cursor reads the rows before the page until the window's start, each row
// exactly once, pinned to the snapshot the first page took.
func TestShowTailKeepsTheNewestRowsAndPagesBackwards(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Long"})
	position := int64(0)
	for turn := 1; turn <= 18; turn++ {
		for _, kind := range []string{"user_text", "tool_call", "tool_call", "assistant_text"} {
			position++
			item := Item{ID: fmt.Sprintf("r%02d", position), Position: position, Kind: kind, Role: "assistant", TurnID: fmt.Sprintf("t%02d", turn), Name: "Bash", Size: 700}
			if kind == "user_text" {
				item.Role = "user"
			}
			if kind != "tool_call" {
				item.Name, item.Text, item.Size = "", strings.Repeat("w", 300), 300
			}
			app.addItems(localThreadID, item)
		}
	}
	server := New(app)

	first := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","turns":6,"max_bytes":1024}`)
	transcript := first["transcript"].(string)
	if !strings.HasSuffix(transcript, "[assistant r72] "+strings.Repeat("w", 300)) {
		t.Fatalf("a tail page must end at the thread's newest row:\n%s", transcript)
	}
	if !strings.Contains(transcript, "--- turn t18 ---\n[user r69]") || first["done"] != false || len(transcript) > 1024 {
		t.Fatalf("first tail page = %d bytes, done %v:\n%s", len(transcript), first["done"], transcript)
	}
	if note, _ := first["note"].(string); !strings.Contains(note, "rows before them") {
		t.Errorf("note = %q", note)
	}

	// A turn that lands after the first page never shows up in a later one.
	app.addItems(localThreadID, Item{ID: "late", Position: 999, Kind: "assistant_text", Role: "assistant", TurnID: "t19", Text: "LATE", Size: 4})

	pages := []string{transcript}
	seen := int(first["items"].(float64))
	page := first
	for range 40 {
		if page["done"] == true {
			break
		}
		// The repeated window parameters ride along unchanged, which is
		// the same read and not a refusal.
		page = call(t, server, localCaller(), "thread_show", mustJSON(t, map[string]any{
			"thread_id": localThreadID, "cursor": page["cursor"], "turns": 6, "window": "tail", "max_bytes": 1024,
		}))
		text := page["transcript"].(string)
		if strings.Contains(text, "LATE") {
			t.Fatal("a row that settled after the cursor was minted appeared in a later page")
		}
		if len(text) > 1024 {
			t.Fatalf("a later page is %d bytes, over the budget", len(text))
		}
		pages = append([]string{text}, pages...)
		seen += int(page["items"].(float64))
	}
	if page["done"] != true {
		t.Fatal("paging never finished")
	}
	if seen != 24 {
		t.Fatalf("paged %d rows, want the 24 of the last six turns", seen)
	}
	whole := strings.Join(pages, "\n")
	for turn := 13; turn <= 18; turn++ {
		if count := strings.Count(whole, fmt.Sprintf("[user r%02d]", (turn-1)*4+1)); count != 1 {
			t.Errorf("turn %d's user row appears %d times across the pages", turn, count)
		}
	}
	if strings.Contains(whole, "[user r45]") {
		t.Error("the pages read past the start of the six-turn window")
	}
	if at := strings.Index(whole, "[user r49]"); at < 0 || at > strings.Index(whole, "[user r69]") {
		t.Error("the pages, oldest first, are not in timeline order")
	}
}

// TestShowKeepsProseWholeUpToThePageBudget: what people said is clipped
// only when it alone outgrows the page, while an included tool output is
// still held to a share of it.
func TestShowKeepsProseWholeUpToThePageBudget(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Long answer"})
	app.addItems(localThreadID,
		Item{ID: "out", Position: 1, Kind: "tool_call", Role: "tool", TurnID: "t1", Name: "Bash", Text: strings.Repeat("o", 30_000), Size: 30_000},
		Item{ID: "ans", Position: 2, Kind: "assistant_text", Role: "assistant", TurnID: "t1", Text: strings.Repeat("a", 30_000), Size: 30_000},
	)
	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","include":["tool_outputs"]}`)
	transcript := result["transcript"].(string)
	if !strings.Contains(transcript, "[assistant ans] "+strings.Repeat("a", 30_000)) {
		t.Error("a 30 KB answer was clipped on a 64 KB page")
	}
	if !strings.Contains(transcript, "clipped at 16.0 KB of 29.3 KB") || !strings.Contains(transcript, "item_id=out") {
		t.Errorf("an included tool output must still clip to its share:\n%s", transcript[:200])
	}
}

// TestShowLastAnswerReadsTheNewestAssistantMessage: what a thread
// concluded, in one call, whole, and said so when the thread is still at
// it or has not answered yet.
func TestShowLastAnswerReadsTheNewestAssistantMessage(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	app.addItems(localThreadID, Item{ID: "i6", Position: 6, Kind: "tool_call", Role: "tool", TurnID: "t2", Name: "Bash", Size: 100})
	server := New(app)

	result := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"last_answer"}`)
	if got := result["transcript"]; got != "--- turn t2 ---\n[assistant i5] two parser cases" {
		t.Fatalf("last_answer = %q", got)
	}
	if result["window"] != WindowLastAnswer || result["done"] != true {
		t.Errorf("result = %v", result)
	}
	if note, _ := result["note"].(string); note != "" {
		t.Errorf("an idle thread's answer needs no note: %q", note)
	}

	app.live[localThreadID] = LiveState{ActiveTurn: true}
	running := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"last_answer"}`)
	if note, _ := running["note"].(string); !strings.Contains(note, "still running") {
		t.Errorf("a running thread's answer must say it may not be final: %q", note)
	}

	app.addThread(Thread{ID: twinThreadID, Title: "Asked"})
	app.addItems(twinThreadID, Item{ID: "q", Position: 1, Kind: "user_text", Role: "user", TurnID: "t1", Text: "hello", Size: 5})
	none := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+twinThreadID+`","window":"last_answer"}`)
	if note, _ := none["note"].(string); !strings.Contains(note, "no assistant message yet") {
		t.Errorf("note = %q", note)
	}
}

// TestShowCursorRefusesOnlyAConflictingParameter: re-issuing the same read
// with the cursor added continues it, and a parameter that names another
// read is refused by name rather than silently winning or losing.
func TestShowCursorRefusesOnlyAConflictingParameter(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Long"})
	for index := 1; index <= 12; index++ {
		app.addItems(localThreadID, Item{
			ID: fmt.Sprintf("i%02d", index), Position: int64(index), Kind: "assistant_text", Role: "assistant",
			TurnID: fmt.Sprintf("t%02d", index), Text: strings.Repeat("y", 400), Size: 400,
		})
	}
	server := New(app)
	first := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"head","turns":8,"include":["thinking","diffs"],"max_bytes":1024}`)
	cursor := first["cursor"]
	same := map[string]any{"thread_id": localThreadID, "cursor": cursor, "window": "head", "turns": 8, "include": []string{"diffs", "thinking", "diffs"}, "max_bytes": 2048}
	next := call(t, server, localCaller(), "thread_show", mustJSON(t, same))
	if !strings.Contains(next["transcript"].(string), "[assistant i03]") {
		t.Fatalf("the repeated read did not continue the window:\n%s", next["transcript"])
	}
	for name, value := range map[string]any{
		"window": "tail", "turns": 7, "context": 1, "item_id": "i01", "since": "2026-01-01T00:00:00Z", "include": []string{"thinking"},
	} {
		args := map[string]any{"thread_id": localThreadID, "cursor": cursor, name: value}
		message := callErr(t, server, localCaller(), "thread_show", mustJSON(t, args), CodeInvalidRequest)
		if !strings.HasPrefix(message, name+" does not match") {
			t.Errorf("%s: refusal = %q", name, message)
		}
	}
}

// TestShowClipsOneLargeItemWithAPointer: the transcript never carries
// megabytes for one row, and the row says where the rest is.
func TestShowClipsOneLargeItemWithAPointer(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Big"})
	app.addItems(localThreadID, Item{ID: "big", Position: 1, Kind: "assistant_text", Role: "assistant", TurnID: "t1", Text: strings.Repeat("x", 50_000), Size: 50_000})

	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","max_bytes":8192}`)
	transcript := result["transcript"].(string)
	if !strings.Contains(transcript, "… clipped at ") || !strings.Contains(transcript, "item_id=big") {
		t.Fatalf("a clipped row must point at thread_item:\n%s", transcript[max(0, len(transcript)-200):])
	}
	if len(transcript) > 8192 {
		t.Fatalf("transcript is %d bytes, over the budget", len(transcript))
	}
}

// TestShowPagesByCursorAndIsPinnedToItsSnapshot: a page stops on the byte
// budget, the cursor continues the same window, and rows that streamed in
// after the cursor was minted never shift the page.
func TestShowPagesByCursorAndIsPinnedToItsSnapshot(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Long"})
	for index := 1; index <= 20; index++ {
		app.addItems(localThreadID, Item{
			ID: "i" + string(rune('a'+index-1)), Position: int64(index), Kind: "assistant_text",
			Role: "assistant", TurnID: "t1", Text: strings.Repeat("y", 400), Size: 400,
		})
	}
	server := New(app)

	first := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"all","max_bytes":1024}`)
	if first["done"] != false {
		t.Fatalf("the first page should stop short: %v", first["done"])
	}
	cursor, _ := first["cursor"].(string)
	if cursor == "" {
		t.Fatal("a short page must return a cursor")
	}
	firstItems := first["items"].(float64)

	// A row that settles after the cursor was minted must not appear.
	app.addItems(localThreadID, Item{ID: "late", Position: 99, Kind: "assistant_text", Role: "assistant", TurnID: "t9", Text: "LATE", Size: 4})

	seen := int(firstItems)
	page := first
	for range 40 {
		if page["done"] == true {
			break
		}
		page = call(t, server, localCaller(), "thread_show", mustJSON(t, map[string]any{"thread_id": localThreadID, "cursor": page["cursor"], "max_bytes": 1024}))
		seen += int(page["items"].(float64))
		if strings.Contains(page["transcript"].(string), "LATE") {
			t.Fatal("a row that settled after the cursor was minted appeared in a later page")
		}
	}
	if page["done"] != true {
		t.Fatal("paging never finished")
	}
	if seen != 20 {
		t.Fatalf("paged %d rows, want the 20 that existed when the window was taken", seen)
	}
}

// TestShowRefusesAWindowChangeOnACursor: the cursor already carries the
// window, so a second, contradictory window would silently win or lose.
func TestShowRefusesAWindowChangeOnACursor(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	server := New(app)
	encoded, err := encodeCursor(cursor{Kind: cursorShow, Thread: localThreadID, Window: WindowAll, From: 1, To: 5, High: 5})
	if err != nil {
		t.Fatal(err)
	}
	callErr(t, server, localCaller(), "thread_show", mustJSON(t, map[string]any{"thread_id": localThreadID, "cursor": encoded, "window": "head"}), CodeInvalidRequest)
}

// TestShowRefusesAForeignCursor: an opaque cursor is not a place the model
// may page to on its own.
func TestShowRefusesAForeignCursor(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	server := New(app)
	callErr(t, server, localCaller(), "thread_show", mustJSON(t, map[string]any{"thread_id": localThreadID, "cursor": "not-a-cursor"}), CodeInvalidRequest)

	other, err := encodeCursor(cursor{Kind: cursorSearch, Offsets: map[string]int{"": 1}})
	if err != nil {
		t.Fatal(err)
	}
	callErr(t, server, localCaller(), "thread_show", mustJSON(t, map[string]any{"thread_id": localThreadID, "cursor": other}), CodeInvalidRequest)

	// A cursor this tool did mint still names one thread: its positions
	// mean nothing in another.
	transcriptThread(app, twinThreadID)
	mine, err := encodeCursor(cursor{Kind: cursorShow, Thread: localThreadID, Window: WindowAll, From: 1, To: 5, High: 5})
	if err != nil {
		t.Fatal(err)
	}
	message := callErr(t, server, localCaller(), "thread_show", mustJSON(t, map[string]any{"thread_id": twinThreadID, "cursor": mine}), CodeInvalidRequest)
	if !strings.Contains(message, "belongs to another thread") {
		t.Errorf("message = %q", message)
	}
}

// TestShowWindows: each window resolves to its own bounds and the result
// says which window it read.
func TestShowWindows(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	server := New(app)

	around := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"around","item_id":"i4","context":0}`)
	if around["window"] != WindowAround {
		t.Errorf("window = %v", around["window"])
	}
	if transcript := around["transcript"].(string); strings.Contains(transcript, "[user i1]") || !strings.Contains(transcript, "[user i4]") {
		t.Errorf("around i4 with no context read the wrong turns:\n%s", transcript)
	}

	head := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"head","turns":1}`)
	if transcript := head["transcript"].(string); strings.Contains(transcript, "[user i4]") {
		t.Errorf("head 1 read past the first turn:\n%s", transcript)
	}

	since := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"since","item_id":"i3"}`)
	if transcript := since["transcript"].(string); strings.Contains(transcript, "[tool i3]") || !strings.Contains(transcript, "[user i4]") {
		t.Errorf("since i3 should start after it:\n%s", transcript)
	}
}

// TestShowWindowArgumentsAreValidated names the missing anchor rather than
// reading the wrong part of a thread.
func TestShowWindowArgumentsAreValidated(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	server := New(app)
	callErr(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"around"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"since"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"sideways"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","max_bytes":10}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","include":["everything"]}`, CodeInvalidRequest)
}

// TestShowIncludeReachesOptionalContent: the default is what people said,
// and include adds the rest.
func TestShowIncludeReachesOptionalContent(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Thinking"})
	app.addItems(localThreadID,
		Item{ID: "i1", Position: 1, Kind: "assistant_text", Role: "assistant", TurnID: "t1", Text: "here goes", Size: 9},
		Item{ID: "i2", Position: 2, Kind: "thinking", Role: "assistant", TurnID: "t1", Text: "the tail of the reasoning", Size: 25},
	)
	server := New(app)

	plain := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`"}`)
	if strings.Contains(plain["transcript"].(string), "the tail of the reasoning") {
		t.Error("thinking was rendered without include")
	}
	withThinking := call(t, server, localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","include":["thinking"]}`)
	if !strings.Contains(withThinking["transcript"].(string), "the tail of the reasoning") {
		t.Error("include thinking did not render it")
	}
	if got := app.slices[len(app.slices)-1].Include; len(got) != 1 || got[0] != IncludeThinking {
		t.Errorf("include reached the app as %v", got)
	}
}

// TestShowToFileDelegatesToTheApp and returns a path the agent can read
// with its own tools.
func TestShowToFileDelegatesToTheApp(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	app.export = ExportFile{Path: "/data/thread-exports/x.txt", Size: 4096, SHA256: "abc"}

	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","window":"all","to_file":true,"include":["all"]}`)
	file := field(t, result["file"], "path")
	if file != "/data/thread-exports/x.txt" {
		t.Fatalf("file = %v", result["file"])
	}
	if _, present := result["transcript"]; present {
		t.Error("to_file must not also render inline")
	}
}

// TestShowOnAnotherComputerRunsThereAndComesBackStamped.
func TestShowOnAnotherComputerRunsThereAndComesBackStamped(t *testing.T) {
	p := newPair(t)
	transcriptThread(p.remote, remoteThreadID)

	result := call(t, p.server, localCaller(), "thread_show", `{"thread_id":"`+remoteThreadID+`"}`)
	if result["computer_id"] != "studio" || result["computer"] != "Studio" {
		t.Fatalf("the result does not name the computer it came from: %v", result)
	}
	if !strings.Contains(result["transcript"].(string), "[user i1] run the tests") {
		t.Fatalf("the peer's transcript did not come back: %v", result["transcript"])
	}
	if len(p.peer.queries) != 1 || p.peer.queries[0] != "thread_show" {
		t.Fatalf("peer queries = %v", p.peer.queries)
	}
	if len(p.local.slices) != 0 {
		t.Fatal("the caller's own store was read for a thread it does not hold")
	}
}

// TestShowForwardsAnExplicitZeroContext: context 0 is a window of its own,
// so it must reach the computer that renders it rather than fall back to
// that computer's default of one turn each side.
func TestShowForwardsAnExplicitZeroContext(t *testing.T) {
	p := newPair(t)
	transcriptThread(p.remote, remoteThreadID)

	result := call(t, p.server, localCaller(), "thread_show", `{"thread_id":"`+remoteThreadID+`","window":"around","item_id":"i4","context":0}`)
	if transcript := result["transcript"].(string); strings.Contains(transcript, "[user i1]") || !strings.Contains(transcript, "[user i4]") {
		t.Fatalf("around i4 with no context read other turns on the peer:\n%s", transcript)
	}
}

// TestComputerIdIsRefusedWithoutPairing: the schema does not carry it, so
// neither does the handler.
func TestComputerIdIsRefusedWithoutPairing(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	callErr(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`","computer_id":"studio"}`, CodeInvalidRequest)
}

// TestShowAsksForAbsoluteBounds: the window is resolved to real positions
// once, and every transcript request carries them, so no query has an open
// end that a store would have to interpret.
func TestShowAsksForAbsoluteBounds(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	server := New(app)

	for _, args := range []string{
		`{"thread_id":"` + localThreadID + `"}`,
		`{"thread_id":"` + localThreadID + `","window":"all"}`,
		`{"thread_id":"` + localThreadID + `","window":"head","turns":1}`,
		`{"thread_id":"` + localThreadID + `","window":"since","item_id":"i1"}`,
	} {
		call(t, server, localCaller(), "thread_show", args)
	}
	if len(app.slices) == 0 {
		t.Fatal("nothing was read")
	}
	for _, query := range app.slices {
		if query.From <= 0 || query.To <= 0 || query.From > query.To {
			t.Fatalf("transcript query %+v does not carry an absolute inclusive range", query)
		}
	}
}

// TestShowOnAThreadWithNoItemsSaysSo rather than reading an open range.
func TestShowOnAThreadWithNoItemsSaysSo(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Fresh"})

	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`"}`)
	if result["done"] != true || result["transcript"] != nil {
		t.Fatalf("result = %v", result)
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "no items") {
		t.Errorf("note = %q", note)
	}
	if len(app.slices) != 0 {
		t.Fatalf("an empty window still read the transcript: %+v", app.slices)
	}
}
