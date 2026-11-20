package trackerapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	// The credential as a bare number is replaced in the body before the
	// decode, which then fails without the number to quote.
	var out struct{ Key int8 }
	_, err = c.JSON(t.Context(), Request{Path: "number"}, &out)
	if err == nil || strings.Contains(err.Error(), raw) || out.Key != 0 {
		t.Errorf("decode: %v %v, want a decode error without the credential", out, err)
	}
}

// chainTexts is the text of err and of every error a caller can unwrap
// from it.
func chainTexts(err error) []string {
	var out []string
	for stack := []error{err}; len(stack) > 0; {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if e == nil {
			continue
		}
		out = append(out, e.Error())
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			stack = append(stack, u.Unwrap())
		case interface{ Unwrap() []error }:
			stack = append(stack, u.Unwrap()...)
		}
	}
	return out
}

// A failure reading a successful answer's body that quotes the tracker's
// bytes repeats no credential, in the error or anything it unwraps to.
func TestBodyReadErrorsAreScrubbed(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	trailer := func(w http.ResponseWriter, r *http.Request) {
		hijackClose(t, w, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n"+r.Header.Get("Authorization")+"\r\n\r\n")
	}
	for name, cred := range map[string]func() *Credential{
		"basic":  func() *Credential { return Basic("user-1", secret.New([]byte(raw))) },
		"bearer": func() *Credential { return Bearer(secret.New([]byte(raw))) },
	} {
		tr := newTracker(t, route{"GET /a": trailer, "POST /a": trailer})
		opts := testOptions(tr.url())
		opts.Credentials = StaticCredentials(cred())
		c, _ := loopbackClient(t, opts)
		calls := map[string]func() error{
			"Do": func() error { _, err := c.Do(t.Context(), Request{Path: "a"}); return err },
			"JSON": func() error {
				var out map[string]any
				_, err := c.JSON(t.Context(), Request{Path: "a"}, &out)
				return err
			},
			"Stream": func() error {
				var dst strings.Builder
				_, err := c.Stream(t.Context(), Request{Path: "a"}, &dst, 100)
				return err
			},
			"write": func() error {
				_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "a", Body: map[string]string{}})
				return err
			},
		}
		for call, run := range calls {
			err := run()
			if _, ok := errors.AsType[*TransientError](err); !ok {
				t.Fatalf("%s %s: %v, want a transient read failure", name, call, err)
			}
			for _, text := range chainTexts(err) {
				for _, form := range secretForms(raw) {
					if strings.Contains(text, form) {
						t.Fatalf("%s %s: %q repeats the credential", name, call, text)
					}
				}
			}
		}
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

// opaqueError hides the error it wraps from its own text.
type opaqueError struct{ inner error }

func (e opaqueError) Error() string { return "request failed" }
func (e opaqueError) Unwrap() error { return e.inner }

// cause scrubs every error a caller can unwrap, not only the outermost
// text, and keeps the sentinels the original matched.
func TestCauseScrubsTheWholeChain(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	tr := newTracker(t, route{"GET /a": reply(http.StatusOK)})
	opts := testOptions(tr.url())
	opts.Credentials = StaticCredentials(Bearer(secret.New([]byte(raw))))
	c, _ := loopbackClient(t, opts)
	if err := c.loadCredential(t.Context(), false, 0); err != nil {
		t.Fatal(err)
	}
	sent := sentWith{onBase: true, cred: c.cred, carried: true}
	for name, err := range map[string]error{
		"hidden":   opaqueError{inner: errors.New("echo Bearer " + raw)},
		"joined":   errors.Join(context.DeadlineExceeded, errors.New("echo "+raw)),
		"sentinel": fmt.Errorf("%w: echo %s", context.DeadlineExceeded, raw),
	} {
		got := c.cause(sent, err)
		for _, text := range chainTexts(got) {
			if strings.Contains(text, raw) {
				t.Errorf("%s: %q repeats the credential", name, text)
			}
		}
		if errors.Is(err, context.DeadlineExceeded) != errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("%s: the scrubbed cause lost its sentinel", name)
		}
	}
	if plain := errors.New("connection reset"); c.cause(sent, plain) != plain {
		t.Error("a cause without the credential was replaced")
	}
}

// A JSON escape hides the key from the textual scrub of StatusError.Body,
// and decoding restores it: the error's Scrub removes it from what a
// caller decodes, for a status, a refused credential and a refused
// redirect, and withholds the text once the credential is gone.
func TestErrorScrubAfterDecoding(t *testing.T) {
	t.Parallel()
	const raw = "lin_api_fake0042"
	// The body escapes the key's "f" as a JSON \u escape; "\x5c" is the backslash.
	const escaped = `{"message":"bad lin_api_` + "\x5c" + `u0066ake0042"}`
	answer := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			writeString(w, escaped)
		}
	}
	tr := newTracker(t, route{
		"POST /graphql": answer(http.StatusBadRequest),
		"GET /me":       answer(http.StatusUnauthorized),
		"POST /moved":   func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusSeeOther) },
	})
	opts := testOptions(tr.url())
	opts.Dialect = testDialect{}
	opts.Credentials = StaticCredentials(Key(secret.New([]byte(raw))))
	c, _ := loopbackClient(t, opts)

	_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "graphql", Body: map[string]string{}})
	status, ok := errors.AsType[*StatusError](err)
	if !ok || status.Message != "bad "+secret.Redacted {
		t.Fatalf("400: %#v, want the message scrubbed", err)
	}
	var fromBody struct{ Message string }
	if err := json.Unmarshal([]byte(status.Body), &fromBody); err != nil || fromBody.Message != "bad "+secret.Redacted {
		t.Fatalf("decoding the error body gave %q, %v; want the key scrubbed as it decodes", fromBody.Message, err)
	}
	// The text a caller decodes on its own, for the Scrub methods below.
	var decoded struct{ Message string }
	if err := json.Unmarshal([]byte(escaped), &decoded); err != nil || !strings.Contains(decoded.Message, raw) {
		t.Fatalf("decoding the answer gave %q, %v; want the key restored", decoded.Message, err)
	}
	_, err = c.Do(t.Context(), Request{Path: "me"})
	refusal, ok := errors.AsType[*AuthError](err)
	if !ok {
		t.Fatalf("401: %v, want an *AuthError", err)
	}
	_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "moved", Body: map[string]string{}})
	redirect, ok := errors.AsType[*StatusError](err)
	if !ok {
		t.Fatalf("303: %v, want a *StatusError", err)
	}
	scrubs := map[string]func(string) string{"status": status.Scrub, "refusal": refusal.Scrub, "redirect": redirect.Scrub}
	for name, scrub := range scrubs {
		if got := scrub(decoded.Message); got != "bad "+secret.Redacted {
			t.Errorf("%s: Scrub = %q", name, got)
		}
	}
	c.ForgetCredential()
	for name, scrub := range scrubs {
		if got := scrub(decoded.Message); got != secret.Redacted {
			t.Errorf("%s: Scrub after ForgetCredential = %q, want the text withheld", name, got)
		}
	}

	media := newTracker(t, route{"GET /file/1": answer(http.StatusBadRequest)})
	_, err = c.Do(t.Context(), Request{Path: media.srv.URL + "/file/1", Absolute: true})
	if offOrigin, ok := errors.AsType[*StatusError](err); !ok || offOrigin.Scrub(raw) != raw {
		t.Fatalf("off the base origin: %v, want text unchanged by a request that carried no credential", err)
	}
	if (&StatusError{}).Scrub(raw) != raw || (&AuthError{}).Scrub(raw) != raw ||
		(*StatusError)(nil).Scrub(raw) != raw || (*AuthError)(nil).Scrub(raw) != raw {
		t.Fatal("an error built without a credential changed the text")
	}
}

// Once the client no longer holds the credential a Response's request
// carried, Scrub withholds any text instead of returning it unscrubbed.
func TestScrubAfterTheCredentialIsGone(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	tr := newTracker(t, route{"GET /a": reply(http.StatusOK)})
	opts := testOptions(tr.url())
	opts.Credentials = StaticCredentials(Bearer(secret.New([]byte(raw))))
	c, _ := loopbackClient(t, opts)
	for _, end := range []func(){c.ForgetCredential, c.Close} {
		resp, err := c.Do(t.Context(), Request{Path: "a"})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Scrub("token " + raw); got != "token "+secret.Redacted {
			t.Fatalf("Scrub while held = %q", got)
		}
		end()
		if got := resp.Scrub("token " + raw); got != secret.Redacted {
			t.Fatalf("Scrub after the credential was zeroed = %q, want it withheld", got)
		}
		if got := resp.Scrub(""); got != "" {
			t.Fatalf("Scrub of nothing = %q", got)
		}
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
