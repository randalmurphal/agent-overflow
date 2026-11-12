package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

const profileCredential = `{"claudeAiOauth":{"accessToken":"bearer-xyz","refreshToken":"r","expiresAt":99999999999,` +
	`"scopes":["user:inference","user:profile"]}}`

func profileServer(t *testing.T, handler http.HandlerFunc) (*http.Client, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: redirectTransport{target: target, inner: http.DefaultTransport}}, &calls
}

// The request is the one Claude Code sends to fill oauthAccount, and the
// answer is read from the fields it validates.
func TestProbeProfileIdentifiesTheCredentialsToken(t *testing.T) {
	var method, path, auth string
	client, _ := profileServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"account":{"uuid":"acct","email":"user@example.com","display_name":"U"},` +
			`"organization":{"uuid":"org-b","name":"Org B","organization_type":"claude_max"}}`))
	})
	profile, err := ProbeProfile(context.Background(), client, []byte(profileCredential))
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/oauth/profile" || auth != "Bearer bearer-xyz" {
		t.Fatalf("request = %s %s auth %q", method, path, auth)
	}
	want := Profile{Email: "user@example.com", OrgID: "org-b", OrgName: "Org B"}
	if profile != want {
		t.Fatalf("profile = %+v, want %+v", profile, want)
	}
}

func TestProbeProfileRefusesAnAnswerWithoutAnOrganization(t *testing.T) {
	client, _ := profileServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"account":{"email":"user@example.com"},"organization":{}}`))
	})
	if _, err := ProbeProfile(context.Background(), client, []byte(profileCredential)); err == nil {
		t.Fatal("an answer naming no organization was accepted as an identity")
	}
}

func TestProbeProfileReportsARejectedBearer(t *testing.T) {
	client, _ := profileServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := ProbeProfile(context.Background(), client, []byte(profileCredential))
	if !errors.Is(err, ErrOAuthUnauthorized) {
		t.Fatalf("err = %v, want ErrOAuthUnauthorized", err)
	}
}

func TestProbeProfileReportsAServerFailure(t *testing.T) {
	client, _ := profileServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := ProbeProfile(context.Background(), client, []byte(profileCredential))
	if err == nil || errors.Is(err, ErrOAuthUnauthorized) {
		t.Fatalf("err = %v, want a server failure", err)
	}
}

// A credential that cannot be identified is answered without a request, as
// Claude Code declines to ask without the scope.
func TestProbeProfileNeverAsksForAnUnidentifiableCredential(t *testing.T) {
	client, calls := profileServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, tc := range []struct {
		name       string
		credential string
		want       error
	}{
		{"no scope", `{"claudeAiOauth":{"accessToken":"a","scopes":["user:inference"]}}`, ErrNoProfileScope},
		{"no scopes field", `{"claudeAiOauth":{"accessToken":"a"}}`, ErrNoProfileScope},
		{"signed out", `{"claudeAiOauth":{"accessToken":"","scopes":["user:profile"]}}`, ErrNoCredentials},
	} {
		if _, err := ProbeProfile(context.Background(), client, []byte(tc.credential)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if *calls != 0 {
		t.Fatalf("%d profile requests for credentials that cannot be identified", *calls)
	}
}
