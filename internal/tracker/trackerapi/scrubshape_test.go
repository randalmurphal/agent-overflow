package trackerapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// shapeBodies are the bodies an answer can repeat tok in: as a JSON
// string, an object key, every byte \u-escaped, outside every string and
// in a body that is not JSON.
func shapeBodies(tok string) []string {
	str := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	var esc strings.Builder
	for _, r := range tok {
		fmt.Fprintf(&esc, "%su%04x", bs, r)
	}
	return []string{
		`{"message":` + str("x "+tok) + `,"errors":{` + str(tok) + `:` + str(tok) + `}}`,
		`{"a":"` + esc.String() + `"}`,
		`[` + tok + `]`,
		`<p>` + tok + `</p>`,
	}
}

// Every credential shape check accepts reaches no Response.Body and no
// error's text, decoded or not, through Do: digits only, one holding a
// quote and one holding a backslash, the last two one byte longer than
// check allows. Once the credential is dropped, every non-empty body is
// refused or withheld. A one-byte secret and a JSON literal are too
// short for check (TestShortSecretIsNotSent).
func TestCredentialShapesThroughDo(t *testing.T) {
	t.Parallel()
	for _, tok := range []string{"424242424242", `tok"en-0042`, `tok` + bs + `en-0042`} {
		if len(tok) <= len(secret.Redacted) {
			t.Fatalf("%q is too short for check", tok)
		}
		bodies := shapeBodies(tok)
		var c *Client
		routes := route{"GET /empty": func(w http.ResponseWriter, r *http.Request) {
			c.ForgetCredential()
			w.WriteHeader(http.StatusNoContent)
		}}
		for i, body := range bodies {
			for _, kind := range []string{"ok", "err", "lost", "lost-err"} {
				status := http.StatusOK
				if strings.HasSuffix(kind, "err") {
					status = http.StatusBadRequest
				}
				routes[fmt.Sprintf("GET /%s/%d", kind, i)] = func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(kind, "lost") {
						c.ForgetCredential()
					}
					w.WriteHeader(status)
					writeString(w, body)
				}
			}
		}
		tr := newTracker(t, routes)
		opts := testOptions(tr.url())
		opts.Dialect = testDialect{}
		opts.Credentials = StaticCredentials(Bearer(secret.New([]byte(tok))))
		c, _ = loopbackClient(t, opts)
		holds := func(what, text string) {
			t.Helper()
			if strings.Contains(text, tok) {
				t.Errorf("%q: %s repeats the credential: %s", tok, what, text)
			}
		}
		decoded := func(what string, body []byte) {
			t.Helper()
			values, ok := jsonValues(body)
			if !ok {
				return
			}
			var texts []string
			for _, v := range values {
				collectStrings(v, &texts)
			}
			holds(what+" decoded", strings.Join(texts, "\x00"))
			again, _ := json.Marshal(values)
			holds(what+" encoded again", string(again))
		}
		for i := range bodies {
			what := fmt.Sprintf("body %d", i)
			resp, err := c.Do(t.Context(), Request{Path: fmt.Sprintf("ok/%d", i)})
			if err != nil {
				t.Fatalf("%q %s: %v", tok, what, err)
			}
			holds(what, string(resp.Body))
			decoded(what, resp.Body)

			_, err = c.Do(t.Context(), Request{Path: fmt.Sprintf("err/%d", i)})
			se, ok := errors.AsType[*StatusError](err)
			if !ok {
				t.Fatalf("%q %s error: %v", tok, what, err)
			}
			holds(what+" error body", se.Body)
			decoded(what+" error body", []byte(se.Body))
			holds(what+" error message", se.Message)
			for name, message := range se.Fields {
				holds(what+" error field", name+message)
			}

			resp, err = c.Do(t.Context(), Request{Path: fmt.Sprintf("lost/%d", i)})
			if !errors.Is(err, ErrCredentialChanged) || resp == nil || resp.Body != nil {
				t.Errorf("%q %s after the credential dropped: %v, want ErrCredentialChanged and no body", tok, what, err)
			}
			_, err = c.Do(t.Context(), Request{Path: fmt.Sprintf("lost-err/%d", i)})
			if se, ok := errors.AsType[*StatusError](err); !ok || se.Body != secret.Redacted || se.Message != "" && se.Message != secret.Redacted {
				t.Errorf("%q %s error after the credential dropped: %#v, want the body withheld", tok, what, err)
			}
		}
		if resp, err := c.Do(t.Context(), Request{Path: "empty"}); err != nil || resp.Status != http.StatusNoContent {
			t.Errorf("%q: an empty answer after the credential dropped: %v", tok, err)
		}
	}
}
