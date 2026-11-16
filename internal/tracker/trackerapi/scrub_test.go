package trackerapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// echo answers status with a body that repeats the request's
// Authorization header, its value without the scheme, and raw in the
// message, a field's name and value, and the text around them.
func echo(status int, raw string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		_, value, _ := strings.Cut(auth, " ")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"message":"bad header %s; value %s; secret %s","errors":{"%s":"%s"}}`, auth, value, raw, auth, raw)
	}
}

// No answer text in an error repeats the credential the request carried,
// in any form it is sent: Basic's encoding, Bearer's value, a bare key,
// the secret alone.
func TestErrorsNeverRepeatTheCredential(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	basic := base64.StdEncoding.EncodeToString([]byte("user-1:" + raw))
	creds := map[string]func() *Credential{
		"basic":  func() *Credential { return Basic("user-1", secret.New([]byte(raw))) },
		"bearer": func() *Credential { return Bearer(secret.New([]byte(raw))) },
		"key":    func() *Credential { return Key(secret.New([]byte(raw))) },
	}
	for name, cred := range creds {
		tr := newTracker(t, route{
			"POST /issue": echo(http.StatusBadRequest, raw),
			"GET /me":     echo(http.StatusUnauthorized, raw),
		})
		opts := testOptions(tr.url())
		opts.Dialect = testDialect{}
		opts.Credentials = StaticCredentials(cred())
		c, _ := loopbackClient(t, opts)

		_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
		status, ok := errors.AsType[*StatusError](err)
		if !ok || status.Message == "" || len(status.Fields) != 1 {
			t.Fatalf("%s: %#v, want a *StatusError with the dialect's message", name, err)
		}
		texts := []string{err.Error(), status.Body, status.Message}
		for field, message := range status.Fields {
			texts = append(texts, field, message)
		}
		_, err = c.Do(t.Context(), Request{Path: "me"})
		refusal, ok := errors.AsType[*AuthError](err)
		if !ok || refusal.Message == "" {
			t.Fatalf("%s: %v, want an *AuthError with the dialect's message", name, err)
		}
		texts = append(texts, err.Error(), refusal.Message)
		for _, text := range texts {
			if strings.Contains(text, raw) || strings.Contains(text, basic) || !strings.Contains(text, secret.Redacted) {
				t.Errorf("%s: %q repeats the credential", name, text)
			}
		}
	}
}

// A transport error that quotes the tracker's bytes, and a decode error
// that quotes the body, do not repeat the credential either.
func TestQuotingErrorsNeverRepeatTheCredential(t *testing.T) {
	t.Parallel()
	const raw = "4242424242"
	tr := newTracker(t, route{
		"GET /line": func(w http.ResponseWriter, r *http.Request) {
			hijackClose(t, w, r.Header.Get("Authorization")+"\r\n\r\n")
		},
		"GET /number": func(w http.ResponseWriter, r *http.Request) {
			writeString(w, `{"key":`+r.Header.Get("Authorization")+`}`)
		},
	})
	opts := testOptions(tr.url())
	opts.Credentials = StaticCredentials(Key(secret.New([]byte(raw))))
	c, _ := loopbackClient(t, opts)
	_, err := c.Do(t.Context(), Request{Path: "line"})
	if err == nil || strings.Contains(err.Error(), raw) || !strings.Contains(err.Error(), secret.Redacted) {
		t.Errorf("malformed answer: %v, want the quoted credential scrubbed", err)
	}
	var out struct{ Key int8 } // overflowed, so the error quotes the number
	_, err = c.JSON(t.Context(), Request{Path: "number"}, &out)
	if err == nil || strings.Contains(err.Error(), raw) || !strings.Contains(err.Error(), secret.Redacted) {
		t.Errorf("decode: %v, want the quoted credential scrubbed", err)
	}
}

// Response.Scrub removes the credential its request carried from text a
// caller takes from a successful answer, and a printed Response shows no
// part of it.
func TestResponseScrub(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	tr := newTracker(t, route{"POST /graphql": echo(http.StatusOK, raw)})
	opts := testOptions(tr.url())
	opts.Credentials = StaticCredentials(Basic("user-1", secret.New([]byte(raw))))
	c, _ := loopbackClient(t, opts)
	resp, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "graphql", ReadOnly: true, Body: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Scrub(string(resp.Body)); strings.Contains(got, raw) || strings.Contains(got, "dXNlci0x") || !strings.Contains(got, secret.Redacted) {
		t.Fatalf("Scrub = %q", got)
	}
	for _, printed := range []string{fmt.Sprintf("%v", resp), fmt.Sprintf("%+v", *resp), fmt.Sprintf("%#v", resp)} {
		if strings.Contains(printed, "dXNlci0x") {
			t.Fatalf("a printed Response shows the credential: %s", printed)
		}
	}

	media := newTracker(t, route{"GET /file/1": func(w http.ResponseWriter, r *http.Request) { writeString(w, raw) }})
	resp, err = c.Do(t.Context(), Request{Path: media.srv.URL + "/file/1", Absolute: true})
	if err != nil || resp.Scrub(raw) != raw {
		t.Fatalf("off the base origin: %v, Scrub changed text the request never carried a credential for", err)
	}
	if (*Response)(nil).Scrub(raw) != raw {
		t.Fatal("a nil Response changed the text")
	}
}

// An error body cut at its bound keeps no leading part of the credential.
func TestErrorBodyCutKeepsNoPartOfTheCredential(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	tr := newTracker(t, route{"GET /a": func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeString(w, "0123456789"+raw)
	}})
	opts := testOptions(tr.url())
	opts.Credentials = StaticCredentials(Bearer(secret.New([]byte(raw))))
	c, _ := loopbackClient(t, opts)
	_, err := c.Do(t.Context(), Request{Path: "a", MaxBodyBytes: 16})
	status, ok := errors.AsType[*StatusError](err)
	if !ok || status.Body != "0123456789" {
		t.Fatalf("Do = %#v, want the body before the credential's first bytes", err)
	}
}
