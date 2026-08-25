package rollout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrSessionMetaNotFound means the file carried no `session_meta` line whose
// `id` matched the session id we were told to read. That is a real
// disagreement between the thread index and the file on disk, not a
// formatting quirk, so callers surface it rather than guessing.
var ErrSessionMetaNotFound = errors.New("rollout: no session_meta matching the session id")

// SessionMeta is the accepted `session_meta` line: the file's own identity
// and provenance.
type SessionMeta struct {
	SessionID      string
	ForkedFromID   string
	ParentThreadID string
	Cwd            string
	Originator     string
	CLIVersion     string
	SubagentKind   string
	ModelProvider  string
	GitBranch      string
	GitCommit      string
	CreatedAt      time.Time

	// HistoryMode is the raw `history_mode` value (`legacy` | `paginated`
	// as of 0.149). An ABSENT field means legacy — upstream's enum defaults
	// to Legacy and the field only exists since 0.147 — but an unrecognised
	// value is carried through verbatim rather than coerced, because a mode
	// this build has never seen is not safely assumed to be either one.
	HistoryMode string

	// HistoryBase is set when this rollout is a CONTINUATION: its history
	// begins inside another rollout file, and the records before
	// EndOrdinalExclusive / EndByteOffset live there. AO does not follow the
	// chain (see ErrHistoryBase / WarnHistoryBase and the TODO on
	// HistoryBase itself).
	HistoryBase *HistoryBase
}

// HistoryBase is `session_meta.history_base` — upstream's `HistoryPosition`
// (codex-rs/protocol/src/protocol.rs at rust-v0.149.0).
//
// TODO(codex-history-chain): follow the chain. The field shape is exactly
// what a follower needs: ThreadID names the PREFIX ROLLOUT (upstream's own
// field name says "thread", but a reverted thread's prefix file carries a
// different id than the thread's), EndOrdinalExclusive is the first ordinal
// NOT inherited, and EndByteOffset is the byte offset in the prefix file one
// past the last inherited record. Following it means: resolve the prefix
// rollout path from the same Codex home (the thread index maps id → path),
// Parse it with a stop-at-EndByteOffset bound, then concatenate its events
// ahead of this file's. Three things must be solved before that ships — the
// prefix may itself carry a history_base (chains nest, so a cycle/depth
// guard is required), the resume cursor becomes two coordinates rather than
// one, and PathInHome must still gate the prefix path. Until then the import
// warns (WarnHistoryBase) instead of silently truncating.
type HistoryBase struct {
	// ThreadID is upstream's own field name for what is really the prefix
	// ROLLOUT id.
	ThreadID string
	// EndOrdinalExclusive is the first ordinal this file does NOT inherit.
	EndOrdinalExclusive uint64
	// EndByteOffset is the offset in the prefix file one past the last
	// inherited record.
	EndByteOffset uint64
}

// headScanLines and headScanBytes bound ReadSessionMeta. Both meta lines of a
// forked file are the first two records, so a handful of lines is generous;
// the byte bound is the backstop for a file whose first line is enormous.
const (
	headScanLines = 32
	headScanBytes = 4 << 20
)

// ReadSessionMeta reads ONLY the accepted session_meta from the head of a
// rollout, without parsing the file.
//
// This is what the import orchestrator uses to recover explicit-fork lineage
// and the session origin marker without fully parsing every candidate.
// A `forked_from_id` does not make either session disposable: both ids are
// independently resumable even though their files share history. This stops
// at the first matching meta line and never reads past the head bound.
//
// sessionID is the uuid from the file name — the meta authority. A file
// carrying a meta line for some OTHER session (the source's meta, embedded by
// a fork) is not a match and is skipped.
func ReadSessionMeta(path, sessionID string) (SessionMeta, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = SessionIDFromPath(path)
	}
	if sessionID == "" {
		return SessionMeta{}, fmt.Errorf("rollout: %s: cannot determine session id", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, fmt.Errorf("rollout: open %s: %w", path, err)
	}
	defer file.Close()

	sc := newScanner(io.LimitReader(file, headScanBytes), 0, DefaultMaxLineBytes, headScanBufferSize)
	for i := 0; i < headScanLines; i++ {
		line, err := sc.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SessionMeta{}, fmt.Errorf("rollout: read %s: %w", path, err)
		}
		if line.Oversized {
			continue
		}
		env, ok := decodeEnvelope(line.Data)
		if !ok || env.Type != typeSessionMeta {
			continue
		}
		meta, ok := decodeSessionMeta(env, sessionID)
		if ok {
			return meta, nil
		}
	}
	return SessionMeta{}, fmt.Errorf("rollout: %s: %w", path, ErrSessionMetaNotFound)
}

// SessionIDFromPath extracts the session uuid a rollout file name ends with
// (`rollout-<timestamp>-<uuid>.jsonl`). It returns "" when the name does not
// have that shape rather than guessing, because the id it produces is the
// authority for which session_meta line is accepted.
func SessionIDFromPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".jsonl")
	if !strings.HasPrefix(base, "rollout-") {
		return ""
	}
	// A uuid is 36 characters with four '-' separators; the timestamp
	// prefix also uses '-', so anchor on the length of the tail.
	const uuidLen = 36
	if len(base) < uuidLen+1 {
		return ""
	}
	candidate := base[len(base)-uuidLen:]
	if base[len(base)-uuidLen-1] != '-' || !looksLikeUUID(candidate) {
		return ""
	}
	return candidate
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// decodeSessionMeta accepts the line only when its own `id` equals sessionID.
func decodeSessionMeta(env envelope, sessionID string) (SessionMeta, bool) {
	var payload sessionMetaPayload
	if json.Unmarshal(env.Payload, &payload) != nil {
		return SessionMeta{}, false
	}
	id := strings.TrimSpace(payload.ID)
	if id == "" {
		// Very old files predate the `id` field and carry only
		// `session_id`; there is exactly one meta line in those, so the
		// alias is safe when `id` is absent entirely.
		id = strings.TrimSpace(payload.SessionID)
	}
	if id == "" || !strings.EqualFold(id, sessionID) {
		return SessionMeta{}, false
	}
	meta := SessionMeta{
		SessionID:      id,
		ForkedFromID:   strings.TrimSpace(payload.ForkedFromID),
		ParentThreadID: strings.TrimSpace(payload.ParentThreadID),
		Cwd:            payload.Cwd,
		Originator:     payload.Originator,
		CLIVersion:     payload.CLIVersion,
		ModelProvider:  payload.ModelProvider,
		HistoryMode:    strings.TrimSpace(payload.HistoryMode),
	}
	var source struct {
		Subagent string `json:"subagent"`
	}
	if json.Unmarshal(payload.Source, &source) == nil {
		meta.SubagentKind = strings.TrimSpace(source.Subagent)
	}
	if payload.HistoryBase != nil {
		meta.HistoryBase = &HistoryBase{
			ThreadID:            strings.TrimSpace(payload.HistoryBase.ThreadID),
			EndOrdinalExclusive: payload.HistoryBase.EndOrdinalExclusive,
			EndByteOffset:       payload.HistoryBase.EndByteOffset,
		}
	}
	if payload.Git != nil {
		meta.GitBranch = payload.Git.Branch
		meta.GitCommit = payload.Git.CommitHash
	}
	if ts := parseTimestamp(payload.Timestamp); !ts.IsZero() {
		meta.CreatedAt = ts
	} else {
		meta.CreatedAt = parseTimestamp(env.Timestamp)
	}
	return meta, true
}

// SourceIdentity is the cheap fingerprint a refresh compares against the one
// recorded at import time to decide whether the file on disk is still the
// same history this thread was built from.
//
// It is deliberately NOT a whole-file digest: a rollout is appended to
// constantly and hashing it on every refresh would cost a full read of every
// candidate. The first record is a rollout's header — it names the thread id,
// the cwd, the git commit, the originator, the creation time and (since
// 0.147) the history mode. Codex rewrites that line when it MIGRATES a thread
// between history modes, which is exactly the case a size comparison cannot
// see: a migrated file can be the same size as, or larger than, the one we
// imported while its byte offsets mean something completely different.
type SourceIdentity struct {
	// MetaHash is sha256 of the file's first line (its raw bytes, without
	// the terminating newline), hex-encoded. Empty when the file has no
	// complete first line yet.
	MetaHash string
	// HistoryMode is the accepted `session_meta`'s raw `history_mode`, with
	// the same absent-means-legacy caveat as SessionMeta.HistoryMode. Empty
	// when the field is absent OR no matching meta line was found — callers
	// compare it for CHANGE, so "" vs "" is agreement.
	HistoryMode string
}

// ReadSourceIdentityAt reads the rollout fingerprint over a handle the caller
// already holds. A file with no matching `session_meta` is not an error: the
// hash alone remains usable, and the parse reports a missing header itself.
//
// It exists so a caller can prove the identity and read the tail from ONE open
// file. Doing each through its own os.Open leaves a window in which Codex
// publishes a migrated rollout over the same path between the two calls: the
// identity check passes against the file that WAS there and the tail is then
// spliced out of the replacement, at byte offsets that address different
// records. A single handle survives the rename — the fd keeps naming the inode
// it opened — so the two answers are guaranteed to describe the same file.
//
// Reads are positional (io.ReaderAt), so the caller's own file offset is
// untouched.
func ReadSourceIdentityAt(file io.ReaderAt, path, sessionID string) (SourceIdentity, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = SessionIDFromPath(path)
	}
	var out SourceIdentity
	sc := newScanner(io.NewSectionReader(file, 0, headScanBytes), 0, DefaultMaxLineBytes, headScanBufferSize)
	for i := 0; i < headScanLines; i++ {
		line, err := sc.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SourceIdentity{}, fmt.Errorf("rollout: read %s: %w", path, err)
		}
		if i == 0 && !line.Oversized {
			sum := sha256.Sum256(line.Data)
			out.MetaHash = hex.EncodeToString(sum[:])
		}
		if line.Oversized || sessionID == "" {
			continue
		}
		env, ok := decodeEnvelope(line.Data)
		if !ok || env.Type != typeSessionMeta {
			continue
		}
		if meta, ok := decodeSessionMeta(env, sessionID); ok {
			out.HistoryMode = meta.HistoryMode
			break
		}
	}
	return out, nil
}
