package threadtools

import (
	"context"
	"strings"
	"testing"
)

// thread_attention raises a flag with a kind and a one-line reason, lowers
// it with none, and refuses everything else before the app sees it.
func TestAttentionFlagsTheCallersThread(t *testing.T) {
	t.Parallel()
	app := newFakeApp("laptop")
	server := New(app)

	got := call(t, server, localCaller(), "thread_attention", `{"kind":"decision","reason":"  Pick the release date  "}`)
	if got["flagged"] != true || got["kind"] != AttentionDecision || got["reason"] != "Pick the release date" {
		t.Errorf("flag result = %v", got)
	}
	got = call(t, server, localCaller(), "thread_attention", `{"kind":"none"}`)
	if got["flagged"] != false || got["kind"] != nil {
		t.Errorf("lower result = %v", got)
	}
	for _, refused := range []struct{ args, says string }{
		{`{"kind":"review"}`, "reason is required"},
		{`{"kind":"problem","reason":"two\nlines"}`, "one line"},
		{`{"kind":"problem","reason":"a\ttab"}`, "one line"},
		{`{"kind":"problem","reason":"fix \u202egnp.exe"}`, "invisible formatting"},
		{`{"kind":"problem","reason":"re\u200bview"}`, "invisible formatting"},
		{`{"kind":"problem","reason":"one\u2028two"}`, "one line"},
		{`{"kind":"problem","reason":"` + strings.Repeat("x", MaxAttentionReasonRunes+1) + `"}`, "at most 120"},
		{`{"kind":"none","reason":"done"}`, "takes no reason"},
		{`{"kind":"urgent","reason":"x"}`, "kind must be one of"},
		{`{"reason":"x"}`, "kind must be one of"},
	} {
		if message := callErr(t, server, localCaller(), "thread_attention", refused.args, CodeInvalidRequest); !strings.Contains(message, refused.says) {
			t.Errorf("%s: refusal %q, want it to say %q", refused.args, message, refused.says)
		}
	}
	call(t, server, localCaller(), "thread_attention", `{"kind":"problem","reason":"`+strings.Repeat("é", MaxAttentionReasonRunes)+`"}`)
	if len(app.attention) != 3 {
		t.Fatalf("calls reaching the app = %+v, want the three valid ones", app.attention)
	}
	if _, err := server.CallForwarded(context.Background(), localCaller(), "thread_attention", []byte(`{"kind":"review","reason":"x"}`)); err == nil {
		t.Error("a forwarded thread_attention was accepted")
	}
	if len(app.attention) != 3 {
		t.Errorf("a forwarded call reached the app: %+v", app.attention)
	}
}
