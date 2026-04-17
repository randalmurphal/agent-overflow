// Package testutil provides shared test helpers for Agent Overflow integration
// tests. Mock binary writers here emit NDJSON (Claude) or JSON-RPC (Codex)
// frames from a shell script, letting tests exercise the full provider +
// triage + store pipeline without touching a real CLI.
//
// Helpers that wire up the *App itself live in the test files under package
// main — App is in the main package and cannot be imported here.
package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteMockClaudeInit writes a shell script that mimics `claude --max-turns 0`
// for the zero-token subscription probe. It emits a single system/init line
// with the provided account JSON (or no account field when empty) then waits
// for stdin close before exiting.
//
// Returns the absolute path of the generated script.
func WriteMockClaudeInit(t *testing.T, dir string, accountJSON string) string {
	t.Helper()
	path := filepath.Join(dir, "mock-claude-init.sh")
	initLine := `{"type":"system","subtype":"init","session_id":"probe-s1","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"2.0.0"`
	if strings.TrimSpace(accountJSON) != "" {
		initLine += `,"account":` + accountJSON
	}
	initLine += `}`

	script := "#!/bin/bash\n" +
		"printf '%s\\n' '" + initLine + "'\n" +
		"read -r _ || true\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteMockClaudeInit: %v", err)
	}
	return path
}

// WriteMockClaudeSession writes a shell script that behaves like the Claude
// CLI in a stream-json session: it reads one line from stdin (the user
// message), then emits the provided NDJSON event frames one per line, then
// stays open until stdin is closed so the session readLoop drains cleanly.
//
// Each entry in events should be a complete JSON object (no trailing newline).
// The script single-quotes them via shell-safe escaping.
//
// Returns the absolute path of the generated script.
func WriteMockClaudeSession(t *testing.T, dir string, events []string) string {
	t.Helper()
	path := filepath.Join(dir, "mock-claude-session.sh")

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	// Consume the initial user message.
	b.WriteString("read -r _ || true\n")
	for _, evt := range events {
		b.WriteString("printf '%s\\n' ")
		b.WriteString(shellSingleQuote(evt))
		b.WriteString("\n")
	}
	// Stay alive until stdin closes so sessions can test reconnect/close.
	b.WriteString("while read -r _; do :; done\n")
	b.WriteString("exit 0\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("WriteMockClaudeSession: %v", err)
	}
	return path
}

// WriteMockClaudeScript writes a Claude-like script that emits a sequence of
// pre-baked NDJSON frames in response to each stdin line. The caller supplies
// a slice-of-slices: responses[i] is the batch of event lines emitted after
// the ith stdin line is read. If the caller sends more lines than len(responses)
// the script stays quiet for the extra lines. The script exits when stdin closes.
//
// This is useful for multi-turn tests where the first Send() arrives (user
// message), the mock emits the first batch, then a second Send() arrives and
// the mock emits the second batch, etc.
func WriteMockClaudeScript(t *testing.T, dir string, responses [][]string) string {
	t.Helper()
	path := filepath.Join(dir, "mock-claude-script.sh")

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("idx=0\n")
	b.WriteString("while IFS= read -r _; do\n")
	b.WriteString("  case $idx in\n")
	for i, batch := range responses {
		b.WriteString(fmt.Sprintf("    %d)\n", i))
		for _, line := range batch {
			b.WriteString("      printf '%s\\n' ")
			b.WriteString(shellSingleQuote(line))
			b.WriteString("\n")
		}
		b.WriteString("      ;;\n")
	}
	b.WriteString("    *) : ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("  idx=$((idx+1))\n")
	b.WriteString("done\n")
	b.WriteString("exit 0\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("WriteMockClaudeScript: %v", err)
	}
	return path
}

// WriteMockCodexSession writes a shell script that behaves like `codex
// app-server`: it reads JSON-RPC requests line-by-line and for each
// line matches against the provided key → response map.
//
// The map is keyed by substring probe ("initialize", "thread/start",
// "thread/resume", "turn/start", etc.). The associated value is a single JSON-RPC
// response template with `%d` placeholders where the matching request's
// numeric id should be substituted. The first matching key wins; use an empty
// key "" for a fallback response.
//
// The script exits when stdin closes.
func WriteMockCodexSession(t *testing.T, dir string, responses map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, "mock-codex-session.sh")

	// Generate a stable ordering: user-specified "" fallback goes last.
	var keys []string
	var fallback string
	hasFallback := false
	for k, v := range responses {
		if k == "" {
			fallback = v
			hasFallback = true
			continue
		}
		keys = append(keys, k)
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("while IFS= read -r line; do\n")
	b.WriteString("  id=$(/bin/echo \"$line\" | /usr/bin/grep -o '\"id\":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')\n")
	b.WriteString("  if [ -z \"$id\" ]; then continue; fi\n")
	for _, key := range keys {
		tmpl := responses[key]
		b.WriteString(fmt.Sprintf("  if /bin/echo \"$line\" | /usr/bin/grep -q %s; then\n", shellSingleQuote(key)))
		b.WriteString(fmt.Sprintf("    printf %s \"$id\"\n", shellSingleQuote(tmpl+"\n")))
		b.WriteString("    continue\n")
		b.WriteString("  fi\n")
	}
	if hasFallback {
		b.WriteString(fmt.Sprintf("  printf %s \"$id\"\n", shellSingleQuote(fallback+"\n")))
	}
	b.WriteString("done\n")
	b.WriteString("exit 0\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("WriteMockCodexSession: %v", err)
	}
	return path
}

// WriteMockGhCLI writes a script that mimics `gh` by matching its arguments
// against the provided responses map. Keys are space-joined argument prefixes
// (e.g. "pr view", "pr diff"). When invoked, the script concatenates its
// positional args, looks up the longest matching prefix, and prints the
// associated value. Unmatched invocations exit 1 with "unsupported" on stderr
// so test failures are loud.
func WriteMockGhCLI(t *testing.T, dir string, responses map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, "mock-gh.sh")

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("cmd=\"$*\"\n")
	// Use an if/elif ladder: for each prefix, check "$cmd" starts with it.
	first := true
	for key, body := range responses {
		cond := fmt.Sprintf("[[ \"$cmd\" == %s* ]]", shellSingleQuote(key))
		if first {
			b.WriteString(fmt.Sprintf("if %s; then\n", cond))
			first = false
		} else {
			b.WriteString(fmt.Sprintf("elif %s; then\n", cond))
		}
		// Escape body by rendering via printf %s and passing through single-quoted.
		b.WriteString(fmt.Sprintf("  printf '%%s\\n' %s\n", shellSingleQuote(body)))
		b.WriteString("  exit 0\n")
	}
	if !first {
		b.WriteString("fi\n")
	}
	b.WriteString("echo \"mock-gh: unsupported invocation: $cmd\" 1>&2\n")
	b.WriteString("exit 1\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("WriteMockGhCLI: %v", err)
	}
	return path
}

// CommitSpec describes a single commit in InitGitRepoWithCommits.
type CommitSpec struct {
	Msg   string
	Files map[string]string
}

// InitGitRepoWithCommits creates a git repo at a temp path and applies the
// supplied commits in order. Each commit writes its Files map (relative path
// → contents), stages them, and commits with Msg. Returns the repo path.
func InitGitRepoWithCommits(t *testing.T, commits []CommitSpec) string {
	t.Helper()

	repo := t.TempDir()
	if err := RunGitAllowError(repo, "init", "-b", "main"); err != nil {
		RunGit(t, repo, "init")
		RunGit(t, repo, "checkout", "-b", "main")
	}
	RunGit(t, repo, "config", "user.name", "Agent Overflow")
	RunGit(t, repo, "config", "user.email", "agent-overflow@example.com")

	for i, commit := range commits {
		for relPath, contents := range commit.Files {
			full := filepath.Join(repo, relPath)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("InitGitRepoWithCommits[%d]: mkdir %s: %v", i, full, err)
			}
			if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
				t.Fatalf("InitGitRepoWithCommits[%d]: write %s: %v", i, full, err)
			}
		}
		RunGit(t, repo, "add", "-A")
		msg := commit.Msg
		if msg == "" {
			msg = fmt.Sprintf("commit %d", i)
		}
		RunGit(t, repo, "commit", "-m", msg, "--allow-empty")
	}
	return repo
}

// shellSingleQuote wraps s in single quotes suitable for `bash`, escaping any
// embedded single quotes as `'\''`. The returned value already includes the
// outer quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
