package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// readFirstFrame reads exactly one message off a freshly dialled
// connection. Every assertion about hello depends on it being FIRST, so
// the helper deliberately does not skip or filter anything.
func readFirstFrame(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	return raw
}

// readPastHello reads frames until one is not the connection's opening
// hello. Every real client does exactly this — hello is unconditional,
// so a test that wants the answer to something it SENT must skip it.
func readPastHello(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Type == frameTypeHello {
			continue
		}
		return raw
	}
}

// The ordering is a contract, not an accident of goroutine scheduling: a
// client that reads hello first can seed its compatibility state before
// anything else lands, and needs no "have I been told yet" branch on
// every other frame.
func TestServer_HelloIsTheFirstFrameOnEveryConnection(t *testing.T) {
	f := newServerFixture(t)
	// Sampled before the DIAL, not before the read: the server stamps the
	// hello while it handles the upgrade, so a window opened afterwards
	// starts later than the value it is bounding and fails whenever the
	// upgrade costs a millisecond.
	before := time.Now().UnixMilli()
	conn := f.dial(t)
	raw := readFirstFrame(t, conn)
	after := time.Now().UnixMilli()

	var hello helloFrame
	if err := json.Unmarshal(raw, &hello); err != nil {
		t.Fatalf("decode hello: %v (raw %s)", err, raw)
	}
	if hello.Type != frameTypeHello {
		t.Fatalf("first frame type = %q, want %q (raw %s)", hello.Type, frameTypeHello, raw)
	}
	if hello.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", hello.ProtocolVersion, ProtocolVersion)
	}
	// The clock reading has to be THIS connection's, not a boot-time
	// snapshot: the field exists so a client can measure its own skew,
	// and a cached value would be confidently wrong by the process uptime.
	if hello.ServerTimeMs < before || hello.ServerTimeMs > after {
		t.Fatalf("serverTimeMs = %d, want within [%d, %d]", hello.ServerTimeMs, before, after)
	}
}

// Capabilities must serialize as [] and never as null. A client reads an
// empty list without a nil check, and "advertises nothing" has to stay
// distinguishable from "too old to send this frame at all".
func TestServer_HelloCapabilitiesAreAnArrayNeverNull(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	raw := string(readFirstFrame(t, conn))
	if !strings.Contains(raw, `"capabilities":[`) {
		t.Fatalf("capabilities not encoded as a JSON array: %s", raw)
	}
}

func TestServer_HelloCarriesBackendIdentity(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.BackendIdentity = func() (string, string) {
			return "backend-uuid-1", "generation-7"
		}
	})
	conn := f.dial(t)

	var hello helloFrame
	if err := json.Unmarshal(readFirstFrame(t, conn), &hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if hello.BackendID != "backend-uuid-1" {
		t.Fatalf("backendId = %q, want %q", hello.BackendID, "backend-uuid-1")
	}
}

// A backend whose store has not opened yet reports an empty id rather
// than inventing one. The client's rule is that empty means UNKNOWN and
// never a wildcard, so the wire has to be able to say it.
func TestServer_HelloOmitsBackendIDWhenIdentityIsUnknown(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)

	var hello helloFrame
	if err := json.Unmarshal(readFirstFrame(t, conn), &hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if hello.BackendID != "" {
		t.Fatalf("backendId = %q, want empty with no BackendIdentity configured", hello.BackendID)
	}
}

// Every advertised capability is one a client may still be asking about
// from an older bundle, so the set is a compatibility contract. Growing
// it is fine and additive; changing what a shipped name MEANS is not,
// and this list is where that decision has to be written down.
func TestServer_AdvertisedCapabilitiesAreFrozen(t *testing.T) {
	want := []string{
		"notifications.remote",
		"passkeys",
	}
	if len(serverCapabilities) != len(want) {
		t.Fatalf("advertised capabilities = %v, want %v — update this list and the name's doc comment in the same change",
			serverCapabilities, want)
	}
	for i, name := range want {
		if serverCapabilities[i] != name {
			t.Fatalf("capability %d = %q, want %q", i, serverCapabilities[i], name)
		}
	}
}

// TestServer_HelloCarriesTheBackendName pins the display name onto the
// opening frame (docs/specs/remote-access.md §10, "Machine name").
//
// A client attached to several backends has to label them, and until
// this field there was no machine name anywhere on the wire — the
// pairing payload carried one and the device discarded it. It is
// DISPLAY only: two backends may legitimately answer the same string,
// so nothing may key on it and `backendId` stays the identity. The
// assertion is on both halves of that: the configured name arrives, and
// the identity beside it is untouched by it.
func TestServer_HelloCarriesTheBackendName(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.BackendName = "workshop-mini"
		cfg.BackendIdentity = func() (string, string) { return "backend-1", "gen-1" }
	})
	var hello helloFrame
	if err := json.Unmarshal(readFirstFrame(t, f.dial(t)), &hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if hello.BackendName != "workshop-mini" {
		t.Fatalf("backendName = %q, want %q", hello.BackendName, "workshop-mini")
	}
	if hello.BackendID != "backend-1" {
		t.Fatalf("backendId = %q, want %q — the name must not displace the identity", hello.BackendID, "backend-1")
	}
}

// TestServer_HelloOmitsAnUnknownBackendName pins the absence.
//
// An unreadable hostname is an empty answer rather than a failure
// (internal/appidentity.HostDisplayName), and an omitted field has to
// stay distinguishable from a backend too old to send one — both mean
// "unknown", and a client falls back to the id for both. Emitting `""`
// would say the same thing in a second spelling every consumer would
// then owe a branch for.
func TestServer_HelloOmitsAnUnknownBackendName(t *testing.T) {
	f := newServerFixture(t)
	raw := readFirstFrame(t, f.dial(t))
	if strings.Contains(string(raw), "backendName") {
		t.Fatalf("hello names an unset backendName: %s", raw)
	}
}
