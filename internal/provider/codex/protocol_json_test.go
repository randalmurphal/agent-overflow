package codex

import (
	"encoding/json"
	"testing"
)

// -- JSON helper tests --

func TestReadNestedString(t *testing.T) {
	data := json.RawMessage(`{"turn":{"id":"t1","status":"completed"}}`)

	if got := readNestedString(data, "turn", "id"); got != "t1" {
		t.Errorf("got %q, want %q", got, "t1")
	}
	if got := readNestedString(data, "turn", "status"); got != "completed" {
		t.Errorf("got %q, want %q", got, "completed")
	}
	if got := readNestedString(data, "turn", "missing"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
	if got := readNestedString(data, "missing", "id"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestReadNestedStringDeep(t *testing.T) {
	data := json.RawMessage(`{"turn":{"error":{"message":"something broke"}}}`)
	if got := readNestedString(data, "turn", "error", "message"); got != "something broke" {
		t.Errorf("got %q, want %q", got, "something broke")
	}
}

func TestReadTopLevelString(t *testing.T) {
	data := json.RawMessage(`{"delta":"hello","other":42}`)

	if got := readTopLevelString(data, "delta"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
	if got := readTopLevelString(data, "missing"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
	// Non-string value should return empty.
	if got := readTopLevelString(data, "other"); got != "" {
		t.Errorf("got %q, want empty string for non-string value", got)
	}
}

func TestReadTopLevelStringInvalidJSON(t *testing.T) {
	if got := readTopLevelString(json.RawMessage(`not json`), "key"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestReadTopLevelIDString(t *testing.T) {
	tests := []struct {
		name string
		data json.RawMessage
		want string
	}{
		{
			name: "string id",
			data: json.RawMessage(`{"requestId":"req-1"}`),
			want: "req-1",
		},
		{
			name: "numeric id",
			data: json.RawMessage(`{"requestId":91}`),
			want: "91",
		},
		{
			name: "missing id",
			data: json.RawMessage(`{"other":true}`),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readTopLevelIDString(tt.data, "requestId"); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadNestedStringInvalidJSON(t *testing.T) {
	if got := readNestedString(json.RawMessage(`not json`), "key"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}
