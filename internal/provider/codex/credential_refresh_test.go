package codex

import (
	"testing"
	"time"
)

func TestCredentialLastRefresh(t *testing.T) {
	want := time.Date(2026, 10, 15, 2, 48, 19, 48123456, time.UTC)
	for _, tc := range []struct {
		name string
		data string
		ok   bool
	}{
		{name: "chatgpt login", data: `{"auth_mode":"chatgpt","tokens":{"access_token":"a"},"last_refresh":"2026-10-15T02:48:19.048123456Z"}`, ok: true},
		{name: "api key login", data: `{"auth_mode":"apikey","OPENAI_API_KEY":"k","last_refresh":null}`},
		{name: "field absent", data: `{"tokens":{}}`},
		{name: "not json", data: `not json`},
		{name: "not a timestamp", data: `{"last_refresh":"yesterday"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CredentialLastRefresh([]byte(tc.data))
			if ok != tc.ok || (ok && !got.Equal(want)) {
				t.Fatalf("CredentialLastRefresh() = %v, %v; want %v, %v", got, ok, want, tc.ok)
			}
		})
	}
}
