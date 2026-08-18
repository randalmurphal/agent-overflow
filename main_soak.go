// main_soak.go owns the --soak boot path: the soak rig's backend half.
//
// A soak run is the REAL desktop app — real Windows launcher, real
// WebView2 window, real SPA — driven for hours against mocked providers
// on an isolated data directory, so a rendering/hang reproduction can
// run beside the developer's own instance without touching their data.
// See docs/architecture/soak-rig.md.
//
// Relationship to --harness: identical isolation (prepareHarness) and
// the same Harness RPC receiver, but a different SHELL. The harness
// prints its own `__AO_HARNESS__` bootstrap line and expects a browser;
// the soak backend is spawned by the Windows launcher, so it must speak
// the ordinary headless `{port, token}` bootstrap contract
// (writeBootstrap) that wsllauncher parses. It also drives itself: the
// steady state the incident needs — a live turn streaming background
// subagent activity indefinitely — is seeded and started at boot rather
// than by a test script.
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

// soakDefaultDataRoot is where a --soak boot puts its data when no
// --data-dir is given. The Windows launcher spells only `--soak` on the
// WSL child's argv — it runs on the Windows side and cannot know a good
// Linux path — so the default has to be resolvable in-process.
//
// It is deliberately NOT under the OS config root: prepareHarness
// refuses that, and the whole point is that a soak never shares a byte
// with the real install.
func soakDefaultDataRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(os.TempDir(), "agent-overflow-soak")
	}
	return filepath.Join(home, ".agent-overflow-soak")
}

// runSoak boots the soak backend: harness-grade isolation, the headless
// bootstrap channel the Windows launcher parses, and the autopilot that
// puts the app into its steady state.
func runSoak(flags cliFlags) {
	paths, err := prepareHarness(flags)
	if err != nil {
		fatalf("soak: %v", err)
	}

	appService := newIsolatedProviderApp(paths, "the soak rig has no OS notification presenter")
	h := newHarness(appService, paths)
	// Before App.Start, exactly as in harness mode: the control server
	// publishes its address/token through providerExtraEnv (write-once
	// before Start) and the autopilot's first send spawns a mock that
	// must find them.
	if err := h.startControl(); err != nil {
		fatalf("soak: start mock control server: %v", err)
	}
	defer h.shutdownControl()

	// The Harness receiver rides along (LocalOnly, same as --harness) so
	// a soak instance is inspectable with the tools an agent already
	// has — HarnessInfo for evidence paths, HarnessListMocks, replay
	// capture — without a second control surface.
	srv := bootTransport(appService, flags.listenAddr, bootTransportOptions{
		RequireReadyForBootstrap: true,
		HarnessReceiver:          h,
	})
	log.Printf("soak: data dir %s (mock provider %s)", paths.DataDir, paths.MockProvider)

	if err := writeBootstrap(flags.printURLFD, srv); err != nil {
		shutdownHeadless(appService, srv)
		fatalf("soak: write bootstrap: %v", err)
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
		log.Printf("soak: startup failed; serving terminal bootstrap failure until shutdown")
		waitForHeadlessShutdown(appService, srv)
		return
	}
	srv.MarkReady()

	// Off the boot goroutine: the autopilot waits for the window to
	// attach and then blocks on a live session start. A soak whose
	// autopilot fails still serves — the operator sees the error in
	// launcher.log and can drive the app by hand.
	go func() {
		select {
		case <-bootCtx.Done():
			return
		case <-time.After(soakArmDelay):
		}
		if err := h.armSoakSteadyState(); err != nil {
			log.Printf("soak: arm steady state: %v", err)
		}
	}()

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

	result, err := h.HarnessSeed(HarnessSeedSpec{Projects: []HarnessSeedProject{{
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
