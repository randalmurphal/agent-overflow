package app

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

const remoteCommandInstructions = `Agent Overflow can run commands on explicitly enabled paired computers.
Use agent-overflow remote list to discover reachable computers and their registered projects.
Use agent-overflow remote run --computer <uuid> --project <uuid> [--workspace <path>] [--id <request-uuid>] [--timeout <seconds>] -- <command> [args...].
Run returns a durable receipt; the job continues if this screen disconnects. Use remote status --computer <uuid> <request-uuid> and remote cancel with the same selectors.
After a lost reply, inspect the printed request UUID or retry the identical command with that --id; never create a new ID to guess whether the first command ran.
Choose the destination deliberately. It has its own files, environment and accounts. Never copy credentials or assume it shares the current checkout. Read remote --help for bounds.`

type appRemotePeers struct {
	ready    atomic.Bool
	wake     chan struct{}
	wakeOnce sync.Once
	wg       sync.WaitGroup
}

func (a *App) signalRemotePeers() {
	select {
	case a.remotePeerWake() <- struct{}{}:
	default:
	}
}

func (a *App) remotePeerWake() chan struct{} {
	a.remotePeers.wakeOnce.Do(func() { a.remotePeers.wake = make(chan struct{}, 1) })
	return a.remotePeers.wake
}

// One bounded discovery loop exists only on backends with a profile manager.
// With no enabled peers it sleeps until configuration changes. Availability
// is only a prompt hint; every command verifies the actual connection again.
func (a *App) startRemotePeerDiscovery() {
	if a.backends == nil {
		return
	}
	wake := a.remotePeerWake()
	a.remotePeers.wg.Add(1)
	go func() {
		defer a.remotePeers.wg.Done()
		for {
			probe, cancel := context.WithTimeout(a.lifeCtx(), 15*time.Second)
			rows, err := a.ListAgentComputers()
			enabled, ready := false, false
			if err == nil {
				for _, row := range a.probeAgentComputers(probe, rows) {
					enabled = true
					if row.Error == "" && len(row.Projects) > 0 {
						ready = true
					}
				}
			}
			cancel()
			if a.remotePeers.ready.Swap(ready) != ready {
				a.reconcileRemoteInstructions()
			}
			if a.lifeCtx().Err() != nil {
				return
			}
			var timer *time.Timer
			var tick <-chan time.Time
			if enabled {
				timer = time.NewTimer(30 * time.Second)
				tick = timer.C
			}
			select {
			case <-a.lifeCtx().Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-wake:
				if timer != nil {
					timer.Stop()
				}
			case <-tick:
			}
		}
	}()
}

func (a *App) remoteInstructionsForThread(thread store.Thread) string {
	if !a.remotePeers.ready.Load() || (thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex)) {
		return ""
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil {
		log.Printf("remote commands: resolve thread scope: %v", err)
		return ""
	}
	if !ok || (scope.IsPhase() && !scope.HasGrant("remote-commands")) {
		return ""
	}
	return remoteCommandInstructions
}

func (a *App) reconcileRemoteInstructions() {
	// Include starting sessions: the reconciler waits for the spawn that may
	// have captured the old availability. Both providers defer spawn-only
	// instruction changes until their existing safe config restart boundary.
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		for _, id := range a.sessionManager().threadIDsForProviderOrStarting(string(name)) {
			if a.shuttingDown.Load() {
				return
			}
			a.reconcileSessionConfig(id)
		}
	}
}
