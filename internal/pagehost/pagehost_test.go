package pagehost

import (
	"strings"
	"testing"
)

func TestMarkWebviewPicksTheRightSeparator(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "no query", in: "http://127.0.0.1:1/", want: "http://127.0.0.1:1/?host=webview"},
		{name: "existing query", in: "http://127.0.0.1:1/?cid=a", want: "http://127.0.0.1:1/?cid=a&host=webview"},
		{name: "empty passes through", in: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MarkWebview(c.in); got != c.want {
				t.Fatalf("MarkWebview(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsWebviewURL(t *testing.T) {
	if !IsWebviewURL(MarkWebview("http://127.0.0.1:1/?cid=a")) {
		t.Fatal("a marked URL did not read as webview-hosted")
	}
	if IsWebviewURL("http://127.0.0.1:1/?t=abc") {
		t.Fatal("a browser page URL read as webview-hosted")
	}
	if IsWebviewURL("http://127.0.0.1:1/?host=browser") {
		t.Fatal("another host value read as webview")
	}
}

// TestDeliveryScriptWritesBothHalves pins the delivery contract the SPA
// reads (frontend/src/lib/transport/pageHost.ts): the global AND the
// event, so the page and the injection may land in either order.
func TestDeliveryScriptWritesBothHalves(t *testing.T) {
	script, err := DeliveryScript("abc-_123==")
	if err != nil {
		t.Fatalf("DeliveryScript: %v", err)
	}
	for _, want := range []string{
		"window." + TicketGlobal + "=\"abc-_123==\"",
		"new Event(\"" + TicketEvent + "\")",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script %q missing %q", script, want)
		}
	}
	// One statement, self-invoking, so evaluating it twice leaves no
	// binding behind and cannot collide with page scope.
	if !strings.HasPrefix(script, "(function(){") || !strings.HasSuffix(script, "})();") {
		t.Fatalf("script is not a self-invoking expression: %q", script)
	}
}

// TestDeliveryScriptRefusesAnythingButABareToken is the whole reason the
// renderer validates instead of escaping: a value that cannot contain a
// quote cannot close the literal it is written into.
func TestDeliveryScriptRefusesAnythingButABareToken(t *testing.T) {
	for _, ticket := range []string{
		"",
		`a";alert(1);"`,
		"a'b",
		"a b",
		"a\nb",
		"a<b",
		"a\\b",
		"a`b",
	} {
		if _, err := DeliveryScript(ticket); err == nil {
			t.Fatalf("DeliveryScript(%q) was rendered; want refusal", ticket)
		}
	}
}
