package domain

import "time"

// Role identifies who authored a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// SessionStatus tracks the lifecycle of a provider session.
type SessionStatus string

const (
	SessionStarting SessionStatus = "starting"
	SessionRunning  SessionStatus = "running"
	SessionReady    SessionStatus = "ready"
	SessionStopped  SessionStatus = "stopped"
	SessionError    SessionStatus = "error"
)

// Thread is the read model for a conversation thread.
type Thread struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	CreatedAt time.Time     `json:"createdAt"`
	Archived  bool          `json:"archived"`
	Session   SessionStatus `json:"session"`
	Messages  []Message     `json:"messages"`
}

// Message is a single message within a thread.
type Message struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"threadId"`
	Role      Role      `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}
