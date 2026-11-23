package trackerapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/secret"
)

// The cost benchmarks measure what the transport spends on one answer:
// the credential scrub of a success body (clean, and holding a \u escape
// that forces the JSON walk), the body read, the per-request overhead of
// a call, and what a new TLS connection costs with and without session
// resumption. Sizes follow a Jira search page of 100 issues (about 200
// KB).

// costPage is a JSON body shaped like a page of Jira issues: n issues
// with nested objects, ADF descriptions whose text holds quotes and line
// breaks (the \" and \n escapes every real body has) and, when special
// is set, one character of special in each description, written as the
// encoder writes it.
func costPage(n int, special string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"isLast":false,"nextPageToken":"token-1","issues":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"%d","key":"WEB-%d","self":"https://example.atlassian.net/rest/api/3/issue/%d","fields":{`, 10000+i, i+1, 10000+i)
		fmt.Fprintf(&b, `"summary":"Issue %d with a title of ordinary length",`, i+1)
		b.WriteString(`"status":{"self":"https://example.atlassian.net/rest/api/3/status/10001","description":"","iconUrl":"https://example.atlassian.net/images/icons/statuses/generic.png","name":"In Progress","id":"10001","statusCategory":{"self":"https://example.atlassian.net/rest/api/3/statuscategory/4","id":4,"key":"indeterminate","colorName":"yellow","name":"In Progress"}},`)
		b.WriteString(`"assignee":{"self":"https://example.atlassian.net/rest/api/3/user?accountId=acc-user1","accountId":"acc-user1","avatarUrls":{"48x48":"https://avatar.example.com/48.png","24x24":"https://avatar.example.com/24.png","16x16":"https://avatar.example.com/16.png","32x32":"https://avatar.example.com/32.png"},"displayName":"User 1","active":true,"timeZone":"UTC","accountType":"atlassian"},`)
		b.WriteString(`"priority":{"self":"https://example.atlassian.net/rest/api/3/priority/3","iconUrl":"https://example.atlassian.net/images/icons/priorities/medium.svg","name":"Medium","id":"3"},`)
		b.WriteString(`"labels":["frontend"],"created":"2026-11-01T09:00:00.000+0000","updated":"2026-11-18T09:00:00.000+0000","customfield_10019":"0|i0001b:",`)
		b.WriteString(`"description":{"type":"doc","version":1,"content":[`)
		for p := range 3 {
			if p > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"type":"paragraph","content":[{"type":"text","text":"Paragraph with \"quoted\" words\nand a second line that describes the work in plain words`)
			if p == 0 {
				b.WriteString(special)
			}
			b.WriteString(`."}]}`)
		}
		b.WriteString(`]}}}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// basicScrubber is the scrubber of an answer to a request that carried
// a Basic credential, as Do and JSON make for every answer.
func basicScrubber() (*scrubber, *Credential) {
	cred := Basic("user1@example.com", secret.New([]byte("ATATT3xFfGF0-invented-token-value-0001")))
	return scrubberFor(cred), cred
}

func benchScrubBody(b *testing.B, body []byte) {
	s, _ := basicScrubber()
	defer s.clear()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		out, err := s.body(body)
		if err != nil || len(out) == 0 {
			b.Fatal(err)
		}
	}
}

// BenchmarkScrubBody is Response.Body's scrub of a 100-issue page: a
// clean body, one whose descriptions each hold a character an encoder
// writes as a \u escape (an HTML-escaping encoder's < as <, a
// non-ASCII letter as é), and one whose single description holds it.
func BenchmarkScrubBody(b *testing.B) {
	bs := string(rune(0x5c))
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"clean", costPage(100, "")},
		{"lt-escaped-every-issue", costPage(100, " a "+bs+"u003c b")},
		{"nonascii-escaped-every-issue", costPage(100, " caf"+bs+"u00e9")},
		{"nonascii-raw-utf8", costPage(100, " café")},
		{"one-escape-in-page", append(costPage(99, ""), []byte(`,{"x":"`+bs+`u003c"}`)...)},
	} {
		b.Run(fmt.Sprintf("%s-%dKB", c.name, len(c.body)>>10), func(b *testing.B) { benchScrubBody(b, c.body) })
	}
}

// BenchmarkMayHold is the scan that lets a clean body through without a
// copy: bytes.Contains for each of the credential's forms, then the
// escape walk.
func BenchmarkMayHold(b *testing.B) {
	body := costPage(100, "")
	s, _ := basicScrubber()
	defer s.clear()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if s.mayHold(body) {
			b.Fatal("a clean body may hold the credential")
		}
	}
}

// BenchmarkMayHoldOneForm is mayHold's form search with the one form a
// Basic credential's other two contain (the token alone): a body holding
// "Basic <encoded>" or "<encoded>" holds neither unless it holds the
// encoded form, so two of the three searches can only repeat the third's
// answer for the encoded pair. It measures a pass per form.
func BenchmarkMayHoldOneForm(b *testing.B) {
	body := costPage(100, "")
	s, _ := basicScrubber()
	defer s.clear()
	one := scrubberOf(s.form(1), s.form(2))
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if one.mayHold(body) {
			b.Fatal("a clean body may hold the credential")
		}
	}
}

// BenchmarkScrubberForms is what every answer, OnAnswer report and
// wrapped error pays for its scrubber: the credential's forms, copied
// into a scrubber on the stack and cleared.
func BenchmarkScrubberForms(b *testing.B) {
	_, cred := basicScrubber()
	b.ReportAllocs()
	for b.Loop() {
		var s scrubber
		s.load(cred)
		s.clear()
	}
}

// unsized hides a reader's length, as a gzip-decoded body does.
type unsized struct{ r io.Reader }

func (u unsized) Read(p []byte) (int, error) { return u.r.Read(p) }

// BenchmarkReadBody is readUpTo of a 200 KB body whose length the reader
// does not know (a compressed answer) and of one whose length the answer
// declared, against one read into a buffer sized once.
func BenchmarkReadBody(b *testing.B) {
	body := costPage(100, "")
	b.Run("readUpTo", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for b.Loop() {
			got, err := readUpTo(unsized{bytes.NewReader(body)}, -1, defaultMaxBodyBytes)
			if err != nil || len(got) != len(body) {
				b.Fatal(err)
			}
		}
	})
	b.Run("readUpTo-declared", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for b.Loop() {
			got, err := readUpTo(unsized{bytes.NewReader(body)}, int64(len(body)), defaultMaxBodyBytes)
			if err != nil || len(got) != len(body) {
				b.Fatal(err)
			}
		}
	})
	b.Run("sized-buffer", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for b.Loop() {
			var buf bytes.Buffer
			buf.Grow(len(body) + bytes.MinRead)
			if _, err := buf.ReadFrom(unsized{bytes.NewReader(body)}); err != nil || buf.Len() != len(body) {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkCall is one Do of a small and a 200 KB answer over an
// in-memory server, against a bare http.Client on the same server: the
// difference is the transport's own cost per call.
func BenchmarkCall(b *testing.B) {
	for _, size := range []struct {
		name string
		body []byte
	}{{"1KB", costPage(1, "")}, {"200KB", costPage(100, "")}} {
		handler := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(size.body)
		}
		b.Run(size.name+"/trackerapi", func(b *testing.B) {
			srv := httptest.NewTestServer(b, http.HandlerFunc(handler))
			c, err := newClient(testOptions("https://example.atlassian.net/"), srv.Client().Transport)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(c.Close)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				resp, err := c.Do(ctx, Request{Path: "rest/api/3/search/jql"})
				if err != nil || len(resp.Body) != len(size.body) {
					b.Fatal(err)
				}
			}
		})
		b.Run(size.name+"/bare-http", func(b *testing.B) {
			srv := httptest.NewTestServer(b, http.HandlerFunc(handler))
			hc := srv.Client()
			b.ReportAllocs()
			for b.Loop() {
				resp, err := hc.Get("https://example.atlassian.net/rest/api/3/search/jql")
				if err != nil {
					b.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || len(got) != len(size.body) {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTLSConnect is one new connection's TLS handshake to a local
// server, as the transport's TLS settings make it (no session cache, so
// every connection verifies the chain in full) and with a client session
// cache, which resumes the session on later connections.
func BenchmarkTLSConnect(b *testing.B) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	b.Cleanup(srv.Close)
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	addr := srv.Listener.Addr().String()
	for _, c := range []struct {
		name  string
		cache tls.ClientSessionCache
	}{{"full", nil}, {"resumed", tls.NewLRUClientSessionCache(8)}} {
		b.Run(c.name, func(b *testing.B) {
			cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "example.com", ClientSessionCache: c.cache}
			var resumed atomic.Int64
			b.ReportAllocs()
			for b.Loop() {
				raw, err := net.Dial("tcp", addr)
				if err != nil {
					b.Fatal(err)
				}
				conn := tls.Client(raw, cfg)
				if err := conn.Handshake(); err != nil {
					b.Fatal(err)
				}
				// A TLS 1.3 ticket arrives after the handshake; one read,
				// untimed, takes it in before the next connection asks.
				b.StopTimer()
				if conn.ConnectionState().DidResume {
					resumed.Add(1)
				}
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Millisecond))
				_, _ = conn.Read(make([]byte, 1))
				_ = conn.Close()
				b.StartTimer()
			}
			b.ReportMetric(float64(resumed.Load())/float64(b.N), "resumed/op")
		})
	}
}

// TestRequestsAskForCompressedAnswers proves that every request the
// production transport sends asks for gzip and that a gzip answer
// reaches the caller decoded: the transport leaves Go's transparent
// compression on.
func TestRequestsAskForCompressedAnswers(t *testing.T) {
	var asked atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := loopbackClient(t, testOptions(srv.URL))
	if _, err := c.Do(t.Context(), Request{Path: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := asked.Load().(string); !strings.Contains(got, "gzip") {
		t.Fatalf("Accept-Encoding %q, want gzip", got)
	}
}
