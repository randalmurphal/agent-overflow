package claudetui

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"time"
)

// hookcmd.go is the client half of the hook relay: a tiny subcommand Claude
// Code runs as every registered hook. It reads the hook payload on stdin,
// forwards it to the session's loopback relay, and writes the relay's response
// to stdout (empty for observe-mode events; the hookSpecificOutput answer for
// AskUserQuestion). main.go short-circuits to RunHookChild before any other
// startup, exactly as it does for the orphan-reaper sidecar.

// HookSubcommand is the argv[1] sentinel that routes to RunHookChild.
const HookSubcommand = "__claude-hook"

// Env vars the session injects into the spawned claude so this subcommand can
// find and authenticate to the relay. Shared by launch.go (writer) and
// RunHookChild (reader).
const (
	envHookURL   = "AO_CLAUDE_HOOK_URL"
	envHookToken = "AO_CLAUDE_HOOK_TOKEN"
)

// hookClientTimeout backstops a relay that never replies. It must exceed the
// relay's answerTimeout (the AskUserQuestion human window) so a legitimately
// slow answer is not cut off; Claude's own configurable hook timeout is the
// tighter bound in practice.
const hookClientTimeout = answerTimeout + time.Minute

// RunHookChild is the entry point for the __claude-hook subcommand. It is
// fail-open by design: any error (missing env, unreachable relay, bad status)
// exits 0 with no stdout, which Claude Code reads as "observe, don't
// interfere" — a relay problem must never wedge the user's agent.
func RunHookChild() {
	url := os.Getenv(envHookURL)
	token := os.Getenv(envHookToken)
	if url == "" || token == "" {
		return
	}

	payload, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set(hookAuthHeader, token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: hookClientTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	// Stream the relay's decision (if any) to stdout for Claude to consume.
	_, _ = io.Copy(os.Stdout, io.LimitReader(resp.Body, 8<<20))
}
