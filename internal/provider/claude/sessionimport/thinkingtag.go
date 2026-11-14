package sessionimport

import (
	"encoding/base64"
	"unicode/utf8"
)

// thinkingtag.go reads the tag a thinking block's `signature` carries.
//
// The signature is base64 of a protobuf message. Its field 2 holds a
// message whose field 1 is a header, and the header's field 8 is a short
// UTF-8 tag: "thinking" for reasoning, "narration" for a block the model
// wrote as prose for the user. Every other field is opaque here. A reader
// of a non-repeated field keeps its last occurrence, so each level takes
// the last one. See claude-wire.md §The signature tag.
//
// The file lives in this package because both the live parser and the
// transcript converter read the tag, and the live parser imports this
// package.

// ThinkingTagNarration is the tag of a thinking block the model wrote as
// prose for the user.
const ThinkingTagNarration = "narration"

// MaxThinkingSignatureBytes bounds the signature a tag is read from. A
// signature observed on the wire is under 2 KiB; one past this bound is
// untagged. The live parser accumulates signature deltas up to the same
// bound.
const MaxThinkingSignatureBytes = 256 << 10

// maxThinkingTagBytes bounds the tag itself.
const maxThinkingTagBytes = 64

// ThinkingTag returns the tag of a thinking block's signature, or "" when
// the signature is empty, is not base64, is not a well-formed message, or
// carries no tag.
func ThinkingTag(signature string) string {
	if signature == "" || len(signature) > MaxThinkingSignatureBytes {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(signature)
		if err != nil {
			return ""
		}
	}
	body, ok := lastBytesField(raw, 2)
	if !ok {
		return ""
	}
	header, ok := lastBytesField(body, 1)
	if !ok {
		return ""
	}
	tag, ok := lastBytesField(header, 8)
	if !ok || len(tag) == 0 || len(tag) > maxThinkingTagBytes || !utf8.Valid(tag) {
		return ""
	}
	return string(tag)
}

// ThinkingIsNarration reports whether a thinking block's signature tags it
// as prose for the user.
func ThinkingIsNarration(signature string) bool {
	return ThinkingTag(signature) == ThinkingTagNarration
}

// lastBytesField returns the last length-delimited occurrence of field in
// one protobuf message. It walks the whole message, skipping varint and
// fixed-width fields, and reports false when the field is absent or the
// message is malformed anywhere: a truncated or foreign blob has no tag.
func lastBytesField(msg []byte, field uint64) ([]byte, bool) {
	var (
		found []byte
		seen  bool
	)
	for len(msg) > 0 {
		key, n := protoVarint(msg)
		if n == 0 {
			return nil, false
		}
		msg = msg[n:]
		switch key & 7 {
		case 0:
			_, n := protoVarint(msg)
			if n == 0 {
				return nil, false
			}
			msg = msg[n:]
		case 1:
			if len(msg) < 8 {
				return nil, false
			}
			msg = msg[8:]
		case 2:
			size, n := protoVarint(msg)
			if n == 0 || size > uint64(len(msg)-n) {
				return nil, false
			}
			end := n + int(size)
			if key>>3 == field {
				found, seen = msg[n:end], true
			}
			msg = msg[end:]
		case 5:
			if len(msg) < 4 {
				return nil, false
			}
			msg = msg[4:]
		default:
			return nil, false
		}
	}
	return found, seen
}

// protoVarint decodes one base-128 varint and returns it with the bytes it
// used, or 0 bytes when the input ends mid-value or the value overflows 64
// bits.
func protoVarint(b []byte) (uint64, int) {
	var value uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if i == 9 && c > 1 {
			return 0, 0
		}
		value |= uint64(c&0x7f) << (7 * uint(i))
		if c < 0x80 {
			return value, i + 1
		}
	}
	return 0, 0
}
