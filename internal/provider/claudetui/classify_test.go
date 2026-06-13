package claudetui

import "testing"

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
