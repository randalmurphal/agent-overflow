package sessionruntime

import (
	"sync/atomic"
	"time"

	"agent-overflow/internal/memory"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
)

// Entry is one live provider session. It is process-local runtime state and is
// never exposed on the wire.
type Entry struct {
	Provider             string
	Token                string
	CredentialGeneration uint64
	CredentialAccountID  string
	CredentialAccount    provider.AccountInfo
	Claude               *claude.Session
	Codex                *codex.Session
	ClaudeTUI            *claudetui.Session
	// Session holds the provider-agnostic handle for providers whose
	// app-layer interactions stay on provider.Session (such as ACP / agy).
	Session       provider.Session
	Conn          provider.Connection
	PoolKey       string
	LaunchOptions provider.SessionOptions
	Liveness      *Liveness
	// MemoryPlan is the memory the session's SessionStart hook delivers,
	// which a compaction delivers again; nil for none.
	MemoryPlan *memory.Plan
}

// ProviderSession returns the provider-agnostic live handle.
func (e Entry) ProviderSession() provider.Session {
	switch {
	case e.Session != nil:
		return e.Session
	case e.Claude != nil:
		return e.Claude
	case e.Codex != nil:
		return e.Codex
	case e.ClaudeTUI != nil:
		return e.ClaudeTUI
	default:
		return nil
	}
}

// Liveness contains the atomic activity counters shared by event handlers and
// the idle reaper.
type Liveness struct {
	LastActivityUnixNano atomic.Int64
	ActiveTurns          atomic.Int32
}

func NewLiveness(now time.Time) *Liveness {
	liveness := &Liveness{}
	liveness.LastActivityUnixNano.Store(now.UnixNano())
	return liveness
}

// BumpActivity advances the activity stamp monotonically.
func (l *Liveness) BumpActivity(now time.Time) {
	if l == nil {
		return
	}
	next := now.UnixNano()
	for {
		previous := l.LastActivityUnixNano.Load()
		if next <= previous {
			next = previous + 1
		}
		if l.LastActivityUnixNano.CompareAndSwap(previous, next) {
			return
		}
	}
}

func decrementActiveTurnsClamped(counter *atomic.Int32) {
	for {
		current := counter.Load()
		if current <= 0 {
			return
		}
		if counter.CompareAndSwap(current, current-1) {
			return
		}
	}
}
