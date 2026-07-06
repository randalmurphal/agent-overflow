package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

const defaultPRUpdateInterval = 45 * time.Second

type PRUpdateSubscriptionResult struct {
	ID       string                `json:"id"`
	ThreadID string                `json:"threadId"`
	PR       gitops.PRReference    `json:"pr"`
	Detail   gitops.PRDetail       `json:"detail"`
	Threads  []gitops.ReviewThread `json:"threads"`
	HeadSHA  string                `json:"headSHA"`
}

type PRUpdatedEvent struct {
	SubscriptionID string                `json:"subscriptionId"`
	ThreadID       string                `json:"threadId"`
	PR             gitops.PRReference    `json:"pr"`
	Detail         gitops.PRDetail       `json:"detail"`
	Threads        []gitops.ReviewThread `json:"threads"`
	HeadSHA        string                `json:"headSHA"`
}

type prUpdateSnapshot struct {
	Detail  gitops.PRDetail       `json:"detail"`
	Threads []gitops.ReviewThread `json:"threads"`
}

type prUpdatePump struct {
	threadID string
	pr       gitops.PRReference
	done     chan struct{}
	last     []byte
}

func (a *App) GetPRDetail(pr gitops.PRReference) (gitops.PRDetail, error) {
	if a.shuttingDown.Load() {
		return gitops.PRDetail{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return gitops.PRDetail{}, err
	}
	return a.gitCore().GetPRDetail("", pr)
}

func (a *App) GetPRDiff(pr gitops.PRReference) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return "", err
	}
	return a.gitCore().GetPRDiff("", pr)
}

func (a *App) ListPRReviewThreads(pr gitops.PRReference) ([]gitops.ReviewThread, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return nil, err
	}
	return a.gitCore().ListReviewThreads("", pr)
}

type SubmitPRReviewResult struct {
	PostedReview       bool   `json:"postedReview"`
	PostedFileComments int    `json:"postedFileComments"`
	PartialFailurePath string `json:"partialFailurePath,omitempty"`
	PartialFailure     string `json:"partialFailure,omitempty"`
}

func mapSubmitPRReviewResult(result gitops.SubmitReviewResult, err error) (SubmitPRReviewResult, error) {
	if err == nil {
		return SubmitPRReviewResult{
			PostedReview:       result.PostedReview,
			PostedFileComments: result.PostedFileComments,
		}, nil
	}
	var partial *gitops.PartialSubmitError
	if errors.As(err, &partial) {
		return SubmitPRReviewResult{
			PostedReview:       partial.PostedReview,
			PostedFileComments: partial.PostedFileComments,
			PartialFailurePath: partial.FailedPath,
			PartialFailure:     partial.Err.Error(),
		}, nil
	}
	return SubmitPRReviewResult{}, err
}

func (a *App) SubmitPRReview(pr gitops.PRReference, review gitops.SubmitReviewRequest) (SubmitPRReviewResult, error) {
	if a.shuttingDown.Load() {
		return SubmitPRReviewResult{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return SubmitPRReviewResult{}, err
	}
	result, err := a.gitCore().SubmitReview("", pr, review)
	return mapSubmitPRReviewResult(result, err)
}

func (a *App) ReplyToPRThread(pr gitops.PRReference, threadID string, databaseID int64, body string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return err
	}
	return a.gitCore().ReplyToThread("", pr, threadID, databaseID, body)
}

func (a *App) SubscribePRUpdates(ctx context.Context, threadID string, pr gitops.PRReference) (PRUpdateSubscriptionResult, error) {
	if a.shuttingDown.Load() {
		return PRUpdateSubscriptionResult{}, ErrShuttingDown
	}
	if threadID == "" {
		return PRUpdateSubscriptionResult{}, errors.New("threadID is required")
	}
	if err := validatePRReference(pr); err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	snapshot, err := a.fetchPRUpdateSnapshot(pr)
	if err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	encoded, err := encodePRUpdateSnapshot(snapshot)
	if err != nil {
		return PRUpdateSubscriptionResult{}, err
	}
	id := uuid.NewString()
	entry := &prUpdatePump{
		threadID: threadID,
		pr:       pr,
		done:     make(chan struct{}),
		last:     encoded,
	}
	a.prUpdatePumpsMu.Lock()
	if a.prUpdatePumps == nil {
		a.prUpdatePumps = make(map[string]*prUpdatePump)
	}
	a.prUpdatePumps[id] = entry
	a.prUpdatePumpsMu.Unlock()
	a.prUpdatePumpWG.Go(func() { a.pumpPRUpdates(id, entry) })

	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.RegisterCleanup(func() { a.unsubscribePRUpdates(id) }) {
			a.unsubscribePRUpdates(id)
			return PRUpdateSubscriptionResult{}, fmt.Errorf("pr updates: connection closing")
		}
	}

	return PRUpdateSubscriptionResult{
		ID:       id,
		ThreadID: threadID,
		PR:       pr,
		Detail:   snapshot.Detail,
		Threads:  snapshot.Threads,
		HeadSHA:  snapshot.Detail.HeadSHA,
	}, nil
}

func (a *App) UnsubscribePRUpdates(subscriptionID string) error {
	a.unsubscribePRUpdates(subscriptionID)
	return nil
}

func (a *App) pumpPRUpdates(id string, entry *prUpdatePump) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pr updates: pump panic for id=%s: %v", id, r)
		}
		a.prUpdatePumpsMu.Lock()
		delete(a.prUpdatePumps, id)
		a.prUpdatePumpsMu.Unlock()
	}()
	interval := a.prUpdatePollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-entry.done:
			return
		case <-a.lifeCtx().Done():
			return
		case <-ticker.C:
			snapshot, err := a.fetchPRUpdateSnapshot(entry.pr)
			if err != nil {
				log.Printf("pr updates: fetch failed for id=%s forge=%s project=%s number=%d: %v", id, entry.pr.Forge, entry.pr.Project(), entry.pr.Number, err)
				continue
			}
			encoded, err := encodePRUpdateSnapshot(snapshot)
			if err != nil {
				log.Printf("pr updates: encode failed for id=%s: %v", id, err)
				continue
			}
			if string(encoded) == string(entry.last) {
				continue
			}
			entry.last = encoded
			select {
			case <-entry.done:
				return
			default:
			}
			a.emit("pr:updated", PRUpdatedEvent{
				SubscriptionID: id,
				ThreadID:       entry.threadID,
				PR:             entry.pr,
				Detail:         snapshot.Detail,
				Threads:        snapshot.Threads,
				HeadSHA:        snapshot.Detail.HeadSHA,
			})
		}
	}
}

func (a *App) unsubscribePRUpdates(id string) {
	a.prUpdatePumpsMu.Lock()
	entry, ok := a.prUpdatePumps[id]
	if ok {
		delete(a.prUpdatePumps, id)
	}
	a.prUpdatePumpsMu.Unlock()
	if !ok {
		return
	}
	close(entry.done)
}

func (a *App) closePRUpdatePumps() {
	a.prUpdatePumpsMu.Lock()
	ids := make([]string, 0, len(a.prUpdatePumps))
	for id := range a.prUpdatePumps {
		ids = append(ids, id)
	}
	a.prUpdatePumpsMu.Unlock()
	for _, id := range ids {
		a.unsubscribePRUpdates(id)
	}
	a.prUpdatePumpWG.Wait()
}

func (a *App) fetchPRUpdateSnapshot(pr gitops.PRReference) (prUpdateSnapshot, error) {
	if a.prUpdateFetchFn != nil {
		return a.prUpdateFetchFn(pr)
	}
	detail, err := a.gitCore().GetPRDetail("", pr)
	if err != nil {
		return prUpdateSnapshot{}, err
	}
	threads, err := a.gitCore().ListReviewThreads("", pr)
	if err != nil {
		return prUpdateSnapshot{}, err
	}
	return prUpdateSnapshot{Detail: detail, Threads: threads}, nil
}

func (a *App) prUpdatePollInterval() time.Duration {
	if a.prUpdateInterval > 0 {
		return a.prUpdateInterval
	}
	return defaultPRUpdateInterval
}

func encodePRUpdateSnapshot(snapshot prUpdateSnapshot) ([]byte, error) {
	return json.Marshal(snapshot)
}

func validatePRReference(pr gitops.PRReference) error {
	if pr.Number <= 0 {
		return fmt.Errorf("PR number must be positive, got %d", pr.Number)
	}
	_, _, err := gitops.SplitProjectForForge(pr.Forge, pr.Project())
	return err
}
