package app

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
)

const defaultRemoteOutputBytes = 8 << 10

type remoteResultOptions struct {
	// Nil means the default; explicit zero requests only receipt metadata.
	MaxOutputBytes *int     `json:"max_output_bytes"`
	WaitSeconds    *float64 `json:"wait_seconds"`
}

func (o remoteResultOptions) validate() error {
	if o.WaitSeconds != nil && (*o.WaitSeconds < 0 || *o.WaitSeconds > 10) {
		return errors.New("wait_seconds must be between 0 and 10")
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
	OutputHint          string              `json:"outputHint,omitempty"`
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
	start := max(0, retained-budget)
	// Never split a UTF-8 rune when limiting the returned tail.
	for start < retained && !utf8.RuneStart(command.Output[start]) {
		start++
	}
	command.Output = command.Output[start:]
	return remoteMCPResult{RemoteCommand: command, ComputerID: computerID, RetainedOutputBytes: int64(retained), OmittedOutputBytes: int64(start)}
}

func (a *App) waitRemoteResult(ctx context.Context, computerID string, command RemoteCommand, options remoteResultOptions) (remoteMCPResult, error) {
	if command.State == "running" && options.WaitSeconds != nil && *options.WaitSeconds > 0 {
		wait, cancel := context.WithTimeout(ctx, time.Duration(*options.WaitSeconds*float64(time.Second)))
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
				delay = min(delay*2, time.Second)
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
	defer func() { result.OutputHint = remoteOutputHint(result) }()
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
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var chunk RemoteLogChunk
	if err := a.backends.CallAgentPeer(call, computerID, "RemoteCommandReadLog", &chunk, command.ID, int64(-1), max(1, budget)); err != nil {
		return result
	}
	result.Log = &chunk.LogInfo
	if !chunk.Expired {
		// JSON replaces incomplete/binary UTF-8 bytes, which can expand the
		// decoded text. Apply the reply budget after that wire conversion too.
		result.Output = remoteResult(computerID, RemoteCommand{Output: chunk.Text}, options).Output
		result.Truncated = chunk.Truncated
		result.RetainedOutputBytes = chunk.RetainedBytes
		result.OmittedOutputBytes = max(0, chunk.RetainedBytes-int64(len(result.Output)))
	}
	return result
}
