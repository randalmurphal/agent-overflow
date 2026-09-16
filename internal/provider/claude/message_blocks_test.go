package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func echoLine(t *testing.T, content any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal echo content: %v", err)
	}
	return encoded
}

// TestUserMessageBlockDigestMatchesItsOwnEcho is the property the merge test
// rests on: the digest of what Send writes equals the digest of the echo that
// comes back carrying those same blocks. Without it every ordinary ack would
// look like a merge candidate.
func TestUserMessageBlockDigestMatchesItsOwnEcho(t *testing.T) {
	attachment := provider.ImageAttachment{MimeType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}
	cases := []struct {
		name        string
		content     string
		attachments []provider.ImageAttachment
	}{
		{"plain text", "hello there", nil},
		{"text around an image", "look at [Image #1] closely", []provider.ImageAttachment{attachment}},
		{"image only", "[Image #1]", []provider.ImageAttachment{attachment}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent, err := UserMessageBlockDigest(tc.content, tc.attachments, false)
			if err != nil {
				t.Fatalf("UserMessageBlockDigest: %v", err)
			}
			blocks, err := buildUserMessageBlocks(tc.content, tc.attachments, false)
			if err != nil {
				t.Fatalf("buildUserMessageBlocks: %v", err)
			}
			echoed := EchoBlockDigest(echoLine(t, blocks))
			if !slices.Equal(sent, echoed) {
				t.Errorf("digest of the sent blocks %v differs from the echo's %v", sent, echoed)
			}
			if len(sent) != len(blocks) {
				t.Errorf("digest has %d entries for %d blocks", len(sent), len(blocks))
			}
		})
	}
}

// TestEchoBlockDigestTreatsStringContentAsOneTextBlock pins the older
// user-message shape against the block builder's single-text-block output, so
// a CLI that flattens an echo to a string still compares equal.
func TestEchoBlockDigestTreatsStringContentAsOneTextBlock(t *testing.T) {
	sent, err := UserMessageBlockDigest("just words", nil, false)
	if err != nil {
		t.Fatalf("UserMessageBlockDigest: %v", err)
	}
	echoed := EchoBlockDigest(echoLine(t, "just words"))
	if !slices.Equal(sent, echoed) {
		t.Errorf("string content digest %v, want the single-text-block digest %v", echoed, sent)
	}
}

// TestUserMessageBlockDigestFollowsTheSlashGuard proves the digest is derived
// from the real builder rather than the caller's text: the outbound guard
// rewrites the first block, and an expectation that missed it would never
// match the echo.
func TestUserMessageBlockDigestFollowsTheSlashGuard(t *testing.T) {
	guarded, err := UserMessageBlockDigest("/plan the work", nil, true)
	if err != nil {
		t.Fatalf("guarded digest: %v", err)
	}
	unguarded, err := UserMessageBlockDigest("/plan the work", nil, false)
	if err != nil {
		t.Fatalf("unguarded digest: %v", err)
	}
	if slices.Equal(guarded, unguarded) {
		t.Fatal("the slash guard did not change the digest, so the expectation is not reading the real blocks")
	}
	blocks, err := buildUserMessageBlocks("/plan the work", nil, true)
	if err != nil {
		t.Fatalf("buildUserMessageBlocks: %v", err)
	}
	if !slices.Equal(guarded, EchoBlockDigest(echoLine(t, blocks))) {
		t.Error("the guarded digest does not match the guarded blocks' echo")
	}
}

// TestBlockDigestSeparatesBlockKindsAndImagePayloads pins what identity means:
// the same bytes under a different media type, or a different payload under the
// same one, are different blocks.
func TestBlockDigestSeparatesBlockKindsAndImagePayloads(t *testing.T) {
	png := provider.ImageAttachment{MimeType: "image/png", Data: []byte("payload")}
	jpeg := provider.ImageAttachment{MimeType: "image/jpeg", Data: []byte("payload")}
	other := provider.ImageAttachment{MimeType: "image/png", Data: []byte("different")}

	digestFor := func(a provider.ImageAttachment) BlockDigest {
		t.Helper()
		digest, err := UserMessageBlockDigest("[Image #1]", []provider.ImageAttachment{a}, false)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return digest
	}
	if slices.Equal(digestFor(png), digestFor(jpeg)) {
		t.Error("media type is not part of an image block's identity")
	}
	if slices.Equal(digestFor(png), digestFor(other)) {
		t.Error("image payload is not part of an image block's identity")
	}
	// The digest never carries the payload itself, only a hash of it.
	if strings.Contains(strings.Join(digestFor(png), " "), "payload") {
		t.Error("the digest embeds the image payload; it must hold only a hash")
	}
}

// TestEchoBlockDigestOnUnusableContent covers the absent and malformed cases:
// both report nothing, which leaves the merge comparison unavailable rather
// than producing a digest that could match by accident.
func TestEchoBlockDigestOnUnusableContent(t *testing.T) {
	for _, content := range []json.RawMessage{nil, json.RawMessage(`{"type":"text"}`), json.RawMessage(`17`)} {
		if got := EchoBlockDigest(content); got != nil {
			t.Errorf("EchoBlockDigest(%s) = %v, want nil", content, got)
		}
	}
}
