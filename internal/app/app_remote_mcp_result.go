package app

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
)

const defaultRemoteOutputBytes = 8 << 10

// remoteOutputHeadBytes is the leading excerpt a reply carries beside its tail
// when the tail alone did not cover the whole output: the first compiler
// error or the command banner sits there, not at the end.
const remoteOutputHeadBytes = 2 << 10

// defaultRemoteRunWaitSeconds keeps an ordinary remote_run behaving like a
// normal tool call: the agent waits for the result. Longer work returns a
// backgrounded receipt and the completion arrives as a message.
// maxRemoteWaitSeconds bounds one call; remoteMCPCallCeiling in
// app_remote_mcp.go is what the providers are told to tolerate above it.
const defaultRemoteRunWaitSeconds = 300
const maxRemoteWaitSeconds = 900

type remoteResultOptions struct {
	// Nil means the default; explicit zero requests only receipt metadata.
	MaxOutputBytes *int     `json:"max_output_bytes"`
	WaitSeconds    *float64 `json:"wait_seconds"`
}

func (o remoteResultOptions) validate() error {
	if o.WaitSeconds != nil && (*o.WaitSeconds < 0 || *o.WaitSeconds > maxRemoteWaitSeconds) {
		return fmt.Errorf("wait_seconds must be between 0 and %d", maxRemoteWaitSeconds)
	}
	if o.MaxOutputBytes != nil && (*o.MaxOutputBytes < 0 || *o.MaxOutputBytes > store.RemoteJobOutputLimit) {
		return errors.New("max_output_bytes must be between 0 and 131072")
	}
	return nil
}

type remoteMCPResult struct {
	RemoteCommand
	ComputerID          string              `json:"computerId"`
	ComputerName        string              `json:"computerName,omitempty"`
	Label               string              `json:"label,omitempty"`
	OutputHead          string              `json:"outputHead,omitempty"`
	OutputHint          string              `json:"outputHint,omitempty"`
	Backgrounded        bool                `json:"backgrounded,omitempty"`
	BackgroundHint      string              `json:"backgroundHint,omitempty"`
	RetainedOutputBytes int64               `json:"retainedOutputBytes"`
	OmittedOutputBytes  int64               `json:"omittedOutputBytes"`
	Log                 *remotejobs.LogInfo `json:"log,omitempty"`
	Notification        string              `json:"notification,omitempty"`
}

func remoteResult(computerID string, command RemoteCommand, options remoteResultOptions) remoteMCPResult {
	budget := defaultRemoteOutputBytes
	if options.MaxOutputBytes != nil {
		budget = *options.MaxOutputBytes
	}
	retained := len(command.Output)
	command.Output = remoteOutputTail(command.Output, budget)
	return remoteMCPResult{RemoteCommand: command, ComputerID: computerID, RetainedOutputBytes: int64(retained), OmittedOutputBytes: int64(retained - len(command.Output))}
}

// remoteOutputTail keeps the newest budget bytes without splitting a rune.
func remoteOutputTail(output string, budget int) string {
	start := max(0, len(output)-budget)
	for start < len(output) && !utf8.RuneStart(output[start]) {
		start++
	}
	return output[start:]
}

// remoteOutputHead keeps the oldest budget bytes without splitting a rune.
func remoteOutputHead(output string, budget int) string {
	end := min(len(output), budget)
	for end > 0 && end < len(output) && !utf8.RuneStart(output[end]) {
		end--
	}
	return output[:end]
}

// errRemoteWaitInterrupted marks a wait ended by this app, not by the
// provider's request: the user stopped the turn, or the session ended. The
// accepted command keeps running and the caller gets a backgrounded receipt.
var errRemoteWaitInterrupted = errors.New("remote wait interrupted")

// waitRemoteResult polls the destination until the command settles or the
// wait ends. ctx is the provider's request; waitCtx is derived from it and
// additionally canceled with errRemoteWaitInterrupted by an interrupt.
func (a *App) waitRemoteResult(ctx, waitCtx context.Context, computerID string, command RemoteCommand, options remoteResultOptions) (remoteMCPResult, error) {
	if command.State == "running" && options.WaitSeconds != nil && *options.WaitSeconds > 0 {
		wait, cancel := context.WithTimeout(waitCtx, time.Duration(*options.WaitSeconds*float64(time.Second)))
		defer cancel()
		delay := 100 * time.Millisecond
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for command.State == "running" {
			select {
			case <-wait.Done():
				if ctx.Err() != nil {
					return remoteMCPResult{}, ctx.Err()
				}
				return a.remoteResultWithLog(ctx, computerID, command, options), nil
			case <-timer.C:
				next, err := a.AgentRemoteStatus(wait, computerID, command.ID)
				if err != nil {
					// A bounded wait ending is not failure of the accepted command.
					if wait.Err() != nil && ctx.Err() == nil {
						return a.remoteResultWithLog(ctx, computerID, command, options), nil
					}
					return remoteMCPResult{}, err
				}
				command = next
				delay = min(delay*2, 2*time.Second)
				timer.Reset(delay)
			}
		}
	}
	return a.remoteResultWithLog(ctx, computerID, command, options), nil
}

// Old peers keep their inline tail. Disk metadata is additive and a log read
// failure must never turn successful command acceptance into a run failure.
func (a *App) remoteResultWithLog(ctx context.Context, computerID string, command RemoteCommand, options remoteResultOptions) (result remoteMCPResult) {
	result = remoteResult(computerID, command, options)
	result.ComputerName = a.remoteComputerNames()[computerID]
	defer func() {
		result.OutputHint = remoteOutputHint(result)
		if result.State == "running" {
			result.Backgrounded = true
			result.BackgroundHint = remoteBackgroundHint
		}
	}()
	if w, err := a.store.GetRemoteWatch(computerID, command.ID); err == nil {
		result.Notification = w.Notification
		result.Label = w.Label
	}
	if a.backends == nil {
		return result
	}
	budget := defaultRemoteOutputBytes
	if options.MaxOutputBytes != nil {
		budget = *options.MaxOutputBytes
	}
	// max_output_bytes bounds the whole excerpt: the head takes at most a
	// quarter of it, so a caller asking for a few bytes gets only the tail.
	headBudget := min(remoteOutputHeadBytes, budget/4)
	tailBudget := budget - headBudget
	excerpt, ok := a.remoteLogExcerpt(ctx, computerID, command.ID, headBudget, tailBudget)
	if !ok {
		return result
	}
	result.Log = &excerpt.Info
	if !excerpt.Info.Expired {
		// JSON replaces incomplete/binary UTF-8 bytes, which can expand the
		// decoded text. Apply the reply budget after that wire conversion too.
		result.Output = remoteOutputTail(excerpt.Tail, tailBudget)
		result.OutputHead = remoteOutputHead(excerpt.Head, headBudget)
		result.Truncated = excerpt.Info.Truncated
		result.RetainedOutputBytes = excerpt.Info.RetainedBytes
		result.OmittedOutputBytes = max(0, excerpt.Info.RetainedBytes-int64(len(result.Output))-int64(len(result.OutputHead)))
	}
	return result
}

const remoteBackgroundHint = "The command is still running and now runs in the background on the destination. Its result arrives as a message in this conversation when it finishes; do not poll for completion. remote_read_log with offset -1 shows its latest output, remote_cancel stops it."

type remoteLogExcerptResult struct {
	Info remotejobs.LogInfo
	Head string
	Tail string
}

// remoteLogExcerpt reads the newest tailBudget bytes of a job's saved log and,
// when that tail did not reach the retained start, the oldest headBudget
// bytes too. Two bounded reads at most; a failure reports false and the
// caller keeps whatever inline tail it already holds.
func (a *App) remoteLogExcerpt(ctx context.Context, computerID, requestID string, headBudget, tailBudget int) (remoteLogExcerptResult, bool) {
	if a.backends == nil {
		return remoteLogExcerptResult{}, false
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var tail RemoteLogChunk
	if err := a.backends.CallAgentPeer(call, computerID, "RemoteCommandReadLog", &tail, requestID, int64(-1), max(1, tailBudget)); err != nil {
		return remoteLogExcerptResult{}, false
	}
	result := remoteLogExcerptResult{Info: tail.LogInfo, Tail: tail.Text}
	if tail.Expired || headBudget < 1 || tail.Offset <= tail.StartOffset || tail.StartOffset != 0 {
		return result, true
	}
	var head RemoteLogChunk
	if err := a.backends.CallAgentPeer(call, computerID, "RemoteCommandReadLog", &head, requestID, int64(0), min(headBudget, int(tail.Offset))); err != nil {
		return result, true
	}
	result.Head = head.Text
	return result, true
}
