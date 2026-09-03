package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubTransfer is a recording AttachmentTransfer. It answers whatever the
// test arranged and remembers what the route handed it — which is the
// half worth pinning, because this package owns the carriers and the
// ticket, never the decision about what an attachment is.
type stubTransfer struct {
	mu sync.Mutex
	// stored is every upload the route delivered, body already drained.
	stored []storedUpload
	// content is what OpenAttachment answers for any id.
	content []byte
	mime    string
	modTime time.Time
	// opened records the (thread, attachment) pairs the route asked for.
	opened []string
	// openErr, when set, is what OpenAttachment answers instead.
	openErr error
	// storeErr, when set, is what StoreAttachment answers after draining.
	storeErr error
}

type storedUpload struct {
	req  AttachmentUpload
	body []byte
	err  error
}

func (s *stubTransfer) OpenAttachment(threadID, attachmentID string) (AttachmentContent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = append(s.opened, threadID+"/"+attachmentID)
	if s.openErr != nil {
		return AttachmentContent{}, s.openErr
	}
	mime := s.mime
	if mime == "" {
		mime = "image/png"
	}
	modTime := s.modTime
	if modTime.IsZero() {
		modTime = time.Unix(1_700_000_000, 0)
	}
	return AttachmentContent{
		MimeType: mime,
		ModTime:  modTime,
		Content:  nopSeekCloser{bytes.NewReader(s.content)},
	}, nil
}

func (s *stubTransfer) StoreAttachment(req AttachmentUpload) (json.RawMessage, error) {
	// Drain OUTSIDE the lock: the route's cap is enforced during this
	// read, and holding a mutex across it would say something untrue
	// about where the time goes.
	body, readErr := io.ReadAll(req.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	captured := req
	captured.Body = nil
	s.stored = append(s.stored, storedUpload{req: captured, body: body, err: readErr})
	if readErr != nil {
		return nil, fmt.Errorf("read body: %w", readErr)
	}
	if s.storeErr != nil {
		return nil, s.storeErr
	}
	if int64(len(body)) != req.Size {
		return nil, fmt.Errorf("body delivered %d bytes, declared %d", len(body), req.Size)
	}
	return json.RawMessage(fmt.Sprintf(`{"id":"att-1","threadId":%q,"size":%d}`, req.ThreadID, req.Size)), nil
}

func (s *stubTransfer) lastStored(t *testing.T) storedUpload {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stored) == 0 {
		t.Fatal("the route never reached the transfer seam")
	}
	return s.stored[len(s.stored)-1]
}

// nopSeekCloser adapts a *bytes.Reader to the ReadSeekCloser the seam
// hands over. Seekable is the requirement, because that is what buys
// Range handling from http.ServeContent.
type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// attachmentFixture is a started server whose transfer seam is the stub.
func attachmentFixture(t *testing.T, stub *stubTransfer) *serverFixture {
	t.Helper()
	return newServerFixtureWith(t, func(cfg *Config) {
		cfg.AttachmentTransfer = stub
	})
}

func pngPayload() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	}
}

// get issues a plain GET, no credential of any kind. These routes are
// reached by the ticket on the URL and nothing else.
func get(t *testing.T, addr, relative string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+relative, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", relative, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func put(t *testing.T, addr, relative string, body io.Reader, contentLength int64) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, "http://"+addr+relative, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.ContentLength = contentLength
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put %s: %v", relative, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestAttachmentDownloadServesTheTicketedBytes is the happy path, and it
// pins the three response properties a caller depends on: the bytes, the
// content type the seam declared, and no-store.
func TestAttachmentDownloadServesTheTicketedBytes(t *testing.T) {
	payload := pngPayload()
	stub := &stubTransfer{content: payload, mime: "image/png"}
	f := attachmentFixture(t, stub)

	relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(relative, "/attachments/thr-1/att-1?") {
		t.Fatalf("minted URL %q does not name the attachment it admits", relative)
	}

	resp := get(t, f.srv.Addr(), relative, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q; a shared cache must not keep bytes a single-use ticket bought", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("served %d bytes, stored %d", len(body), len(payload))
	}
}

// TestAttachmentDownloadTicketIsSpentOnce is the property the whole
// mechanism rests on. The second presentation is a 404 and never reaches
// the seam.
func TestAttachmentDownloadTicketIsSpentOnce(t *testing.T) {
	stub := &stubTransfer{content: pngPayload()}
	f := attachmentFixture(t, stub)

	relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if resp := get(t, f.srv.Addr(), relative, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", resp.StatusCode)
	}
	resp := get(t, f.srv.Addr(), relative, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("replayed status = %d, want 404", resp.StatusCode)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.opened) != 1 {
		t.Fatalf("the seam was reached %d times; a spent ticket must be refused before any work", len(stub.opened))
	}
}

// TestAttachmentDownloadRefusesAMissingTicket covers the two shapes a
// caller with nothing produces, and pins that both answer 404 rather than
// distinguishing "never existed" from "spent".
func TestAttachmentDownloadRefusesAMissingTicket(t *testing.T) {
	stub := &stubTransfer{content: pngPayload()}
	f := attachmentFixture(t, stub)

	for _, relative := range []string{
		"/attachments/thr-1/att-1",
		"/attachments/thr-1/att-1?ticket=not-a-real-ticket",
	} {
		resp := get(t, f.srv.Addr(), relative, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", relative, resp.StatusCode)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.opened) != 0 {
		t.Fatalf("the seam was reached %d times without a live ticket", len(stub.opened))
	}
}

// TestAttachmentDownloadRefusesAPathTheTicketDoesNotName is the subject
// binding: a live ticket presented against a different attachment is
// refused rather than serving whatever the ticket said, which is what
// keeps the path from being decorative.
func TestAttachmentDownloadRefusesAPathTheTicketDoesNotName(t *testing.T) {
	stub := &stubTransfer{content: pngPayload()}
	f := attachmentFixture(t, stub)

	relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ticket := relative[strings.Index(relative, "?"):]

	for _, path := range []string{
		"/attachments/thr-1/att-OTHER",
		"/attachments/thr-OTHER/att-1",
	} {
		resp := get(t, f.srv.Addr(), path+ticket, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.opened) != 0 {
		t.Fatalf("the seam was reached %d times for an attachment the ticket did not name", len(stub.opened))
	}
}

// TestAttachmentDownloadTicketsAreNotUploadTickets pins the two-book
// split: a ticket from one book is not spendable at the other route,
// whatever its subject would parse as.
func TestAttachmentDownloadTicketsAreNotUploadTickets(t *testing.T) {
	stub := &stubTransfer{content: pngPayload()}
	f := attachmentFixture(t, stub)

	download, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint download: %v", err)
	}
	query := download[strings.Index(download, "?"):]
	resp := put(t, f.srv.Addr(), "/attachments/upload"+query, bytes.NewReader(pngPayload()), int64(len(pngPayload())))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a download ticket at the upload route: status = %d, want 404", resp.StatusCode)
	}

	upload, err := f.srv.MintAttachmentUploadTicket("thr-1", "a.png", "image/png", 16)
	if err != nil {
		t.Fatalf("mint upload: %v", err)
	}
	query = upload[strings.Index(upload, "?"):]
	got := get(t, f.srv.Addr(), "/attachments/thr-1/att-1"+query, nil)
	if got.StatusCode != http.StatusNotFound {
		t.Fatalf("an upload ticket at the download route: status = %d, want 404", got.StatusCode)
	}
}

// TestAttachmentDownloadServesARange is the Range half. Single-range is
// all the SPA needs, and it comes from http.ServeContent rather than from
// anything hand-rolled here.
func TestAttachmentDownloadServesARange(t *testing.T) {
	payload := pngPayload()
	stub := &stubTransfer{content: payload}
	f := attachmentFixture(t, stub)

	relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resp := get(t, f.srv.Addr(), relative, map[string]string{"Range": "bytes=4-11"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != fmt.Sprintf("bytes 4-11/%d", len(payload)) {
		t.Fatalf("Content-Range = %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, payload[4:12]) {
		t.Fatalf("range body = %v, want %v", body, payload[4:12])
	}
}

// TestAttachmentDownloadHidesASeamFailure: the ticket was live and its
// subject was ours, so the failure is our bookkeeping rather than a
// caller being refused — and the wire still says only 404.
func TestAttachmentDownloadHidesASeamFailure(t *testing.T) {
	stub := &stubTransfer{openErr: errors.New("attachment: id \"att-1\" not found")}
	f := attachmentFixture(t, stub)

	relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resp := get(t, f.srv.Addr(), relative, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "att-1") {
		t.Fatalf("the seam's prose reached the wire: %q", body)
	}
}

// TestAttachmentDownloadRefusesWithoutASeam covers a boot whose store is
// not open yet: the routes exist, and they answer the same 404 rather
// than a shape that says a seam is missing.
func TestAttachmentDownloadRefusesWithoutASeam(t *testing.T) {
	f := newServerFixture(t)
	if _, err := f.srv.MintAttachmentDownloadTicket("", "att-1"); err == nil {
		t.Fatal("expected a mint with no thread to be refused")
	}
	relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if resp := get(t, f.srv.Addr(), relative, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestAttachmentUploadStoresTheTicketedBody is the write happy path. The
// metadata all comes from the SUBJECT, so the assertions are about what
// the seam received rather than about what the request said.
func TestAttachmentUploadStoresTheTicketedBody(t *testing.T) {
	stub := &stubTransfer{}
	f := attachmentFixture(t, stub)
	payload := pngPayload()

	relative, err := f.srv.MintAttachmentUploadTicket("thr-1", "shot.png", "image/png", int64(len(payload)))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(relative, "/attachments/upload?") {
		t.Fatalf("minted URL %q is not the upload route", relative)
	}

	resp := put(t, f.srv.Addr(), relative, bytes.NewReader(payload), int64(len(payload)))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 200", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var created struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created row: %v", err)
	}
	if created.ID == "" || created.ThreadID != "thr-1" {
		t.Fatalf("created row = %+v; the route must answer the row the seam made", created)
	}

	stored := stub.lastStored(t)
	if stored.req.ThreadID != "thr-1" || stored.req.Filename != "shot.png" || stored.req.MimeType != "image/png" {
		t.Fatalf("metadata = %+v; every field must come from the ticket subject", stored.req)
	}
	if stored.req.Size != int64(len(payload)) || !bytes.Equal(stored.body, payload) {
		t.Fatalf("delivered %d bytes for a declared %d", len(stored.body), stored.req.Size)
	}
}

// TestAttachmentUploadTicketIsSpentOnce: a retry re-mints, it does not
// replay. That is why a failed upload is a fresh ticket rather than a
// resumable one.
func TestAttachmentUploadTicketIsSpentOnce(t *testing.T) {
	stub := &stubTransfer{}
	f := attachmentFixture(t, stub)
	payload := pngPayload()

	relative, err := f.srv.MintAttachmentUploadTicket("thr-1", "shot.png", "image/png", int64(len(payload)))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if resp := put(t, f.srv.Addr(), relative, bytes.NewReader(payload), int64(len(payload))); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", resp.StatusCode)
	}
	resp := put(t, f.srv.Addr(), relative, bytes.NewReader(payload), int64(len(payload)))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("replayed status = %d, want 404", resp.StatusCode)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.stored) != 1 {
		t.Fatalf("the seam was reached %d times; a spent ticket must be refused before the body is read", len(stub.stored))
	}
}

// TestAttachmentUploadRefusesADeclaredLengthMismatch is the early exit: a
// Content-Length that disagrees with the ticket is refused before the
// body is read at all.
func TestAttachmentUploadRefusesADeclaredLengthMismatch(t *testing.T) {
	stub := &stubTransfer{}
	f := attachmentFixture(t, stub)
	payload := pngPayload()

	relative, err := f.srv.MintAttachmentUploadTicket("thr-1", "shot.png", "image/png", int64(len(payload)))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resp := put(t, f.srv.Addr(), relative, bytes.NewReader(payload[:8]), 8)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.stored) != 0 {
		t.Fatal("the seam was reached for a body whose declared length the ticket did not admit")
	}
}

// TestAttachmentUploadRefusesAnOversizeBodyMidStream is the cap that
// matters: the body lies about its length (chunked, so it declares none)
// and is cut off during the read rather than after landing.
func TestAttachmentUploadRefusesAnOversizeBodyMidStream(t *testing.T) {
	stub := &stubTransfer{}
	f := attachmentFixture(t, stub)

	relative, err := f.srv.MintAttachmentUploadTicket("thr-1", "shot.png", "image/png", 32)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// ContentLength -1 makes net/http send this chunked, so nothing
	// declares a length and only the read can refuse it.
	oversize := bytes.Repeat([]byte{0x41}, 64<<10)
	resp := put(t, f.srv.Addr(), relative, bytes.NewReader(oversize), -1)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 413", resp.StatusCode, body)
	}
	stored := stub.lastStored(t)
	if stored.err == nil {
		t.Fatal("the seam drained the whole body; the cap must fire during the read")
	}
	if int64(len(stored.body)) > stored.req.Size {
		t.Fatalf("the seam saw %d bytes past a %d-byte cap", len(stored.body), stored.req.Size)
	}
}

// TestAttachmentUploadRefusesUnmintableMetadata pins the encode-side
// guard: a value that could not round-trip through the subject is refused
// where it enters rather than discovered where it is read.
func TestAttachmentUploadRefusesUnmintableMetadata(t *testing.T) {
	f := attachmentFixture(t, &stubTransfer{})

	cases := []struct {
		name               string
		thread, file, mime string
		size               int64
	}{
		{"no thread", "", "a.png", "image/png", 8},
		{"no filename", "thr-1", "", "image/png", 8},
		{"zero size", "thr-1", "a.png", "image/png", 0},
		{"negative size", "thr-1", "a.png", "image/png", -1},
		{"separator in filename", "thr-1", "a\x00b.png", "image/png", 8},
		{"separator in thread", "thr\x001", "a.png", "image/png", 8},
		{"separator in mime", "thr-1", "a.png", "image/\x00png", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.srv.MintAttachmentUploadTicket(tc.thread, tc.file, tc.mime, tc.size); err == nil {
				t.Fatal("expected the mint to be refused")
			}
		})
	}
}

// TestAttachmentTicketsLapse pins the deadline half of the book. Both
// books carry one; without it a ticket a client abandoned would sit
// spendable until eviction pushed it out.
func TestAttachmentTicketsLapse(t *testing.T) {
	f := attachmentFixture(t, &stubTransfer{content: pngPayload()})

	now := time.Now()
	f.srv.attachmentDownloadTickets.now = func() time.Time { return now }
	relative, err := f.srv.MintAttachmentDownloadTicket("thr-1", "att-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	f.srv.attachmentDownloadTickets.now = func() time.Time { return now.Add(attachmentTicketTTL + time.Second) }
	if resp := get(t, f.srv.Addr(), relative, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a lapsed ticket: status = %d, want 404", resp.StatusCode)
	}
}

// TestAttachmentTicketBooksAreBounded pins the other bound. Minting
// without ever transferring must not grow the book.
func TestAttachmentTicketBooksAreBounded(t *testing.T) {
	f := attachmentFixture(t, &stubTransfer{})

	for i := 0; i < maxOutstandingAttachmentTickets*2; i++ {
		if _, err := f.srv.MintAttachmentDownloadTicket("thr-1", fmt.Sprintf("att-%d", i)); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		if _, err := f.srv.MintAttachmentUploadTicket("thr-1", "a.png", "image/png", 8); err != nil {
			t.Fatalf("mint upload %d: %v", i, err)
		}
	}
	if got := f.srv.attachmentDownloadTickets.outstanding(); got > maxOutstandingAttachmentTickets {
		t.Fatalf("download book holds %d, over its bound of %d", got, maxOutstandingAttachmentTickets)
	}
	if got := f.srv.attachmentUploadTickets.outstanding(); got > maxOutstandingAttachmentTickets {
		t.Fatalf("upload book holds %d, over its bound of %d", got, maxOutstandingAttachmentTickets)
	}
}

// TestAttachmentSubjectsRoundTrip is the encode/decode pair on its own.
// The subject is the only place a filename crosses unescaped, so its
// round trip is worth pinning apart from the routes that use it.
func TestAttachmentSubjectsRoundTrip(t *testing.T) {
	thread, attachment := "thr-ünï/code", "att-1"
	gotThread, gotAttachment, ok := parseDownloadSubject(downloadSubject(thread, attachment))
	if !ok || gotThread != thread || gotAttachment != attachment {
		t.Fatalf("download subject round trip = (%q, %q, %v)", gotThread, gotAttachment, ok)
	}

	upload, ok := parseUploadSubject(uploadSubject("thr-1", "image/png", "a b?c=d&e.png", 4096))
	if !ok {
		t.Fatal("upload subject did not parse")
	}
	if upload.ThreadID != "thr-1" || upload.MimeType != "image/png" ||
		upload.Filename != "a b?c=d&e.png" || upload.Size != 4096 {
		t.Fatalf("upload subject round trip = %+v", upload)
	}

	for _, bad := range []string{"", "one-field", "a\x00b", "thr\x00notanumber\x00image/png\x00a.png", "thr\x000\x00image/png\x00a.png"} {
		if _, ok := parseUploadSubject(bad); ok {
			t.Fatalf("parseUploadSubject(%q) accepted a subject it did not write", bad)
		}
	}
}

// The transfer window is a floor that grows with a body the floor could
// not carry at the minimum rate: a 10 MiB image sits at the floor, a
// 50 MiB file needs five times as long, and an unknown length (what
// net/http reports as -1) is the floor because only images, which the
// floor covers, are ever served without a declared length.
func TestAttachmentTransferWindowForScalesWithTheDeclaredLength(t *testing.T) {
	cases := []struct {
		size int64
		want time.Duration
	}{
		{-1, AttachmentTransferWindow},
		{0, AttachmentTransferWindow},
		{10 << 20, AttachmentTransferWindow},
		{50 << 20, time.Duration((50<<20)/attachmentMinTransferRate) * time.Second},
	}
	for _, tc := range cases {
		if got := AttachmentTransferWindowFor(tc.size); got != tc.want {
			t.Errorf("AttachmentTransferWindowFor(%d) = %v, want %v", tc.size, got, tc.want)
		}
	}
	if AttachmentTransferWindowFor(50<<20) <= AttachmentTransferWindow {
		t.Fatal("a 50 MiB body must get more than the floor")
	}
}

// trickleReader delivers its bytes one at a time, pausing between them,
// so a body takes longer to arrive than the server's write timeout. It
// is the shape of a real upload from a phone on a slow link, which is
// the case the transfer window exists for.
type trickleReader struct {
	remaining int
	pause     time.Duration
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.pause)
	r.remaining--
	p[0] = 0x89
	return 1, nil
}

// TestAttachmentUploadAnswersABodySlowerThanTheWriteTimeout is the
// regression guard for the half of the transfer window that used to be
// missing. net/http arms the WRITE deadline when the request headers
// finish reading, before the handler runs, so a route that extended only
// its READ deadline still had the server's write timeout to answer in,
// counted from before the first body byte arrived. A body slower than
// that timeout therefore uploaded successfully and then failed on the
// created-row JSON, which reads to the composer as the upload failing.
func TestAttachmentUploadAnswersABodySlowerThanTheWriteTimeout(t *testing.T) {
	const (
		size        = 4
		writeWindow = 150 * time.Millisecond
		perByte     = 120 * time.Millisecond
	)
	stub := &stubTransfer{}
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.AttachmentTransfer = stub
		// Both, because both are what the route replaces. A short read
		// timeout alone would prove only the half that already worked.
		cfg.HTTPReadTimeout = writeWindow
		cfg.HTTPWriteTimeout = writeWindow
	})

	relative, err := f.srv.MintAttachmentUploadTicket("thr-1", "slow.png", "image/png", size)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	resp := put(t, f.srv.Addr(), relative, &trickleReader{remaining: size, pause: perByte}, size)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 200: a transfer inside the window must be answered", resp.StatusCode, body)
	}
	// Read the body too: the failure this guards against is the ANSWER
	// being cut off, which a status line alone can outrun.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read created row: %v; the answer was cut off by a deadline the transfer window should have replaced", err)
	}
	if !bytes.Contains(body, []byte(`"threadId":"thr-1"`)) {
		t.Fatalf("created row = %s, want the seam's row", body)
	}
	if stored := stub.lastStored(t); int64(len(stored.body)) != size {
		t.Fatalf("delivered %d bytes, declared %d", len(stored.body), size)
	}
}

// deadlineRecorder is a ResponseWriter that only records the deadlines a
// handler sets on it. http.NewResponseController finds both methods
// here, so the helper's whole effect is observable without a socket.
type deadlineRecorder struct {
	http.ResponseWriter
	read  time.Time
	write time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error  { d.read = t; return nil }
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error { d.write = t; return nil }

// TestExtendTransferDeadlineSetsBothHalves pins the property directly, so
// a future caller cannot reintroduce a per-direction selector: every
// route on this mux whose payload is bytes needs both, and which one a
// given route "obviously" needs is exactly the reasoning that was wrong.
func TestExtendTransferDeadlineSetsBothHalves(t *testing.T) {
	recorder := &deadlineRecorder{}
	before := time.Now()
	extendTransferDeadline(recorder, AttachmentTransferWindow)

	floor := before.Add(AttachmentTransferWindow)
	if recorder.read.Before(floor) {
		t.Fatalf("read deadline = %v, want at least %v", recorder.read, floor)
	}
	if recorder.write.Before(floor) {
		t.Fatalf("write deadline = %v, want at least %v", recorder.write, floor)
	}
}
