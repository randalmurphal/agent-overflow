package webview2host

import (
	"strings"
	"testing"
)

func TestParseTargetID(t *testing.T) {
	// Shape taken from a real WebView2 Target.getTargetInfo response, via
	// the CDP session CallDevToolsProtocolMethod opens on the page.
	const response = `{"targetInfo":{"targetId":"7A1F0C2D3E4B5A69","type":"page",` +
		`"title":"Example","url":"https://example.test/","attached":true,"canAccessOpener":false}}`
	got, err := ParseTargetID(response)
	if err != nil {
		t.Fatalf("ParseTargetID: %v", err)
	}
	if got != "7A1F0C2D3E4B5A69" {
		t.Fatalf("targetId = %q, want 7A1F0C2D3E4B5A69", got)
	}
}

func TestParseTargetIDRejectsUnusableResponses(t *testing.T) {
	for _, tc := range []struct{ name, response, wantErr string }{
		{"empty", "", "decode"},
		{"truncated", `{"targetInfo":{`, "decode"},
		{"no targetInfo", `{}`, "no targetId"},
		{"blank targetId", `{"targetInfo":{"targetId":""}}`, "no targetId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A page whose target id we cannot read is a page the backend
			// cannot attach chromedp to, so this must be an error rather
			// than an empty string reported as success.
			_, err := ParseTargetID(tc.response)
			if err == nil {
				t.Fatalf("ParseTargetID(%q) = nil error, want %q", tc.response, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
