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
)

func (s *Session) startRolloutSubagentNotificationObserver(path string) {
	path, offset, err := prepareRolloutSubagentNotificationObserver(path, s.rootThreadID())
	if err != nil {
		log.Printf("codex: rollout notification observer disabled: %v", err)
		return
	}
	s.rolloutObserverWG.Add(1)
	go func() {
		defer s.rolloutObserverWG.Done()
		s.watchRolloutSubagentNotifications(s.ctx, path, offset)
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

		data := chunk
		if len(partial) > 0 {
			data = append(partial, chunk...)
		}
		partial = partial[:0]
		for len(data) > 0 {
			line, rest, found := bytes.Cut(data, []byte{'\n'})
			if !found {
				if len(data) <= rolloutSubagentNotificationMaxLineBytes {
					if !droppingOversizedLine {
						partial = append([]byte(nil), data...)
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
	}
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
