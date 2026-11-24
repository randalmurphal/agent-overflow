package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/errorsx"
)

// stubTracker records what the tracker routes hand the app side. An
// upload reads its body to the end, as the tracker request does.
type stubTracker struct {
	mu        sync.Mutex
	uploads   []TrackerUpload
	bodies    []string
	attachErr error

	downloads   []string
	content     string
	downloadErr error
}

func (s *stubTracker) AttachTrackerFile(upload TrackerUpload) (json.RawMessage, error) {
	body, err := io.ReadAll(upload.Body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, upload)
	s.bodies = append(s.bodies, string(body))
	if s.attachErr != nil {
		return nil, s.attachErr
	}
	return json.RawMessage(`{"parts":[{"state":"applied"}]}`), nil
}

func (s *stubTracker) DownloadTrackerAttachment(_ context.Context, connection, issue, attachment string, to io.Writer) error {
	s.mu.Lock()
	s.downloads = append(s.downloads, connection+"/"+issue+"/"+attachment)
	s.mu.Unlock()
	if s.content != "" {
		if _, err := io.WriteString(to, s.content); err != nil {
			return err
		}
	}
	return s.downloadErr
}

func (s *stubTracker) uploadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

func trackerFixture(t *testing.T, stub TrackerTransfer) *serverFixture {
	t.Helper()
	return newServerFixtureWith(t, func(cfg *Config) { cfg.TrackerTransfer = stub })
}

func readTransferError(t *testing.T, resp *http.Response) trackerTransferError {
	t.Helper()
	var got trackerTransferError
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode the failure body: %v", err)
	}
	return got
}

// A file lands with the connection, ticket, name and length its ticket
// names and the bytes the request carried, answered with the write's
// outcome; the ticket is spent once.
func TestTrackerUploadDeliversTheTicketedFile(t *testing.T) {
	stub := &stubTracker{}
	f := trackerFixture(t, stub)
	relative, err := f.srv.MintTrackerUploadTicket("conn-1", "10101", "notes.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(relative, "/attachments/tracker/upload?ticket=") {
		t.Fatalf("minted URL %q is outside the attachment subtree", relative)
	}
	resp := put(t, f.srv.Addr(), relative, strings.NewReader("hello"), 5)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"parts":[{"state":"applied"}]}` {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", resp.Header)
	}
	u := stub.uploads[0]
	if u.Connection != "conn-1" || u.Issue != "10101" || u.Filename != "notes.txt" || u.Size != 5 || stub.bodies[0] != "hello" || u.Context == nil {
		t.Fatalf("delivered %+v %q", u, stub.bodies[0])
	}
	if again := put(t, f.srv.Addr(), relative, strings.NewReader("hello"), 5); again.StatusCode != http.StatusNotFound || stub.uploadCount() != 1 {
		t.Fatalf("a reused ticket: status %d, %d uploads", again.StatusCode, stub.uploadCount())
	}
	// A download ticket is no upload's, and a forged one nobody's.
	down, err := f.srv.MintTrackerDownloadTicket("conn-1", "10101", "501", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if resp := put(t, f.srv.Addr(), "/attachments/tracker/upload"+down[strings.Index(down, "?"):], strings.NewReader("hello"), 5); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a download ticket at the upload: %d", resp.StatusCode)
	}
	if resp := put(t, f.srv.Addr(), "/attachments/tracker/upload?ticket=forged", strings.NewReader("hello"), 5); resp.StatusCode != http.StatusNotFound || stub.uploadCount() != 1 {
		t.Fatalf("a forged ticket: %d", resp.StatusCode)
	}
}

// A declared length other than the ticket's never reaches the tracker; a
// streamed body that runs long fails its read at the ticket's length.
func TestTrackerUploadRefusesAnotherLength(t *testing.T) {
	stub := &stubTracker{}
	f := trackerFixture(t, stub)
	mint := func() string {
		relative, err := f.srv.MintTrackerUploadTicket("conn-1", "10101", "notes.txt", 5)
		if err != nil {
			t.Fatal(err)
		}
		return relative
	}
	if resp := put(t, f.srv.Addr(), mint(), strings.NewReader("hell"), 4); resp.StatusCode != http.StatusBadRequest || stub.uploadCount() != 0 {
		t.Fatalf("a declared short body: status %d, %d uploads", resp.StatusCode, stub.uploadCount())
	}
	resp := put(t, f.srv.Addr(), mint(), io.MultiReader(strings.NewReader("hello, world")), -1)
	if resp.StatusCode != http.StatusInternalServerError || stub.uploadCount() != 0 {
		t.Fatalf("a streamed long body: status %d, %d uploads", resp.StatusCode, stub.uploadCount())
	}
}

// A refusal the app worded is answered with its code and sentence and a
// 400; any other failure with a 500.
func TestTrackerUploadRefusals(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{errorsx.Public("tracker_connection_off", "this tracker connection is turned off", errors.New("off")), http.StatusBadRequest, "tracker_connection_off"},
		{errors.New("the store failed"), http.StatusInternalServerError, ErrCodeMethodError},
	} {
		f := trackerFixture(t, &stubTracker{attachErr: tc.err})
		relative, err := f.srv.MintTrackerUploadTicket("conn-1", "10101", "notes.txt", 5)
		if err != nil {
			t.Fatal(err)
		}
		resp := put(t, f.srv.Addr(), relative, strings.NewReader("hello"), 5)
		if got := readTransferError(t, resp); resp.StatusCode != tc.status || got.Code != tc.code || got.Message == "" {
			t.Errorf("%v: status %d, %+v", tc.err, resp.StatusCode, got)
		}
	}
}

// An attachment is a save whatever it holds, with no declared length (a
// body larger than net/http's buffer, which would otherwise count a small
// one); the path must agree with the ticket, which is spent once.
func TestTrackerDownloadServesASave(t *testing.T) {
	stub := &stubTracker{content: "<html><script>alert(1)</script></html>" + strings.Repeat("x", 64<<10)}
	f := trackerFixture(t, stub)
	relative, err := f.srv.MintTrackerDownloadTicket("conn-1", "10101", "501", "page.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(relative, "/attachments/tracker/501?ticket=") {
		t.Fatalf("minted URL %q", relative)
	}
	resp := get(t, f.srv.Addr(), relative, nil)
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != stub.content {
		t.Fatalf("status %d body %q %v", resp.StatusCode, body, err)
	}
	h := resp.Header
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("Content-Disposition") != `attachment; filename=page.html` ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" || resp.ContentLength != -1 {
		t.Fatalf("headers %v, length %d", h, resp.ContentLength)
	}
	if stub.downloads[0] != "conn-1/10101/501" {
		t.Fatalf("downloaded %v", stub.downloads)
	}
	if again := get(t, f.srv.Addr(), relative, nil); again.StatusCode != http.StatusNotFound {
		t.Fatalf("a reused ticket: %d", again.StatusCode)
	}
	other, err := f.srv.MintTrackerDownloadTicket("conn-1", "10101", "501", "page.html")
	if err != nil {
		t.Fatal(err)
	}
	if resp := get(t, f.srv.Addr(), strings.Replace(other, "/501?", "/502?", 1), nil); resp.StatusCode != http.StatusNotFound || len(stub.downloads) != 1 {
		t.Fatalf("another attachment's path: %d", resp.StatusCode)
	}
}

// A failure before the first byte is answered with its sentence; one after
// it cuts the response, which the client reads as a failed transfer; an
// empty attachment is an empty save.
func TestTrackerDownloadFailures(t *testing.T) {
	download := func(stub *stubTracker) *http.Response {
		f := trackerFixture(t, stub)
		relative, err := f.srv.MintTrackerDownloadTicket("conn-1", "10101", "501", "a.txt")
		if err != nil {
			t.Fatal(err)
		}
		return get(t, f.srv.Addr(), relative, nil)
	}
	resp := download(&stubTracker{downloadErr: errorsx.Public("tracker_failed_not_found", "Jira did not return this attachment", errors.New("404"))})
	if got := readTransferError(t, resp); resp.StatusCode != http.StatusBadRequest || got.Code != "tracker_failed_not_found" ||
		resp.Header.Get("Content-Disposition") != "" {
		t.Fatalf("a refusal before the first byte: status %d, %+v, headers %v", resp.StatusCode, got, resp.Header)
	}
	resp = download(&stubTracker{content: strings.Repeat("x", 64<<10), downloadErr: errors.New("the tracker reset the connection")})
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Fatalf("a download cut after its first byte read whole: %d bytes", len(body))
	}
	resp = download(&stubTracker{})
	if body, err := io.ReadAll(resp.Body); err != nil || resp.StatusCode != http.StatusOK || len(body) != 0 ||
		resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("an empty attachment: status %d, %q %v", resp.StatusCode, body, err)
	}
}

// A value that cannot ride a subject is refused where it enters.
func TestTrackerTicketsRefuseUnencodableValues(t *testing.T) {
	f := trackerFixture(t, &stubTracker{})
	for _, mint := range []func() (string, error){
		func() (string, error) { return f.srv.MintTrackerUploadTicket("", "10101", "a.txt", 1) },
		func() (string, error) { return f.srv.MintTrackerUploadTicket("conn-1", "10101", "a\x00b.txt", 1) },
		func() (string, error) { return f.srv.MintTrackerUploadTicket("conn-1", "10101", "a.txt", -1) },
		func() (string, error) { return f.srv.MintTrackerDownloadTicket("conn-1", "", "501", "a.txt") },
		func() (string, error) { return f.srv.MintTrackerDownloadTicket("conn-1", "10101", "501", "a\x00b") },
	} {
		if _, err := mint(); err == nil {
			t.Error("an unencodable value was minted")
		}
	}
}
