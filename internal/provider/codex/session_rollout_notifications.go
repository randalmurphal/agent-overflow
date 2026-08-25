package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	rolloutSubagentNotificationPollInterval = 150 * time.Millisecond
	rolloutSubagentNotificationReadChunk    = 64 * 1024
	rolloutSubagentNotificationMaxLineBytes = 16 * 1024 * 1024
	// rolloutSubagentNotificationPartialKeepBytes bounds the capacity the
	// reusable partial-line buffer is allowed to carry once a line COMPLETES.
	// The steady state is a buffer of at most one read chunk; a single
	// pathological line can grow it to rolloutSubagentNotificationMaxLineBytes,
	// and holding that for the rest of the session would pin 16 MiB on a thread
	// that saw one bad line. Above this the buffer is dropped and the next
	// partial re-allocates.
	rolloutSubagentNotificationPartialKeepBytes = 64 * 1024
)

// sessionRolloutTailState is the arming state of the rollout tail — the narrow
// reader that recovers detached-child mailbox deliveries a RESUMED session
// cannot see any other way.
//
// It exists because the opt-in that exposes those deliveries live
// (`experimentalRawEvents`) is a `thread/start` field held in the app-server's
// in-memory ThreadState: `thread/resume` has no such field, so a resumed thread
// never gets `rawResponseItem/completed` and the mailbox record that closes a
// spawn card is invisible. Tailing the rollout file is how AO gets it back.
//
// The tail is therefore NOT started for every resume: a thread that never
// spawned an agent cannot hit that gap, and polling its rollout file every
// 150ms for the life of the session buys nothing. Two things arm it, both
// evidence that this session CAN hit the gap — the app layer reporting
// unresolved spawn children on the thread at resume time
// (Config.ResumeHasUnresolvedSubagents), and a live spawn observed on the wire
// afterwards (registerChildOwnership). Once armed it runs until session end.
//
// Guarded by mu; zeroed by Close with the other session-scoped groups.
type sessionRolloutTailState struct {
	// path is the rollout file `thread/resume` named for this thread, recorded
	// unarmed. Empty on a fresh `thread/start` session — which keeps its raw
	// events and must never tail — so arming one is structurally a no-op.
	path string
	// started is the one-shot latch. The observer goroutine runs at most once
	// per session; a failed preparation latches too, because a rollout path
	// this reader refuses is not going to become acceptable on the next spawn.
	started bool
}

// prepareRolloutSubagentNotificationTail records the rollout file a resumed
// thread writes to WITHOUT starting anything. Called from the `thread/resume`
// response, which is the only place the path is stated, and before the collab
// rehydration below it — a spawn observed on the read loop can arm the tail at
// any moment after that, and it needs the path already recorded.
func (s *Session) prepareRolloutSubagentNotificationTail(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	s.mu.Lock()
	s.rolloutTail.path = path
	s.mu.Unlock()
}

// armRolloutSubagentNotificationTail starts the tail if a rollout path was
// recorded for this session and nothing has started it yet. Idempotent, and
// deliberately one-way: there is no stop-when-done condition, because the next
// spawn would only have to arm it again.
//
// reason names what proved this session can hit the raw-events gap; it is a log
// line only, so a future arming site costs one string.
func (s *Session) armRolloutSubagentNotificationTail(reason string) {
	if s.closing.Load() {
		return
	}
	s.mu.Lock()
	path := s.rolloutTail.path
	if path == "" || s.rolloutTail.started {
		s.mu.Unlock()
		return
	}
	s.rolloutTail.started = true
	s.mu.Unlock()

	resolved, offset, err := prepareRolloutSubagentNotificationObserver(path, s.rootThreadID())
	if err != nil {
		log.Printf("codex: rollout notification observer disabled: %v", err)
		return
	}
	s.rolloutObserverWG.Add(1)
	go func() {
		defer s.rolloutObserverWG.Done()
		s.watchRolloutSubagentNotifications(s.ctx, resolved, offset)
	}()
}

func (s *Session) watchRolloutSubagentNotifications(ctx context.Context, path string, offset int64) {
	ticker := time.NewTicker(rolloutSubagentNotificationPollInterval)
	defer ticker.Stop()

	var partial []byte
	var droppingOversizedLine bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.readDone:
			return
		case <-ticker.C:
		}

		chunk, nextOffset, err := readRolloutAppend(path, offset)
		if err != nil {
			log.Printf("codex: read rollout notifications %s: %v", path, err)
			return
		}
		offset = nextOffset
		if len(chunk) == 0 {
			continue
		}

		// The assembled view is built INTO the reusable buffer rather than into
		// a fresh allocation, and the unterminated tail is copied back into it
		// at the bottom of the loop. A fresh copy per tick cost ~28MB/h of
		// allocation churn on a tail that is mostly waiting.
		data := chunk
		if len(partial) > 0 {
			partial = append(partial, chunk...)
			data = partial
		}
		// carry is whatever the pass could not terminate; it aliases either
		// chunk or partial's own array, so it is only valid until the retain
		// below copies it.
		var carry []byte
		for len(data) > 0 {
			line, rest, found := bytes.Cut(data, []byte{'\n'})
			if !found {
				if len(data) <= rolloutSubagentNotificationMaxLineBytes {
					if !droppingOversizedLine {
						carry = data
					}
				} else {
					if !droppingOversizedLine {
						log.Printf("codex: dropping oversized rollout notification line from %s", path)
					}
					droppingOversizedLine = true
				}
				break
			}
			if droppingOversizedLine {
				droppingOversizedLine = false
				data = rest
				continue
			}
			if len(line) > rolloutSubagentNotificationMaxLineBytes {
				log.Printf("codex: dropping oversized rollout notification line from %s", path)
				data = rest
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			s.emitSubagentNotificationsFromRolloutLine(bytes.TrimSpace(line))
			data = rest
		}
		partial = retainRolloutPartialLine(partial, carry)
	}
}

// retainRolloutPartialLine copies an unterminated tail back into the reusable
// buffer, and sheds the buffer entirely once a long line has completed.
//
// carry may ALIAS buf's own array (it is a suffix of the assembled view), which
// append's copy handles as a memmove; and it can never be longer than what buf
// already holds in that case, so the aliasing path never reallocates. When it
// aliases the freshly-read chunk instead, buf is empty and the copy is the
// ordinary one.
func retainRolloutPartialLine(buf, carry []byte) []byte {
	if len(carry) == 0 {
		if cap(buf) > rolloutSubagentNotificationPartialKeepBytes {
			// One pathological line grew this buffer up to
			// rolloutSubagentNotificationMaxLineBytes. The line is behind us;
			// keeping its capacity would pin that memory for the rest of the
			// session on the strength of a single bad record.
			return nil
		}
		return buf[:0]
	}
	return append(buf[:0], carry...)
}

func prepareRolloutSubagentNotificationObserver(path string, threadID string) (string, int64, error) {
	path, err := normalizeRolloutNotificationPath(path, threadID)
	if err != nil {
		return "", 0, err
	}
	offset, err := rolloutNotificationFileSize(path)
	if err != nil {
		return "", 0, err
	}
	return path, offset, nil
}

func normalizeRolloutNotificationPath(path string, threadID string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("missing rollout path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve rollout path %q: %w", path, err)
	}
	abs = filepath.Clean(abs)
	base := filepath.Base(abs)
	if filepath.Ext(base) != ".jsonl" || !strings.HasPrefix(base, "rollout-") {
		return "", fmt.Errorf("unexpected rollout path %q", abs)
	}
	if threadID = strings.TrimSpace(threadID); threadID != "" && !strings.Contains(base, threadID) {
		return "", fmt.Errorf("rollout path %q does not match thread %q", abs, threadID)
	}
	return abs, nil
}

func rolloutNotificationFileSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("rollout path %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("rollout path %s is not a regular file", path)
	}
	return info.Size(), nil
}

func readRolloutAppend(path string, offset int64) ([]byte, int64, error) {
	size, err := rolloutNotificationFileSize(path)
	if err != nil {
		return nil, offset, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer file.Close()

	if size < offset {
		offset = 0
	}
	if size == offset {
		return nil, offset, nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	readSize := min(size-offset, rolloutSubagentNotificationReadChunk)
	chunk := make([]byte, int(readSize))
	n, err := file.Read(chunk)
	if err != nil && err != io.EOF {
		return nil, offset, err
	}
	return chunk[:n], offset + int64(n), nil
}

func (s *Session) emitSubagentNotificationsFromRolloutLine(line []byte) bool {
	if len(line) == 0 || (!bytes.Contains(line, []byte("subagent_notification")) &&
		!bytes.Contains(line, []byte(interAgentMessageTypePrefix))) {
		return false
	}

	var record struct {
		Type    string                     `json:"type"`
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return false
	}
	if record.Payload == nil {
		return false
	}
	switch record.Type {
	case "response_item":
		return s.emitResolvedSubagentNotificationsFromRawMessageItem(record.Payload)
	case "inter_agent_communication":
		notification, ok := extractSubagentCompletionFromInterAgentCommunication(record.Payload)
		if !ok {
			return false
		}
		return s.emitResolvedSubagentNotifications([]subagentNotification{notification}, "", true)
	default:
		return false
	}
}
