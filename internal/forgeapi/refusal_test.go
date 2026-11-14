package forgeapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A write the forge answered 401 stays refused when re-reading the login
// then fails: the error proves nothing was applied, keeps the 401 and
// carries the re-read's failure, classified as it was alone. A read's
// error is the re-read's failure alone, as before.
func TestRefusedWriteKeepsItsRefusalWhenTheLoginRereadFails(t *testing.T) {
	t.Parallel()
	reread := &TransientError{Err: errors.New("reading the github login timed out after 1m0s: keyring locked")}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/user") {
					_, _ = fmt.Fprint(w, `{"id":1}`)
					return
				}
				http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			}))
			defer srv.Close()
			src := &testSource{tokens: []string{testSecret}}
			s, _ := liveService(t, src, srv)
			gh := s.GitHub("github.com")
			if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil {
				t.Fatal(err)
			}
			src.set(reread)
			request := Request{Method: method, Path: "repos/o/r/pulls/1/reviews"}
			if method == http.MethodPost {
				request.Body = map[string]string{"event": "COMMENT"}
			}
			_, err := gh.Do(t.Context(), request)
			if _, ok := errors.AsType[*TransientError](err); !ok || !strings.Contains(err.Error(), "keyring locked") {
				t.Fatalf("err = %v; want the re-read's transient failure in it", err)
			}
			refusal, refused := errors.AsType[*RefusalError](err)
			status, hasStatus := errors.AsType[*StatusError](err)
			if method == http.MethodGet {
				if refused || hasStatus {
					t.Fatalf("a read's err = %v; want the re-read's failure alone", err)
				}
				return
			}
			if !refused || refusal.Step != RefusalStepCredential || !WriteNotApplied(err) {
				t.Fatalf("err = %v; want a credential RefusalError that proves nothing was applied", err)
			}
			if !hasStatus || status.Status != http.StatusUnauthorized {
				t.Fatalf("err = %v; want the write's 401 readable", err)
			}
		})
	}
}

// A write's 4xx whose body is cut short is still the forge's refusal: it
// settles by its status. A read's cut body is transient, as before.
func TestRefusedWriteWithACutBodyIsStillRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			panic(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 422 Unprocessable Entity\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"message\":")
		_ = buf.Flush()
	}))
	defer srv.Close()
	gh := isolatedService(t, srv).GitHub("github.com")
	_, err := gh.Do(t.Context(), Request{Method: http.MethodPost, Path: "repos/o/r/pulls/1/reviews", Body: map[string]string{}})
	if status, ok := errors.AsType[*StatusError](err); !ok || status.Status != http.StatusUnprocessableEntity || !WriteNotApplied(err) {
		t.Fatalf("a write's cut 422 = %v; want the 422, not applied", err)
	}
	_, err = gh.Do(t.Context(), Request{Path: "repos/o/r/pulls/1/reviews"})
	if _, ok := errors.AsType[*TransientError](err); !ok {
		t.Fatalf("a read's cut 422 = %v; want transient", err)
	}
	if _, ok := errors.AsType[*StatusError](err); ok {
		t.Fatalf("a read's cut 422 = %v; want no status", err)
	}
}
