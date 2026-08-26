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
	"agent-overflow/internal/providerschema"
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

	// `codex exec --ephemeral` is checked before the protocol sniff below,
	// which reads every argv without `app-server` as Claude — a one-shot Codex
	// text-generation run carries no such marker (see textgen.go).
	if isCodexTextGenInvocation(args) {
		runCodexTextGen(args)
		return
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

	// Same for one-shot text generation (`claude -p --output-format json`):
	// a prompt in, one structured answer out, no turn lifecycle.
	if protocol == scenario.ProviderClaude && isClaudeTextGenInvocation(args) {
		runClaudeTextGen(args)
		return
	}

	// Both real CLIs validate a structured-output schema before the turn runs
	// and exit non-zero when it breaks strict mode. Mirroring that here is what
	// keeps the harness honest: a mock that accepts any schema lets a workflow
	// suite pass green while every real provider run would fail at spawn.
	if schema := flagValue(args, "--json-schema"); schema != "" {
		rejectInvalidOutputSchema("--json-schema", []byte(schema))
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

	// Claude's permission configuration is entirely argv, so it is
	// observable the moment the process starts. Codex's arrives later on
	// thread/start and is reported from the adapter.
	if protocol == scenario.ProviderClaude {
		rep.report(control.Report{
			Kind:          control.ReportSessionConfig,
			SessionConfig: claudeSessionConfig(args),
		})
	}

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

// rejectInvalidOutputSchema exits the way a real CLI does when it is handed a
// structured-output schema it cannot accept, naming every broken rule so the
// harness failure points at the generator instead of at a mystery timeout.
func rejectInvalidOutputSchema(source string, schema []byte) {
	rejectSchema(source, providerschema.Validate(schema))
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

// flagValues returns every argument following each occurrence of flag, in
// argv order. `--disallowedTools` is repeated once per tool by buildArgs.
func flagValues(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// claudeSessionConfig reads the permission configuration out of the argv the
// app spawned this mock with. This is the harness's view of what AO asked the
// Claude CLI for — the mapping under test in
// internal/provider/claude/options.go.
func claudeSessionConfig(args []string) *control.SessionConfig {
	return &control.SessionConfig{
		PermissionMode:  flagValue(args, "--permission-mode"),
		DisallowedTools: flagValues(args, "--disallowedTools"),
	}
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
