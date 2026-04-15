package design

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const (
	artifactRenderedEvent = "design:artifact"
	optionsPresentedEvent = "design:options"
	optionChosenEvent     = "design:chosen"
)

type pendingChoice struct {
	threadID string
	request  DesignOptionsRequest
	options  map[string]DesignOption
	resultCh chan choiceResolution
}

type choiceResolution struct {
	result ChoiceResult
	err    error
}

// Reactor coordinates design-mode tool calls, artifact storage, and pending
// user option requests.
type Reactor struct {
	artifacts *ArtifactStore
	emit      func(eventName string, data any)

	mu      sync.Mutex
	pending map[string]*pendingChoice
}

// NewReactor constructs a design reactor backed by an artifact store.
func NewReactor(artifacts *ArtifactStore, emit func(eventName string, data any)) *Reactor {
	return &Reactor{
		artifacts: artifacts,
		emit:      emit,
		pending:   make(map[string]*pendingChoice),
	}
}

// Render stores a rendered HTML artifact and emits it to the frontend.
func (r *Reactor) Render(threadID string, input RenderInput) (DesignArtifact, error) {
	if r.artifacts == nil {
		return DesignArtifact{}, fmt.Errorf("design reactor unavailable")
	}

	artifact, err := r.artifacts.Store(
		strings.TrimSpace(threadID),
		strings.TrimSpace(input.HTML),
		strings.TrimSpace(input.Title),
		strings.TrimSpace(input.Description),
		"render",
	)
	if err != nil {
		return DesignArtifact{}, err
	}

	r.emitEvent(artifactRenderedEvent, artifact)
	return artifact, nil
}

// PresentOptions stores option artifacts, emits an interactive request, and
// blocks until the user chooses one option or the context is cancelled.
func (r *Reactor) PresentOptions(
	ctx context.Context,
	threadID string,
	input PresentOptionsInput,
) (ChoiceResult, error) {
	threadID = strings.TrimSpace(threadID)
	request, err := r.storeOptions(threadID, input)
	if err != nil {
		return ChoiceResult{}, err
	}

	resultCh := make(chan choiceResolution, 1)

	r.mu.Lock()
	r.pending[request.RequestID] = &pendingChoice{
		threadID: threadID,
		request:  request,
		options:  optionsByID(request.Options),
		resultCh: resultCh,
	}
	r.mu.Unlock()

	r.emitEvent(optionsPresentedEvent, request)

	select {
	case <-ctx.Done():
		r.resolveRequest(request.RequestID, choiceResolution{
			err: fmt.Errorf("design option request cancelled: %w", ctx.Err()),
		})
		return ChoiceResult{}, ctx.Err()
	case resolution := <-resultCh:
		return resolution.result, resolution.err
	}
}

// ChooseOption resolves a pending design-choice request.
func (r *Reactor) ChooseOption(threadID, requestID, optionID string) error {
	threadID = strings.TrimSpace(threadID)
	requestID = strings.TrimSpace(requestID)
	optionID = strings.TrimSpace(optionID)

	if threadID == "" {
		return fmt.Errorf("thread ID is required")
	}
	if requestID == "" {
		return fmt.Errorf("request ID is required")
	}
	if optionID == "" {
		return fmt.Errorf("option ID is required")
	}

	r.mu.Lock()
	pending, ok := r.pending[requestID]
	if ok {
		delete(r.pending, requestID)
	}
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("design request %s not found", requestID)
	}
	if pending.threadID != threadID {
		return fmt.Errorf("design request %s does not belong to thread %s", requestID, threadID)
	}

	option, ok := pending.options[optionID]
	if !ok {
		return fmt.Errorf("design option %s not found for request %s", optionID, requestID)
	}

	result := ChoiceResult{
		Chosen: option.ID,
		Title:  option.Title,
	}
	pending.resultCh <- choiceResolution{result: result}
	r.emitEvent(optionChosenEvent, map[string]string{
		"threadId":  threadID,
		"requestId": requestID,
		"optionId":  option.ID,
		"title":     option.Title,
	})
	return nil
}

// PendingRequest returns the oldest pending design-choice request for a thread.
func (r *Reactor) PendingRequest(threadID string) (DesignOptionsRequest, bool) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return DesignOptionsRequest{}, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, request := range r.pending {
		if request.threadID == threadID {
			return request.request, true
		}
	}
	return DesignOptionsRequest{}, false
}

// TeardownThread cancels all pending design-choice requests for a thread.
func (r *Reactor) TeardownThread(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}

	var pending []*pendingChoice

	r.mu.Lock()
	for requestID, request := range r.pending {
		if request.threadID != threadID {
			continue
		}
		delete(r.pending, requestID)
		pending = append(pending, request)
	}
	r.mu.Unlock()

	for _, request := range pending {
		request.resultCh <- choiceResolution{err: fmt.Errorf("design mode session ended")}
	}
}

func (r *Reactor) storeOptions(threadID string, input PresentOptionsInput) (DesignOptionsRequest, error) {
	if r.artifacts == nil {
		return DesignOptionsRequest{}, fmt.Errorf("design reactor unavailable")
	}
	if threadID == "" {
		return DesignOptionsRequest{}, fmt.Errorf("thread ID is required")
	}

	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return DesignOptionsRequest{}, fmt.Errorf("prompt is required")
	}
	if len(input.Options) < 2 {
		return DesignOptionsRequest{}, fmt.Errorf("at least 2 options are required")
	}

	request := DesignOptionsRequest{
		RequestID: uuid.New().String(),
		ThreadID:  threadID,
		Prompt:    prompt,
		Options:   make([]DesignOption, 0, len(input.Options)),
	}

	seenIDs := make(map[string]struct{}, len(input.Options))
	for _, option := range input.Options {
		optionID := strings.TrimSpace(option.ID)
		if optionID == "" {
			return DesignOptionsRequest{}, fmt.Errorf("option ID is required")
		}
		if _, exists := seenIDs[optionID]; exists {
			return DesignOptionsRequest{}, fmt.Errorf("duplicate option ID %s", optionID)
		}
		seenIDs[optionID] = struct{}{}

		artifact, err := r.artifacts.Store(
			threadID,
			strings.TrimSpace(option.HTML),
			strings.TrimSpace(option.Title),
			strings.TrimSpace(option.Description),
			"option",
		)
		if err != nil {
			return DesignOptionsRequest{}, err
		}

		request.Options = append(request.Options, DesignOption{
			ID:          optionID,
			Title:       artifact.Title,
			Description: artifact.Description,
			ArtifactID:  artifact.ID,
		})
	}

	return request, nil
}

func (r *Reactor) resolveRequest(requestID string, resolution choiceResolution) {
	r.mu.Lock()
	request, ok := r.pending[requestID]
	if ok {
		delete(r.pending, requestID)
	}
	r.mu.Unlock()

	if ok {
		request.resultCh <- resolution
	}
}

func (r *Reactor) emitEvent(eventName string, data any) {
	if r.emit != nil {
		r.emit(eventName, data)
	}
}

func optionsByID(options []DesignOption) map[string]DesignOption {
	result := make(map[string]DesignOption, len(options))
	for _, option := range options {
		result[option.ID] = option
	}
	return result
}
