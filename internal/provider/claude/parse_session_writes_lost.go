package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// maxSessionWritesLostCodeRunes bounds the wire error code before it is
// named in the error text.
const maxSessionWritesLostCodeRunes = 64

// recordSessionWritesLost handles `system/session_writes_lost` (2.1.317+;
// schema read from the 2.1.321 bundle): the CLI could not save lines of a
// session transcript, its ~/.claude.json, or both, because the disk was
// full (`ENOSPC`) or over quota (`EDQUOT`). The CLI sends it at most once,
// as it exits with status 0, after the last result.
//
// It emits no event. Its usual cause of exit is AO closing the session,
// after which triage drops the thread's wire events, so the text waits on
// the Session until the read loop ends (lostWrites) and the app reports it
// once from its close and self-exit paths. A repeat keeps the first text.
func (p *Parser) recordSessionWritesLost(raw map[string]json.RawMessage) {
	if p == nil || p.sessionWritesLost != "" {
		return
	}
	var lost []string
	if err := json.Unmarshal(raw["lost"], &lost); err != nil {
		lost = nil
	}
	errorCode := boundedClaudeFallbackField(readRawString(raw["error_code"]), maxSessionWritesLostCodeRunes)
	p.sessionWritesLost = sessionWritesLostText(errorCode, lost)
}

// takeSessionWritesLost returns and clears the recorded text. Read-loop
// goroutine only, like the rest of the parser.
func (p *Parser) takeSessionWritesLost() string {
	if p == nil {
		return ""
	}
	text := p.sessionWritesLost
	p.sessionWritesLost = ""
	return text
}

// sessionWritesLostText names what was lost and why. Both wire enums are
// open: an unknown `lost` value reads as "other session data", and an
// unknown code is named.
func sessionWritesLostText(errorCode string, lost []string) string {
	var transcript, config, other bool
	for _, value := range lost {
		switch strings.TrimSpace(value) {
		case "transcript":
			transcript = true
		case "global_config":
			config = true
		default:
			other = true
		}
	}
	var parts []string
	if transcript {
		parts = append(parts, "the conversation transcript")
	}
	if config {
		parts = append(parts, "its settings file (~/.claude.json)")
	}
	if other || len(parts) == 0 {
		if len(parts) == 0 {
			parts = append(parts, "some of its session data")
		} else {
			parts = append(parts, "other session data")
		}
	}
	subject := strings.Join(parts, " and ")
	if len(parts) == 3 {
		subject = parts[0] + ", " + parts[1] + " and " + parts[2]
	}
	var cause string
	switch errorCode {
	case "ENOSPC":
		cause = "the disk is full"
	case "EDQUOT":
		cause = "the disk quota is used up"
	case "":
		cause = "the disk is full or over quota"
	default:
		cause = fmt.Sprintf("a disk error (%s)", errorCode)
	}
	text := fmt.Sprintf("Claude could not save %s because %s.", subject, cause)
	if transcript {
		text += " Resuming this conversation may miss part of it."
	}
	return text
}

// lostWrites carries the session_writes_lost text from the read loop to
// the app. The read loop stores it before it emits the session's final
// "disconnected" status and before Close returns.
type lostWrites struct {
	mu   sync.Mutex
	text string
}

func (l *lostWrites) store(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.text == "" {
		l.text = text
	}
}

func (l *lostWrites) take() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	text := l.text
	l.text = ""
	return text
}

// TakeLostWritesError returns, once, the error text for a
// `system/session_writes_lost` this process sent, or "" when it sent none.
// The answer is complete once Close has returned or the session's
// "disconnected" status has been delivered; both happen after the read
// loop reached EOF.
func (s *Session) TakeLostWritesError() string {
	return s.lostWrites.take()
}
