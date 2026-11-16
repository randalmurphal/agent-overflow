package trackerapi

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// secretForms are the ways a printer could show raw or the Basic pair:
// as text, hex in either case, and the decimal bytes %d gives a slice.
func secretForms(raw string) []string {
	pair := base64.StdEncoding.EncodeToString([]byte("user-1:" + raw))
	forms := []string{raw, pair}
	for _, s := range []string{raw, pair} {
		forms = append(forms, hex.EncodeToString([]byte(s)), strings.ToUpper(hex.EncodeToString([]byte(s))))
		forms = append(forms, strings.Trim(fmt.Sprintf("%d", []byte(s)), "[]"))
	}
	return forms
}

// printed is every rendering of v the credential redaction test checks:
// each fmt verb, Sprint, JSON, and slog's JSON and text handlers.
func printed(t *testing.T, v any) []string {
	t.Helper()
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("answer", "v", v)
	slog.New(slog.NewTextHandler(&logged, nil)).Info("answer", "v", v)
	out := []string{logged.String(), fmt.Sprint(v)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		out = append(out, fmt.Sprintf(verb, v))
	}
	if encoded, err := json.Marshal(v); err == nil {
		out = append(out, string(encoded))
	}
	if m, ok := v.(interface{ MarshalText() ([]byte, error) }); ok {
		if text, err := m.MarshalText(); err == nil {
			out = append(out, string(text))
		}
	}
	return out
}

// Nothing a Response or an error retains, for scrubbing or otherwise, is
// printed by any formatter, encoder or logger, at the top level or nested
// in exported and unexported fields.
func TestRetainedCredentialIsUnprintable(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	creds := map[string]func() *Credential{
		"basic":  func() *Credential { return Basic("user-1", secret.New([]byte(raw))) },
		"bearer": func() *Credential { return Bearer(secret.New([]byte(raw))) },
		"key":    func() *Credential { return Key(secret.New([]byte(raw))) },
	}
	for name, cred := range creds {
		tr := newTracker(t, route{
			"GET /ok":     reply(http.StatusOK),
			"POST /issue": echo(http.StatusBadRequest, raw),
			"GET /me":     echo(http.StatusUnauthorized, raw),
		})
		opts := testOptions(tr.url())
		opts.Dialect = testDialect{}
		opts.Credentials = StaticCredentials(cred())
		c, _ := loopbackClient(t, opts)
		resp, err := c.Do(t.Context(), Request{Path: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
		status, ok := errors.AsType[*StatusError](err)
		if !ok {
			t.Fatalf("%s: %v, want a *StatusError", name, err)
		}
		_, err = c.Do(t.Context(), Request{Path: "me"})
		refusal, ok := errors.AsType[*AuthError](err)
		if !ok {
			t.Fatalf("%s: %v, want an *AuthError", name, err)
		}
		type holder struct {
			Resp    Response
			Ptr     *Response
			Nested  struct{ Inner *Response }
			Status  *StatusError
			Auth    AuthError
			hidden  *Response
			inline  Response
			failure error
		}
		h := holder{Resp: *resp, Ptr: resp, Status: status, Auth: *refusal, hidden: resp, inline: *resp, failure: err}
		h.Nested.Inner = resp
		for _, v := range []any{resp, *resp, status, *status, refusal, *refusal, h, &h} {
			for _, out := range printed(t, v) {
				for _, form := range secretForms(raw) {
					if strings.Contains(out, form) {
						t.Fatalf("%s: printing %T shows %q: %s", name, v, form, out)
					}
				}
			}
		}
	}
}
