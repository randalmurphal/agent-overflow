package threadtools

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// thread_item: read inside one item, in ranges, once the agent knows which
// one. Exactly one selector per call, so a result is never a mix of two
// readings of the same payload.
//
// Nothing here loads a payload whole. An offset read takes one range, a
// line range walks chunks counting newlines, and a query scans chunks with
// an overlap that keeps a match's context available without a second pass.

// maxQueryBytes bounds a literal search string. A query longer than the
// scan overlap could straddle a chunk in ways the scan cannot see.
const maxQueryBytes = 4096

type itemArgs struct {
	ThreadID   string `json:"thread_id"`
	ComputerID string `json:"computer_id"`
	ItemID     string `json:"item_id"`
	Offset     *int64 `json:"offset"`
	MaxBytes   int    `json:"max_bytes"`
	Lines      string `json:"lines"`
	Query      string `json:"query"`
	Cursor     string `json:"cursor"`
}

type itemMatch struct {
	Offset  int64  `json:"offset"`
	Context string `json:"context"`
}

type itemResult struct {
	ThreadID   string `json:"thread_id"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	ItemID     string `json:"item_id"`
	Kind       string `json:"kind,omitempty"`
	Size       int64  `json:"size"`
	Offset     int64  `json:"offset"`
	Bytes      int    `json:"bytes"`
	Text       string `json:"text,omitempty"`
	// EOF is true when this read reached the end of the payload.
	EOF bool `json:"eof"`
	// FirstLine and LastLine are set by a line range.
	FirstLine int `json:"first_line,omitempty"`
	LastLine  int `json:"last_line,omitempty"`
	// Query results.
	Query      string      `json:"query,omitempty"`
	Matches    []itemMatch `json:"matches,omitempty"`
	MatchCount int         `json:"match_count,omitempty"`
	More       bool        `json:"more,omitempty"`
	Cursor     string      `json:"cursor,omitempty"`
	Note       string      `json:"note,omitempty"`
}

func (c *session) item(ctx context.Context, raw json.RawMessage) (any, error) {
	var args itemArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	if trim(args.ItemID) == "" {
		return nil, invalidf("item_id is required. Take it from a thread_show row or a thread_search hit.")
	}
	page, err := decodeCursor(args.Cursor, cursorItem)
	if err != nil {
		return nil, err
	}
	if page.Query != "" && trim(args.Query) == "" {
		args.Query = page.Query
	}
	if selectors := exactlyOne(args.Offset != nil, trim(args.Lines) != "", trim(args.Query) != ""); selectors != 1 {
		return nil, invalidf("Pass exactly one selector: offset with max_bytes to read a byte range, lines for a line range, or query to search inside the item. This call passed %d.", selectors)
	}
	if args.MaxBytes < 0 || args.MaxBytes > MaxItemBytes {
		return nil, invalidf("max_bytes must be between 1 and %d.", MaxItemBytes)
	}
	maxBytes := int64(args.MaxBytes)
	if maxBytes == 0 {
		maxBytes = DefaultItemBytes
	}
	target, err := c.resolve(ctx, args.ThreadID, trim(args.ComputerID))
	if err != nil {
		return nil, err
	}
	if !target.Local {
		return c.itemOnPeer(ctx, target, args)
	}

	reader := payloadReader{app: c.app, threadID: target.ThreadID, itemID: trim(args.ItemID)}
	meta, err := reader.meta(ctx)
	if err != nil {
		return nil, err
	}
	id, name := c.stamp(Computer{ID: target.ComputerID, Name: target.Computer})
	result := itemResult{ThreadID: target.ThreadID, ComputerID: id, Computer: name, ItemID: reader.itemID, Kind: meta.Kind, Size: meta.Size}
	if note := partialNote(target.Partial); note != "" {
		result.Note = note
	}
	switch {
	case trim(args.Query) != "":
		return c.itemQuery(ctx, reader, meta, result, trim(args.Query), page.Offset)
	case trim(args.Lines) != "":
		return c.itemLines(ctx, reader, meta, result, trim(args.Lines), maxBytes)
	default:
		return c.itemRange(ctx, reader, meta, result, *args.Offset, maxBytes)
	}
}

// itemRange reads one byte range. A negative offset reads from the end and
// clamps to the start; a range that would split a UTF-8 character is
// widened outward to whole characters; an offset at or past the end is an
// empty read reporting the size, which is an answer and not an error.
func (c *session) itemRange(ctx context.Context, reader payloadReader, meta Payload, result itemResult, offset, maxBytes int64) (any, error) {
	start := offset
	if start < 0 {
		start = meta.Size + start
		if start < 0 {
			start = 0
		}
	}
	if start >= meta.Size {
		result.Offset, result.EOF = meta.Size, true
		result.Note = appendNote(result.Note, "That offset is at or past the end of the item, which holds "+humanBytes(meta.Size)+".")
		return result, nil
	}
	end := start + maxBytes
	if end > meta.Size {
		end = meta.Size
	}
	// Read a few bytes on each side so the range can be widened outward
	// to whole characters without a second round trip.
	low := start - 3
	if low < 0 {
		low = 0
	}
	high := end + 3
	if high > meta.Size {
		high = meta.Size
	}
	payload, err := reader.read(ctx, low, high-low)
	if err != nil {
		return nil, err
	}
	from := widenLeft(payload.Bytes, int(start-low))
	to := widenRight(payload.Bytes, int(end-low))
	text := payload.Bytes[from:to]
	result.Offset = low + int64(from)
	result.Bytes = len(text)
	result.Text = string(text)
	result.EOF = result.Offset+int64(result.Bytes) >= meta.Size
	if !result.EOF {
		result.Note = appendNote(result.Note, "Continue at offset "+strconv.FormatInt(result.Offset+int64(result.Bytes), 10)+".")
	}
	return result, nil
}

// widenLeft moves an index back to the start of the character it is
// inside. widenRight moves an index forward to the next character start.
func widenLeft(data []byte, index int) int {
	if index <= 0 {
		return 0
	}
	if index >= len(data) {
		return len(data)
	}
	for index > 0 && !utf8.RuneStart(data[index]) {
		index--
	}
	return index
}

func widenRight(data []byte, index int) int {
	if index >= len(data) {
		return len(data)
	}
	if index < 0 {
		return 0
	}
	for index < len(data) && !utf8.RuneStart(data[index]) {
		index++
	}
	return index
}

// itemLines walks the payload counting newlines and returns the requested
// 1-based inclusive range, clipped to the byte budget.
func (c *session) itemLines(ctx context.Context, reader payloadReader, meta Payload, result itemResult, spec string, maxBytes int64) (any, error) {
	first, last, err := parseLineRange(spec)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	line := 1
	var offset int64
	started := false
	var startOffset int64
	truncated := false
	for offset < meta.Size {
		payload, err := reader.read(ctx, offset, itemScanChunk)
		if err != nil {
			return nil, err
		}
		if len(payload.Bytes) == 0 {
			break
		}
		for index := 0; index < len(payload.Bytes); index++ {
			if line > last {
				break
			}
			if line >= first {
				if !started {
					started, startOffset = true, offset+int64(index)
				}
				if int64(out.Len()) >= maxBytes {
					truncated = true
					break
				}
				out.WriteByte(payload.Bytes[index])
			}
			if payload.Bytes[index] == '\n' {
				line++
			}
		}
		offset += int64(len(payload.Bytes))
		if line > last || truncated {
			break
		}
	}
	result.FirstLine, result.LastLine = first, min(last, line)
	result.Offset = startOffset
	result.Text = out.String()
	result.Bytes = out.Len()
	result.EOF = offset >= meta.Size && !truncated
	if truncated {
		result.Note = appendNote(result.Note, "The line range was longer than max_bytes and stopped early. Read the rest with offset "+strconv.FormatInt(startOffset+int64(out.Len()), 10)+", or ask for fewer lines.")
	}
	if !started {
		result.Note = appendNote(result.Note, "The item has fewer than "+strconv.Itoa(first)+" lines; it holds "+strconv.Itoa(line)+".")
	}
	return result, nil
}

func parseLineRange(spec string) (int, int, error) {
	bad := invalidf("lines must be a 1-based inclusive range written \"first-last\", \"first-\" to read to the end, or \"first\" for one line.")
	first, rest, hasRange := strings.Cut(spec, "-")
	start, err := strconv.Atoi(trim(first))
	if err != nil || start < 1 {
		return 0, 0, bad
	}
	if !hasRange {
		return start, start, nil
	}
	if trim(rest) == "" {
		return start, 1 << 30, nil
	}
	end, err := strconv.Atoi(trim(rest))
	if err != nil || end < start {
		return 0, 0, bad
	}
	return start, end, nil
}

// itemQuery scans for a literal string and reports up to MaxItemMatches
// offsets with a little context each, plus a cursor for the rest.
func (c *session) itemQuery(ctx context.Context, reader payloadReader, meta Payload, result itemResult, query string, from int64) (any, error) {
	if len(query) > maxQueryBytes {
		return nil, invalidf("query must be at most %d bytes of literal text.", maxQueryBytes)
	}
	needle := []byte(query)
	result.Query = query
	scanFrom := from
	matches := make([]itemMatch, 0, MaxItemMatches)
	more := false
	for scanFrom < meta.Size {
		bufStart := scanFrom - ItemMatchContext
		if bufStart < 0 {
			bufStart = 0
		}
		payload, err := reader.read(ctx, bufStart, itemScanChunk)
		if err != nil {
			return nil, err
		}
		if len(payload.Bytes) == 0 {
			break
		}
		bufEnd := bufStart + int64(len(payload.Bytes))
		final := bufEnd >= meta.Size
		limit := bufEnd
		if !final {
			limit -= int64(len(needle)) + ItemMatchContext
		}
		if limit <= scanFrom && !final {
			// The scan chunk cannot make progress with this needle.
			break
		}
		cursorIn := int(scanFrom - bufStart)
		for cursorIn < len(payload.Bytes) {
			found := bytes.Index(payload.Bytes[cursorIn:], needle)
			if found < 0 {
				break
			}
			at := bufStart + int64(cursorIn+found)
			if at >= limit && !final {
				break
			}
			if len(matches) == MaxItemMatches {
				more = true
				break
			}
			matches = append(matches, itemMatch{Offset: at, Context: matchContext(payload.Bytes, int(at-bufStart), len(needle))})
			cursorIn = cursorIn + found + len(needle)
			scanFrom = at + int64(len(needle))
		}
		if more || final {
			if !more {
				scanFrom = meta.Size
			}
			break
		}
		if next := limit; next > scanFrom {
			scanFrom = next
		} else {
			break
		}
	}
	result.Matches, result.MatchCount, result.More = matches, len(matches), more
	result.Offset = 0
	result.EOF = !more
	if more {
		encoded, err := encodeCursor(cursor{Kind: cursorItem, Thread: result.ThreadID, Query: query, Offset: scanFrom})
		if err != nil {
			return nil, err
		}
		result.Cursor = encoded
		result.Note = appendNote(result.Note, "More matches exist. Pass cursor back unchanged for the next page.")
	}
	if len(matches) == 0 {
		result.Note = appendNote(result.Note, "No match. The search is literal and case-sensitive, not a regular expression.")
	} else {
		result.Note = appendNote(result.Note, "Read a match with offset set to its offset.")
	}
	return result, nil
}

func matchContext(buffer []byte, at, length int) string {
	low := widenRight(buffer, max(0, at-ItemMatchContext))
	high := widenLeft(buffer, min(len(buffer), at+length+ItemMatchContext))
	if high < low {
		high = low
	}
	return string(buffer[low:high])
}

// payloadReader is the chunked view of one item's stored bytes.
type payloadReader struct {
	app      App
	threadID string
	itemID   string
}

// meta reads the payload's size and kind without any of its bytes.
func (r payloadReader) meta(ctx context.Context) (Payload, error) {
	return r.app.ItemPayload(ctx, PayloadQuery{ThreadID: r.threadID, ItemID: r.itemID, Offset: 0, MaxBytes: 0})
}

func (r payloadReader) read(ctx context.Context, offset, maxBytes int64) (Payload, error) {
	return r.app.ItemPayload(ctx, PayloadQuery{ThreadID: r.threadID, ItemID: r.itemID, Offset: offset, MaxBytes: maxBytes})
}

func (c *session) itemOnPeer(ctx context.Context, target Target, args itemArgs) (any, error) {
	peer, err := c.app.Peer(ctx, target.ComputerID)
	if err != nil {
		return nil, err
	}
	args.ThreadID, args.ComputerID = target.ThreadID, ""
	raw, err := forwardItemArgs(args)
	if err != nil {
		return nil, err
	}
	answer, err := peer.Query(ctx, "thread_item", raw)
	if err != nil {
		return nil, err
	}
	var result itemResult
	if err := peerResult(answer, &result); err != nil {
		return nil, err
	}
	result.ComputerID, result.Computer = c.stamp(Computer{ID: target.ComputerID, Name: target.Computer})
	if note := partialNote(target.Partial); note != "" {
		result.Note = appendNote(result.Note, note)
	}
	return result, nil
}

// forwardItemArgs keeps a zero offset, which forwardArgs would drop: for
// thread_item, offset 0 is a real selector and its absence is another.
func forwardItemArgs(args itemArgs) (json.RawMessage, error) {
	fields := map[string]any{"thread_id": args.ThreadID, "item_id": args.ItemID}
	if args.Offset != nil {
		fields["offset"] = *args.Offset
	}
	if args.MaxBytes != 0 {
		fields["max_bytes"] = args.MaxBytes
	}
	if trim(args.Lines) != "" {
		fields["lines"] = args.Lines
	}
	if trim(args.Query) != "" {
		fields["query"] = args.Query
	}
	if args.Cursor != "" {
		fields["cursor"] = args.Cursor
	}
	return json.Marshal(fields)
}
