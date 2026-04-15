package orchestration

import (
	"time"

	"agent-overflow/internal/domain"
)

// apply updates the in-memory read model based on an event.
// This is the pure projection function — it assumes the event is already validated.
func (e *Engine) apply(evt domain.Event) {
	switch evt.Kind {
	case domain.ThreadCreated:
		e.applyThreadCreated(evt)
	case domain.ThreadDeleted:
		e.applyThreadDeleted(evt)
	case domain.ThreadRenamed:
		e.applyThreadRenamed(evt)
	case domain.ThreadArchived:
		e.applyThreadArchived(evt)
	case domain.MessageSent:
		e.applyMessageSent(evt)
	case domain.TurnStartRequested:
		e.applyTurnStartRequested(evt)
	case domain.TurnCompleted:
		e.applyTurnCompleted(evt)
	case domain.SessionSet:
		e.applySessionSet(evt)
	case domain.SessionStopRequested:
		e.applySessionStopped(evt)
	}
}

func (e *Engine) applyThreadCreated(evt domain.Event) {
	title := "New Thread"
	if p, ok := evt.Payload.(map[string]any); ok {
		if t, ok := p["title"].(string); ok {
			title = t
		}
	}
	e.threads[evt.ThreadID] = &domain.Thread{
		ID:        evt.ThreadID,
		Title:     title,
		CreatedAt: evt.OccurredAt,
		Session:   domain.SessionStopped,
	}
}

func (e *Engine) applyThreadDeleted(evt domain.Event) {
	delete(e.threads, evt.ThreadID)
}

func (e *Engine) applyThreadRenamed(evt domain.Event) {
	t, ok := e.threads[evt.ThreadID]
	if !ok {
		return
	}
	if p, ok := evt.Payload.(map[string]any); ok {
		if title, ok := p["title"].(string); ok {
			t.Title = title
		}
	}
}

func (e *Engine) applyThreadArchived(evt domain.Event) {
	if t, ok := e.threads[evt.ThreadID]; ok {
		t.Archived = true
	}
}

func (e *Engine) applyMessageSent(evt domain.Event) {
	t, ok := e.threads[evt.ThreadID]
	if !ok {
		return
	}
	p, ok := evt.Payload.(map[string]any)
	if !ok {
		return
	}
	msgID, _ := p["messageId"].(string)
	role, _ := p["role"].(string)
	content, _ := p["content"].(string)
	tsMillis, _ := p["timestamp"].(float64)

	t.Messages = append(t.Messages, domain.Message{
		ID:        msgID,
		ThreadID:  evt.ThreadID,
		Role:      domain.Role(role),
		Content:   content,
		Timestamp: time.UnixMilli(int64(tsMillis)),
	})
}

func (e *Engine) applyTurnStartRequested(evt domain.Event) {
	if t, ok := e.threads[evt.ThreadID]; ok {
		t.Session = domain.SessionRunning
	}
}

func (e *Engine) applyTurnCompleted(evt domain.Event) {
	if t, ok := e.threads[evt.ThreadID]; ok {
		t.Session = domain.SessionReady
	}
}

func (e *Engine) applySessionSet(evt domain.Event) {
	t, ok := e.threads[evt.ThreadID]
	if !ok {
		return
	}
	if p, ok := evt.Payload.(map[string]any); ok {
		if status, ok := p["status"].(string); ok {
			t.Session = domain.SessionStatus(status)
		}
	}
}

func (e *Engine) applySessionStopped(evt domain.Event) {
	if t, ok := e.threads[evt.ThreadID]; ok {
		t.Session = domain.SessionStopped
	}
}
