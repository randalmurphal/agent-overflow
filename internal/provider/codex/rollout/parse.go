package rollout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-overflow/internal/importir"
)

// ErrSourceShrank means the file is smaller than the offset we were asked to
// resume from. A rollout is append-only, so that can only mean the file was
// replaced or truncated and the cursor no longer addresses the same history.
var ErrSourceShrank = errors.New("rollout: source file is smaller than the resume offset")

// ParseOptions configures Parse.
type ParseOptions struct {
	// Path is the rollout JSONL file.
	Path string
	// SessionID is the uuid from the file name and the authority for which
	// `session_meta` line belongs to this file. Empty derives it from Path.
	SessionID string
	// FromOffset resumes a tail read. It must be an EndOffset a previous
	// Parse returned; any other value risks starting mid-record.
	FromOffset int64
	// MaxLineBytes caps one line; 0 uses DefaultMaxLineBytes.
	MaxLineBytes int
}

// ParseResult is one rollout file turned into import events.
type ParseResult struct {
	// Meta is the accepted session_meta. Zero when the file had none —
	// Warnings names that; it is not fatal, because a file whose head was
	// lost still has usable history.
	Meta SessionMeta
	// Events are in file order, with the source coordinates of the line
	// each came from. See importir.Event and the "Source coordinates"
	// section of this package's AGENTS.md.
	Events []importir.Event
	// EndOffset is the first byte after the last COMPLETE line consumed.
	// Feeding it back as FromOffset resumes exactly where this call
	// stopped, including across a rollout that was mid-write.
	EndOffset int64
	// CorruptLines counts lines skipped as unreadable (invalid JSON,
	// embedded NUL, or past MaxLineBytes).
	CorruptLines int
	// UnknownTypes counts skipped lines by their qualified type. Codex's
	// rollout enum is open; an entry here is information, not an error.
	UnknownTypes map[string]int
	Warnings     []importir.Warning
}

// Parse reads a Codex rollout file into import events.
//
// Two properties the callers depend on:
//
//   - Skip-unknown is mandatory. Every unrecognised envelope type, payload
//     type, or tool shape is counted and skipped. Codex ships new rollout
//     variants between releases (`world_state` and
//     `inter_agent_communication_metadata` are both post-0.142 additions), and
//     an importer that failed on them would break on the next Codex update.
//   - EndOffset is exact and resumable. A trailing half-written line is not
//     consumed, so the cursor always lands on a record boundary.
func Parse(ctx context.Context, opts ParseOptions) (ParseResult, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return ParseResult{}, errors.New("rollout: Path is required")
	}
	sessionID := strings.TrimSpace(opts.SessionID)
	if sessionID == "" {
		sessionID = SessionIDFromPath(opts.Path)
	}
	if sessionID == "" {
		return ParseResult{}, fmt.Errorf("rollout: %s: cannot determine session id", opts.Path)
	}
	opts.SessionID = sessionID
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = DefaultMaxLineBytes
	}
	if opts.FromOffset < 0 {
		opts.FromOffset = 0
	}

	file, err := os.Open(opts.Path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("rollout: open %s: %w", opts.Path, err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return ParseResult{}, fmt.Errorf("rollout: stat %s: %w", opts.Path, err)
	}
	if opts.FromOffset > stat.Size() {
		return ParseResult{}, fmt.Errorf("rollout: %s (offset %d, size %d): %w",
			opts.Path, opts.FromOffset, stat.Size(), ErrSourceShrank)
	}
	if err := requireRecordBoundary(file, opts); err != nil {
		return ParseResult{}, err
	}

	pre, err := preScan(ctx, file, opts)
	if err != nil {
		return ParseResult{}, err
	}
	if _, err := file.Seek(opts.FromOffset, io.SeekStart); err != nil {
		return ParseResult{}, fmt.Errorf("rollout: seek %s: %w", opts.Path, err)
	}

	c := newConverter(opts, pre)
	if err := c.run(ctx, file); err != nil {
		return ParseResult{}, err
	}
	return c.result(), nil
}

// requireRecordBoundary confirms FromOffset still lands one byte past a
// newline.
//
// The size check alone only catches a file that SHRANK. A rollout that was
// truncated and then re-grown past the cursor is the same size or larger
// while the cursor now addresses the middle of a record that is not the one
// it was taken after — resuming there splices a foreign session's tail onto
// the thread as if it were the same conversation. Every EndOffset this
// package hands out sits immediately after a newline, so the byte before a
// valid cursor is one; anything else means the file diverged.
func requireRecordBoundary(f *os.File, opts ParseOptions) error {
	if opts.FromOffset <= 0 {
		return nil
	}
	var probe [1]byte
	if _, err := f.ReadAt(probe[:], opts.FromOffset-1); err != nil {
		return fmt.Errorf("rollout: %s: cannot read the byte before resume offset %d (%v): %w",
			opts.Path, opts.FromOffset, err, ErrSourceShrank)
	}
	if probe[0] != '\n' {
		return fmt.Errorf("rollout: %s: resume offset %d no longer follows a record boundary: %w",
			opts.Path, opts.FromOffset, ErrSourceShrank)
	}
	return nil
}

// preScan is the cheap first pass. It answers the file-global questions the
// conversion pass cannot answer as it goes:
//
//  1. which session_meta line is ours (a fork embeds the source's meta too,
//     and it may come first);
//  2. whether the file carries `event_msg` messages and reasoning at all —
//     modern rollouts carry BOTH the event_msg record and its
//     `response_item` mirror for the same content, and only files old enough
//     to have no event_msg twin may fall back to the mirror.
//
// It always starts at offset 0 (a tail refresh still needs the meta and the
// same dedup decision the first import made) but stops as soon as every
// question is answered, which on a modern rollout is within the first turn.
//
// `compacted` is NOT one of the questions, and that is the whole point of the
// short-circuit: a file that never compacted — the overwhelming majority —
// could never settle a "does a compacted record exist anywhere" question
// without reading to EOF, which turned every parse into two full passes and
// made a tail refresh cost a whole-file read. The compaction dedup is a
// running flag on the converter instead (see converter.sawCompacted); what
// this pass contributes is a seed for it.
//
// The seed is only ever needed for the HEAD REGION [0, FromOffset) — the
// lines the conversion pass will never see. So on a tail refresh the pass
// keeps reading past `settled()` until it has either seen a `compacted`
// record or reached FromOffset, and no further (see needsCompactionSeed).
// Without that, a refresh whose cursor lands between a `compacted` record
// and its `context_compacted` twin starts with the flag clear and writes a
// SECOND divider for a compaction the first import already recorded.
func preScan(ctx context.Context, file *os.File, opts ParseOptions) (preScanResult, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return preScanResult{}, fmt.Errorf("rollout: seek %s: %w", opts.Path, err)
	}
	var out preScanResult
	sc := newScanner(file, 0, opts.MaxLineBytes, scanBufferSize)
	for lines := 0; ; lines++ {
		if lines%512 == 0 {
			if err := ctx.Err(); err != nil {
				return preScanResult{}, err
			}
		}
		line, err := sc.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return preScanResult{}, fmt.Errorf("rollout: read %s: %w", opts.Path, err)
		}
		if line.Oversized {
			continue
		}
		env, ok := decodeEnvelope(line.Data)
		if !ok {
			continue
		}
		switch env.Type {
		case typeSessionMeta:
			if !out.metaFound {
				if meta, ok := decodeSessionMeta(env, opts.SessionID); ok {
					out.meta = meta
					out.metaFound = true
				}
			}
		case typeCompacted:
			out.sawCompacted = true
		case typeEventMsg:
			switch payloadType(env.Payload) {
			case "user_message", "agent_message":
				out.hasEventMsgMessage = true
			case "agent_reasoning":
				out.hasEventMsgReasoning = true
			}
		}
		if out.settled() && !needsCompactionSeed(opts, out, line.Next) {
			break
		}
	}
	return out, nil
}

// needsCompactionSeed reports whether the pass must keep reading purely to
// decide `sawCompacted` for the region the conversion pass will not reach.
//
// Bounded three ways: it only applies to a tail refresh, it stops at the
// first `compacted` record, and it never reads past FromOffset — from there
// on the converter reads the same lines and its own running flag takes over.
func needsCompactionSeed(opts ParseOptions, out preScanResult, next int64) bool {
	return opts.FromOffset > 0 && !out.sawCompacted && next < opts.FromOffset
}

// preScanResult carries the first pass's answers.
type preScanResult struct {
	meta                 SessionMeta
	metaFound            bool
	hasEventMsgMessage   bool
	hasEventMsgReasoning bool
	// sawCompacted is a SEED for converter.sawCompacted, not an answer: it
	// is true only when a `compacted` record appeared in the region this
	// pass read. False means "not yet seen", never "not in the file" — see
	// settled. What IS guaranteed is the part the converter cannot see for
	// itself: on a tail refresh the pass does not stop short of FromOffset
	// with this still clear (needsCompactionSeed).
	sawCompacted bool
}

// settled reports that every question this pass can answer HAS been answered,
// so the rest of the file need not be read. `sawCompacted` is deliberately
// absent: proving a negative there costs a full read of every file that never
// compacted. The head-region seed is a separate, bounded condition on top of
// this one, not a fourth question — see needsCompactionSeed.
func (p preScanResult) settled() bool {
	return p.metaFound && p.hasEventMsgMessage && p.hasEventMsgReasoning
}

// run drives the conversion pass.
func (c *converter) run(ctx context.Context, r io.Reader) error {
	sc := newScanner(r, c.opts.FromOffset, c.opts.MaxLineBytes, scanBufferSize)
	c.endOffset = c.opts.FromOffset
	for lines := 0; ; lines++ {
		if lines%512 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		line, err := sc.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("rollout: read %s: %w", c.opts.Path, err)
		}
		c.endOffset = line.Next
		c.lineStart = line.Start
		c.lineNext = line.Next
		if line.Oversized {
			c.corrupt++
			continue
		}
		env, ok := decodeEnvelope(line.Data)
		if !ok {
			c.corrupt++
			continue
		}
		if ts := parseTimestamp(env.Timestamp); !ts.IsZero() {
			c.lastTimestamp = ts
		}
		c.convert(env)
	}
	c.finish()
	return nil
}
