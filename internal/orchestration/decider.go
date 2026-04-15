package orchestration

import (
	"fmt"
	"time"

	"agent-overflow/internal/domain"

	"github.com/google/uuid"
)

// decide validates a command against the current read model and produces events.
// This is the pure decision function — no side effects.
func (e *Engine) decide(cmd domain.Command) ([]domain.Event, error) {
	switch cmd.Kind {
	case domain.CmdCreateThread:
		return e.decideCreateThread(cmd)
	case domain.CmdDeleteThread:
		return e.decideDeleteThread(cmd)
	case domain.CmdSendMessage:
		return e.decideSendMessage(cmd)
	case domain.CmdStartTurn:
		return e.decideStartTurn(cmd)
	case domain.CmdCompleteTurn:
		return e.decideCompleteTurn(cmd)
	default:
		return nil, fmt.Errorf("unknown command kind: %s", cmd.Kind)
	}
}

func (e *Engine) decideCreateThread(cmd domain.Command) ([]domain.Event, error) {
	threadID := cmd.ThreadID
	if threadID == "" {
		threadID = uuid.NewString()
	}
	if _, exists := e.threads[threadID]; exists {
		return nil, fmt.Errorf("thread %s already exists", threadID)
	}

	title := "New Thread"
	if p, ok := cmd.Payload.(map[string]any); ok {
		if t, ok := p["title"].(string); ok && t != "" {
			title = t
		}
	}

	return []domain.Event{{
		Kind:     domain.ThreadCreated,
		ThreadID: threadID,
		Payload: map[string]any{
			"title": title,
		},
	}}, nil
}

func (e *Engine) decideDeleteThread(cmd domain.Command) ([]domain.Event, error) {
	if _, exists := e.threads[cmd.ThreadID]; !exists {
		return nil, fmt.Errorf("thread %s not found", cmd.ThreadID)
	}
	return []domain.Event{{
		Kind:     domain.ThreadDeleted,
		ThreadID: cmd.ThreadID,
	}}, nil
}

func (e *Engine) decideSendMessage(cmd domain.Command) ([]domain.Event, error) {
	if _, exists := e.threads[cmd.ThreadID]; !exists {
		return nil, fmt.Errorf("thread %s not found", cmd.ThreadID)
	}
	p, ok := cmd.Payload.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid payload for send message")
	}
	content, _ := p["content"].(string)
	role, _ := p["role"].(string)
	if content == "" {
		return nil, fmt.Errorf("message content is empty")
	}
	if role == "" {
		role = string(domain.RoleUser)
	}

	return []domain.Event{{
		Kind:     domain.MessageSent,
		ThreadID: cmd.ThreadID,
		Payload: map[string]any{
			"messageId": uuid.NewString(),
			"role":      role,
			"content":   content,
			"timestamp": time.Now().UnixMilli(),
		},
	}}, nil
}

func (e *Engine) decideStartTurn(cmd domain.Command) ([]domain.Event, error) {
	thread, exists := e.threads[cmd.ThreadID]
	if !exists {
		return nil, fmt.Errorf("thread %s not found", cmd.ThreadID)
	}
	if thread.Session == domain.SessionRunning {
		return nil, fmt.Errorf("thread %s already has a turn in progress", cmd.ThreadID)
	}
	return []domain.Event{{
		Kind:     domain.TurnStartRequested,
		ThreadID: cmd.ThreadID,
	}}, nil
}

func (e *Engine) decideCompleteTurn(cmd domain.Command) ([]domain.Event, error) {
	if _, exists := e.threads[cmd.ThreadID]; !exists {
		return nil, fmt.Errorf("thread %s not found", cmd.ThreadID)
	}
	return []domain.Event{{
		Kind:     domain.TurnCompleted,
		ThreadID: cmd.ThreadID,
	}}, nil
}
