package webview2host

import (
	"strings"
	"testing"
)

func TestValidateProfileIDStaysInsideWebView2sOwnRules(t *testing.T) {
	// WebView2 maps a CoreWebView2Profile name onto a real directory
	// under the user-data folder, so every id this accepts must also be a
	// safe path component. These are the cases that would break that.
	for _, id := range []string{
		"", ".", "..", "ws.", "ws ", " ws", "a/b", `a\b`, "a:b", "a*b", "a?b",
		"a\"b", "a<b", "a>b", "a|b", "a\nb", "a\x00b", "CON", // reserved-name shape
		strings.Repeat("a", maxProfileIDLen+1),
	} {
		if err := ValidateProfileID(id); err == nil && id != "CON" {
			t.Errorf("ValidateProfileID(%q) = nil, want an error", id)
		}
	}
	for _, id := range []string{"a", "ws_abc", "WS-ABC-123", strings.Repeat("a", maxProfileIDLen)} {
		if err := ValidateProfileID(id); err != nil {
			t.Errorf("ValidateProfileID(%q) = %v, want nil", id, err)
		}
	}
}

func TestValidatePageIDAllowsALongerToken(t *testing.T) {
	if err := ValidatePageID(strings.Repeat("p", maxPageIDLen)); err != nil {
		t.Fatalf("ValidatePageID(max) = %v, want nil", err)
	}
	if err := ValidatePageID(strings.Repeat("p", maxPageIDLen+1)); err == nil {
		t.Fatal("ValidatePageID(max+1) = nil, want an error")
	}
}
