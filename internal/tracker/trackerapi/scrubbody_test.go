package trackerapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// bodySecret holds a "/", which JSON may write as the escape \/.
const bodySecret = "s3cr3t/value-0042"

// bs is a backslash, built at run time so no edit of this source turns an
// escape into the letter it stands for.
var bs = string(rune(0x5c))

// Every body a success answer can carry the credential in reaches the
// caller with it replaced: Response.Body as bytes, and every value Do's
// body or JSON decodes.
func TestSuccessBodiesAreScrubbed(t *testing.T) {
	t.Parallel()
	basic := base64.StdEncoding.EncodeToString([]byte("user-1:" + bodySecret))
	unicode := bs + "u0073" + bodySecret[1:]             // "s" as a \u escape
	slash := strings.Replace(bodySecret, "/", bs+"/", 1) // "/" as \/
	for name, body := range map[string]string{
		"raw":            `{"a":"before ` + bodySecret + ` after","n":12345678901234567890,"h":"<&>"}`,
		"unicode escape": `{"a":"` + unicode + `"}`,
		"slash escape":   `{"a":"` + slash + `"}`,
		"object key":     `{"` + bodySecret + `":{"b":1}}`,
		"escaped key":    `{"` + unicode + `":1}`,
		"nested array":   `{"a":[1,[true,{"b":[null,"x ` + bodySecret + `"]}]]}`,
		"basic header":   `{"a":"Basic ` + basic + `"}`,
		"basic value":    `{"a":"` + basic + `"}`,
		"second value":   `{"a":1} {"b":"` + bodySecret + `"}`,
	} {
		c := echoClient(t, body)
		resp, err := c.Do(t.Context(), Request{Path: "a"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		noSecret(t, name+": the body", string(resp.Body), basic)
		values, ok := jsonValues(resp.Body)
		if !ok || len(values) != strings.Count(body, "} {")+1 {
			t.Fatalf("%s: the body %s is not the answer's JSON values", name, resp.Body)
		}
		var texts []string
		for _, v := range values {
			collectStrings(v, &texts)
		}
		decoded := strings.Join(texts, " ")
		noSecret(t, name+": the decoded strings", decoded, basic)
		if !strings.Contains(decoded, secret.Redacted) {
			t.Errorf("%s: %q holds no %s", name, decoded, secret.Redacted)
		}
		if name == "raw" && (!bytes.Contains(resp.Body, []byte("12345678901234567890")) || !bytes.Contains(resp.Body, []byte(`"<&>"`))) {
			t.Errorf("raw: a number or markup changed: %s", resp.Body)
		}
		if name == "second value" {
			continue // JSON decodes one value
		}
		var out any
		if _, err := c.JSON(t.Context(), Request{Path: "a"}, &out); err != nil {
			t.Fatalf("%s: JSON: %v", name, err)
		}
		got, _ := json.Marshal(out)
		noSecret(t, name+": JSON's value", string(got), basic)
	}

	c := echoClient(t, `<html>`+bodySecret+` and Basic `+basic+`</html>`)
	resp, err := c.Do(t.Context(), Request{Path: "a"})
	if err != nil || string(resp.Body) != "<html>"+secret.Redacted+" and "+secret.Redacted+"</html>" {
		t.Fatalf("a body that is not JSON: %q %v", resp.Body, err)
	}
}

// echoClient is a client sending a Basic credential of bodySecret to a
// tracker that answers GET /a with 200 and body.
func echoClient(t *testing.T, body string) *Client {
	t.Helper()
	tr := newTracker(t, route{"GET /a": func(w http.ResponseWriter, r *http.Request) { writeString(w, body) }})
	opts := testOptions(tr.url())
	opts.Credentials = StaticCredentials(Basic("user-1", secret.New([]byte(bodySecret))))
	c, _ := loopbackClient(t, opts)
	return c
}

func noSecret(t *testing.T, what, text, basic string) {
	t.Helper()
	if strings.Contains(text, bodySecret) || strings.Contains(text, basic) {
		t.Errorf("%s repeats the credential: %s", what, text)
	}
}

// bodyScrubber is the scrubber of a request that carried a Basic
// credential of bodySecret.
func bodyScrubber(t *testing.T) *scrubber {
	t.Helper()
	s := &scrubber{forms: Basic("user-1", secret.New([]byte(bodySecret))).forms()}
	t.Cleanup(s.clear)
	return s
}

// A body that holds no form of the credential is handed over as the
// slice read, with no copy and, unless an escape could spell part of a
// form, no allocation.
func TestCleanBodyIsKeptAsRead(t *testing.T) {
	s := bodyScrubber(t)
	none := &scrubber{}
	clean := []byte(`{"issues":[{"key":"WEB-1","fields":{"summary":"line one` + bs + `nline ` + bs + `"two` + bs + `"","n":1.50}}],"isLast":true}`)
	escaped := []byte(`{"summary":"caf` + bs + `u00e9"}`)
	notJSON := []byte(`<html>Service unavailable</html>`)
	for _, tc := range []struct {
		name   string
		s      *scrubber
		body   []byte
		allocs float64
	}{
		{"clean", s, clean, 0},
		{"not JSON", s, notJSON, 0},
		{"no credential carried", none, escaped, 0},
		// A \u escape could spell any byte, so it is decoded to be sure.
		{"harmless escape", s, escaped, -1},
	} {
		out, err := tc.s.body(tc.body)
		if err != nil || len(out) != len(tc.body) || &out[0] != &tc.body[0] {
			t.Errorf("%s: got a copy or an error: %q %v", tc.name, out, err)
		}
		if tc.allocs < 0 {
			continue
		}
		if n := testing.AllocsPerRun(100, func() { _, _ = tc.s.body(tc.body) }); n != tc.allocs {
			t.Errorf("%s: %v allocations, want %v", tc.name, n, tc.allocs)
		}
	}
	if out, err := s.body(nil); out != nil || err != nil {
		t.Errorf("an empty body: %q %v", out, err)
	}
}

// A credential that reads as a JSON number is outside every string: it
// is replaced as bytes, and the body no longer decodes.
func TestCredentialOutsideStrings(t *testing.T) {
	t.Parallel()
	s := &scrubber{forms: [][]byte{[]byte("4242424242")}}
	for _, body := range []string{`{"a":"x","n":4242424242}`, `[1,4242424242]`} {
		out, err := s.body([]byte(body))
		if err != nil || bytes.Contains(out, []byte("4242424242")) || json.Valid(out) {
			t.Errorf("%s: got %s %v, want the number replaced", body, out, err)
		}
	}
}

// A form holding a quote is written escaped once encoded again, where no
// byte search finds it: each string, object keys included, is scrubbed
// as it decodes.
func TestCredentialThatJSONEscapes(t *testing.T) {
	t.Parallel()
	const form = `tok"en`
	s := &scrubber{forms: [][]byte{[]byte(form)}}
	q := bs + `"`
	out, err := s.body([]byte(`{"tok` + q + `en":{"v":["x tok` + q + `en"]}}`))
	values, ok := jsonValues(out)
	if err != nil || !ok {
		t.Fatalf("got %s %v", out, err)
	}
	var texts []string
	collectStrings(values[0], &texts)
	if joined := strings.Join(texts, " "); strings.Contains(joined, form) || strings.Count(joined, secret.Redacted) != 2 {
		t.Errorf("decoded strings %q, want both forms replaced", joined)
	}
}

// Once the credential is dropped, a body with any string cannot be
// checked and is refused; one without a string cannot hold it and is kept.
func TestBodyAfterTheCredentialIsGone(t *testing.T) {
	t.Parallel()
	lost := &scrubber{lost: true}
	for _, body := range []string{`{"a":1}`, `["x"]`, `<html>x</html>`, `"x"`} {
		if out, err := lost.body([]byte(body)); !errors.Is(err, ErrCredentialChanged) || out != nil {
			t.Errorf("%s: %q %v, want ErrCredentialChanged", body, out, err)
		}
	}
	for _, body := range []string{`{}`, `[1,2,true,null]`, `3`, ``} {
		if out, err := lost.body([]byte(body)); err != nil || string(out) != body {
			t.Errorf("%s: %q %v, want it kept", body, out, err)
		}
	}

	var c *Client
	tr := newTracker(t, route{"POST /a": func(w http.ResponseWriter, r *http.Request) {
		c.ForgetCredential()
		writeString(w, `{"id":"1"}`)
	}})
	opts := testOptions(tr.url())
	c, _ = loopbackClient(t, opts)
	resp, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "a", Body: map[string]string{}})
	if !errors.Is(err, ErrCredentialChanged) || WriteNotApplied(err) || resp == nil || resp.Status != http.StatusOK || resp.Body != nil {
		t.Fatalf("Do = %+v %v, want ErrCredentialChanged with the write's outcome unknown and the answer without its body", resp, err)
	}
}

// The answer as read is cleared once a scrubbed copy replaces it, so only
// the scrubbed body outlives the call.
func TestScrubbedAnswerIsCleared(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	cred := Bearer(secret.New([]byte(bodySecret)))
	for _, tc := range []struct {
		body    string
		cleared bool
	}{
		{`{"a":"` + bodySecret + `"}`, true},
		{`{"a":"clean"}`, false},
	} {
		raw := []byte(tc.body)
		resp, err := c.response(&call{method: http.MethodGet}, &answer{status: http.StatusOK, body: raw, sent: sentWith{carried: true, cred: cred}})
		if err != nil || strings.Contains(string(resp.Body), bodySecret) {
			t.Fatalf("%s: %q %v", tc.body, resp.Body, err)
		}
		if isZero(raw) != tc.cleared {
			t.Errorf("%s: the answer as read is %q, want cleared %v", tc.body, raw, tc.cleared)
		}
	}
}
