package threadtools

import (
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

// TestShowCollapsesAToolCallToOneLineWithItsSize and points at the tool
// that reads it, so a multi-megabyte output never lands in a transcript.
func TestShowCollapsesAToolCallToOneLineWithItsSize(t *testing.T) {
	app := newFakeApp("Laptop")
	transcriptThread(app, localThreadID)
	result := call(t, New(app), localCaller(), "thread_show", `{"thread_id":"`+localThreadID+`"}`)

	transcript := result["transcript"].(string)
	want := "[tool i3] Bash (2.3 MB, not shown; read it with thread_item thread_id=" + localThreadID + " item_id=i3)"
	if !strings.Contains(transcript, want) {
		t.Fatalf("transcript is missing the tool one-liner %q:\n%s", want, transcript)
	}
	if strings.Count(transcript, "\n[tool i3]") > 1 {
		t.Error("the tool call was rendered more than once")
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
