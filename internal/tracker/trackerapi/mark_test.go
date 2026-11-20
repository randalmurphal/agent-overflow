package trackerapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// Answers drawn under the credential from one read of the source carry
// equal marks; a read that replaced the credential, a re-read after
// ForgetCredential and another client give different ones; a request off
// the base origin carries the zero mark.
func TestCredentialMarks(t *testing.T) {
	t.Parallel()
	media := newTracker(t, route{
		"GET /file/1": reply(http.StatusOK),
		"GET /file/2": reply(http.StatusBadRequest),
	})
	tr := newTracker(t, route{
		"GET /ok":           reply(http.StatusOK),
		"GET /issue/WEB-99": reply(http.StatusNotFound),
		"GET /me":           failing(1, reply(http.StatusUnauthorized), reply(http.StatusOK)),
	})
	opts := testOptions(tr.url())
	opts.Credentials = &testSource{secrets: []string{testSecret, testSecret2}}
	c, _ := loopbackClient(t, opts)
	mark := func(r Request) CredentialMark {
		t.Helper()
		resp, err := c.Do(t.Context(), r)
		if status, ok := errors.AsType[*StatusError](err); ok {
			return status.CredentialMark()
		}
		if err != nil {
			t.Fatal(err)
		}
		return resp.CredentialMark()
	}

	if c.CredentialMark() != (CredentialMark{}) {
		t.Fatal("a client that read no credential has a mark")
	}
	first := mark(Request{Path: "ok"})
	if first == (CredentialMark{}) || mark(Request{Path: "issue/WEB-99"}) != first {
		t.Fatal("a 404 and a 200 under the first read carry different marks, or a zero one")
	}
	if c.CredentialMark() != first {
		t.Fatal("the client's mark is not the mark of the answers it draws")
	}
	// The 401 under the first read makes the client read again and send
	// once more: the answer the caller gets is the retry's.
	replaced := mark(Request{Path: "me"})
	if replaced == first || mark(Request{Path: "ok"}) != replaced {
		t.Fatal("the retried call's mark is not the replacing read's")
	}
	if c.CredentialMark() != replaced {
		t.Fatal("the client's mark did not follow the replacing read")
	}
	c.ForgetCredential()
	if c.CredentialMark() != (CredentialMark{}) {
		t.Fatal("the client has a mark after ForgetCredential")
	}
	again := mark(Request{Path: "ok"})
	if again == replaced || again == (CredentialMark{}) || c.CredentialMark() != again {
		t.Fatal("a read after ForgetCredential kept the earlier mark")
	}
	for _, path := range []string{"/file/1", "/file/2"} {
		if got := mark(Request{Path: media.srv.URL + path, Absolute: true}); got != (CredentialMark{}) {
			t.Fatalf("%s off the base origin: mark %v, want zero", path, got)
		}
	}
	if c.CredentialMark() != again {
		t.Fatal("an answer off the base origin moved the client's mark")
	}
	defer func() {
		c.Close()
		if c.CredentialMark() != (CredentialMark{}) {
			t.Error("a closed client has a mark")
		}
	}()

	other, _ := loopbackClient(t, testOptions(tr.url()))
	resp, err := other.Do(t.Context(), Request{Path: "ok"})
	if err != nil || resp.CredentialMark() == first {
		t.Fatalf("another client's first read: %v, marked as this client's", err)
	}
	if (*Response)(nil).CredentialMark() != (CredentialMark{}) || (*StatusError)(nil).CredentialMark() != (CredentialMark{}) ||
		(*AuthError)(nil).CredentialMark() != (CredentialMark{}) {
		t.Fatal("a nil answer has a mark")
	}
}

// A re-read that returns an equal credential keeps the one held: the
// refusal it ends in, and the answers after it, carry the mark of the
// answers before it.
func TestCredentialMarkSurvivesAnEqualReread(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /ok":     reply(http.StatusOK),
		"GET /denied": reply(http.StatusUnauthorized),
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	resp, err := c.Do(t.Context(), Request{Path: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(t.Context(), Request{Path: "denied"})
	refusal, ok := errors.AsType[*AuthError](err)
	if !ok || refusal.CredentialMark() != resp.CredentialMark() {
		t.Fatalf("%v: the refusal after an equal re-read is marked as another read", err)
	}
	after, err := c.Do(t.Context(), Request{Path: "ok"})
	if err != nil || after.CredentialMark() != resp.CredentialMark() {
		t.Fatalf("%v: an answer after an equal re-read is marked as another read", err)
	}
}

// A mark prints nothing of the credential or the user through any
// formatter or encoder.
func TestCredentialMarkIsUnprintable(t *testing.T) {
	t.Parallel()
	const raw = "s3cr3t-value-0042"
	tr := newTracker(t, route{"GET /ok": reply(http.StatusOK)})
	opts := testOptions(tr.url())
	opts.Credentials = StaticCredentials(Basic("user-1", secret.New([]byte(raw))))
	c, _ := loopbackClient(t, opts)
	resp, err := c.Do(t.Context(), Request{Path: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.CredentialMark()
	type holder struct {
		Mark   CredentialMark
		Ptr    *CredentialMark
		hidden CredentialMark
	}
	for _, v := range []any{m, &m, holder{Mark: m, Ptr: &m, hidden: m}} {
		for _, out := range printed(t, v) {
			for _, form := range append(secretForms(raw), "user-1") {
				if strings.Contains(out, form) {
					t.Fatalf("printing %T shows %q: %s", v, form, out)
				}
			}
		}
	}
}
