package rollout

import (
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
	ThreadSource   string
	ModelProvider  string
	GitBranch      string
	GitCommit      string
	CreatedAt      time.Time
}

// IsSubagent reports whether this file is a spawned child's rollout. The
// thread index excludes those from List; this is the file-level check for
// callers that reached a path some other way.
func (m SessionMeta) IsSubagent() bool { return m.ThreadSource == "subagent" }

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
// This is what the import orchestrator uses to exclude fork ancestors: a
// candidate whose `forked_from_id` names another candidate means the two are
// the same conversation, and importing both would duplicate it. Answering
// that for every candidate must not cost a full parse of every rollout, so
// this stops at the first matching meta line and never reads past the head
// bound.
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
		ThreadSource:   strings.TrimSpace(payload.ThreadSource),
		ModelProvider:  payload.ModelProvider,
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
