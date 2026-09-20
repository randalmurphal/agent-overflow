package threadtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestEveryToolNameDispatches, so a tool that is listed is a tool that
// runs.
func TestEveryToolNameDispatches(t *testing.T) {
	app := newFakeApp("Laptop")
	server := New(app)
	for _, name := range ToolNames {
		_, err := server.Call(context.Background(), localCaller(), name, json.RawMessage(`{}`))
		if err == nil {
			continue
		}
		if code, message := publicMessage(err); code == "" || strings.Contains(message, "There is no tool named") {
			t.Errorf("%s did not dispatch: %v", name, err)
		}
	}
}

// TestUnknownToolNamesTheOnesThatExist.
func TestUnknownToolNamesTheOnesThatExist(t *testing.T) {
	server := New(newFakeApp("Laptop"))
	message := callErr(t, server, localCaller(), "thread_delete", `{}`, CodeInvalidRequest)
	for _, name := range ToolNames {
		if !strings.Contains(message, name) {
			t.Fatalf("the refusal does not list %s: %q", name, message)
		}
	}
}

// TestACallWithoutACallingThreadIsRefused: every tool acts as the calling
// thread, so a call that cannot name one cannot act.
func TestACallWithoutACallingThreadIsRefused(t *testing.T) {
	server := New(newFakeApp("Laptop"))
	_, err := server.Call(context.Background(), Caller{ComputerID: "laptop"}, "thread_search", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a call with no calling thread was allowed")
	}
	if code, _ := publicMessage(err); code != CodeInvalidRequest {
		t.Fatalf("code = %q", code)
	}

	var empty *Server
	if _, err := empty.Call(context.Background(), localCaller(), "thread_search", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a server with no app answered a call")
	}
}

// TestShapeFollowsPairingWithoutRestartingAnything: the tool list and the
// instructions are computed per call from what the app reports now.
func TestShapeFollowsPairingWithoutRestartingAnything(t *testing.T) {
	app := newFakeApp("Laptop")
	app.hits = []Hit{hit(localThreadID, "Work", LiveState{})}
	server := New(app)

	solo := call(t, server, localCaller(), "thread_search", `{}`)
	if _, present := solo["computers"]; present {
		t.Fatal("an unpaired call answered in the paired shape")
	}
	app.computers = []Computer{{ID: "studio", Name: "Studio"}}
	app.peers["studio"] = brokenPeer{computer: Computer{ID: "studio", Name: "Studio"}, err: publicf(CodeUnreachable, "Studio is offline.")}
	paired := call(t, server, localCaller(), "thread_search", `{}`)
	if _, present := paired["computers"]; !present {
		t.Fatal("pairing a computer did not change the shape of the next call")
	}
}

// TestUnknownArgumentsAreRefusedByName, which is how a model learns the
// parameter it invented does not exist.
func TestUnknownArgumentsAreRefusedByName(t *testing.T) {
	server := New(newFakeApp("Laptop"))
	message := callErr(t, server, localCaller(), "thread_search", `{"sort":"recent"}`, CodeInvalidRequest)
	if !strings.Contains(message, "sort") {
		t.Errorf("the refusal does not name the field: %q", message)
	}
	callErr(t, server, localCaller(), "thread_search", `{"limit":"twenty"}`, CodeInvalidRequest)
	callErr(t, server, localCaller(), "thread_search", `not json`, CodeInvalidRequest)
}

// TestAFailingAppIsReportedNotSwallowed.
func TestAFailingAppIsReportedNotSwallowed(t *testing.T) {
	app := newFakeApp("Laptop")
	app.failWith = publicf(CodeNotFound, "the index is closed")
	_, err := New(app).Call(context.Background(), localCaller(), "thread_show", json.RawMessage(`{"thread_id":"`+localThreadID+`"}`))
	if err == nil {
		t.Fatal("a failing app produced a result")
	}
}

// TestAForwardedCallDoesNotFanOutAgain: a call another computer forwarded
// here runs on this computer alone. Two computers paired with each other
// would otherwise answer each other's fan-out until the bounds expired.
func TestAForwardedCallDoesNotFanOutAgain(t *testing.T) {
	app := newFakeApp("Laptop")
	app.hits = []Hit{hit(localThreadID, "Work", LiveState{})}
	app.computers = []Computer{{ID: "studio", Name: "Studio"}}
	app.peers["studio"] = brokenPeer{computer: Computer{ID: "studio", Name: "Studio"}, err: publicf(CodeUnreachable, "Studio is offline.")}
	server := New(app)

	for _, name := range []string{"thread_search", "thread_options"} {
		paired := call(t, server, localCaller(), name, `{}`)
		if _, present := paired["computers"]; !present {
			t.Fatalf("%s answered a local call in the solo shape", name)
		}
		forwarded, err := server.Call(WithForwarded(context.Background()), localCaller(), name, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("forwarded %s: %v", name, err)
		}
		data, err := json.Marshal(forwarded)
		if err != nil {
			t.Fatalf("forwarded %s: marshal result: %v", name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("forwarded %s: decode result: %v", name, err)
		}
		if _, present := decoded["computers"]; present {
			t.Fatalf("forwarded %s fanned out to another computer", name)
		}
		if _, present := decoded["errors"]; present {
			t.Fatalf("forwarded %s reached a peer: %v", name, decoded["errors"])
		}
	}
}

// TestARowCarryingOnlyAComputerIdIsStillNamed: a request receipt records
// the computer the work went to, not what this computer calls it, and the
// name is what the model reads back.
func TestARowCarryingOnlyAComputerIdIsStillNamed(t *testing.T) {
	c := &session{caller: localCaller(), computers: []Computer{{ID: "studio", Name: "Studio"}}}
	if id, name := c.stamp(Computer{ID: "studio"}); id != "studio" || name != "Studio" {
		t.Fatalf("stamp(id only) = %q, %q", id, name)
	}
	if id, name := c.stamp(Computer{}); id != "laptop" || name != "Laptop" {
		t.Fatalf("stamp(own) = %q, %q", id, name)
	}
	// A computer this one is no longer paired with keeps its id and has no
	// name to give, which is better than borrowing another computer's.
	if id, name := c.stamp(Computer{ID: "forgotten"}); id != "forgotten" || name != "" {
		t.Fatalf("stamp(unpaired) = %q, %q", id, name)
	}
}
