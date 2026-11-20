package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/provider"
)

// ErrSessionNotFound means the CLI refused `--resume` because it holds no
// conversation under that session id. It is the only start failure that
// proves the conversation is gone; every other failure says nothing about it.
var ErrSessionNotFound = errors.New("claude: the conversation to resume does not exist")

// ErrConversationEmpty means the CLI loaded the conversation `--resume` named
// and it holds no user message: a transcript of system or attachment rows
// only, or a chain the CLI's resume filters emptied. A turn sent to it runs on
// no history.
var ErrConversationEmpty = errors.New("claude: the resumed conversation holds no user message")

// ErrContentUnproven means the CLI loaded the conversation `--resume` named
// and did not say that it holds a user message: it refused get_context_usage,
// answered without messageBreakdown or its userMessageTokens, answered
// something unreadable, or took a command AO wrote before the count was read.
// The wrapped message names which.
var ErrContentUnproven = errors.New("claude: the resumed conversation's content is unproven")

// IsConversationUnproven reports whether a start's outcome leaves no proven
// conversation to continue: the CLI holds none under the id, the one it
// loaded holds no user message, or it did not say whether it holds one.
func IsConversationUnproven(err error) bool {
	return errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrConversationEmpty) || errors.Is(err, ErrContentUnproven)
}

// ErrResumedOtherSession means the CLI initialized on a session other than
// the one `--resume` named, so the process does not hold the requested
// conversation, and whether that conversation still exists is unknown.
var ErrResumedOtherSession = errors.New("claude: the process initialized on a different session")

// startOutcome is how one process's start ended: it loaded its conversation,
// or it failed before it did. The read loop resolves a failure or an init
// before it hands the deciding frame to onEvent, so that outcome never waits
// on the caller's event handling; AwaitStart resolves a load its probe proved.
// An outcome that arrives before anyone waits is kept.
type startOutcome struct {
	done chan struct{}
	once sync.Once
	err  error
	// byInit says a `system/init` resolved the outcome. Written before done
	// closes and read only after it has.
	byInit bool
	// prompted and commanded say an input was written to the process: a
	// prompt (a person's, a queued or a workflow message), or a command AO
	// issued on its own (provider.SendOptions.InternalCommand). Set before
	// the write (noteInput).
	prompted, commanded atomic.Bool
	// resume is the session `--resume` names on a start that keeps that
	// session; empty on a fresh start or a fork, which initialize on a session
	// id of their own.
	resume string
	// initialized is read and written only on the read loop.
	initialized bool
}

func newStartOutcome(cfg Config) *startOutcome {
	outcome := &startOutcome{done: make(chan struct{})}
	if !cfg.ForkSession {
		outcome.resume = strings.TrimSpace(cfg.Resume)
	}
	return outcome
}

func (o *startOutcome) resolve(err error) {
	o.resolveFrom(err, false)
}

func (o *startOutcome) resolveFrom(err error, byInit bool) {
	o.once.Do(func() {
		o.err = err
		o.byInit = byInit
		close(o.done)
	})
}

// noteInput records that an input is about to be written to the process.
func (o *startOutcome) noteInput(internalCommand bool) {
	if o == nil {
		return
	}
	if internalCommand {
		o.commanded.Store(true)
	} else {
		o.prompted.Store(true)
	}
}

// observe resolves the outcome from one parsed frame and returns the frame to
// hand on. The first `system/init` settles it. An error `result` before that
// init is the CLI failing its startup (for example a `--resume` naming a
// conversation it does not hold): the frame is marked
// provider.FailureReasonStartup, which is the one rule every consumer of a
// failed start reads. Runs on the read loop only.
func (o *startOutcome) observe(evt provider.ProviderEvent) provider.ProviderEvent {
	if o == nil || o.initialized {
		return evt
	}
	switch evt.Kind {
	case provider.EventInit:
		o.initialized = true
		o.resolveFrom(o.initError(evt), true)
	case provider.EventTurnComplete:
		wire, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta)
		if !ok || wire == nil || wire.ErrorMessage == "" {
			return evt
		}
		failure := provider.FailureMeta{Class: provider.FailureFatal}
		if evt.Failure != nil {
			failure = *evt.Failure
		}
		failure.Reason = provider.FailureReasonStartup
		evt.Failure = &failure
		o.resolve(startFailure(wire.ErrorMessage))
	}
	return evt
}

func (o *startOutcome) initError(evt provider.ProviderEvent) error {
	if o.resume == "" {
		return nil
	}
	var info provider.SessionInfo
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &info); err != nil {
			return fmt.Errorf("claude: read the session the process initialized on: %w", err)
		}
	}
	if info.SessionID != o.resume {
		return fmt.Errorf("%w: --resume named %s, the process reported %q", ErrResumedOtherSession, o.resume, info.SessionID)
	}
	return nil
}

// startFailure classifies the error a CLI reports before init. Both spellings
// mean the CLI found no conversation for the id: "No conversation found with
// session ID: <id>" (2.1.88 `cli/print.ts`, 2.1.325 bundle) and the refusal of
// an id that is not one ("--resume requires a valid session ID ...", the same
// two sources).
func startFailure(message string) error {
	for _, missing := range []string{"No conversation found", "--resume requires a valid session ID"} {
		if strings.Contains(message, missing) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, message)
		}
	}
	return fmt.Errorf("claude: the session failed to start: %s", message)
}

// answered resolves the start as loaded when the CLI answers a control
// request, which it does only from the message loop it starts after the
// resume load. Runs on the read loop, so a failure the CLI wrote before the
// answer has already resolved the outcome and is kept.
func (o *startOutcome) answered() {
	if o != nil {
		o.resolve(nil)
	}
}

// exited resolves the outcome of a process whose read loop ended before init.
// closing says the host closed the session itself.
func (o *startOutcome) exited(proc *provider.Process, closing bool) {
	if o == nil {
		return
	}
	select {
	case <-o.done:
		return
	default:
	}
	if closing {
		o.resolve(errors.New("claude: the session was closed before it initialized"))
		return
	}
	reason := "claude: the process exited before it initialized"
	if proc != nil {
		if err := provider.WaitProcessExitErr(proc); err != nil {
			reason += " (" + err.Error() + ")"
		}
		if tail := provider.SanitizeChildStderr(proc.StderrTail()); tail != "" {
			reason += ": " + tail
		}
	}
	o.resolve(errors.New(reason))
}

// AwaitStart waits until this process loaded the conversation it was started
// for, or failed before it did. It returns nil once the CLI answers a control
// request or initializes on the session `--resume` named (any session for a
// fresh start or a fork), ErrSessionNotFound when the CLI reported that
// conversation does not exist, ErrResumedOtherSession for an init on another
// session, and an error naming the cause when the process failed to start,
// exited or was closed first. An outcome reached before the call is returned
// at once.
//
// A print-mode CLI writes its first init only for its first input, so the
// proof before any send is a `mcp_status` control request: the CLI answers
// control requests only from its message loop, which it starts after the
// resume load returns, and a load that fails writes its error result first
// (claude-wire.md §Start outcome before the first input). Any answer,
// success or error, proves the load. The answer says nothing about which
// session the CLI loaded.
//
// A loaded resume is then checked for content once per call
// (resumedContent): only a nonzero user-message count proves it, a zero is
// ErrConversationEmpty and any other answer ErrContentUnproven. The count is
// the resumed conversation's only before the first input, so the check is
// valid only then, and the caller holds every input off the process until
// AwaitStart returns. A process that already took an input is proof on its
// own and its content is not read: a prompt was written to it, or its start
// resolved through an init, which the CLI writes only for an input. A command
// AO wrote on its own (provider.SendOptions.InternalCommand) before the count
// was read may be in it, which is ErrContentUnproven.
//
// The wait ends with ctx and has no budget of its own: the load can be
// silent for as long as its SessionStart hooks run.
func (s *Session) AwaitStart(ctx context.Context) error {
	if s == nil || s.start == nil {
		return errors.New("claude: the session was not started by NewSession")
	}
	if err := s.awaitLoad(ctx); err != nil || s.start.resume == "" {
		return err
	}
	if s.start.byInit && !s.start.commanded.Load() {
		return nil
	}
	return s.resumedContent(ctx)
}

func (s *Session) awaitLoad(ctx context.Context) error {
	select {
	case <-s.start.done:
		return s.start.err
	default:
	}
	// The read loop resolves the outcome from the answer (startOutcome.
	// answered). A probe that could not be written leaves it to the read
	// loop too: the process is closing or gone, and its exit resolves it.
	if requestID, _, err := s.writeControlRequest("mcp_status", map[string]any{"subtype": "mcp_status"}); err == nil {
		defer s.releaseControlRequest(requestID)
	}
	select {
	case <-s.start.done:
		return s.start.err
	case <-ctx.Done():
		return fmt.Errorf("claude: wait for the session to load: %w", ctx.Err())
	}
}

// resumedContent reads get_context_usage once (claude-wire.md §Start outcome
// before the first input) and proves the loaded resume only when its
// messageBreakdown counts user-message tokens. Like the load, the wait has
// no budget of its own: ctx bounds it.
func (s *Session) resumedContent(ctx context.Context) error {
	if took, err := s.tookInput(); took {
		return err
	}
	requestID, answer, err := s.writeControlRequest("get_context_usage", map[string]any{"subtype": "get_context_usage"})
	if err != nil {
		return fmt.Errorf("claude: read the resumed conversation's content: %w", err)
	}
	defer s.releaseControlRequest(requestID)
	select {
	case res := <-answer:
		if res == nil {
			return errors.New("claude: the session closed before it reported the resumed conversation's content")
		}
		// An input written while the request was out may be in the count.
		if took, err := s.tookInput(); took {
			return err
		}
		if !res.ok {
			return fmt.Errorf("%w: get_context_usage refused: %s", ErrContentUnproven, res.errMsg)
		}
		tokens, err := userMessageTokens(res.payload)
		switch {
		case err != nil:
			return fmt.Errorf("%w: %v", ErrContentUnproven, err)
		case tokens == 0:
			return fmt.Errorf("%w: get_context_usage counts 0 user-message tokens", ErrConversationEmpty)
		case tokens < 0:
			return fmt.Errorf("%w: get_context_usage counts %d user-message tokens", ErrContentUnproven, tokens)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("claude: wait for the resumed conversation's content: %w", ctx.Err())
	}
}

// tookInput reports whether an input was written to the process, and what
// that means for the content check: nil for a prompt, which the process
// took, so it is proof on its own; ErrContentUnproven for a command AO wrote
// on its own, which may be in the count.
func (s *Session) tookInput() (bool, error) {
	switch {
	case s.start.prompted.Load():
		return true, nil
	case s.start.commanded.Load():
		return true, fmt.Errorf("%w: a command AO wrote before the count was read may be in it", ErrContentUnproven)
	default:
		return false, nil
	}
}

// userMessageTokens reads messageBreakdown.userMessageTokens from a
// get_context_usage payload. An answer without it is an error naming what is
// missing.
func userMessageTokens(payload json.RawMessage) (int, error) {
	var decoded struct {
		MessageBreakdown *struct {
			UserMessageTokens *int `json:"userMessageTokens"`
		} `json:"messageBreakdown"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return 0, fmt.Errorf("get_context_usage answer unreadable: %w", err)
	}
	if decoded.MessageBreakdown == nil {
		return 0, errors.New("get_context_usage answered with no messageBreakdown")
	}
	if decoded.MessageBreakdown.UserMessageTokens == nil {
		return 0, errors.New("get_context_usage messageBreakdown has no userMessageTokens")
	}
	return *decoded.MessageBreakdown.UserMessageTokens, nil
}
