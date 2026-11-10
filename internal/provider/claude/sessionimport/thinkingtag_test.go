package sessionimport

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// Signatures here are built by hand. A signature from a real session is
// never committed.

func protoBytes(field int, value []byte) []byte {
	out := protoUvarint(uint64(field)<<3 | 2)
	out = append(out, protoUvarint(uint64(len(value)))...)
	return append(out, value...)
}

func protoInt(field int, value uint64) []byte {
	return append(protoUvarint(uint64(field)<<3), protoUvarint(value)...)
}

func protoUvarint(value uint64) []byte {
	var out []byte
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// taggedSignatureBytes is the observed layout: field 2 holds a header
// (field 1: two varints, then the tag in field 8) followed by opaque
// fields, all beside an opaque top-level field 1.
func taggedSignatureBytes(tag string) []byte {
	header := concat(protoInt(1, 3), protoInt(2, 7), protoBytes(8, []byte(tag)))
	body := concat(
		protoBytes(1, header),
		protoBytes(2, []byte("opaque-two")),
		protoBytes(3, []byte{0xff, 0x00, 0x80}),
		protoInt(4, 1<<40),
		protoBytes(5, make([]byte, 300)),
	)
	return concat(protoBytes(1, []byte("opaque-top")), protoBytes(2, body))
}

func taggedSignature(tag string) string {
	return base64.StdEncoding.EncodeToString(taggedSignatureBytes(tag))
}

func TestThinkingTagReadsTheHeaderTag(t *testing.T) {
	minimal := []byte{0x12, 0x0d, 0x0a, 0x0b, 0x42, 0x09, 'n', 'a', 'r', 'r', 'a', 't', 'i', 'o', 'n'}
	fixed := concat(
		[]byte{0x09, 1, 2, 3, 4, 5, 6, 7, 8}, // field 1, fixed64
		[]byte{0x1d, 1, 2, 3, 4},             // field 3, fixed32
		taggedSignatureBytes("narration"),
	)
	lastWins := concat(taggedSignatureBytes("thinking"), taggedSignatureBytes("narration"))
	headerLastWins := protoBytes(2, protoBytes(1, concat(
		protoBytes(8, []byte("thinking")),
		protoBytes(8, []byte("narration")),
	)))
	cases := []struct {
		name      string
		signature string
		tag       string
	}{
		{"minimal narration", base64.StdEncoding.EncodeToString(minimal), "narration"},
		{"narration with opaque siblings", taggedSignature("narration"), "narration"},
		{"thinking", taggedSignature("thinking"), "thinking"},
		{"unpadded base64", strings.TrimRight(taggedSignature("narration"), "="), "narration"},
		{"fixed-width fields skipped", base64.StdEncoding.EncodeToString(fixed), "narration"},
		{"last field 2 wins", base64.StdEncoding.EncodeToString(lastWins), "narration"},
		{"last field 8 wins", base64.StdEncoding.EncodeToString(headerLastWins), "narration"},
		{"empty", "", ""},
		{"not base64", "!!not base64!!", ""},
		{"garbage bytes", base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}), ""},
		{"no field 2", base64.StdEncoding.EncodeToString(protoBytes(1, []byte("opaque"))), ""},
		{"no header", base64.StdEncoding.EncodeToString(protoBytes(2, protoBytes(2, []byte("opaque")))), ""},
		{"no tag field", base64.StdEncoding.EncodeToString(protoBytes(2, protoBytes(1, protoInt(1, 3)))), ""},
		{"empty tag", base64.StdEncoding.EncodeToString(protoBytes(2, protoBytes(1, protoBytes(8, nil)))), ""},
		{"tag is not UTF-8", base64.StdEncoding.EncodeToString(protoBytes(2, protoBytes(1, protoBytes(8, []byte{0xff, 0xfe})))), ""},
		{"tag as varint", base64.StdEncoding.EncodeToString(protoBytes(2, protoBytes(1, protoInt(8, 9)))), ""},
		{"group wire type", base64.StdEncoding.EncodeToString(append([]byte{0x0b}, taggedSignatureBytes("narration")...)), ""},
		{"length past the end", base64.StdEncoding.EncodeToString([]byte{0x12, 0x7f, 0x0a, 0x01}), ""},
		{"trailing garbage after the tag", base64.StdEncoding.EncodeToString(append(taggedSignatureBytes("narration"), 0x12)), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThinkingTag(tc.signature); got != tc.tag {
				t.Fatalf("ThinkingTag = %q, want %q", got, tc.tag)
			}
			if got, want := ThinkingIsNarration(tc.signature), tc.tag == "narration"; got != want {
				t.Fatalf("ThinkingIsNarration = %v, want %v", got, want)
			}
		})
	}
}

func TestThinkingTagRejectsEveryTruncation(t *testing.T) {
	whole := taggedSignatureBytes("narration")
	if ThinkingTag(base64.StdEncoding.EncodeToString(whole)) != "narration" {
		t.Fatal("the whole signature must carry the tag")
	}
	for cut := 1; cut < len(whole); cut++ {
		// A cut that lands on a top-level field boundary leaves a
		// well-formed prefix; only the one after the opaque field 1
		// exists here, and it holds no field 2.
		if got := ThinkingTag(base64.StdEncoding.EncodeToString(whole[:cut])); got != "" {
			t.Fatalf("signature cut at byte %d of %d read tag %q", cut, len(whole), got)
		}
	}
}

func TestThinkingTagBoundsItsInput(t *testing.T) {
	oversized := concat(protoBytes(1, make([]byte, MaxThinkingSignatureBytes)), taggedSignatureBytes("narration"))
	if got := ThinkingTag(base64.StdEncoding.EncodeToString(oversized)); got != "" {
		t.Fatalf("oversized signature read tag %q", got)
	}
	long := strings.Repeat("n", maxThinkingTagBytes+1)
	if got := ThinkingTag(taggedSignature(long)); got != "" {
		t.Fatalf("oversized tag read as %q", got)
	}
}

// The 2.1.132 capture predates the tag: its signature is opaque.
func TestThinkingTagIsAbsentFromThePreTagFixture(t *testing.T) {
	const fixture = "../../../../docs/references/fixtures/claude/opus47_thinking_summarized.ndjson"
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	signatures := 0
	for scanner.Scan() {
		var row struct {
			Message struct {
				Content []struct {
					Type      string `json:"type"`
					Signature string `json:"signature"`
				} `json:"content"`
			} `json:"message"`
			Event struct {
				Delta struct {
					Type      string `json:"type"`
					Signature string `json:"signature"`
				} `json:"delta"`
			} `json:"event"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		var found []string
		if row.Event.Delta.Type == "signature_delta" {
			found = append(found, row.Event.Delta.Signature)
		}
		for _, block := range row.Message.Content {
			if block.Type == "thinking" && block.Signature != "" {
				found = append(found, block.Signature)
			}
		}
		for _, signature := range found {
			signatures++
			if ThinkingIsNarration(signature) {
				t.Fatalf("fixture signature read as narration (tag %q)", ThinkingTag(signature))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if signatures == 0 {
		t.Fatal("fixture holds no thinking signature")
	}
}

// A transcript thinking block whose signature carries the narration tag
// converts to a thinking event carrying the marker beside its signature;
// every other block converts exactly as before.
func TestConvertMarksNarrationThinkingBlocks(t *testing.T) {
	narration := taggedSignature("narration")
	reasoning := taggedSignature("thinking")
	events, warnings := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "do it", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			map[string]any{"type": "thinking", "thinking": "Read the file; the fix is one line.", "signature": narration},
			map[string]any{"type": "thinking", "thinking": "hmm", "signature": reasoning},
			map[string]any{"type": "thinking", "thinking": "unsigned"},
			textBlock("done"),
		}, "2026-01-01T00:00:01.000Z"),
	)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	thinking := eventsOfKind(events, provider.EventThinking)
	if len(thinking) != 3 {
		t.Fatalf("thinking events = %d, want 3", len(thinking))
	}
	marked := decodeMeta(t, thinking[0].Meta)
	if marked[provider.MetaNarrationKey] != true || marked["signature"] != narration || len(marked) != 2 {
		t.Fatalf("narration block meta = %s", thinking[0].Meta)
	}
	if !thinking[0].ContentPresent || thinking[0].ItemID != "msg_1#0" {
		t.Fatalf("narration block event = %+v", thinking[0])
	}
	if got, want := string(thinking[1].Meta), `{"signature":"`+reasoning+`"}`; got != want {
		t.Fatalf("reasoning block meta = %s, want %s", got, want)
	}
	if len(thinking[2].Meta) != 0 {
		t.Fatalf("unsigned block meta = %s, want none", thinking[2].Meta)
	}
	for _, evt := range eventsOfKind(events, provider.EventTextDelta) {
		if len(evt.Meta) != 0 {
			t.Fatalf("text block meta = %s, want none", evt.Meta)
		}
	}
}
