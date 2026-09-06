package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transferclient"
	"agent-overflow/internal/transferfiles"
	"agent-overflow/internal/transferwire"
)

type handoffReceiver struct {
	mu                          sync.Mutex
	id, grant, directory, phase string
	secret                      []byte
	mutations, activations      int
	forcedError                 error
}

func (h *handoffReceiver) Authorize(_ context.Context, id, grant string) bool {
	return id == h.id && subtle.ConstantTimeCompare([]byte(grant), []byte(h.grant)) == 1
}
func (h *handoffReceiver) Status(context.Context, string) (transferwire.State, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	progress, err := transferfiles.ReadUpload(h.directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return transferwire.State{}, err
	}
	return transferwire.State{Phase: h.phase, SHA256: progress.SHA256, Size: progress.Size, Received: progress.Received}, nil
}
func (h *handoffReceiver) BeginUpload(_ context.Context, _ string, request transferwire.Upload) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mutations++
	if h.forcedError != nil {
		return h.forcedError
	}
	_, err := transferfiles.BeginUpload(h.directory, request.SHA256, request.Size)
	return err
}
func (h *handoffReceiver) ReceiveChunk(ctx context.Context, _ string, offset, size int64, digest string, input io.Reader) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mutations++
	_, err := transferfiles.ReceiveChunk(ctx, h.directory, offset, size, digest, input)
	return err
}
func (h *handoffReceiver) Prepare(ctx context.Context, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phase == "prepared" {
		return nil
	}
	archive, progress, err := transferfiles.UploadedArchive(h.directory)
	if err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := transferfiles.Extract(ctx, f, progress.SHA256, filepath.Join(h.directory, "verified")); err != nil {
		return err
	}
	h.phase = "prepared"
	return nil
}
func (h *handoffReceiver) Activate(_ context.Context, _ string, secret []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !bytes.Equal(secret, h.secret) || (h.phase != "prepared" && h.phase != "complete") {
		return transferwire.ErrNotReady
	}
	if h.phase != "complete" {
		h.activations++
		h.phase = "complete"
	}
	return nil
}
func (h *handoffReceiver) Cancel(_ context.Context, _ string, secret []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phase == "complete" || !bytes.Equal(secret, h.secret) {
		return transferwire.ErrConflict
	}
	h.phase = "canceled"
	return nil
}

func handoffFixture(t *testing.T) (*Server, *handoffReceiver, string) {
	t.Helper()
	backend := entityid.New()
	endpoint := &handoffReceiver{id: entityid.New(), grant: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa5}, 32)), directory: t.TempDir(), phase: "preparing", secret: bytes.Repeat([]byte{0xb6}, 32)}
	server, err := New(Config{Dispatcher: NewDispatcher(), EventBus: NewEventBus(4), Token: "local-page-token", ThreadTransfers: endpoint, BackendIdentity: func() (string, string) { return backend, "generation" }})
	if err != nil {
		t.Fatal(err)
	}
	return server, endpoint, backend
}

func TestThreadTransferWireStreamsAndRecoversLostAcknowledgments(t *testing.T) {
	server, receiver, backend := handoffFixture(t)
	handler := server.buildHTTPServer().Handler
	var loseChunk, loseActivation atomic.Bool
	loseChunk.Store(true)
	loseActivation.Store(true)
	httpServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lose := (strings.HasSuffix(r.URL.Path, "/chunk") && loseChunk.CompareAndSwap(true, false)) || (strings.HasSuffix(r.URL.Path, "/activate") && loseActivation.CompareAndSwap(true, false))
		if !lose {
			handler.ServeHTTP(w, r)
			return
		}
		// Commit the request but drop its response, as when a network dies at
		// precisely the point where the sender cannot know what was accepted.
		recorded := httptest.NewRecorder()
		handler.ServeHTTP(recorded, r)
		if recorded.Code != http.StatusOK {
			t.Errorf("request before lost reply: %d %s", recorded.Code, recorded.Body.String())
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer httpServer.Close()
	client, err := transferclient.New(transferclient.Offer{Version: 1, BackendID: backend, OperationID: receiver.id, Endpoint: httpServer.URL, CertFingerprint: servercert.Fingerprint(httpServer.Certificate().Raw), Grant: receiver.grant})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	state, err := client.Status(ctx)
	if err != nil || state.Phase != "preparing" {
		t.Fatalf("status: %+v %v", state, err)
	}
	source := t.TempDir()
	content := bytes.Repeat([]byte("provider history\n"), 8192)
	if err := os.WriteFile(filepath.Join(source, "session.jsonl"), content, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tar")
	digest, err := transferfiles.Create(ctx, archive, []transferfiles.Source{{Root: source, Path: "session.jsonl", Name: "native/session.jsonl"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.BeginUpload(ctx, transferwire.Upload{SHA256: digest, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	chunk := body[:4096]
	hash := sha256.Sum256(chunk)
	if _, err := client.Chunk(ctx, 0, hex.EncodeToString(hash[:]), chunk); err == nil {
		t.Fatal("lost reply was reported as success")
	}
	state, err = client.Status(ctx)
	if err != nil || state.Received != int64(len(chunk)) {
		t.Fatalf("checkpoint after lost reply: %+v %v", state, err)
	}
	if _, err := client.Chunk(ctx, 0, hex.EncodeToString(hash[:]), chunk); err != nil {
		t.Fatalf("duplicate range: %v", err)
	}
	remaining := body[state.Received:]
	hash = sha256.Sum256(remaining)
	if _, err := client.Chunk(ctx, state.Received, hex.EncodeToString(hash[:]), remaining); err != nil {
		t.Fatal(err)
	}
	state, err = client.Prepare(ctx)
	if err != nil || state.Phase != "prepared" {
		t.Fatalf("prepare: %+v %v", state, err)
	}
	secret := base64.RawURLEncoding.EncodeToString(receiver.secret)
	if _, err := client.Activate(ctx, secret); err == nil {
		t.Fatal("lost activation reply was reported as success")
	}
	state, err = client.Status(ctx)
	if err != nil || state.Phase != "complete" {
		t.Fatalf("recover activation: %+v %v", state, err)
	}
	if _, err := client.Activate(ctx, secret); err != nil {
		t.Fatalf("activation retry: %v", err)
	}
	if receiver.activations != 1 {
		t.Fatalf("activated %d times", receiver.activations)
	}
	if _, err := client.Cancel(ctx, secret); err == nil {
		t.Fatal("canceled completed activation")
	}
	got, err := os.ReadFile(filepath.Join(receiver.directory, "verified", "native", "session.jsonl"))
	if err != nil || !bytes.Equal(content, got) {
		t.Fatal("wire transfer changed native bytes")
	}
}

func TestThreadTransferAdmissionPrecedesEveryMutation(t *testing.T) {
	for _, name := range []string{"missing grant", "page credential", "query grant", "wrong operation", "wrong backend", "wrong version", "cleartext peer", "oversized chunk", "missing chunk length", "malformed digest", "oversized control", "oversized archive", "wrong method", "cancel without source proof"} {
		t.Run(name, func(t *testing.T) {
			server, receiver, backend := handoffFixture(t)
			url := "https://localhost" + ThreadTransferPrefix + receiver.id + "/upload"
			body := `{"sha256":"` + strings.Repeat("a", 64) + `","size":1024}`
			r := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
			r.RemoteAddr = "127.0.0.1:34567"
			r.TLS = &tls.ConnectionState{}
			r.Header.Set("Authorization", "Bearer "+receiver.grant)
			r.Header.Set(transferwire.VersionHeader, "1")
			r.Header.Set(transferwire.BackendHeader, backend)
			switch name {
			case "missing grant":
				r.Header.Del("Authorization")
			case "page credential":
				r.Header.Set("Authorization", "Bearer local-page-token")
			case "query grant":
				r.Header.Del("Authorization")
				r.URL.RawQuery = "grant=" + receiver.grant
			case "wrong operation":
				r.URL.Path = ThreadTransferPrefix + entityid.New() + "/upload"
			case "wrong backend":
				r.Header.Set(transferwire.BackendHeader, entityid.New())
			case "wrong version":
				r.Header.Set(transferwire.VersionHeader, "2")
			case "cleartext peer":
				r.TLS = nil
				r.RemoteAddr = "192.168.1.8:1234"
			case "oversized chunk", "missing chunk length", "malformed digest":
				r.URL.Path = ThreadTransferPrefix + receiver.id + "/chunk"
				r.Method = http.MethodPut
				r.Header.Set(transferwire.OffsetHeader, "0")
				r.Header.Set(transferwire.DigestHeader, strings.Repeat("a", 64))
				if name == "oversized chunk" {
					r.ContentLength = transferwire.MaxChunkBytes + 1
				}
				if name == "missing chunk length" {
					r.ContentLength = -1
				}
				if name == "malformed digest" {
					r.Header.Set(transferwire.DigestHeader, "invalid")
				}
			case "oversized control":
				r.Body = io.NopCloser(strings.NewReader(`{"extra":"` + strings.Repeat("x", 5000) + `"}`))
			case "oversized archive":
				r.Body = io.NopCloser(strings.NewReader(`{"sha256":"` + strings.Repeat("a", 64) + `","size":99999999999999}`))
			case "wrong method":
				r.Method = http.MethodDelete
			case "cancel without source proof":
				r.URL.Path = ThreadTransferPrefix + receiver.id + "/cancel"
				r.Body = io.NopCloser(strings.NewReader(`{}`))
			}
			w := httptest.NewRecorder()
			server.buildHTTPServer().Handler.ServeHTTP(w, r)
			if w.Code < 400 || receiver.mutations != 0 {
				t.Fatalf("admitted %s: %d, mutations %d", name, w.Code, receiver.mutations)
			}
			if receiver.phase != "preparing" {
				t.Fatal("unauthorized request changed transfer phase")
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("offered CORS on a host-to-host route")
			}
			if strings.Contains(w.Body.String(), receiver.grant) {
				t.Fatal("refusal echoed grant")
			}
		})
	}
}

func TestThreadTransferInternalErrorsStayOffWire(t *testing.T) {
	server, receiver, backend := handoffFixture(t)
	receiver.forcedError = errors.New("private /home/person/provider/path secret detail")
	r := httptest.NewRequest(http.MethodPost, "https://localhost"+ThreadTransferPrefix+receiver.id+"/upload", strings.NewReader(`{"sha256":"`+strings.Repeat("a", 64)+`","size":1024}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer "+receiver.grant)
	r.Header.Set(transferwire.VersionHeader, "1")
	r.Header.Set(transferwire.BackendHeader, backend)
	w := httptest.NewRecorder()
	server.buildHTTPServer().Handler.ServeHTTP(w, r)
	if w.Code != 500 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), "transfer_failed") {
		t.Fatalf("unsafe error: %d %s", w.Code, w.Body.String())
	}
}

func TestThreadTransferProtocolAndByteLimitsStayAligned(t *testing.T) {
	if ThreadTransferPrefix != transferwire.PathPrefix || transferfiles.MaxUploadChunk != transferwire.MaxChunkBytes || transferfiles.MaxArchiveBytes != transferwire.MaxUploadBytes {
		t.Fatal("transfer protocol drift")
	}
}
