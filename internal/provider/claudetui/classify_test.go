package claudetui

import (
	"encoding/json"
	"testing"
)

func TestClassifyRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
		want requestClass
	}{
		{
			name: "agent turn — populated tools and real budget",
			body: `{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"},{"name":"Read"}]}`,
			want: classAgent,
		},
		{
			name: "quota preflight — max_tokens 1",
			body: `{"model":"claude-haiku","max_tokens":1,"tools":[{"name":"Bash"}]}`,
			want: classPreflight,
		},
		{
			name: "quota preflight — max_tokens 0",
			body: `{"model":"claude-haiku","max_tokens":0}`,
			want: classPreflight,
		},
		{
			name: "auxiliary — no tools (title/topic generation)",
			body: `{"model":"claude-haiku","max_tokens":512,"tools":[]}`,
			want: classAuxiliary,
		},
		{
			name: "auxiliary — tools omitted entirely",
			body: `{"model":"claude-haiku","max_tokens":512}`,
			want: classAuxiliary,
		},
		{
			name: "nested sub-call — all server tools",
			body: `{"model":"claude-haiku","max_tokens":4096,"tools":[{"type":"web_search_20250305"}]}`,
			want: classNestedSubcall,
		},
		{
			name: "agent — mix of custom and server tools is still a main-loop turn",
			body: `{"model":"claude-haiku","max_tokens":4096,"tools":[{"name":"Bash"},{"type":"web_search_20250305"}]}`,
			want: classAgent,
		},
		{
			name: "suggestion mode — full tools but the synthetic marker on the last user message",
			body: `{"model":"claude-opus-4-8","max_tokens":64000,"tools":[{"name":"Bash"},{"name":"Read"}],"messages":[{"role":"user","content":"make a diagram"},{"role":"assistant","content":"here"},{"role":"user","content":"[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]\nPredict the next input."}]}`,
			want: classSuggestion,
		},
		{
			name: "suggestion mode — marker in array-form content block",
			body: `{"model":"claude-opus-4-8","max_tokens":64000,"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":[{"type":"text","text":"[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}]}]}`,
			want: classSuggestion,
		},
		{
			name: "agent — a real turn that merely mentions the marker mid-text is NOT suggestion mode",
			body: `{"model":"claude-opus-4-8","max_tokens":64000,"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"explain what [SUGGESTION MODE: ...] does in the binary"}]}`,
			want: classAgent,
		},
		{
			name: "unparseable body",
			body: `{not json`,
			want: classUnparseable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, req := classifyRequest([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("classifyRequest = %v, want %v", got, tc.want)
			}
			if tc.want == classAgent && req == nil {
				t.Fatal("agent classification must return the parsed request")
			}
			if tc.want != classAgent && req != nil {
				t.Fatalf("non-agent classification must return nil request, got %+v", req)
			}
		})
	}
}

func TestBlockText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"string", `"hello world"`, "hello world"},
		{"blocks", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a b"},
		{"mixed", `[{"type":"text","text":"keep"},{"type":"tool_use","id":"x"}]`, "keep"},
		{"empty", ``, ""},
		{"null", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockText(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("blockText(%s) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLastUserText(t *testing.T) {
	// Picks the LAST user message, skipping a trailing assistant, and reads
	// array-form content.
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"first"}`),
		json.RawMessage(`{"role":"assistant","content":"reply"}`),
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"second"}]}`),
	}
	if got := lastUserText(msgs); got != "second" {
		t.Errorf("lastUserText = %q, want %q", got, "second")
	}
	if got := lastUserText(nil); got != "" {
		t.Errorf("lastUserText(nil) = %q, want empty", got)
	}
	only := []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"x"}`)}
	if got := lastUserText(only); got != "" {
		t.Errorf("lastUserText(no user) = %q, want empty", got)
	}
}

func TestToolNames(t *testing.T) {
	_, req := classifyRequest([]byte(`{"max_tokens":32000,"tools":[{"name":"Bash"},{"type":"web_search_20250305"},{"name":"Read"}]}`))
	if req == nil {
		t.Fatal("expected agent request")
	}
	got := req.toolNames()
	want := []string{"Bash", "Read"}
	if len(got) != len(want) {
		t.Fatalf("toolNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("toolNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
