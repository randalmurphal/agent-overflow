package pairbootstrap

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProtocolV1Vector(t *testing.T) {
	private := make([]byte, 32)
	private[31] = 1
	clientKey, err := ecdh.P256().NewPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	private[31] = 2
	serverKey, err := ecdh.P256().NewPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	r := Reveal{PublicKey: encode(clientKey.PublicKey().Bytes()), Nonce: encode(bytes.Repeat([]byte{1}, 32)), Label: "Windows <desktop>", Platform: "windows"}
	ch := Challenge{ID: encode(bytes.Repeat([]byte{3}, 16)), PublicKey: encode(serverKey.PublicKey().Bytes()), Nonce: encode(bytes.Repeat([]byte{2}, 32)), ExpiresAtMs: 1800000000000}
	co := commit(r)
	_, transcript, sas, err := derive(clientKey, ch.PublicKey, co, ch, r)
	if err != nil {
		t.Fatal(err)
	}
	if co.Commitment != "ZpkYtuGlZIDghykdEPQMVEptueBG2vMUY7sYLks2yZ4" || encode(transcript) != "lRyIaY9SrURkOSXY3MiNvULiUD4hFJ23BJ6Rl91IhkU" || sas != "305627" {
		t.Fatal("version 1 transcript changed; existing clients would derive a different comparison")
	}
}

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := Start("My computer", "windows")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func invitation() (Invitation, error) {
	return Invitation{LinkID: "link", URL: "https://host/#secret-invitation"}, nil
}
func startBook(t *testing.T) (*Book, Snapshot) {
	t.Helper()
	b := NewBook(nil)
	s := b.Open(invitation)
	t.Cleanup(func() { b.Close(s.WindowID) })
	return b, s
}
func handshake(t *testing.T, b *Book, c *Client) (Challenge, Reveal, SealedInvitation) {
	t.Helper()
	ch, err := b.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Reveal(ch.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	return ch, r, sealed
}

func TestExchangeAuthenticatesInvitationAndRetriesWithoutReminting(t *testing.T) {
	var minted atomic.Int32
	b := NewBook(nil)
	s := b.Open(func() (Invitation, error) { minted.Add(1); return invitation() })
	t.Cleanup(func() { b.Close(s.WindowID) })
	c := newClient(t)
	ch, r, sealed := handshake(t, b, c)
	invite, sas, err := c.Open(sealed)
	if err != nil || invite.URL != "https://host/#secret-invitation" || len(sas) != 6 || sas != b.Snapshot(s.WindowID).VerificationNumber {
		t.Fatalf("exchange failed: %v", err)
	}
	if next, err := b.Begin(c.Commitment()); err != nil || next != ch {
		t.Fatal("begin retry changed challenge", err)
	}
	if next, err := b.Reveal(ch.ID, r); err != nil || next != sealed || minted.Load() != 1 {
		t.Fatal("reveal retry reminted", err)
	}
	if _, err := b.Begin(newClient(t).Commitment()); !errors.Is(err, ErrBusy) {
		t.Fatal("another initiator replaced request", err)
	}
	for _, v := range []any{ch, sealed} {
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), sas) || strings.Contains(string(raw), "secret-invitation") {
			t.Fatal("wire exposed invitation or SAS")
		}
	}
}

func TestCommitmentAndFrozenChallengeRejectTranscriptReplacement(t *testing.T) {
	b, _ := startBook(t)
	c := newClient(t)
	ch, err := b.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Reveal){"key": func(r *Reveal) { r.PublicKey = newClient(t).reveal.PublicKey }, "nonce": func(r *Reveal) { r.Nonce = encode(random(32)) }, "label": func(r *Reveal) { r.Label = "Attacker" }, "platform": func(r *Reveal) { r.Platform = "attacker" }} {
		t.Run(name, func(t *testing.T) {
			bad := r
			change(&bad)
			if _, err := b.Reveal(ch.ID, bad); !errors.Is(err, ErrInvalid) {
				t.Fatal("modified commitment accepted", err)
			}
		})
	}
	other := ch
	other.Nonce = encode(random(32))
	if _, err := c.Reveal(other); !errors.Is(err, ErrInvalid) {
		t.Fatal("client disclosed secret to replacement challenge")
	}
	if _, err := b.Reveal("different-request", r); !errors.Is(err, ErrInvalid) {
		t.Fatal("cross-request reveal accepted")
	}
	bad := newClient(t)
	bad.reveal.PublicKey = encode(make([]byte, 65))
	bad.commitment = commit(bad.reveal)
	b2, _ := startBook(t)
	ch2, _ := b2.Begin(bad.Commitment())
	if _, err := b2.Reveal(ch2.ID, bad.reveal); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid curve point accepted")
	}
}

func TestInterceptingRelayCannotReuseHostCiphertextOrComparison(t *testing.T) {
	host, hostWindow := startBook(t)
	attacker := newClient(t)
	_, _, hostSealed := handshake(t, host, attacker)
	// The relay can decrypt the invitation it obtained as its own initiator,
	// but cannot show that exchange's digits in the victim's trusted client.
	victim := newClient(t)
	relay, _ := startBook(t)
	_, _, relaySealed := handshake(t, relay, victim)
	if _, _, err := victim.Open(hostSealed); !errors.Is(err, ErrInvalid) {
		t.Fatal("cross-exchange ciphertext accepted")
	}
	_, victimSAS, err := victim.Open(relaySealed)
	if err != nil {
		t.Fatal(err)
	}
	if victimSAS == host.Snapshot(hostWindow.WindowID).VerificationNumber {
		t.Fatal("intercepting relay unexpectedly matched comparison")
	}
	// An opaque byte relay is the ordinary successful handshake: without
	// either private key a third client cannot decrypt its invitation.
	third := newClient(t)
	if _, _, err := third.Open(hostSealed); !errors.Is(err, ErrInvalid) {
		t.Fatal("uninitialized client decrypted invitation")
	}
}

func TestCiphertextAndTranscriptTamperingFails(t *testing.T) {
	b, _ := startBook(t)
	c := newClient(t)
	_, _, sealed := handshake(t, b, c)
	raw, _ := decode(sealed.Nonce, 12)
	raw[0] ^= 1
	bad := sealed
	bad.Nonce = encode(raw)
	if _, _, err := c.Open(bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("changed nonce accepted")
	}
	bad = sealed
	bad.Ciphertext = "AAAA"
	if _, _, err := c.Open(bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("truncated ciphertext accepted")
	}
	c.transcript[0] ^= 1
	if _, _, err := c.Open(sealed); !errors.Is(err, ErrInvalid) {
		t.Fatal("changed transcript accepted")
	}
}

func TestCloseDuringMintCancelsLateInvitationAndPreservesNewWindow(t *testing.T) {
	started, finish, canceled := make(chan struct{}), make(chan struct{}), make(chan string, 1)
	b := NewBook(func(id string) { canceled <- id })
	old := b.Open(func() (Invitation, error) { close(started); <-finish; return invitation() })
	c := newClient(t)
	ch, _ := b.Begin(c.Commitment())
	r, _ := c.Reveal(ch)
	done := make(chan error, 1)
	go func() { _, err := b.Reveal(ch.ID, r); done <- err }()
	<-started
	b.Close(old.WindowID)
	next := b.Open(invitation)
	defer b.Close(next.WindowID)
	b.Close(old.WindowID)
	close(finish)
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatal("late mint succeeded", err)
	}
	if id := <-canceled; id != "link" {
		t.Fatal("wrong invitation canceled")
	}
	if !b.Snapshot(next.WindowID).Open || b.Snapshot(old.WindowID).Open {
		t.Fatal("stale UI closed replacement window")
	}
}

func TestExpiryCancelsInvitationWithoutFurtherRequests(t *testing.T) {
	canceled := make(chan string, 1)
	b := NewBook(func(id string) { canceled <- id })
	w := b.open(invitation, time.Second)
	handshake(t, b, newClient(t))
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("expiry did not cancel invitation")
	}
	if b.Snapshot(w.WindowID).Open {
		t.Fatal("expired window remained open")
	}
	if _, err := b.Begin(newClient(t).Commitment()); !errors.Is(err, ErrClosed) {
		t.Fatal("expired window accepted request")
	}
}

func TestHTTPSExchangeAndNoRedirect(t *testing.T) {
	b, _ := startBook(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path || r.Method != "POST" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected bootstrap request")
		}
		var req Request
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad", 400)
			return
		}
		switch req.Op {
		case "begin":
			v, err := b.Begin(req.Commitment)
			if err != nil {
				http.Error(w, "bad", 400)
				return
			}
			json.NewEncoder(w).Encode(v)
		case "reveal":
			v, err := b.Reveal(req.ID, req.Reveal)
			if err != nil {
				http.Error(w, "bad", 400)
				return
			}
			json.NewEncoder(w).Encode(v)
		}
	}))
	defer server.Close()
	hc := NewHTTPClient(nil)
	defer hc.CloseIdleConnections()
	if _, sas, err := Exchange(context.Background(), hc, server.URL, "Laptop", "darwin"); err != nil || sas != b.Snapshot("").VerificationNumber {
		t.Fatal("real TLS exchange failed", err)
	}
	var hits atomic.Int32
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Redirect(w, r, server.URL, 302) }))
	defer redirect.Close()
	if _, _, err := Exchange(context.Background(), hc, redirect.URL, "Laptop", "darwin"); err == nil || hits.Load() != 1 {
		t.Fatal("redirect accepted")
	}
}

func TestNormalizeAddress(t *testing.T) {
	for _, raw := range []string{"localhost", "machine.local:443", "https://192.168.1.1:6000/", "[::1]:6000"} {
		if _, err := NormalizeAddress(raw); err != nil {
			t.Errorf("valid %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "http://host", "https://user:password@host", "host/path", "host?secret=x", "host#secret", "host:0", "host:65536", "host:", "https://host?", "host name", strings.Repeat("a", 1025)} {
		if _, err := NormalizeAddress(raw); err == nil {
			t.Errorf("invalid %q accepted", raw)
		}
	}
}
