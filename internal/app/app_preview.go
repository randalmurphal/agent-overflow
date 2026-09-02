package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/devscan"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
)

// The dev-server list: which loopback ports on this machine are serving
// pages, which thread each belongs to, and which of them a preview URL
// can be minted for (docs/specs/remote-access.md §7, the port gateway).
//
// The scan itself lives in internal/devscan, which reads and nothing
// else. This file is the two things the App owns around it: WHO the
// owners are — the live provider sessions and thread terminals a
// listener can descend from — and WHEN a scan is worth doing at all.
//
// The "when" is the whole reason there is a gate here rather than a
// plain ticker. Discovery exists to serve a person looking at this
// machine's dev servers from ANOTHER device; on a desktop-only install
// nobody ever reads the list, and a poll that scanned regardless would
// walk /proc and dial loopback ports every three seconds forever for an
// answer nothing consumes. So the loop scans only while a session that
// is both off this machine and granted `preview:open` is attached, and
// GetDevServers scans on demand for everyone else.

const (
	// devServerScanInterval is the published cadence. Fast enough that a
	// dev server an agent just started shows up while the person is still
	// reading the message that mentioned it; slow enough that the probe's
	// 15s verdict memo absorbs nearly every tick without a dial.
	devServerScanInterval = 3 * time.Second

	// devServerScanTimeout bounds one whole scan. The enumerator is file
	// reads and the probe has its own per-candidate bound, so this is the
	// backstop for a machine with a great many candidates at once, and it
	// stays under the cadence so passes cannot pile up.
	devServerScanTimeout = 2 * time.Second
)

// previewState is everything the scan loop and the RPCs share. Its zero
// value is an App with no loop running and no scanner, which is what
// every fixture that never calls Start gets.
type previewState struct {
	// scan serializes scans. The loop and an on-demand GetDevServers can
	// both want one; the scanner is safe for concurrent use, but two
	// overlapping passes would each see half of the other's grace
	// bookkeeping, and one at a time costs nothing at this cadence.
	scan sync.Mutex

	mu sync.Mutex
	// scanner is nil until the loop or the first RPC builds one. One per
	// App, because the probe memo and the grace deadlines are its state.
	// A fixture may install its own; see previewScanner.
	scanner devServerScanner
	// halted is why discovery stopped, once it has. Every error a scan
	// can return comes from the ENUMERATOR — this platform has no way to
	// look, or the socket tables cannot be read — and neither answer
	// changes on a retry. The probe never errors; a port that does not
	// answer is a false verdict, not a failure. So the loop stops on the
	// first one and every later read returns the same sentence rather
	// than an empty list, which would read as "nothing is listening".
	halted error
	// last is the newest published list, so a caller that arrives between
	// ticks is answered without a scan of its own.
	last []devscan.DevServer
}

// startDevServerPreviews starts the discovery loop. Called from Start;
// exits when the life context is cancelled.
func (a *App) startDevServerPreviews() {
	ctx := a.lifeCtx()
	go func() {
		ticker := time.NewTicker(devServerScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !a.devServerScanWanted() {
					continue
				}
				if _, err := a.scanDevServers(ctx); err != nil {
					// Every error a scan can return is the machine saying
					// it cannot be looked at, and that does not change on
					// a retry. Stopping is what makes it an answer rather
					// than a log line repeated every three seconds; the
					// RPC returns the same sentence to whoever asks.
					log.Printf("devscan: dev-server discovery stopped: %v", err)
					return
				}
			}
		}
	}()
}

// devServerScanWanted reports whether anybody off this machine is in a
// position to use the list. Channel SUBSCRIPTION is deliberately not the
// signal: an SPA subscriber takes every channel by default, so it would
// answer yes for the webview sitting in front of the machine.
func (a *App) devServerScanWanted() bool {
	if a.shuttingDown.Load() {
		return false
	}
	bus := a.eventBus.Load()
	if bus == nil {
		return false
	}
	return bus.RemoteReceiverCount(eventchan.DevServerList.String()) > 0
}

// scanDevServers runs one pass and publishes it. The returned list is
// also cached, so an RPC arriving between ticks is answered from it.
func (a *App) scanDevServers(ctx context.Context) ([]devscan.DevServer, error) {
	a.preview.scan.Lock()
	defer a.preview.scan.Unlock()

	if err := a.previewHalted(); err != nil {
		return nil, err
	}

	scanCtx, cancel := context.WithTimeout(ctx, devServerScanTimeout)
	defer cancel()

	servers, err := a.previewScanner().Scan(scanCtx, a.devServerOwners(), a.currentSettings().Network.PreviewPorts)
	if err != nil {
		a.preview.mu.Lock()
		a.preview.halted = err
		a.preview.last = nil
		a.preview.mu.Unlock()
		return nil, err
	}

	a.preview.mu.Lock()
	a.preview.last = servers
	a.preview.mu.Unlock()

	a.emit(eventchan.DevServerList, devscan.DevServerList{
		Servers:     servers,
		PreviewHost: a.previewHost(),
	})
	return servers, nil
}

// devServerScanner is what app_preview.go needs of a scan.
// devscan.Scanner is the one production implementation; the interface
// exists so a fixture can install its own, which is what makes the
// refusal below possible.
type devServerScanner interface {
	Scan(ctx context.Context, owners []devscan.Owner, allowed []int) ([]devscan.DevServer, error)
}

// refusingScanner is what a test binary gets when no fixture installed a
// scanner. A real scan reads this machine's socket tables and then DIALS
// every candidate port on loopback — on a developer's box those ports
// belong to their own work, and on macOS the enumerator shells out
// besides. A fixture that wants list behaviour installs a fake with the
// rows it means to assert.
type refusingScanner struct{}

func (refusingScanner) Scan(context.Context, []devscan.Owner, []int) ([]devscan.DevServer, error) {
	return nil, errors.New(
		"tests must not scan this machine's listening ports; assign app.preview.scanner a fake")
}

// previewScanner returns the App's one scanner, building it on first
// use. An install that never looks at a dev server never allocates it.
//
// testing.Testing() is false in every production process, so the refusal
// costs an ordinary run nothing.
func (a *App) previewScanner() devServerScanner {
	a.preview.mu.Lock()
	defer a.preview.mu.Unlock()
	if a.preview.scanner == nil {
		if testing.Testing() {
			a.preview.scanner = refusingScanner{}
		} else {
			a.preview.scanner = devscan.New()
		}
	}
	return a.preview.scanner
}

// previewHalted returns why discovery stopped, once it has, so the
// answer comes from memory rather than being re-asked of a machine that
// already said no.
func (a *App) previewHalted() error {
	a.preview.mu.Lock()
	defer a.preview.mu.Unlock()
	return a.preview.halted
}

// devServerOwners is the set of processes this app started that a
// listening socket can be traced back to: each thread's provider session
// and each thread's running terminals.
//
// Both spawn paths call procutil.ConfigureGroup, so each of these leads
// its own process group and PGID equals PID. They are carried separately
// anyway, because the two are matched differently: a dev server that
// daemonised has left the ancestor chain and is still in the group.
func (a *App) devServerOwners() []devscan.Owner {
	var owners []devscan.Owner
	if a.sessionRuntime != nil {
		for _, process := range a.sessionRuntime.ThreadProcesses() {
			owners = append(owners, devscan.Owner{
				ThreadID: process.ThreadID,
				PID:      process.PID,
				PGID:     process.PID,
			})
		}
	}
	if a.terminals != nil {
		for _, process := range a.terminals.ThreadProcesses() {
			owners = append(owners, devscan.Owner{
				ThreadID: process.ThreadID,
				PID:      process.PID,
				PGID:     process.PID,
			})
		}
	}
	return owners
}

// previewHost is the authority a preview URL on this machine names. The
// tailnet name wins: it works from anywhere the owner's devices are,
// while the LAN address only works on this network. Empty means this
// backend has no address to share a preview on at all, which the client
// renders as its own sentence rather than as a broken link.
func (a *App) previewHost() string {
	if status := a.tailnetStatus(); status.Running && status.DNSName != "" {
		return status.DNSName
	}
	if a.currentSettings().Network.BindAll {
		return network.DiscoverLocalLANIP()
	}
	return ""
}

// GetDevServers returns this machine's dev-server list.
//
// It scans on demand rather than reading whatever the loop last
// published, because the loop is idle on every install nobody is
// watching this machine from — which includes the ordinary desktop case
// where this call is the only reader there will ever be.
//
// `preview:open`, not a read scope. The list names every loopback port
// on this host that answers like a page, along with the process holding
// each one: that is a port-scan of the machine, and it rides the same
// execute-tier capability as the gateway it feeds.
//
//ao:scope preview:open
//ao:route selected
func (a *App) GetDevServers(ctx context.Context) (devscan.DevServerList, error) {
	servers, err := a.scanDevServers(ctx)
	if err != nil {
		return devscan.DevServerList{}, err
	}
	return devscan.DevServerList{
		Servers:     servers,
		PreviewHost: a.previewHost(),
	}, nil
}

// AllowPreviewPort adds a port to this machine's hand-named preview set,
// so a dev server the scan could not attribute to a thread becomes
// reachable from the owner's other devices. Returns the whole set.
//
// `access:admin` and deliberately no step-up. The act exposes the
// owner's own dev server to the owner's own paired devices, over the
// same authenticated listener everything else here rides; it changes
// nothing about what this backend binds, which is the class of change
// step-up exists for.
//
//ao:scope access:admin
//ao:route selected
func (a *App) AllowPreviewPort(ctx context.Context, port int) ([]int, error) {
	return a.setPreviewPorts(ctx, func(current []int) []int {
		for _, existing := range current {
			if existing == port {
				return current
			}
		}
		return append(current, port)
	})
}

// DisallowPreviewPort removes a port from the hand-named preview set.
// A port the scan attributed to a live thread stays in the list on its
// own account: this call removes the owner's standing choice, and the
// answer says what the set is now.
//
//ao:scope access:admin
//ao:route selected
func (a *App) DisallowPreviewPort(ctx context.Context, port int) ([]int, error) {
	return a.setPreviewPorts(ctx, func(current []int) []int {
		kept := make([]int, 0, len(current))
		for _, existing := range current {
			if existing != port {
				kept = append(kept, existing)
			}
		}
		return kept
	})
}

// setPreviewPorts applies one change to the persisted set and publishes
// a fresh list, so the screen that asked for the change sees its effect
// without waiting for a tick that may never come.
//
// The port is validated by settings.SetNetwork, which is the one write
// path for this key; a refusal is returned verbatim, naming the value.
func (a *App) setPreviewPorts(ctx context.Context, change func([]int) []int) ([]int, error) {
	if a.settings == nil {
		return nil, fmt.Errorf("settings service unavailable")
	}
	stored := a.currentSettings().Network
	next := stored
	next.PreviewPorts = change(append([]int(nil), stored.PreviewPorts...))

	// The write announces itself: the settings service's one change
	// observer emits settings:updated (app_settings_broadcast.go), so
	// every other attached screen converges without a second emit here.
	updated, err := a.settings.SetNetwork(next)
	if err != nil {
		return nil, err
	}

	// Best effort: the set is already persisted, and a scan that fails
	// here is the platform saying it cannot look — which the caller of
	// GetDevServers is told about, and which must not turn a successful
	// write into a failed one.
	if _, scanErr := a.scanDevServers(ctx); scanErr != nil {
		log.Printf("devscan: preview set updated but the list could not be refreshed: %v", scanErr)
	}
	return previewPortsOf(updated.Network), nil
}

// previewPortsOf answers with a non-nil slice, so a client that emptied
// the set receives `[]` rather than `null`.
func previewPortsOf(n settings.NetworkSettings) []int {
	if n.PreviewPorts == nil {
		return []int{}
	}
	return n.PreviewPorts
}
