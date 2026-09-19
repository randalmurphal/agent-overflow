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
