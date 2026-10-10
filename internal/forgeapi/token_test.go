package forgeapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const testSecret = "fake-token-1"

func TestTokenNeverFormatsItsSecret(t *testing.T) {
	t.Parallel()
	token := newToken([]byte(testSecret), AuthBearer)
	type holder struct {
		Token  Token
		Ptr    *Token
		Nested struct{ Inner Token }
	}
	h := holder{Token: *token, Ptr: token}
	h.Nested.Inner = *token

	outputs := []string{
		fmt.Sprintf("%v %s %+v %#v %q %x %X %d", token, token, token, token, token, token, token, token),
		fmt.Sprintf("%v %s %+v %#v %q %x", *token, *token, *token, *token, *token, *token),
		fmt.Sprintf("%v %+v %#v %s", h, h, h, h),
		fmt.Sprint(token, *token, h),
		token.String(),
		token.GoString(),
	}
	for _, value := range []any{token, *token, h} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("json.Marshal(%T): %v", value, err)
		}
		outputs = append(outputs, string(encoded))
	}
	text, err := token.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(text))
	for _, out := range outputs {
		if strings.Contains(out, testSecret) {
			t.Fatalf("a formatter printed the secret: %q", out)
		}
		if !strings.Contains(out, "redacted") {
			t.Fatalf("output %q does not say %s", out, redacted)
		}
	}
}

func TestTokenAuthorizesAndZeroes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		header AuthHeader
		name   string
		want   string
	}{
		{AuthBearer, "Authorization", "Bearer " + testSecret},
		{AuthPrivateToken, "Private-Token", testSecret},
	} {
		source := []byte(testSecret)
		token := newToken(source, tc.header)
		clear(source)
		req, _ := http.NewRequest(http.MethodGet, "https://example.test/", nil)
		token.authorize(req)
		if got := req.Header.Get(tc.name); got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, got, tc.want)
		}
		backing := token.b
		token.Zero()
		for _, c := range backing {
			if c != 0 {
				t.Fatalf("Zero left secret bytes behind: %q", backing)
			}
		}
		after, _ := http.NewRequest(http.MethodGet, "https://example.test/", nil)
		token.authorize(after)
		if len(after.Header) != 0 {
			t.Fatalf("a zeroed token still set headers: %v", after.Header)
		}
	}
}

func TestFingerprintIsSHA256Hex(t *testing.T) {
	t.Parallel()
	// sha256("fake-token-1")
	got := fingerprint([]byte(testSecret))
	if len(got) != 64 || strings.Contains(got, testSecret) {
		t.Fatalf("fingerprint = %q", got)
	}
	if got != fingerprint([]byte(testSecret)) || got == fingerprint([]byte("fake-token-2")) {
		t.Fatal("fingerprint is not a stable function of the token")
	}
}
