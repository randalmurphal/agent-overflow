package app

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

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
	ComputerID          string `json:"computerId"`
	RetainedOutputBytes int    `json:"retainedOutputBytes"`
	OmittedOutputBytes  int    `json:"omittedOutputBytes"`
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
	return remoteMCPResult{RemoteCommand: command, ComputerID: computerID, RetainedOutputBytes: retained, OmittedOutputBytes: start}
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
				return remoteResult(computerID, command, options), nil
			case <-timer.C:
				next, err := a.AgentRemoteStatus(wait, computerID, command.ID)
				if err != nil {
					// A bounded wait ending is not failure of the accepted command.
					if wait.Err() != nil && ctx.Err() == nil {
						return remoteResult(computerID, command, options), nil
					}
					return remoteMCPResult{}, err
				}
				command = next
				delay = min(delay*2, time.Second)
				timer.Reset(delay)
			}
		}
	}
	return remoteResult(computerID, command, options), nil
}
