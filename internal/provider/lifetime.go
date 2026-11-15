package provider

import (
	"context"
	"errors"

	"agent-overflow/internal/procutil"
)

// Lifetime is what ends a provider process: Spawn takes one and refuses the
// zero value before starting anything. It has two forms, and in both the
// orphan reaper is handed the group just after the leader forks and released
// once Wait has seen the leader exit, so the group ends if the backend dies
// first. Nothing else ends a provider process the backend leaves behind: its
// timeout lived in the backend, and the Claude CLI does not exit when its
// stdin closes.
type Lifetime struct {
	reaper  procutil.Reaper
	bounded bool
}

// Watched is the lifetime of a process that runs until its owner closes it:
// a session, a sign-in waiting on a person, or a read whose length follows
// what it reads, such as a transfer walking a conversation's subagents.
func Watched(reaper procutil.Reaper) Lifetime {
	return Lifetime{reaper: reaper}
}

// Bounded is the lifetime of a process that finishes on its own within its
// context's deadline: a probe, an account read, a one-shot app-server request.
// Spawn refuses a bounded launch whose context has no deadline.
func Bounded(reaper procutil.Reaper) Lifetime {
	return Lifetime{reaper: reaper, bounded: true}
}

// ErrNoLifetime is Spawn's error for a launch that does not say what ends it.
var ErrNoLifetime = errors.New("provider: spawn: no lifetime: nothing would end the process with the backend")

// ErrUnbounded is Spawn's error for a bounded launch whose context has no
// deadline.
var ErrUnbounded = errors.New("provider: spawn: a bounded launch needs a context deadline")

// check refuses a lifetime that would leave the process to outlive the
// backend.
func (l Lifetime) check(ctx context.Context) error {
	if l.reaper == nil {
		return ErrNoLifetime
	}
	if _, ok := ctx.Deadline(); l.bounded && !ok {
		return ErrUnbounded
	}
	return nil
}
