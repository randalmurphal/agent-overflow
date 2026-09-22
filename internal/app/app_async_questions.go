package app

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// ListAsyncQuestions reads outstanding questions or one historical question group.
//
//ao:scope threads:read
func (a *App) ListAsyncQuestions(threadID, itemID string) ([]store.AsyncQuestion, error) {
	return a.store.ListAsyncQuestions(threadID, itemID)
}

// SubmitAsyncAnswers sends the explicitly answered subset without consuming the composer draft.
//
//ao:scope threads:operate
func (a *App) SubmitAsyncAnswers(ctx context.Context, threadID, sendID string, answers []store.AsyncQuestionAnswer) error {
	if err := a.requireAutonomyForThread(ctx, threadID, ""); err != nil {
		return err
	}
	unlock, err := a.lockSendAdmission(ctx, threadID, sendID)
	if err != nil {
		return err
	}
	defer unlock()
	err = a.queueAgentNotice(ctx, agentNotice{
		threadID: threadID, sendID: sendID,
		prepare: func() (string, bool, error) {
			thread, err := a.store.GetThread(threadID)
			if err != nil {
				return "", false, err
			}
			if thread.Provider != "codex" {
				return "", false, fmt.Errorf("async answers require a Codex thread")
			}
			if err := a.store.CheckThreadExecutionAccess(thread); err != nil {
				return "", false, err
			}
			body, already, err := a.store.AsyncAnswerMessage(threadID, sendID, answers)
			if err != nil || already {
				return body, false, err
			}
			if _, found, err := a.findRecordedSend(threadID, sendID); err != nil {
				return "", false, err
			} else if found {
				return "", false, fmt.Errorf("submission identity already belongs to a different message")
			}
			return body, true, nil
		},
		persist: func(item store.FlushQueueItem) error { return a.store.QueueAsyncAnswers(item, answers) },
		startFailed: func(err error) string {
			return "Your answers are queued, but the agent could not start: " + err.Error()
		},
	})
	if errors.Is(err, store.ErrAsyncQuestionHandled) {
		return fmt.Errorf("%w: %v", transport.ErrAlreadyHandled, err)
	}
	if err != nil {
		return err
	}
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	return nil
}

// SetAsyncQuestionDismissed changes only the selected unanswered question.
//
//ao:scope threads:operate
func (a *App) SetAsyncQuestionDismissed(ctx context.Context, threadID, itemID string, index int, dismissed bool) error {
	unlock, err := a.threadApplication().LockMutable(ctx, threadID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.store.SetAsyncQuestionDismissed(threadID, itemID, index, dismissed); err != nil {
		if errors.Is(err, store.ErrAsyncQuestionHandled) {
			return fmt.Errorf("%w: %v", transport.ErrAlreadyHandled, err)
		}
		return err
	}
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	return nil
}
