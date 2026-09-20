package mcpargs

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDecodeRefusesAnythingButOneClosedObject. The envelope a transport
// carries may hold metadata; the arguments may not, because a field the
// schema does not list is a mistake the model has to be told about.
func TestDecodeRefusesAnythingButOneClosedObject(t *testing.T) {
	var args struct {
		Path string `json:"path"`
	}
	if err := Decode(json.RawMessage(`{"path":"file"}`), &args); err != nil || args.Path != "file" {
		t.Fatalf("arguments: %+v, %v", args, err)
	}
	if err := Decode(nil, &args); err != nil {
		t.Fatalf("absent arguments: %v", err)
	}
	if err := Decode(json.RawMessage(`{"path":"file","_meta":{}}`), &args); err == nil {
		t.Fatal("metadata inside tool arguments bypassed the closed schema")
	}
}

// TestDecodeErrorsNameTheFieldWithoutEchoingValues: a refusal reaches the
// model, so it states the field and the type the schema wants and nothing
// the caller supplied.
func TestDecodeErrorsNameTheFieldWithoutEchoingValues(t *testing.T) {
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
		err := Decode(json.RawMessage(tc.raw), &args)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("%s: %v", tc.raw, err)
		}
	}
}

// TestDecodeBoundsAnUnknownFieldName so a refusal cannot be used to echo a
// payload of the caller's own back to the model.
func TestDecodeBoundsAnUnknownFieldName(t *testing.T) {
	var args struct {
		Count int `json:"count"`
	}
	long := strings.Repeat("x", maxFieldNameBytes*2)
	err := Decode(json.RawMessage(`{"`+long+`":1}`), &args)
	if err == nil {
		t.Fatal("an overlong unknown field was accepted")
	}
	if len(err.Error()) > maxFieldNameBytes+120 {
		t.Fatalf("the refusal is %d bytes: %s", len(err.Error()), err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("the field name was not clipped: %s", err)
	}
}
