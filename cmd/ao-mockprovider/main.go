// ao-mockprovider impersonates both provider CLIs the app spawns —
// Claude Code (NDJSON stream-json over stdio) and the Codex app-server
// (JSON-RPC 2.0 over stdio) — for the agent test harness. Which
// protocol to speak is sniffed from argv (the app passes "app-server"
// only when spawning Codex). Behaviour is driven by a scenario script
// (internal/harness/scenario) acquired from the harness control
// channel (internal/harness/control), from AO_MOCK_SCENARIO_FILE, or
// from a built-in fallback so the app never hangs on a silent mock.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("ao-mockprovider: ")

	args := os.Args[1:]
	if slices.Contains(args, "--version") {
		fmt.Println(versionString)
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("getwd: %v", err)
	}

	protocol := scenario.ProviderClaude
	if slices.Contains(args, "app-server") {
		protocol = scenario.ProviderCodex
	}

	// The app's Claude account probe (`--max-turns 0`, see
	// internal/provider/claude/probe.go) is a short-lived side
	// invocation, not a session — serve it without registering a mock.
	if protocol == scenario.ProviderClaude && isClaudeProbeInvocation(args) {
		runClaudeProbe(cwd)
		return
	}

	resumeRef := flagValue(args, "--resume")
	src := acquireScenario(protocol, cwd, resumeRef)

	w := newLineWriter(os.Stdout)
	rep := &reporter{client: src.client}

	base := scenario.Vars{"CWD": cwd}
	switch protocol {
	case scenario.ProviderClaude:
		sessionID := resumeRef
		if sessionID == "" {
			sessionID = "mock-claude-" + randomHex(4)
		}
		base["SESSION_ID"] = sessionID
	case scenario.ProviderCodex:
		threadID := "mock-codex-thread"
		if src.sc.Codex != nil && src.sc.Codex.ThreadID != "" {
			threadID = src.sc.Codex.ThreadID
		}
		base["THREAD_ID"] = threadID
		base["SESSION_ID"] = threadID
	}

	e := newEngine(src.sc, src.fixtureRoot, cwd, w, rep, base)
	installSignalHandler(e)

	if src.client != nil {
		go src.client.Poll(context.Background(), e.handleCommand)
	}

	var adapter interface {
		protocolAdapter
		readStdin()
	}
	switch protocol {
	case scenario.ProviderClaude:
		adapter = newClaudeAdapter(e, w)
	case scenario.ProviderCodex:
		adapter = newCodexAdapter(e, w, src.sc.Codex)
	}
	e.adapter = adapter
	go e.run()
	adapter.readStdin() // blocks until stdin EOF, then terminates the process
}

// isClaudeProbeInvocation matches the account probe's argv. A regular
// session never carries `--max-turns 0` (buildArgs only appends the
// flag for MaxTurns > 0), so the pair is an unambiguous discriminator.
func isClaudeProbeInvocation(args []string) bool {
	return flagValue(args, "--max-turns") == "0"
}

// flagValue returns the argument following the given flag, or "".
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("generate random id: %v", err)
	}
	return hex.EncodeToString(buf)
}

// installSignalHandler makes SIGTERM/SIGINT a clean exit — the app
// tears mocks down like real providers and a signal death must not
// read as a provider crash.
func installSignalHandler(e *engine) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		sig := <-ch
		log.Printf("received %s; exiting", sig)
		e.terminate(0)
	}()
}

// reporter is a nil-safe wrapper around the optional control client so
// engine code can report unconditionally.
type reporter struct {
	client *control.Client
}

func (r *reporter) report(rep control.Report) {
	if r != nil && r.client != nil {
		r.client.Report(rep)
	}
}
