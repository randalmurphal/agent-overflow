package testutil

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWriteMockClaudeInitEmitsInitLine sanity-checks the init-script helper:
// running the script with no stdin input should emit a valid system/init
// JSON line and then exit when stdin closes.
func TestWriteMockClaudeInitEmitsInitLine(t *testing.T) {
	t.Parallel()

	path := WriteMockClaudeInit(t, t.TempDir(),
		`{"subscriptionType":"max_20x","tokenSource":"oauth","apiProvider":"anthropic"}`)

	cmd := exec.Command(path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	reader := bufio.NewReader(stdout)
	out := readLineWithTimeout(t, reader, 3*time.Second)
	if !strings.Contains(out, `"subtype":"init"`) {
		t.Fatalf("first line = %q, want system/init", out)
	}
	if !strings.Contains(out, `"max_20x"`) {
		t.Fatalf("init line missing account: %q", out)
	}
	_ = stdin.Close()
}

// TestWriteMockClaudeSessionEmitsAllEvents verifies that after the initial
// user message is consumed, the script emits every event line in order.
func TestWriteMockClaudeSessionEmitsAllEvents(t *testing.T) {
	t.Parallel()

	events := []string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"opus","cwd":"/tmp","tools":[],"claude_code_version":"1"}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Hi"}]}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}
	path := WriteMockClaudeSession(t, t.TempDir(), events)

	cmd := exec.Command(path)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	// Send a user message so the script's initial read completes.
	if _, err := stdin.Write([]byte("user-hello\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	reader := bufio.NewReader(stdout)
	for i, want := range events {
		line := readLineWithTimeout(t, reader, 3*time.Second)
		if line != want {
			t.Fatalf("event[%d] = %q, want %q", i, line, want)
		}
	}
	_ = stdin.Close()
}

// TestWriteMockCodexSessionRespondsToInitialize verifies the mock responds
// with a matching id for the initialize JSON-RPC request.
func TestWriteMockCodexSessionRespondsToInitialize(t *testing.T) {
	t.Parallel()

	path := WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:    `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`:  `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"t-1"}}}`,
		`"method":"thread/resume"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"t-1"}}}`,
	})

	cmd := exec.Command(path)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	// Send an initialize request.
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	if _, err := stdin.Write([]byte(req)); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	reader := bufio.NewReader(stdout)
	line := readLineWithTimeout(t, reader, 3*time.Second)
	if !strings.Contains(line, `"id":1`) {
		t.Fatalf("response = %q, missing id=1", line)
	}
	if !strings.Contains(line, `"result":{}`) {
		t.Fatalf("response = %q, expected empty result", line)
	}

	req2 := `{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n"
	if _, err := stdin.Write([]byte(req2)); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	line = readLineWithTimeout(t, reader, 3*time.Second)
	if !strings.Contains(line, `"id":2`) || !strings.Contains(line, `"t-1"`) {
		t.Fatalf("thread/start response = %q", line)
	}

	_ = stdin.Close()
}

// TestWriteMockGhCLIResolvesMatchingSubcommand checks the gh mock matches on
// argument prefixes.
func TestWriteMockGhCLIResolvesMatchingSubcommand(t *testing.T) {
	t.Parallel()

	path := WriteMockGhCLI(t, t.TempDir(), map[string]string{
		"pr view":  `{"number":42,"title":"Test PR"}`,
		"pr list":  `[{"number":42}]`,
	})

	out, err := exec.Command(path, "pr", "view", "42", "--json", "number,title").Output()
	if err != nil {
		t.Fatalf("gh pr view: %v", err)
	}
	if !strings.Contains(string(out), `"number":42`) {
		t.Fatalf("pr view output = %q", string(out))
	}

	// Mismatched subcommand should fail.
	err = exec.Command(path, "repo", "view").Run()
	if err == nil {
		t.Fatal("expected failure for unsupported invocation")
	}
}

// TestInitGitRepoWithCommitsProducesExpectedHistory verifies commits apply in
// order, tree contents match, and HEAD points to the final commit.
func TestInitGitRepoWithCommitsProducesExpectedHistory(t *testing.T) {
	t.Parallel()

	repo := InitGitRepoWithCommits(t, []CommitSpec{
		{Msg: "first", Files: map[string]string{"a.txt": "one"}},
		{Msg: "second", Files: map[string]string{"b.txt": "two"}},
	})

	data, err := os.ReadFile(repo + "/a.txt")
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(data) != "one" {
		t.Fatalf("a.txt = %q, want 'one'", string(data))
	}

	log, err := exec.Command("git", "-C", repo, "log", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	// Newest commit first.
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) < 2 {
		t.Fatalf("git log = %v, want >=2 commits", lines)
	}
	if lines[0] != "second" {
		t.Fatalf("HEAD commit = %q, want 'second'", lines[0])
	}
}

func readLineWithTimeout(t *testing.T, r *bufio.Reader, d time.Duration) string {
	t.Helper()
	ch := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := r.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		ch <- strings.TrimRight(line, "\n")
	}()
	select {
	case line := <-ch:
		return line
	case err := <-errCh:
		t.Fatalf("read: %v", err)
		return ""
	case <-time.After(d):
		t.Fatal("timed out reading stdout")
		return ""
	}
}
