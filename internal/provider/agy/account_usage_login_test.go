package agy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/testutil/mockexec"
)

func TestParseCredentialAndPolicyHelpers(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"token":{"access_token":"ya29.abc","refresh_token":"1//ref-xyz","expiry":"2026-12-01T10:00:00Z"}}`)
	cred, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if cred.AccessToken != "ya29.abc" || cred.RefreshToken != "1//ref-xyz" || !cred.HasExpiry {
		t.Fatalf("unexpected cred: %+v", cred)
	}
	expMs, ok := CredentialExpiresAt(raw)
	if !ok || expMs != time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("CredentialExpiresAt = (%d, %v)", expMs, ok)
	}
	fp, ok := CredentialRefreshTokenFingerprint(raw)
	if !ok || len(fp) != 64 {
		t.Fatalf("CredentialRefreshTokenFingerprint = (%q, %v)", fp, ok)
	}
	if _, err := ParseCredential([]byte(`{}`)); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty token err = %v, want ErrNoCredentials", err)
	}
}

func TestIdentifyAccountAndQuotaSummaryParsing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/userinfo"):
			_, _ = w.Write([]byte(`{"id":"google-123","email":"dev@example.com","name":"Dev User","hd":"example.com"}`))
		case strings.HasSuffix(r.URL.Path, ":retrieveUserQuotaSummary"):
			_, _ = w.Write([]byte(`{
				"groups": [
					{
						"displayName": "Gemini",
						"buckets": [
							{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.6,"resetTime":"2026-11-22T14:00:00Z"},
							{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.85,"resetTime":"2026-11-29T00:00:00Z"},
							{"bucketId":"disabled-bucket","window":"5h","remainingFraction":0.1,"disabled":true}
						]
					},
					{
						"displayName": "Third-party",
						"buckets": [
							{"bucketId":"3p-5h","window":"5h","remainingFraction":0.25,"resetTime":"2026-11-22T15:00:00Z"}
						]
					}
				]
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cred := []byte(`{"token":{"access_token":"ya29.good","refresh_token":"1//ref"}}`)
	info, err := identifyAccountAtURL(context.Background(), srv.Client(), srv.URL+"/userinfo", cred)
	if err != nil {
		t.Fatalf("identifyAccountAtURL: %v", err)
	}
	if info.Email != "dev@example.com" || info.DisplayName != "Dev User" || info.OrgID != "google-123" || info.OrgName != "example.com" || info.TokenSource != "oauth-business" {
		t.Fatalf("unexpected AccountInfo: %+v", info)
	}

	snap, err := probeRateLimitsAtEndpoints(context.Background(), srv.Client(), []string{srv.URL}, cred, now)
	if err != nil {
		t.Fatalf("probeRateLimitsAtEndpoints: %v", err)
	}
	if snap.Provider != "agy" || !snap.Complete || len(snap.Limits) != 3 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap.Limits[0].LimitID != "gemini-5h" || snap.Limits[0].UsedPercent != 40 || snap.Limits[0].WindowMins != 300 {
		t.Fatalf("unexpected limit 0: %+v", snap.Limits[0])
	}
	if snap.Limits[1].LimitID != "gemini-weekly" || snap.Limits[1].UsedPercent != 15 || snap.Limits[1].WindowMins != 10080 {
		t.Fatalf("unexpected limit 1: %+v", snap.Limits[1])
	}
	if snap.Limits[2].LimitID != "3p-5h" || snap.Limits[2].LimitName != "Claude & GPT (shared) · 5-hour" || snap.Limits[2].UsedPercent != 75 {
		t.Fatalf("unexpected limit 2: %+v", snap.Limits[2])
	}

	// Unauthorized returns ErrOAuthUnauthorized.
	badCred := []byte(`{"token":{"access_token":"ya29.bad"}}`)
	if _, err := probeRateLimitsAtEndpoints(context.Background(), srv.Client(), []string{srv.URL}, badCred, now); !errors.Is(err, ErrOAuthUnauthorized) {
		t.Fatalf("unauthorized err = %v, want ErrOAuthUnauthorized", err)
	}
}

func TestAgyQuotaRateLimit429(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	cred := []byte(`{"token":{"access_token":"ya29.good"}}`)
	_, err := probeRateLimitsAtEndpoints(context.Background(), srv.Client(), []string{srv.URL}, cred, time.Now())
	var rl RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter() != 45*time.Second {
		t.Fatalf("expected RateLimitedError(45s), got %v", err)
	}
}

func TestAgyLoginFlowAndValidation(t *testing.T) {
	t.Parallel()
	geminiDir := t.TempDir()
	script := `#!/bin/bash
printf 'Open this URL to sign in:\n'
printf 'https://accounts.google.com/o/oauth2/auth?client_id=test&redirect_uri=https%%3A%%2F%%2Fantigravity.google%%2Foauth-callback&response_type=code&state=state-xyz-123\n'
IFS= read -r code
if [ "$code" != "4/0A-valid-code" ]; then
  printf 'invalid code\n' >&2
  exit 1
fi
mkdir -p "` + filepath.Join(geminiDir, "antigravity-cli") + `"
printf '{"token":{"access_token":"ya29.fresh","refresh_token":"1//fresh-ref"}}\n' > "` + filepath.Join(geminiDir, "antigravity-cli", "antigravity-oauth-token") + `"
`
	bin := filepath.Join(t.TempDir(), "fake-agy-login.sh")
	mockexec.Write(t, bin, script)

	sess, err := StartLogin(context.Background(), LoginConfig{
		Binary:        bin,
		WorkDir:       t.TempDir(),
		GeminiDir:     geminiDir,
		AppDataSubdir: "antigravity-cli",
		Reaper:        procutiltest.Reaper(),
	})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if !strings.Contains(sess.AuthorizeURL(), "state=state-xyz-123") {
		t.Fatalf("unexpected AuthorizeURL: %q", sess.AuthorizeURL())
	}
	if _, err := sess.ValidatePaste("https://evil.example.com/?code=123&state=state-xyz-123"); !errors.Is(err, ErrInvalidLoginCode) {
		t.Fatalf("expected ErrInvalidLoginCode for wrong redirect host, got %v", err)
	}
	if err := sess.SubmitCode(context.Background(), "4/0A-valid-code"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(geminiDir, "antigravity-cli", "antigravity-oauth-token"))
	if err != nil {
		t.Fatalf("read written credential: %v", err)
	}
	cred, err := ParseCredential(raw)
	if err != nil || cred.AccessToken != "ya29.fresh" {
		t.Fatalf("unexpected stored credential: %+v err=%v", cred, err)
	}
}
