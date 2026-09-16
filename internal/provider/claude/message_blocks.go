package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/provider"
)

// message_blocks.go fingerprints the Anthropic Messages content blocks a user
// message carries, on BOTH sides of the wire: the blocks Send writes to stdin
// and the blocks a `user{isReplay:true}` echo brings back.
//
// Why a fingerprint exists at all: at a turn boundary the CLI merges N
// consecutive queued prompt commands into one message whose content is the
// flat concatenation of every member's blocks, and acknowledges it under the
// LAST member's uuid (claude-wire.md §Queued-message consumption). AO joins a
// drain it dispatches itself, but a message flushed at one drain and another
// flushed at a later drain reach the CLI queue separately and are merged there.
// The discriminator is the survivor's echo: it carries strictly MORE blocks
// than AO sent under that uuid, and the extra leading blocks are byte-identical
// to what AO sent for the earlier message(s). Triage folds on that evidence
// (internal/triage/claude_merge_fold.go) and refuses to guess on anything else.
//
// Fingerprints rather than the blocks themselves because the comparison is
// registry state that outlives the send: a per-block hash is ~70 bytes whatever
// the message weighs, so retaining the recent sends of a thread costs nothing
// and an inlined image never sits in memory twice.

// BlockDigest is the per-block fingerprint list of one user message's content,
// in wire order. Equal elements mean byte-identical blocks, so the comparisons
// the merge test needs are plain string-slice ones and stay in the consumer
// (internal/triage/claude_merge_digest.go) where the provider vocabulary is
// already provider-neutral.
type BlockDigest []string

// userContentBlock is one content block reduced to the fields that decide
// identity. `data` holds an image's base64 payload exactly as it rides the
// wire, so the two builders below compare the same bytes without either side
// re-encoding.
type userContentBlock struct {
	kind      string
	text      string
	mediaType string
	data      string
	// raw is the canonical JSON of a block type this file does not model.
	// Hashing it keeps the digest total: an unmodelled block still has a
	// stable identity, and a message containing one can still be recognised
	// (or, if the shapes differ, provably not recognised).
	raw string
}

func (b userContentBlock) digest() string {
	switch b.kind {
	case "text":
		return "t:" + hashString(b.text)
	case "image":
		return "i:" + b.mediaType + ":" + hashString(b.data)
	default:
		return "?:" + b.kind + ":" + hashString(b.raw)
	}
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func blockDigest(blocks []userContentBlock) BlockDigest {
	if len(blocks) == 0 {
		return nil
	}
	digest := make(BlockDigest, 0, len(blocks))
	for _, block := range blocks {
		digest = append(digest, block.digest())
	}
	return digest
}

// UserMessageBlockDigest fingerprints the content blocks Send will write for
// this message. It runs the REAL block builder rather than re-deriving the
// shape, so the expectation triage registers and the bytes that reach stdin
// cannot drift apart — including the outbound slash guard, which changes the
// first text block.
func UserMessageBlockDigest(content string, attachments []provider.ImageAttachment, forceModelProse bool) (BlockDigest, error) {
	blocks, err := buildUserMessageBlocks(content, attachments, forceModelProse)
	if err != nil {
		return nil, err
	}
	return blockDigest(outboundUserBlocks(blocks)), nil
}

// outboundUserBlocks reduces the block maps buildUserMessageBlocks produced.
// The shapes are this file's own, so the reads are direct; anything else is
// carried through as an unmodelled block rather than dropped.
func outboundUserBlocks(blocks []map[string]any) []userContentBlock {
	out := make([]userContentBlock, 0, len(blocks))
	for _, block := range blocks {
		kind, _ := block["type"].(string)
		switch kind {
		case "text":
			text, _ := block["text"].(string)
			out = append(out, userContentBlock{kind: kind, text: text})
		case "image":
			source, _ := block["source"].(map[string]any)
			mediaType, _ := source["media_type"].(string)
			data, _ := source["data"].(string)
			out = append(out, userContentBlock{kind: kind, mediaType: mediaType, data: data})
		default:
			out = append(out, userContentBlock{kind: kind, raw: canonicalJSON(block)})
		}
	}
	return out
}

// EchoBlockDigest fingerprints the content blocks a replayed user envelope
// carries. `content` is the envelope's `message.content`.
//
// A plain string content is ONE text block: that is what the SDK's older
// user-message shape is, and buildUserMessageBlocks produces a single text
// block for a message with no images, so the two agree.
func EchoBlockDigest(content json.RawMessage) BlockDigest {
	blocks, ok := echoUserBlocks(content)
	if !ok {
		return nil
	}
	return blockDigest(blocks)
}

func echoUserBlocks(content json.RawMessage) ([]userContentBlock, bool) {
	if len(content) == 0 {
		return nil, false
	}
	var asString string
	if json.Unmarshal(content, &asString) == nil {
		return []userContentBlock{{kind: "text", text: asString}}, true
	}
	var asBlocks []map[string]json.RawMessage
	if json.Unmarshal(content, &asBlocks) != nil {
		return nil, false
	}
	out := make([]userContentBlock, 0, len(asBlocks))
	for _, block := range asBlocks {
		kind := readRawString(block["type"])
		switch kind {
		case "text":
			out = append(out, userContentBlock{kind: kind, text: readRawString(block["text"])})
		case "image":
			var source struct {
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			}
			if len(block["source"]) > 0 {
				_ = json.Unmarshal(block["source"], &source)
			}
			out = append(out, userContentBlock{kind: kind, mediaType: source.MediaType, data: source.Data})
		default:
			out = append(out, userContentBlock{kind: kind, raw: canonicalJSONRaw(block)})
		}
	}
	return out, true
}

// canonicalJSON / canonicalJSONRaw render an unmodelled block with its keys in
// a fixed order. Go's json.Marshal already sorts map keys; the raw form is
// written explicitly because json.RawMessage values must not be re-quoted.
//
// The two renderings are NOT guaranteed to agree byte for byte across the wire
// (the raw form preserves the sender's own number and escape spelling), so an
// unmodelled block is a digest that stays stable on its own side and simply
// fails to match the other. That is the safe direction: the fold declines. It
// costs nothing today because buildUserMessageBlocks emits only text and image
// blocks; this branch exists so an echo carrying something else still produces
// a total, comparable digest instead of a silently truncated one.
func canonicalJSON(block map[string]any) string {
	encoded, err := json.Marshal(block)
	if err != nil {
		return fmt.Sprintf("%v", block)
	}
	return string(encoded)
}

func canonicalJSONRaw(block map[string]json.RawMessage) string {
	keys := make([]string, 0, len(block))
	for key := range block {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			continue
		}
		out.Write(encodedKey)
		out.WriteByte(':')
		out.Write(block[key])
	}
	out.WriteByte('}')
	return out.String()
}
