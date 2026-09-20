package threadtools

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

// TestPublicMessageNeverEchoesAnUnreviewedCause. A per-item error row is
// model-visible text, so an error that carries no reviewed prose becomes a
// fixed refusal and its own words stay in this computer's log.
func TestPublicMessageNeverEchoesAnUnreviewedCause(t *testing.T) {
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	app := newFakeApp("Laptop")
	app.failWith = errors.New("open /Users/someone/Library/agent-overflow/threads.sqlite: permission denied")

	result := call(t, New(app), localCaller(), "thread_update",
		`{"thread_ids":["`+localThreadID+`"],"archived":true}`)
	row := rows(t, result["results"])[0].(map[string]any)
	message, _ := row["error"].(string)
	if strings.Contains(message, "sqlite") || strings.Contains(message, "/Users/") {
		t.Fatalf("the raw cause reached the model: %q", message)
	}
	if message != "Thread tools could not complete that part of the call. The cause is in this computer's log." {
		t.Fatalf("error = %q", message)
	}
	if row["error_code"] != CodeInvalidRequest {
		t.Errorf("error_code = %v", row["error_code"])
	}
	if !strings.Contains(logged.String(), "permission denied") {
		t.Errorf("the cause was discarded rather than logged: %q", logged.String())
	}
}

// TestPublicMessageKeepsReviewedProse, which is the whole point of the
// public wrapper: a refusal written for the model comes through unchanged.
func TestPublicMessageKeepsReviewedProse(t *testing.T) {
	code, message := publicMessage(publicf(CodeNotFound, "No thread matches %q.", "abc123"))
	if code != CodeNotFound || message != `No thread matches "abc123".` {
		t.Fatalf("publicMessage = %q, %q", code, message)
	}
	if code, message := publicMessage(nil); code != "" || message != "" {
		t.Fatalf("publicMessage(nil) = %q, %q", code, message)
	}
}

// TestComputerListNamesAnUnnamedComputerByItsId, because the id is what
// the next call has to pass.
func TestComputerListNamesAnUnnamedComputerByItsId(t *testing.T) {
	list := computerList([]Computer{{ID: "studio", Name: "Studio"}, {ID: "shed"}})
	if list != "Studio and computer shed" {
		t.Fatalf("computerList = %q", list)
	}
	if got := NameOfComputer(Computer{}); got != "this computer" {
		t.Errorf("NameOfComputer of nothing = %q", got)
	}
}
