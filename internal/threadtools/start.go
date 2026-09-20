package threadtools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/provider"
)

// thread_spawn, thread_send and thread_ask: the three calls that start
// work in another thread. All three mint a request, so all three return a
// token, and all three take the same wait.
//
// Everything checkable without the app is checked here, so a refusal costs
// the agent one call and never a half-started request: the prompt, the
// enums, the wait bounds, the self-send rule, and the project a spawn on
// another computer must name. What only the destination can know (a
// provider it does not offer, a thread deleted since resolution) it
// refuses itself.

type spawnArgs struct {
	Prompt        string `json:"prompt"`
	Title         string `json:"title"`
	FromThread    string `json:"from_thread"`
	ComputerID    string `json:"computer_id"`
	ProjectID     string `json:"project_id"`
	WorkspacePath string `json:"workspace_path"`
	Worktree      string `json:"worktree"`
	Base          string `json:"base"`
	BaseLocal     bool   `json:"base_local"`
	Group         string `json:"group"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Effort        string `json:"effort"`
	Mode          string `json:"mode"`
	RuntimeMode   string `json:"runtime_mode"`
	WaitSeconds   *int   `json:"wait_seconds"`
	Notify        bool   `json:"notify"`
}

type sendArgs struct {
	ThreadID    string `json:"thread_id"`
	ComputerID  string `json:"computer_id"`
	Message     string `json:"message"`
	WaitSeconds *int   `json:"wait_seconds"`
	Notify      bool   `json:"notify"`
}

type askArgs struct {
	ThreadID    string `json:"thread_id"`
	ComputerID  string `json:"computer_id"`
	Question    string `json:"question"`
	WaitSeconds *int   `json:"wait_seconds"`
	Notify      bool   `json:"notify"`
}

// ackResult is what a spawn, send, ask or remind hands back. Its note is
// the recovery hint: what to do now, named with the token to do it with.
type ackResult struct {
	Token      string `json:"token"`
	Kind       string `json:"kind,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state"`
	Outcome    string `json:"outcome"`
	AnswerKind string `json:"answer_kind,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Revision   int64  `json:"revision"`
	Notify     bool   `json:"notify"`
	Delivered  string `json:"delivered,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	Note       string `json:"note,omitempty"`
}

func (c *session) spawn(ctx context.Context, raw json.RawMessage) (any, error) {
	var args spawnArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	call := SpawnCall{
		Prompt:            trim(args.Prompt),
		Title:             trim(args.Title),
		ComputerID:        trim(args.ComputerID),
		ProjectID:         trim(args.ProjectID),
		WorkspacePath:     trim(args.WorkspacePath),
		WorktreeBranch:    trim(args.Worktree),
		WorktreeBase:      trim(args.Base),
		WorktreeBaseLocal: args.BaseLocal,
		Group:             trim(args.Group),
		Provider:          trim(args.Provider),
		Model:             trim(args.Model),
		Effort:            trim(args.Effort),
		Mode:              trim(args.Mode),
		RuntimeMode:       trim(args.RuntimeMode),
		Notify:            args.Notify,
	}
	if err := checkText(call.Prompt, "prompt"); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(call.Title) > MaxTitleRunes {
		return nil, invalidf("title must be at most %d characters.", MaxTitleRunes)
	}
	if utf8.RuneCountInString(call.Group) > MaxTitleRunes {
		return nil, invalidf("group must be at most %d characters.", MaxTitleRunes)
	}
	if call.Mode != "" && !slices.Contains([]string{"chat", "plan"}, call.Mode) {
		return nil, invalidf("mode must be chat or plan. The permission level is runtime_mode, a separate parameter.")
	}
	if call.RuntimeMode != "" && !provider.IsRuntimeMode(provider.RuntimeMode(call.RuntimeMode)) {
		return nil, invalidf("runtime_mode must be one of %s.", joinNames(runtimeModeEnum()))
	}
	// A fork inherits its source's checkout, so a parameter that picks one
	// is refused here rather than silently ignored by the spawn. This comes
	// before the worktree rules below, which would otherwise answer
	// from_thread with base by asking for a worktree that is also refused.
	if trim(args.FromThread) != "" {
		placement := ""
		switch {
		case call.WorkspacePath != "":
			placement = "workspace_path"
		case call.WorktreeBranch != "":
			placement = "worktree"
		case call.WorktreeBase != "":
			placement = "base"
		case call.WorktreeBaseLocal:
			placement = "base_local"
		}
		if placement != "" {
			return nil, invalidf("Pass either from_thread or %s, not both: a fork runs in its source's workspace. Spawn a fresh thread to work in a checkout of your own choosing.", placement)
		}
	}
	if call.WorktreeBranch != "" && call.WorkspacePath != "" {
		return nil, invalidf("Pass either workspace_path to run in an existing checkout or worktree to cut a fresh one on that branch, not both.")
	}
	if call.WorktreeBranch == "" && (call.WorktreeBase != "" || call.WorktreeBaseLocal) {
		return nil, invalidf("base and base_local describe the worktree that worktree cuts, so pass worktree with them.")
	}
	wait, err := waitSeconds(args.WaitSeconds, 0)
	if err != nil {
		return nil, err
	}
	call.WaitSeconds = wait

	if ref := trim(args.FromThread); ref != "" {
		source, err := c.resolve(ctx, ref, "")
		if err != nil {
			return nil, err
		}
		if call.ComputerID != "" && source.ComputerID != "" && call.ComputerID != source.ComputerID {
			return nil, invalidf("from_thread %s lives on %s, and a fork runs on its own computer. Drop computer_id, or spawn a fresh thread there instead of forking.", source.ThreadID, NameOfComputer(Computer{ID: source.ComputerID, Name: source.Computer}))
		}
		call.FromThread, call.FromThreadComputer = source.ThreadID, source.ComputerID
		// A fork runs where its source lives, and a source on this
		// computer runs here however the row names this computer.
		call.ComputerID = source.Destination()
	}
	if err := c.checkSpawnDestination(ctx, &call); err != nil {
		return nil, err
	}
	ack, err := c.app.Spawn(ctx, c.caller, call)
	if err != nil {
		return nil, err
	}
	return c.ack(ack, "spawn"), nil
}

// checkSpawnDestination enforces the one rule the destination cannot
// infer: projects are registered per computer, so a spawn on another
// computer names one of its projects. The refusal carries that computer's
// projects and worktrees, so the mistake costs one call and no separate
// discovery tool exists for it.
func (c *session) checkSpawnDestination(ctx context.Context, call *SpawnCall) error {
	if call.ComputerID == "" || call.ProjectID != "" || call.FromThread != "" {
		return nil
	}
	computer, local, ok := c.computerByID(call.ComputerID)
	if !ok {
		return publicf(CodeInvalidRequest, "There is no computer %q. thread_options lists the ones you can reach.", call.ComputerID)
	}
	if local {
		call.ComputerID = ""
		return nil
	}
	peer, err := c.app.Peer(ctx, computer.ID)
	if err != nil {
		return err
	}
	raw, err := peer.Query(ctx, "thread_options", json.RawMessage(`{}`))
	if err != nil {
		return err
	}
	var answer optionsAnswerShape
	if err := peerResult(raw, &answer); err != nil {
		return err
	}
	row := answer.computerOptions
	if len(answer.Computers) > 0 {
		row = answer.Computers[0]
	}
	return publicf(CodeInvalidRequest, "A thread on %s needs project_id: projects are registered per computer and cannot be inherited. %s offers: %s.", NameOfComputer(computer), NameOfComputer(computer), describeProjects(row.Projects))
}

func describeProjects(projects []ProjectOption) string {
	if len(projects) == 0 {
		return "no registered projects"
	}
	parts := make([]string, 0, len(projects))
	for _, project := range projects {
		part := project.Name + " (" + project.ID + ")"
		// Roots are few and are the paths a spawn wants; worktrees can
		// run to dozens, so they are counted and left to thread_options.
		roots := make([]string, 0, len(project.Workspaces))
		worktrees := 0
		for _, workspace := range project.Workspaces {
			if workspace.Worktree {
				worktrees++
				continue
			}
			roots = append(roots, workspace.Path)
		}
		if len(roots) > 0 {
			part += " at " + strings.Join(roots, ", ")
		}
		switch worktrees {
		case 0:
		case 1:
			part += " with 1 worktree, listed by thread_options"
		default:
			part += fmt.Sprintf(" with %d worktrees, listed by thread_options", worktrees)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func (c *session) send(ctx context.Context, raw json.RawMessage) (any, error) {
	var args sendArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	message := trim(args.Message)
	if err := checkText(message, "message"); err != nil {
		return nil, err
	}
	wait, err := waitSeconds(args.WaitSeconds, 0)
	if err != nil {
		return nil, err
	}
	target, err := c.resolveOther(ctx, args.ThreadID, trim(args.ComputerID), "send to")
	if err != nil {
		return nil, err
	}
	ack, err := c.app.Send(ctx, c.caller, SendCall{
		ThreadID: target.ThreadID, ComputerID: target.Destination(),
		Message: message, WaitSeconds: wait, Notify: args.Notify,
	})
	if err != nil {
		return nil, err
	}
	return c.ack(ack, "send"), nil
}

func (c *session) ask(ctx context.Context, raw json.RawMessage) (any, error) {
	var args askArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	question := trim(args.Question)
	if err := checkText(question, "question"); err != nil {
		return nil, err
	}
	wait, err := waitSeconds(args.WaitSeconds, DefaultAskWaitSeconds)
	if err != nil {
		return nil, err
	}
	target, err := c.resolveOther(ctx, args.ThreadID, trim(args.ComputerID), "ask")
	if err != nil {
		return nil, err
	}
	ack, err := c.app.Ask(ctx, c.caller, AskCall{
		ThreadID: target.ThreadID, ComputerID: target.Destination(),
		Question: question, WaitSeconds: wait, Notify: args.Notify,
	})
	if err != nil {
		return nil, err
	}
	return c.ack(ack, "ask"), nil
}

// resolveOther resolves a target and refuses the caller's own thread. A
// thread id is globally unique, so the id alone settles it.
func (c *session) resolveOther(ctx context.Context, ref, hint, verb string) (Target, error) {
	target, err := c.resolve(ctx, ref, hint)
	if err != nil {
		return Target{}, err
	}
	if target.ThreadID == c.caller.ThreadID {
		return Target{}, publicf(CodeSelfSend, "A thread cannot %s itself. Use thread_remind to wake yourself later, or thread_spawn to hand the work to a new thread.", verb)
	}
	return target, nil
}

func checkText(text, field string) error {
	if text == "" {
		return invalidf("%s is required and cannot be blank.", field)
	}
	if len(text) > MaxPromptBytes {
		return invalidf("%s must be at most %d bytes.", field, MaxPromptBytes)
	}
	return nil
}

func waitSeconds(value *int, fallback int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < 0 || *value > MaxWaitSeconds {
		return 0, invalidf("wait_seconds must be between 0 and %d. It is how long this call waits, not how long the work may run.", MaxWaitSeconds)
	}
	return *value, nil
}

// ack renders a request receipt and states what to do next.
func (c *session) ack(receipt RequestAck, kind string) ackResult {
	id, name := c.stamp(Computer{ID: receipt.ComputerID, Name: receipt.Computer})
	result := ackResult{
		Token:      receipt.Token,
		Kind:       firstNonEmpty(receipt.Kind, kind),
		ThreadID:   receipt.ThreadID,
		ComputerID: id,
		Computer:   name,
		Title:      receipt.Title,
		State:      receipt.State,
		Outcome:    receipt.Outcome,
		AnswerKind: receipt.AnswerKind,
		Answer:     receipt.Answer,
		Revision:   receipt.Revision,
		Notify:     receipt.Notify,
		Delivered:  receipt.Delivered,
		ExpiresAt:  unixMsToRFC3339(receipt.ExpiresAt),
	}
	result.Note = ackNote(result)
	return result
}

func ackNote(result ackResult) string {
	switch result.Outcome {
	case OutcomeSettled:
		if result.AnswerKind == AnswerFinal {
			return "That thread ended its turn without calling thread_reply, so this is its last message and may not be the answer you asked for. Send again if you need one."
		}
		return "Settled. This reply is the delivery; no message will arrive for it."
	case OutcomeBlocked:
		return "That thread is waiting on the user for an approval or a question, so it cannot answer right now. The request stays open: leave it to the user, check it later with thread_status token " + result.Token + ", or stop it with thread_cancel."
	case OutcomeUnconfirmed:
		return "The call failed before it was known whether the work started. Do NOT start it again: check thread_status with token " + result.Token + "."
	default:
		// Only a request with notify armed wakes this thread. Without it
		// the work runs unattended, and promising a message would leave
		// the agent waiting for one that is never sent.
		if result.Notify {
			return "Still working. The answer will arrive in this thread as a message at your next turn boundary; you do not need to poll. To wait again instead of ending your turn, call thread_status with token " + result.Token + "."
		}
		return "Still working, and nothing will wake this thread when it finishes: no wait and no notify were asked for. Read the answer with thread_status token " + result.Token + ", or ask again with notify or wait_seconds to have it delivered."
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
