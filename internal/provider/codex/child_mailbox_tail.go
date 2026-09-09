package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

type childMailboxTail struct {
	path    string
	offset  int64
	partial []byte
	failed  bool
}

func (s *Session) registerChildMailboxTail(threadID, path string) {
	s.mu.Lock()
	enabled := s.rolloutTail.path != "" && s.collab.childParentByThread[threadID] != ""
	existing := s.rolloutTail.children[threadID]
	live := s.collab.childPathOwnerLive[s.collab.agentPathByThread[threadID]]
	s.mu.Unlock()
	if !enabled || existing != nil || path == "" || s.closing.Load() {
		return
	}
	resolved, offset, err := prepareRolloutSubagentNotificationObserver(path, threadID)
	if err != nil {
		s.warnCollabHistory("Codex child mailbox could not be observed", err)
		return
	}
	// A live spawn owns a newly created file. Read its initial NEW_TASK too.
	if live {
		offset = 0
	}
	s.mu.Lock()
	if s.rolloutTail.children == nil {
		s.rolloutTail.children = make(map[string]*childMailboxTail)
	}
	if s.rolloutTail.children[threadID] != nil {
		s.mu.Unlock()
		return
	}
	s.rolloutTail.children[threadID] = &childMailboxTail{path: resolved, offset: offset}
	if s.rolloutTail.activeChildren == nil {
		s.rolloutTail.activeChildren = make(map[string]*childMailboxTail)
	}
	s.rolloutTail.activeChildren[threadID] = s.rolloutTail.children[threadID]
	if s.rolloutTail.childWake == nil {
		s.rolloutTail.childWake = make(chan struct{}, 1)
	}
	s.rolloutTail.childRegistrations = append(s.rolloutTail.childRegistrations, threadID)
	select {
	case s.rolloutTail.childWake <- struct{}{}:
	default:
	}
	start := !s.rolloutTail.childWatcherStarted
	s.rolloutTail.childWatcherStarted = true
	s.mu.Unlock()
	if start {
		s.startCollabAsync(s.watchChildMailboxes)
	}
	s.armRolloutSubagentNotificationTail("reusable child attached")
}

// One worker reads active recipients, with bounded work and retained partial
// lines. Idle children retain only their cursor for a later follow-up.
func (s *Session) watchChildMailboxes() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.warnCollabHistory("Codex mailbox file watcher could not start", err)
		return
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			s.warnCollabHistory("Codex mailbox file watcher could not close", err)
		}
	}()
	watched := make(map[string]bool)
	byPath := make(map[string]string)
	s.mu.Lock()
	wake := s.rolloutTail.childWake
	s.mu.Unlock()
	ticker := time.NewTicker(rolloutSubagentNotificationPollInterval)
	defer ticker.Stop()
	retained := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.readDone:
			return
		case <-wake:
			s.mu.Lock()
			registrations := s.rolloutTail.childRegistrations
			s.rolloutTail.childRegistrations = nil
			paths := make(map[string]string, len(registrations))
			for _, id := range registrations {
				if tail := s.rolloutTail.children[id]; tail != nil {
					paths[id] = tail.path
				}
			}
			s.mu.Unlock()
			for id, path := range paths {
				dir := filepath.Dir(path)
				if !watched[dir] {
					if err := watcher.Add(dir); err != nil {
						s.warnCollabHistory("Codex mailbox directory could not be watched", err)
						continue
					}
					watched[dir] = true
				}
				byPath[path] = id
			}
			continue
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if id := byPath[event.Name]; id != "" {
				s.armChildMailboxRead(id)
			}
			continue
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			s.warnCollabHistory("Codex mailbox file observation failed", err)
			for _, id := range byPath {
				s.armChildMailboxRead(id)
			}
			continue
		case <-ticker.C:
		}
		s.mu.Lock()
		active := make(map[string]*childMailboxTail)
		for id, tail := range s.rolloutTail.activeChildren {
			active[id] = tail
		}

		s.mu.Unlock()
		for id, tail := range active {
			chunk, offset, err := readRolloutAppend(tail.path, tail.offset)
			if err != nil {
				retained -= len(tail.partial)
				tail.partial = nil
				s.mu.Lock()
				tail.failed = true
				delete(s.rolloutTail.activeChildren, id)
				s.mu.Unlock()
				s.warnCollabHistory("Codex child mailbox observation stopped", err)
				continue
			}
			tail.offset = offset
			if len(chunk) == 0 {
				s.mu.Lock()
				runtime := s.collab.childRuntimeByThread[id]
				if runtime.phase != childRuntimeRunning && runtime.phase != childRuntimePending && runtime.phase != childRuntimeStopping && len(tail.partial) == 0 {
					delete(s.rolloutTail.activeChildren, id)
				}
				s.mu.Unlock()
				continue
			}
			retained -= len(tail.partial)
			data := append(tail.partial, chunk...)
			tail.partial = nil
			for len(data) > 0 {
				line, rest, ok := bytes.Cut(data, []byte{'\n'})
				if !ok {
					if len(data)+retained > rolloutSubagentNotificationMaxLineBytes {
						s.mu.Lock()
						tail.failed = true
						delete(s.rolloutTail.activeChildren, id)
						s.mu.Unlock()
						s.warnCollabHistory("Codex child mailbox exceeded the partial-record memory limit", fmt.Errorf("child %s", id))
						break
					}
					tail.partial = append([]byte(nil), data...)
					retained += len(tail.partial)
					break
				}
				s.emitChildMailboxRolloutLine(id, line)
				data = rest
			}
		}
	}
}

func (s *Session) emitChildMailboxRolloutLine(threadID string, line []byte) {
	if !bytes.Contains(line, []byte(`"agent_message"`)) {
		return
	}
	var record struct {
		Timestamp time.Time                  `json:"timestamp"`
		Type      string                     `json:"type"`
		Payload   map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		s.warnCollabHistory("Codex child mailbox record could not be decoded", err)
		return
	}
	if record.Type != "response_item" {
		return
	}
	n, ok := extractSubagentCompletionFromRawAgentMessageItem(record.Payload)
	if !ok {
		return
	}
	n.Timestamp = record.Timestamp
	recipient := s.parentToolUseForAgentPath(n.Recipient)
	if recipient == "" || recipient != s.parentToolUseForProviderThread(threadID) {
		return
	}
	s.emitSubagentNotification(n, true)
}

func (s *Session) armChildMailboxRead(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tail := s.rolloutTail.children[threadID]; tail != nil && !tail.failed {
		if s.rolloutTail.activeChildren == nil {
			s.rolloutTail.activeChildren = make(map[string]*childMailboxTail)
		}
		s.rolloutTail.activeChildren[threadID] = tail
	}
}
