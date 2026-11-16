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
	unlock, err := a.lockClientSendAdmission(ctx, threadID, sendID)
	if err != nil {
		return err
	}
	defer unlock()
	return a.queueAsyncAnswers(threadID, sendID, answers, agentNotice{
		startFailed: func(err error) string {
			return "Your answers are queued, but the agent could not start: " + err.Error()
		},
		// The person answering may have opened an archived thread to do it.
		fromPerson: true,
	})
}

// queueAsyncAnswers claims answers to threadID's async questions and queues
// them as one message, under sendID, whose admission lock its caller holds:
// the person's (SubmitAsyncAnswers) or the spawner's agent's, written for a
// worker (answerAsyncWorkerPrompt). notice brings who wrote it and what the
// thread is told when its session will not start. A question already
// answered or dismissed is transport.ErrAlreadyHandled.
func (a *App) queueAsyncAnswers(threadID, sendID string, answers []store.AsyncQuestionAnswer, notice agentNotice) error {
	notice.threadID, notice.sendID = threadID, sendID
	notice.prepare = func() (string, bool, error) {
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
	}
	notice.persist = func(item store.FlushQueueItem) error { return a.store.QueueAsyncAnswers(item, answers) }
	err := a.queueAgentNotice(notice)
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
