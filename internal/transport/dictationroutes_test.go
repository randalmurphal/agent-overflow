package transport

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// stubDictation records the pieces the route delivers. It reads each the
// way the recording does: exactly the ticket's size, then refuses more.
type stubDictation struct {
	mu        sync.Mutex
	chunks    []DictationChunk
	bodies    [][]byte
	appendErr error
}

func (s *stubDictation) AppendDictationChunk(chunk DictationChunk) error {
	body := make([]byte, chunk.Size)
	if _, err := io.ReadFull(chunk.Body, body); err != nil {
		return err
	}
	var extra [1]byte
	if n, err := chunk.Body.Read(extra[:]); n > 0 {
		return errors.New("piece longer than its ticket")
	} else if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if s.appendErr != nil {
		return s.appendErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, chunk)
	s.bodies = append(s.bodies, body)
	return nil
}

func (s *stubDictation) received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chunks)
}

func dictationFixture(t *testing.T, stub DictationTransfer) *serverFixture {
	t.Helper()
	return newServerFixtureWith(t, func(cfg *Config) {
		cfg.DictationTransfer = stub
	})
}

func mintDictation(t *testing.T, f *serverFixture, recordingID string, seq int, size int64) string {
	t.Helper()
	relative, err := f.srv.MintDictationChunkTicket(recordingID, seq, size)
	if err != nil {
		t.Fatalf("mint dictation ticket: %v", err)
	}
	return relative
}

// A piece lands with the recording, place and length its ticket names, the
// bytes the request carried, and nothing a client could cache.
func TestDictationUploadDeliversTheTicketedPiece(t *testing.T) {
	stub := &stubDictation{}
	f := dictationFixture(t, stub)

	relative := mintDictation(t, f, "rec-1", 3, 5)
	if !strings.HasPrefix(relative, "/attachments/dictation?ticket=") {
		t.Fatalf("minted URL %q is outside the attachment subtree", relative)
	}
	resp := put(t, f.srv.Addr(), relative, strings.NewReader("hello"), 5)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if stub.received() != 1 {
		t.Fatalf("pieces delivered = %d, want 1", stub.received())
	}
	chunk := stub.chunks[0]
	if chunk.RecordingID != "rec-1" || chunk.Seq != 3 || chunk.Size != 5 || string(stub.bodies[0]) != "hello" {
		t.Fatalf("delivered %+v %q", chunk, stub.bodies[0])
	}

	again := put(t, f.srv.Addr(), relative, strings.NewReader("hello"), 5)
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("reused ticket status = %d, want 404", again.StatusCode)
	}
	if stub.received() != 1 {
		t.Fatal("a reused ticket delivered a second piece")
	}
}

// The request contributes bytes and nothing else: a length other than the
// ticket's, declared or streamed, never reaches the recording.
func TestDictationUploadRefusesAPieceOfAnotherLength(t *testing.T) {
	stub := &stubDictation{}
	f := dictationFixture(t, stub)

	short := put(t, f.srv.Addr(), mintDictation(t, f, "rec-1", 0, 5), strings.NewReader("hell"), 4)
	if short.StatusCode != http.StatusBadRequest {
		t.Fatalf("declared short status = %d, want 400", short.StatusCode)
	}
	long := put(t, f.srv.Addr(), mintDictation(t, f, "rec-1", 0, 5), io.MultiReader(strings.NewReader("hello, world")), -1)
	if long.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed long status = %d, want 413", long.StatusCode)
	}
	if stub.received() != 0 {
		t.Fatalf("pieces delivered = %d, want none", stub.received())
	}
}

func TestDictationUploadRefusals(t *testing.T) {
	refused := &stubDictation{appendErr: errors.New("piece 1 out of order, expected 0")}
	f := dictationFixture(t, refused)
	if resp := put(t, f.srv.Addr(), mintDictation(t, f, "rec-1", 1, 3), bytes.NewReader([]byte("abc")), 3); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("refused piece status = %d, want 400", resp.StatusCode)
	}
	if resp := put(t, f.srv.Addr(), "/attachments/dictation?ticket=forged", bytes.NewReader([]byte("abc")), 3); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forged ticket status = %d, want 404", resp.StatusCode)
	}

	unwired := newServerFixtureWith(t, func(*Config) {})
	relative, err := unwired.srv.MintDictationChunkTicket("rec-1", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if resp := put(t, unwired.srv.Addr(), relative, bytes.NewReader([]byte("abc")), 3); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no dictation transfer status = %d, want 404", resp.StatusCode)
	}

	for _, bad := range []struct {
		id   string
		seq  int
		size int64
	}{{"", 0, 1}, {"rec-1", -1, 1}, {"rec-1", 0, 0}} {
		if _, err := f.srv.MintDictationChunkTicket(bad.id, bad.seq, bad.size); err == nil {
			t.Fatalf("minted a ticket for %+v", bad)
		}
	}
}
