package transport

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/bundle"
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
//
// Both spellings are pinned. The browser flag is conditional, so the
// unconditional list must keep NOT carrying it — a deployment fact that
// leaked into the always-advertised set would tell every windowless
// backend it can drive a browser.
func TestServer_AdvertisedCapabilitiesAreFrozen(t *testing.T) {
	want := []string{
		"notifications.remote",
		"passkeys",
		"commands.remote.v1",
		"pairing.networks.v1",
		"device-name.v1",
	}
	assertCapabilities(t, serverCapabilities, want)
	assertCapabilities(t, serverCapabilitiesWithBrowser, append(append([]string{}, want...), "browser"))
}

func TestFilePreviewCapabilityRequiresExecutionHostSupport(t *testing.T) {
	for _, browser := range []bool{false, true} {
		for _, transfers := range []bool{false, true} {
			base := advertisedCapabilities(func() bool { return browser }, transfers, false)
			for _, flag := range base {
				if flag == CapabilityFilePreview {
					t.Fatal("frontend controller advertised file previews")
				}
			}
			withFiles := advertisedCapabilities(func() bool { return browser }, transfers, true)
			assertCapabilities(t, withFiles, append(append([]string{}, base...), "preview.files.v1"))
		}
	}
}

func assertCapabilities(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("advertised capabilities = %v, want %v — update this list and the name's doc comment in the same change",
			got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("capability %d = %q, want %q", i, got[i], name)
		}
	}
}

// The browser flag is advertised if and only if this backend has an
// engine. Nothing about the CALLER moves it: a serve host with no
// Chromium installed has no browser tools for anyone, and a client told
// otherwise would offer a browser surface that can never work
// (docs/specs/remote-access.md §7).
func TestServer_HelloAdvertisesBrowserOnlyWhenTheEngineExists(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available func() bool
		want      bool
	}{
		{name: "no hook wired", available: nil, want: false},
		{name: "no engine on this machine", available: func() bool { return false }, want: false},
		{name: "engine available", available: func() bool { return true }, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerFixtureWith(t, func(cfg *Config) { cfg.BrowserAvailable = tc.available })
			var hello helloFrame
			if err := json.Unmarshal(readFirstFrame(t, f.dial(t)), &hello); err != nil {
				t.Fatalf("decode hello: %v", err)
			}
			if got := slices.Contains(hello.Capabilities, CapabilityBrowser); got != tc.want {
				t.Fatalf("capabilities = %v, browser advertised = %v, want %v", hello.Capabilities, got, tc.want)
			}
			// The rest of the set is unconditional and must be unaffected
			// either way: this flag is additive, not a replacement.
			for _, always := range []string{CapabilityRemoteNotifications, CapabilityPasskeys} {
				if !slices.Contains(hello.Capabilities, always) {
					t.Fatalf("capabilities = %v, missing %q", hello.Capabilities, always)
				}
			}
		})
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

// TestServer_HelloCarriesTheBundleItServes pins the three additive
// fields wave 6g-a put on this frame against the manifest they come
// from (internal/bundle).
//
// They are on the FRAME rather than behind a route because the one
// client that reads them compares them against something it already
// holds on every connection, and a shell that had to fetch a document to
// learn "nothing changed" would pay a round trip per connect forever.
// The assertion is that what the frame says and what the routes serve
// are one answer, not two that happen to agree today.
func TestServer_HelloCarriesTheBundleItServes(t *testing.T) {
	tree := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("export const a = 1;\n")},
	}
	spa := bundle.New(tree, "4.5.6")
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.Bundle = spa })

	var hello helloFrame
	if err := json.Unmarshal(readFirstFrame(t, f.dial(t)), &hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	manifest, err := spa.Manifest()
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if hello.BundleID != manifest.ID {
		t.Errorf("bundleId = %q, want the manifest's %q", hello.BundleID, manifest.ID)
	}
	if hello.BundleVersion != "4.5.6" {
		t.Errorf("bundleVersion = %q, want the link-time stamp", hello.BundleVersion)
	}
	if hello.MinShellBuild != bundle.MinShellBuild {
		t.Errorf("minShellBuild = %d, want %d", hello.MinShellBuild, bundle.MinShellBuild)
	}
}

// A backend that serves no bundle omits all three rather than sending
// empty ones. Absent is what a shell reads as "this backend does not
// supply bundles" — the same answer a backend too old to send them
// gives, and the one that leaves the phone running what it has.
func TestServer_HelloOmitsBundleFieldsWithoutABundle(t *testing.T) {
	f := newServerFixture(t)
	raw := string(readFirstFrame(t, f.dial(t)))
	for _, field := range []string{"bundleId", "bundleVersion", "minShellBuild"} {
		if strings.Contains(raw, field) {
			t.Fatalf("hello names %s with no configured bundle: %s", field, raw)
		}
	}
}
