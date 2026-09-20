package threadtools

import (
	"strings"
	"testing"
)

func itemApp(name, threadID, itemID, kind, body string) *fakeApp {
	app := newFakeApp(name)
	app.addThread(Thread{ID: threadID, Title: "Log"})
	app.addItems(threadID, Item{ID: itemID, Position: 1, Kind: kind, Role: "tool", TurnID: "t1", Name: "Bash", Size: int64(len(body))})
	app.addPayload(threadID, itemID, kind, []byte(body))
	return app
}

// TestItemReadsAByteRangeAndSaysWhereToContinue.
func TestItemReadsAByteRangeAndSaysWhereToContinue(t *testing.T) {
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", strings.Repeat("abcdefghij", 100))
	result := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","offset":10,"max_bytes":20}`)

	if result["text"] != "abcdefghijabcdefghij" {
		t.Fatalf("text = %v", result["text"])
	}
	if result["offset"] != float64(10) || result["bytes"] != float64(20) || result["size"] != float64(1000) {
		t.Fatalf("range metadata is wrong: %v", result)
	}
	if result["eof"] != false {
		t.Error("eof should be false in the middle of an item")
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "Continue at offset 30.") {
		t.Errorf("note = %q", note)
	}
	if result["kind"] != "tool_output" {
		t.Errorf("kind = %v", result["kind"])
	}
}

// TestItemNegativeOffsetReadsFromTheEnd, which is how an agent looks at
// the tail of a long command output.
func TestItemNegativeOffsetReadsFromTheEnd(t *testing.T) {
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", "0123456789")
	result := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","offset":-4}`)
	if result["text"] != "6789" || result["offset"] != float64(6) || result["eof"] != true {
		t.Fatalf("tail read = %v", result)
	}

	// A negative offset larger than the item clamps to the start.
	clamped := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","offset":-999}`)
	if clamped["text"] != "0123456789" || clamped["offset"] != float64(0) {
		t.Fatalf("clamped read = %v", clamped)
	}
}

// TestItemWidensARangeToWholeCharacters: a range that would split a UTF-8
// character is widened outward, because half a character is not text.
func TestItemWidensARangeToWholeCharacters(t *testing.T) {
	body := strings.Repeat("é", 10) // two bytes each
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", body)
	result := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","offset":1,"max_bytes":2}`)

	if result["text"] != "éé" {
		t.Fatalf("text = %q, want the two whole characters the range touched", result["text"])
	}
	if result["offset"] != float64(0) || result["bytes"] != float64(4) {
		t.Fatalf("the widened range is not reported: %v", result)
	}
	if !strings.HasPrefix(result["text"].(string), "é") {
		t.Error("the read starts inside a character")
	}
}

// TestItemOffsetPastTheEndIsAnEmptyRead, not a refusal: the answer the
// agent needs is the size.
func TestItemOffsetPastTheEndIsAnEmptyRead(t *testing.T) {
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", "0123456789")
	result := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","offset":5000}`)
	if _, present := result["text"]; present {
		t.Errorf("a past-the-end read returned text: %v", result["text"])
	}
	if result["size"] != float64(10) || result["eof"] != true || result["offset"] != float64(10) {
		t.Fatalf("past-the-end read = %v", result)
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "past the end") {
		t.Errorf("note = %q", note)
	}
}

// TestItemReadsALineRange in every spelling the schema documents.
func TestItemReadsALineRange(t *testing.T) {
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", "one\ntwo\nthree\nfour\n")
	server := New(app)

	span := call(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","lines":"2-3"}`)
	if span["text"] != "two\nthree\n" || span["first_line"] != float64(2) || span["last_line"] != float64(3) {
		t.Fatalf("lines 2-3 = %v", span)
	}
	one := call(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","lines":"4"}`)
	if one["text"] != "four\n" {
		t.Fatalf("lines 4 = %v", one["text"])
	}
	toEnd := call(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","lines":"3-"}`)
	if toEnd["text"] != "three\nfour\n" || toEnd["eof"] != true {
		t.Fatalf("lines 3- = %v", toEnd)
	}
	past := call(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","lines":"9-10"}`)
	if past["text"] != nil {
		t.Fatalf("lines past the end returned %v", past["text"])
	}
	if note, _ := past["note"].(string); !strings.Contains(note, "fewer than 9 lines") {
		t.Errorf("note = %q", note)
	}
	for _, spec := range []string{"0-2", "3-1", "x", "-4"} {
		callErr(t, server, localCaller(), "thread_item", mustJSON(t, map[string]any{"thread_id": localThreadID, "item_id": "i3", "lines": spec}), CodeInvalidRequest)
	}
}

// TestItemLineRangeStopsOnTheByteBudget and says how to continue.
func TestItemLineRangeStopsOnTheByteBudget(t *testing.T) {
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", strings.Repeat("a line of output\n", 500))
	result := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","lines":"1-500","max_bytes":1024}`)
	if length := len(result["text"].(string)); length > 1024+17 {
		t.Fatalf("the line range returned %d bytes, over the budget", length)
	}
	if result["eof"] != false {
		t.Error("a clipped line range is not eof")
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "stopped early") {
		t.Errorf("note = %q", note)
	}
}

// TestItemQueryReportsOffsetsWithContext.
func TestItemQueryReportsOffsetsWithContext(t *testing.T) {
	body := strings.Repeat("filler ", 40) + "PANIC: nil map\n" + strings.Repeat("filler ", 40) + "PANIC: nil map\n"
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", body)
	result := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","query":"PANIC"}`)

	matches := rows(t, result["matches"])
	if len(matches) != 2 || result["match_count"] != float64(2) {
		t.Fatalf("matches = %v", result["matches"])
	}
	first := matches[0].(map[string]any)
	if first["offset"] != float64(strings.Index(body, "PANIC")) {
		t.Errorf("offset = %v, want %d", first["offset"], strings.Index(body, "PANIC"))
	}
	if context, _ := first["context"].(string); !strings.Contains(context, "PANIC: nil map") {
		t.Errorf("context = %q", context)
	}
	if result["more"] != nil {
		t.Errorf("more = %v, want absent", result["more"])
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "Read a match with offset") {
		t.Errorf("note = %q", note)
	}

	miss := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","query":"nowhere"}`)
	if miss["match_count"] != nil {
		t.Errorf("match_count = %v, want absent for no matches", miss["match_count"])
	}
	if note, _ := miss["note"].(string); !strings.Contains(note, "literal and case-sensitive") {
		t.Errorf("note = %q", note)
	}
}

// TestItemQueryCapsMatchesAndPagesTheRest.
func TestItemQueryCapsMatchesAndPagesTheRest(t *testing.T) {
	const total = 60
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", strings.Repeat("xx needle yy\n", total))
	server := New(app)

	first := call(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","query":"needle"}`)
	if got := len(rows(t, first["matches"])); got != MaxItemMatches {
		t.Fatalf("first page has %d matches, want the %d cap", got, MaxItemMatches)
	}
	if first["more"] != true || first["cursor"] == nil {
		t.Fatalf("a capped page must offer a cursor: %v", first)
	}

	second := call(t, server, localCaller(), "thread_item", mustJSON(t, map[string]any{"thread_id": localThreadID, "item_id": "i3", "cursor": first["cursor"]}))
	if got := len(rows(t, second["matches"])); got != total-MaxItemMatches {
		t.Fatalf("second page has %d matches, want %d", got, total-MaxItemMatches)
	}
	if second["more"] != nil || second["query"] != "needle" {
		t.Fatalf("the last page is not final or lost the query: %v", second)
	}
	firstOffset := field(t, rows(t, second["matches"])[0], "offset").(float64)
	lastOfFirst := field(t, rows(t, first["matches"])[MaxItemMatches-1], "offset").(float64)
	if firstOffset <= lastOfFirst {
		t.Fatalf("the second page repeats offsets: %v then %v", lastOfFirst, firstOffset)
	}
}

// TestItemTakesExactlyOneSelector, so a result is never a mix of two
// readings of the same payload.
// TestItemWithNoSelectorReadsFromTheStart: a bare item id is the first
// read an agent makes after thread_show points at an item, and for most
// items it is the whole item.
func TestItemWithNoSelectorReadsFromTheStart(t *testing.T) {
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", "body")
	result := call(t, New(app), localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3"}`)
	if result["text"] != "body" || result["eof"] != true {
		t.Fatalf("result = %v", result)
	}
	if result["offset"] != float64(0) {
		t.Fatalf("offset = %v, want 0", result["offset"])
	}
}

func TestItemTakesAtMostOneSelector(t *testing.T) {
	app := itemApp("Laptop", localThreadID, "i3", "tool_output", "body")
	server := New(app)
	message := callErr(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","offset":0,"lines":"1-2"}`, CodeInvalidRequest)
	if !strings.Contains(message, "at most one selector") {
		t.Errorf("message = %q", message)
	}
	callErr(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","query":"a","lines":"1-2"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","offset":0}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"i3","offset":0,"max_bytes":99999999}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_item", `{"thread_id":"`+localThreadID+`","item_id":"nope","offset":0}`, CodeNotFound)
}

// TestItemOnAnotherComputerKeepsAZeroOffsetSelector: offset 0 is a real
// selector and its absence is another, so it has to survive forwarding.
func TestItemOnAnotherComputerKeepsAZeroOffsetSelector(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Log"})
	p.remote.addItems(remoteThreadID, Item{ID: "i3", Position: 1, Kind: "tool_output", Role: "tool", TurnID: "t1", Size: 6})
	p.remote.addPayload(remoteThreadID, "i3", "tool_output", []byte("remote"))

	result := call(t, p.server, localCaller(), "thread_item", `{"thread_id":"`+remoteThreadID+`","item_id":"i3","offset":0,"max_bytes":3}`)
	if result["text"] != "rem" {
		t.Fatalf("text = %v", result["text"])
	}
	if result["computer_id"] != "studio" || result["computer"] != "Studio" {
		t.Fatalf("the result does not name the computer it came from: %v", result)
	}
	if len(p.remote.reads) == 0 {
		t.Fatal("the destination never read its own payload")
	}
}
