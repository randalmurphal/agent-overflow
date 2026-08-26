// main_soak.go owns the --soak boot path: the LAUNCHER-SHELL isolated
// backend, and the soak preset that runs on top of it.
//
// It is the REAL desktop app — real Windows launcher, real WebView2
// window, real SPA — running against mocked providers on an isolated
// data directory, so it can be driven (or left running for hours) beside
// the developer's own instance without touching their data.
//
// Relationship to --harness: identical isolation (prepareHarness) and
// the same Harness RPC receiver, but a different SHELL. The harness
// prints its own `__AO_HARNESS__` bootstrap line and expects a browser;
// this backend is spawned by the Windows launcher, so it must speak the
// ordinary headless `{port, token}` bootstrap contract (writeBootstrap)
// that wsllauncher parses. `--soak` is the launcher-owned wire name for
// that shell — historical, never typed by a user.
//
// Relationship to the SOAK: `--autopilot` is the one preset this file
// also owns. It seeds two threads and starts a live turn streaming
// background subagent activity indefinitely — the steady state the
// renderer-hang incident needs (docs/architecture/soak-rig.md). Without
// it the instance boots and waits for whoever is driving it, which is
// the ordinary Windows-harness case.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/harness"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harness/scenario"
)

const (
	// soakScenarioName is the embedded library scenario the soak drives:
	// three async local_agent subagents streaming forever, no `result`.
	soakScenarioName = "soak-background-agents"
	// soakScenarioFileName, when present in the soak data ROOT, replaces
	// the embedded scenario. This is the cadence knob that does not need
	// a rebuild: copy the library JSON out, edit its delayMs values, drop
	// it here, restart the soak.
	soakScenarioFileName = "soak-scenario.json"
	// soakProjectName labels the generated workspace under
	// <dataRoot>/workspaces/.
	soakProjectName = "soak-workspace"
	// soakIdleThreadTitle / soakActiveThreadTitle identify the two seeded
	// threads. The active one is looked up by title on a restart, so the
	// soak re-arms its live turn against the same thread instead of
	// growing a new one per boot.
	soakIdleThreadTitle   = "Soak: idle thread"
	soakActiveThreadTitle = "Soak: background agents"
	// soakPrompt is the message that opens the never-completing turn.
	soakPrompt = "Launch three background review agents and let them run."
	// soakArmDelay lets the frontend attach before the live turn starts,
	// so the soak's steady state includes the streaming UI from the first
	// frames rather than a burst of replayed history.
	soakArmDelay = 3 * time.Second
)

// soakDefaultDataRoot / harnessDefaultDataRoot are where a
// launcher-spawned --soak boot puts its data when no --data-dir is
// given, one root per profile. The Windows launcher spells only the
// profile's own flags on the WSL child's argv — it runs on the Windows
// side and cannot know a good Linux path — so the default has to be
// resolvable in-process.
//
// A fixed home-dir root, deliberately NOT the per-worktree rule
// instanceinfo.DataRootFor implements: a launcher boot has no meaningful
// cwd (it starts wherever the interop hop left it, and
// relocateOffWindowsDriveMount moves it again), and an operator asking
// "where is the Windows harness's database" needs one well-known answer.
//
// Both are deliberately outside the OS config root: prepareHarness
// refuses that, and the whole point is that an isolated instance never
// shares a byte with the real install.
func soakDefaultDataRoot() string { return launcherDefaultDataRoot("agent-overflow-soak") }

func harnessDefaultDataRoot() string { return launcherDefaultDataRoot("agent-overflow-harness") }

func launcherDefaultDataRoot(name string) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(os.TempDir(), name)
	}
	return filepath.Join(home, "."+name)
}

// The per-worktree default data root a hand-started isolated boot uses
// (`make harness`, `make soak-window`, `ao-harness up`) lives in
// internal/harness/instanceinfo — the backend, the Makefile and the CLI
// all have to name the same directory for a checkout, and the CLI cannot
// import package main. See instanceinfo.DataRootFor.

// isolatedBootMode is what an isolated boot calls itself: what discovery
// files stamp, what a window title shows, and what an operator greps the
// log for. "soak" means the autopilot is RUNNING — never merely that the
// launcher spawned this backend — because the mode is how a reader tells
// an instance that is driving itself from one waiting to be driven.
func isolatedBootMode(flags cliFlags) instanceinfo.Mode {
	if flags.autopilot {
		return instanceinfo.ModeSoak
	}
	return instanceinfo.ModeHarness
}

// runSoak boots the launcher-shell isolated backend: harness-grade
// isolation, the headless bootstrap channel the Windows launcher parses,
// and — only with --autopilot — the soak preset that puts the app into
// its streaming steady state.
func runSoak(flags cliFlags) {
	mode := isolatedBootMode(flags)
	label := string(mode)

	if flags.window {
		requireWindowedBuild()
	}
	paths, err := prepareHarness(flags)
	if err != nil {
		fatalf("%s: %v", label, err)
	}
	paths.AssetsFreshness = warnIfEmbeddedDistStale()
	if flags.window {
		// After prepareHarness, before the first GLib call — see
		// isolateWebviewStorage for why both ends of that window matter.
		isolateWebviewStorage(paths.DataRoot)
	}

	appService := newIsolatedProviderApp(paths)
	h := newHarness(appService, paths)
	// Before App.Start, exactly as in harness mode: the control server
	// publishes its address/token through providerExtraEnv (write-once
	// before Start) and the autopilot's first send spawns a mock that
	// must find them.
	if err := h.startControl(); err != nil {
		fatalf("%s: start mock control server: %v", label, err)
	}
	defer h.shutdownControl()

	// The Harness receiver rides along (LocalOnly, same as --harness) so
	// this instance is inspectable with the tools an agent already
	// has — HarnessInfo for evidence paths, HarnessListMocks, replay
	// capture — without a second control surface.
	srv := bootTransport(appService, flags.listenAddr, bootTransportOptions{
		RequireReadyForBootstrap: true,
		HarnessReceiver:          h,
		// Same opt-in as --harness: booting this shell is already an
		// explicit operator act on an isolated data root, so
		// FRONTEND_DEVSERVER_URL is honoured here even in a
		// production-stamped binary. Without this, `ao-harness up --soak
		// --dev-assets` sets the variable and nothing reads it.
		AllowDevServerAssets: true,
	})
	log.Printf("%s: data dir %s (mock provider %s)", label, paths.DataDir, paths.MockProvider)

	if err := writeBootstrap(flags.printURLFD, srv); err != nil {
		shutdownHeadless(appService, srv)
		fatalf("%s: write bootstrap: %v", label, err)
	}
	// Same stdout-hygiene contract as runHeadless: the launcher parses
	// stdout for the bootstrap sentinel and nothing else.
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)

	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	if err := appService.Start(bootCtx); err != nil {
		log.Printf("app: service startup: %v", err)
		srv.MarkStartupFailed()
		log.Printf("%s: startup failed; serving terminal bootstrap failure until shutdown", label)
		waitForHeadlessShutdown(appService, srv)
		return
	}
	srv.MarkReady()

	// Same contract as the harness: discovery files describe an instance
	// that is ready to attach, so they land after MarkReady and go away on
	// a graceful exit. This is exactly the instance a tool wants to attach
	// to hours later, when nobody still has its stdout — and the one whose
	// launcher pid a teardown needs.
	instance := publishInstance(srv, paths, mode, flags.window, flags.launcherPID)
	defer instance.remove()

	if flags.autopilot {
		// Latched BEFORE the goroutine starts, so there is no window in
		// which a --autopilot instance reports "off".
		h.setSoakAutopilot(soakAutopilotArming)
		// Off the boot goroutine: the autopilot waits for the window to
		// attach and then blocks on a live session start. A soak whose
		// autopilot fails still serves — the operator sees the error in
		// launcher-soak.log and can drive the app by hand.
		go func() {
			select {
			case <-bootCtx.Done():
				return
			case <-time.After(soakArmDelay):
			}
			// The outcome is latched as well as logged: publishInstance has
			// already stamped this instance ModeSoak, so without the latch a
			// soak that never armed is indistinguishable from a working one
			// to everything except whoever is tailing launcher-soak.log.
			if err := h.armSoakSteadyState(); err != nil {
				log.Printf("soak: arm steady state: %v", err)
				h.setSoakAutopilot(soakAutopilotFailedPrefix + err.Error())
				return
			}
			h.setSoakAutopilot(soakAutopilotArmed)
		}()
	}

	if flags.window {
		// Any autopilot goroutine above keeps running: --window changes
		// only who hosts the window (this process instead of the Windows
		// launcher), never what the instance drives.
		if err := runWindowedShell(appService, srv, isolatedWindowTitle(mode, instance.id)); err != nil {
			instance.remove()
			h.shutdownControl()
			fatalf("%s: %v", label, err)
		}
		return
	}
	waitForHeadlessShutdown(appService, srv)
}

// armSoakSteadyState puts the app into the shape the incident occurred
// in: one idle thread, plus one thread whose turn is streaming three
// background subagents indefinitely.
//
// Idempotent across restarts by design. Fixtures are seeded only when
// the store is empty, and the live turn is re-armed on the SAME thread
// every boot, so a soak that is restarted ten times still has two
// threads — the DB is the one thing a long soak accumulates, and a fresh
// project per boot would make it accumulate faster than the streaming
// does.
func (h *Harness) armSoakSteadyState() error {
	if err := h.installSoakScenario(); err != nil {
		return err
	}
	threadID, err := h.ensureSoakThreads()
	if err != nil {
		return err
	}
	log.Printf("soak: arming live background-agent turn on thread %s", threadID)
	if err := h.app.SendMessage(threadID, soakPrompt, nil); err != nil {
		return fmt.Errorf("send soak prompt: %w", err)
	}
	return nil
}

// installSoakScenario points every Claude mock at the soak script: the
// operator's override file when one exists, otherwise the embedded
// library entry. Validation is HarnessSetScenario's, so a hand-edited
// override fails here — at boot, in launcher.log — rather than as frames
// that never arrive.
func (h *Harness) installSoakScenario() error {
	spec := HarnessScenarioSpec{Name: soakScenarioName}
	override := filepath.Join(h.paths.DataRoot, soakScenarioFileName)
	switch raw, err := os.ReadFile(override); {
	case err == nil:
		log.Printf("soak: using scenario override %s", override)
		spec = HarnessScenarioSpec{Scenario: json.RawMessage(raw)}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read scenario override %s: %w", override, err)
	}
	if _, err := h.HarnessSetScenario(spec); err != nil {
		return fmt.Errorf("install soak scenario: %w", err)
	}
	return nil
}

// ensureSoakThreads returns the id of the thread the live turn runs on,
// seeding the two-thread fixture first when the store is empty.
func (h *Harness) ensureSoakThreads() (string, error) {
	threads, err := h.app.ListThreads()
	if err != nil {
		return "", fmt.Errorf("list threads: %w", err)
	}
	for _, t := range threads {
		if t.Title == soakActiveThreadTitle {
			return t.ID, nil
		}
	}
	if len(threads) > 0 {
		// Somebody drove this data dir by hand. Re-arming a thread we did
		// not seed would send an unexpected prompt into their work, so
		// say so and stop.
		return "", fmt.Errorf(
			"soak data dir holds %d thread(s) but none titled %q; "+
				"delete the data root to reseed, or start the turn by hand",
			len(threads), soakActiveThreadTitle)
	}

	result, err := h.seed(HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: soakProjectName,
		Repo: &harness.RepoSpec{Commits: []harness.CommitSpec{{
			Message: "soak workspace",
			Files: map[string]string{
				"README.md": "# soak workspace\n\nGenerated by the soak rig (docs/architecture/soak-rig.md).\n",
			},
		}}},
		Threads: []HarnessSeedThread{
			{
				Title:    soakIdleThreadTitle,
				Provider: "claude",
				Turns: []HarnessSeedTurn{{
					UserText: "What does this workspace contain?",
					Items: []HarnessSeedItem{{
						Kind:    "assistant_text",
						Summary: "One README. Nothing is running in this thread — it is the idle half of the soak fixture.",
					}},
				}},
			},
			{
				Title:    soakActiveThreadTitle,
				Provider: "claude",
				Turns: []HarnessSeedTurn{{
					UserText: "Ready when you are.",
					Items: []HarnessSeedItem{{
						Kind:    "assistant_text",
						Summary: "Standing by. Send the word and I will fan out three background agents.",
					}},
				}},
			},
		},
	}}})
	if err != nil {
		return "", fmt.Errorf("seed soak fixtures: %w", err)
	}
	if len(result.Projects) != 1 || len(result.Projects[0].ThreadIDs) != 2 {
		return "", fmt.Errorf("seed soak fixtures: unexpected result %+v", result)
	}
	// Seed order is spec order: idle first, active second.
	return result.Projects[0].ThreadIDs[1], nil
}

// soakScenarioIsShipped is referenced by the soak's own test to prove the
// named scenario exists in the embedded library — a rename there would
// otherwise only surface as a failed arm hours into a run.
func soakScenarioIsShipped() error {
	_, s, err := scenario.LoadLibrary(soakScenarioName)
	if err != nil {
		return err
	}
	if s.Provider != scenario.ProviderClaude {
		return fmt.Errorf("scenario %q is a %s script, want claude", soakScenarioName, s.Provider)
	}
	return nil
}
