package design

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// ErrDesignSessionEnded is returned when a pending design-choice request is
// cancelled because the design-mode session was torn down.
var ErrDesignSessionEnded = errors.New("design mode session ended")

// ErrUnknownDispatchTool wraps Dispatch errors when the requested tool name is
// not one of the known design tools. Provider adapters use errors.Is to map
// this to a transport-appropriate error code.
var ErrUnknownDispatchTool = errors.New("unknown design tool")

// ErrInvalidDispatchArgs wraps Dispatch errors when the rawInput cannot be
// decoded into the expected tool input shape.
var ErrInvalidDispatchArgs = errors.New("invalid design tool arguments")

// Tool name constants for the design-mode wire surface. The same names appear
// in the system prompt, the Codex MCP tool list, and the Claude event-watcher
// filter — keep them centralized so a rename moves all three together.
const (
	ToolRenderDesign   = "render_design"
	ToolPresentOptions = "present_options"
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

type validatedPresentOption struct {
	ID          string
	Title       string
	Description string
	HTML        string
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

// DispatchResult is the JSON-serializable payload returned to the calling
// provider after a design-mode tool call completes.
type DispatchResult struct {
	// Payload is the structured tool result. Provider adapters JSON-marshal it
	// into the protocol's tool-result envelope.
	Payload any
}

// Dispatch routes a design-mode tool call through the reactor. Both Claude and
// Codex provider adapters call into this single entry point; the Codex MCP
// server is now a thin translator and the Claude event-watcher dispatches
// without forking on tool name. ctx may be cancelled by the caller (provider
// session ending) to abandon a blocking PresentOptions call; TeardownThread
// is the in-band cancel path keyed by thread id.
func (r *Reactor) Dispatch(ctx context.Context, threadID, toolName string, rawInput json.RawMessage) (DispatchResult, error) {
	if r == nil {
		return DispatchResult{}, fmt.Errorf("design reactor unavailable")
	}
	threadID = strings.TrimSpace(threadID)
	toolName = strings.TrimSpace(toolName)

	switch toolName {
	case ToolRenderDesign:
		var input RenderInput
		if err := json.Unmarshal(rawInput, &input); err != nil {
			return DispatchResult{}, fmt.Errorf("%w: %s: %v", ErrInvalidDispatchArgs, toolName, err)
		}
		artifact, err := r.Render(threadID, input)
		if err != nil {
			return DispatchResult{}, err
		}
		return DispatchResult{Payload: map[string]string{
			"status":     "rendered",
			"artifactId": artifact.ID,
		}}, nil

	case ToolPresentOptions:
		var input PresentOptionsInput
		if err := json.Unmarshal(rawInput, &input); err != nil {
			return DispatchResult{}, fmt.Errorf("%w: %s: %v", ErrInvalidDispatchArgs, toolName, err)
		}
		result, err := r.PresentOptions(ctx, threadID, input)
		if err != nil {
			return DispatchResult{}, err
		}
		return DispatchResult{Payload: result}, nil

	default:
		return DispatchResult{}, fmt.Errorf("%w: %s", ErrUnknownDispatchTool, toolName)
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
	select {
	case pending.resultCh <- choiceResolution{result: result}:
	default:
	}
	r.emitEvent(optionChosenEvent, map[string]string{
		"threadId":  threadID,
		"requestId": requestID,
		"optionId":  option.ID,
		"title":     option.Title,
	})
	return nil
}

// PendingRequest returns a pending design-choice request for the thread, if any.
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
		select {
		case request.resultCh <- choiceResolution{err: ErrDesignSessionEnded}:
		default:
		}
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

	validated, err := validatePresentOptions(input.Options)
	if err != nil {
		return DesignOptionsRequest{}, err
	}

	request := DesignOptionsRequest{
		RequestID: uuid.New().String(),
		ThreadID:  threadID,
		Prompt:    prompt,
		Options:   make([]DesignOption, 0, len(validated)),
	}

	for _, option := range validated {
		artifact, storeErr := r.artifacts.Store(
			threadID,
			option.HTML,
			option.Title,
			option.Description,
			"option",
		)
		if storeErr != nil {
			return DesignOptionsRequest{}, storeErr
		}

		request.Options = append(request.Options, DesignOption{
			ID:          option.ID,
			Title:       artifact.Title,
			Description: artifact.Description,
			ArtifactID:  artifact.ID,
		})
	}

	return request, nil
}

func validatePresentOptions(options []PresentOptionInput) ([]validatedPresentOption, error) {
	validated := make([]validatedPresentOption, 0, len(options))
	seenIDs := make(map[string]struct{}, len(options))

	for _, option := range options {
		next, err := validatePresentOption(option)
		if err != nil {
			return nil, err
		}
		if _, exists := seenIDs[next.ID]; exists {
			return nil, fmt.Errorf("duplicate option ID %s", next.ID)
		}
		seenIDs[next.ID] = struct{}{}
		validated = append(validated, next)
	}

	return validated, nil
}

func validatePresentOption(option PresentOptionInput) (validatedPresentOption, error) {
	validated := validatedPresentOption{
		ID:          strings.TrimSpace(option.ID),
		Title:       strings.TrimSpace(option.Title),
		Description: strings.TrimSpace(option.Description),
		HTML:        strings.TrimSpace(option.HTML),
	}
	if validated.ID == "" {
		return validatedPresentOption{}, fmt.Errorf("option ID is required")
	}
	if validated.Title == "" {
		return validatedPresentOption{}, fmt.Errorf("title is required")
	}
	if validated.HTML == "" {
		return validatedPresentOption{}, fmt.Errorf("html is required")
	}
	return validated, nil
}

func (r *Reactor) resolveRequest(requestID string, resolution choiceResolution) {
	r.mu.Lock()
	request, ok := r.pending[requestID]
	if ok {
		delete(r.pending, requestID)
	}
	r.mu.Unlock()

	if ok {
		select {
		case request.resultCh <- resolution:
		default:
		}
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
