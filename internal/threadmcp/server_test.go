package threadmcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolCallEnvelopeAllowsMetadataButArgumentsStayStrict(t *testing.T) {
	call, err := DecodeToolCall(json.RawMessage(`{"name":"test","arguments":{"path":"file"},"_meta":{"progressToken":3,"callId":"call"},"extension":{"value":true}}`))
	if err != nil || call.Name != "test" {
		t.Fatalf("metadata-bearing call: %+v, %v", call, err)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := DecodeArgs(call.Arguments, &args); err != nil || args.Path != "file" {
		t.Fatalf("arguments: %+v, %v", args, err)
	}
	if err := DecodeArgs(json.RawMessage(`{"path":"file","_meta":{}}`), &args); err == nil {
		t.Fatal("metadata inside tool arguments bypassed the closed schema")
	}
	for _, invalid := range []string{`null`, `{}`, `[]`, `{"name":3}`, `{"name":"test"} {}`} {
		if _, err := DecodeToolCall(json.RawMessage(invalid)); err == nil {
			t.Errorf("accepted invalid envelope: %s", invalid)
		}
	}
}

func TestJSONContentTypeAllowsParametersAndCasing(t *testing.T) {
	for _, accepted := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON", " application/json "} {
		if !jsonContentType(accepted) {
			t.Errorf("jsonContentType(%q) = false", accepted)
		}
	}
	for _, refused := range []string{"", "text/plain", "text/plain;charset=UTF-8", "application/json-patch+json", "multipart/form-data"} {
		if jsonContentType(refused) {
			t.Errorf("jsonContentType(%q) = true", refused)
		}
	}
}

func TestCapabilitiesRotateAndCloseCannotReopen(t *testing.T) {
	server := New("test-tools", "", func(string) []map[string]any { return nil }, nil)
	t.Cleanup(func() { _ = server.Close() })
	first, err := server.RegisterThread("thread", "first")
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreadEnabled("thread", false)
	second, err := server.RegisterThread("thread", "second")
	if err != nil {
		t.Fatal(err)
	}
	if first["test-tools"].(map[string]any)["url"] == second["test-tools"].(map[string]any)["url"] {
		t.Fatal("capability not rotated")
	}
	server.RevokeThread("thread", "first")
	if !server.HasThread("thread") || server.ThreadEnabled("thread") {
		t.Fatal("stale cleanup revoked replacement or reset toggle")
	}
	server.RevokeThread("thread", "second")
	if server.HasThread("thread") {
		t.Fatal("capability not revoked")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.RegisterThread("thread", "third"); err == nil {
		t.Fatal("closed server reopened")
	}
}

func TestArgumentErrorsNameTheFieldWithoutEchoingValues(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"count":"secret-value"}`, `"count" must be an integer`},
		{`{"typo":"secret-value"}`, `Unknown argument "typo"`},
		{`null`, `must be a JSON object`},
		{`{"count":`, `Invalid argument JSON`},
		{`{} {}`, `extra JSON`},
	} {
		var args struct {
			Count int `json:"count"`
		}
		err := DecodeArgs(json.RawMessage(tc.raw), &args)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("%s: %v", tc.raw, err)
		}
	}
}
