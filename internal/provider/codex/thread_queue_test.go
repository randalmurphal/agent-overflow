package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// queueFakeScript builds a fake app-server that answers initialize with the
// given userAgent, starts a thread, records every `thread/queue/*` request
// frame it sees to capturePath (one JSON line per request), and answers each
// method from replies[method].
//
// `thread/start` echoes back the `approvalsReviewer` it was sent, the way a
// supported app-server does — the handshake VERIFIES that echo
// (verifyApprovalsReviewerEcho), so a mock answering with a fixed body could
// not host a session in the auto runtime mode at all.
func queueFakeScript(t *testing.T, userAgent, threadID, capturePath string, replies map[string]string) string {
	t.Helper()
	return queueFakeScriptWithErrors(t, userAgent, threadID, capturePath, replies, nil)
}

// queueFakeScriptWithErrors is queueFakeScript plus methods answered with a
// JSON-RPC error. Used for the refusal paths, where "the request failed" has
// to be distinguishable from "the server answered {}".
func queueFakeScriptWithErrors(
	t *testing.T, userAgent, threadID, capturePath string, replies, failures map[string]string,
) string {
	t.Helper()
	var branches strings.Builder
	for method, message := range failures {
		fmt.Fprintf(&branches, `    if echo "$line" | grep -q '"method":"%s"'; then
        echo "$line" >> %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"error\":{\"code\":-32600,\"message\":%s}}"
        continue
    fi
`, method, capturePath, bashJSON(fmt.Sprintf("%q", message)))
	}
	for method, reply := range replies {
		fmt.Fprintf(&branches, `    if echo "$line" | grep -q '"method":"%s"'; then
        echo "$line" >> %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
`, method, capturePath, bashJSON(reply))
	}
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        reviewer=$(echo "$line" | grep -o '"approvalsReviewer":"[^"]*"' | head -1 | cut -d'"' -f4)
        if [ -z "$reviewer" ]; then
            reviewer="user"
        fi
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"%s\"},\"approvalsReviewer\":\"$reviewer\"}}"
        continue
    fi
%s    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
done
`,
		bashJSON(fmt.Sprintf(`{"userAgent":%q}`, userAgent)),
		threadID,
		branches.String(),
	)
	path := t.TempDir() + "/codex"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

func newQueueSession(t *testing.T, binary string, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	return newQueueSessionWithConfig(t, binary, onEvent, nil)
}

// newQueueSessionWithConfig builds the queue-test session, letting a caller
// fill in the Config axes a case needs — the resume id and BeforeResume hook,
// or the OwnsQueuedClientID predicate the foreign-submission notice reads.
func newQueueSessionWithConfig(
	t *testing.T, binary string, onEvent func(provider.ProviderEvent), customize func(*Config),
) *Session {
	t.Helper()
	if onEvent == nil {
		onEvent = func(provider.ProviderEvent) {}
	}
	cfg := Config{
		Binary:  binary,
		Model:   "test-model",
		WorkDir: "/tmp",
	}
	if customize != nil {
		customize(&cfg)
	}
	s, err := NewSession(context.Background(), testThread, cfg, onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func capturedRequests(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode captured request %q: %v", line, err)
		}
		out = append(out, frame)
	}
	return out
}

func requestParams(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	params, ok := frame["params"].(map[string]any)
	if !ok {
		t.Fatalf("request has no params object: %+v", frame)
	}
	return params
}

// TestThreadQueueIsGatedOnTheHandshakeVersion is the whole version-gating
// decision in one test: the flag is frozen at handshake time off the
// app-server's own userAgent, and every method refuses rather than sending a
// request an older app-server would answer with invalid_params.
func TestThreadQueueIsGatedOnTheHandshakeVersion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		userAgent string
		native    bool
	}{
		{"at the floor", "codex_cli_rs/0.148.0 (Ubuntu 24.04; x86_64) codex_cli_rs/0.148.0", true},
		{"above the floor", "codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) codex_cli_rs/0.149.0", true},
		{"below the floor", "codex_cli_rs/0.147.0 (Ubuntu 24.04; x86_64) codex_cli_rs/0.147.0", false},
		{"no userAgent at all", "", false},
		{"unparseable", "who knows", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := t.TempDir() + "/queue-requests.jsonl"
			binary := queueFakeScript(t, tc.userAgent, "codex-thread-q", capture, nil)
			s := newQueueSession(t, binary, nil)

			if got := s.ThreadQueueNative(); got != tc.native {
				t.Fatalf("ThreadQueueNative() = %v, want %v for userAgent %q", got, tc.native, tc.userAgent)
			}
			if tc.native {
				return
			}
			// Every entry point refuses with the same typed sentinel, so the
			// caller has one thing to branch on.
			if _, err := s.QueueList(context.Background()); !IsThreadQueueUnsupported(err) {
				t.Errorf("QueueList err = %v, want ErrThreadQueueUnsupported", err)
			}
			if _, err := s.QueueDelete(context.Background(), "sub-1"); !IsThreadQueueUnsupported(err) {
				t.Errorf("QueueDelete err = %v, want ErrThreadQueueUnsupported", err)
			}
			if _, err := s.PurgeQueue(context.Background()); !IsThreadQueueUnsupported(err) {
				t.Errorf("PurgeQueue err = %v, want ErrThreadQueueUnsupported", err)
			}
			// A refusal must not have put anything on the wire.
			if _, err := os.Stat(capture); !os.IsNotExist(err) {
				t.Errorf("a gated-off session still sent a thread/queue request: %v", err)
			}
		})
	}
}

// TestQueueDeleteReportsTheMatchedState pins that `deleted:false` is a STATE
// (the row was already dispatched or already gone), not an error.
func TestQueueDeleteReportsTheMatchedState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		want  bool
	}{
		{"matched", `{"deleted":true}`, true},
		{"matched nothing", `{"deleted":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := t.TempDir() + "/queue-requests.jsonl"
			binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
				"thread/queue/delete": tc.reply,
			})
			s := newQueueSession(t, binary, nil)

			deleted, err := s.QueueDelete(context.Background(), "sub-7")
			if err != nil {
				t.Fatalf("QueueDelete: %v", err)
			}
			if deleted != tc.want {
				t.Fatalf("deleted = %v, want %v", deleted, tc.want)
			}
			params := requestParams(t, capturedRequests(t, capture)[0])
			if params["queuedSubmissionId"] != "sub-7" {
				t.Errorf("queuedSubmissionId = %v, want sub-7 (upstream's spelling)", params["queuedSubmissionId"])
			}
		})
	}
}

// TestQueueListReportsAPrefixAsIncompleteNotAsTheWholeQueue covers both
// non-terminating conditions: a server echoing the same cursor forever cannot
// spin the loop, and what the walk has in hand when it stops is a PREFIX.
//
// Reporting that prefix with a nil error is the shape that corrupts both
// callers — PurgeQueue would report a complete rollback purge over a partial
// list (leaving a rolled-back message armed to re-run on the next resume) and
// the resume re-arm would omit the rows past the cut, stamping the user's own
// prompts `external-queue`. The rows still come back, because a prefix is
// useful to a caller that only wants to recognise what it can see; what must
// not come back is "this is the queue".
func TestQueueListReportsAPrefixAsIncompleteNotAsTheWholeQueue(t *testing.T) {
	capture := t.TempDir() + "/queue-requests.jsonl"
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		"thread/queue/list": `{"data":[{"id":"sub-1","input":[{"type":"text","text":"one"}],` +
			`"clientUserMessageId":"user:1"}],"nextCursor":"stuck"}`,
	})
	s := newQueueSession(t, binary, nil)

	items, err := s.QueueList(context.Background())
	if !IsThreadQueueListIncomplete(err) {
		t.Fatalf("QueueList error = %v, want ErrThreadQueueListIncomplete", err)
	}
	// Page 1 advances onto "stuck"; page 2 sees the same cursor and stops.
	if len(items) != 2 {
		t.Fatalf("got %d submissions, want 2 (one per page before the repeat stops the walk)", len(items))
	}
	if got := len(capturedRequests(t, capture)); got > queueListPageCap {
		t.Fatalf("list sent %d requests, cap is %d", got, queueListPageCap)
	}
	if items[0].ID != "sub-1" || items[0].Text != "one" {
		t.Fatalf("submission = %+v, want the wire row flattened", items[0])
	}
}

// TestPurgeQueueRefusesToReportACompletePurgeOverAPartialList is the purge
// half of the same rule. The rollback purge exists so a message the user just
// truncated cannot re-run on the next resume; a purge that deletes a prefix
// and answers "done" leaves exactly that hazard armed with nothing saying so.
func TestPurgeQueueRefusesToReportACompletePurgeOverAPartialList(t *testing.T) {
	capture := t.TempDir() + "/queue-requests.jsonl"
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		"thread/queue/list": `{"data":[{"id":"sub-1","input":[{"type":"text","text":"one"}],` +
			`"clientUserMessageId":"user:1"}],"nextCursor":"stuck"}`,
		"thread/queue/delete": `{"deleted":true}`,
	})
	s := newQueueSession(t, binary, nil)

	purge, err := s.PurgeQueue(context.Background())
	if err == nil {
		t.Fatal("PurgeQueue reported success over a list that never reached the end of the queue")
	}
	if !IsThreadQueueListIncomplete(err) {
		t.Fatalf("PurgeQueue error = %v, want it to carry ErrThreadQueueListIncomplete", err)
	}
	// Best-effort is still best-effort: what it COULD see is gone.
	if len(purge.Deleted) == 0 {
		t.Error("PurgeQueue deleted nothing; a partial list is still worth purging")
	}
}

// TestQueueChangedIsSilentForASubmissionTheAppOwns is the notification half of
// the reconcile. A `thread/queue/changed` says only `{threadId}`, so the
// notice has to be evidence-driven — and the evidence is ownership, which only
// the app layer can answer. A row its store accounts for must not be announced
// to the user as having come from outside Agent Overflow.
func TestQueueChangedIsSilentForASubmissionTheAppOwns(t *testing.T) {
	capture := t.TempDir() + "/queue-requests.jsonl"
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		"thread/queue/list": `{"data":[{"id":"sub-7","input":[{"type":"text","text":"hi"}],` +
			`"clientUserMessageId":"user:1"}],"nextCursor":null}`,
	})
	var mu sync.Mutex
	notices := 0
	var asked []string
	s := newQueueSessionWithConfig(t, binary, func(evt provider.ProviderEvent) {
		if evt.Kind != provider.EventNotification {
			return
		}
		if got, ok := metaValue(t, evt.Meta, "kind"); ok && got == "external_queue" {
			mu.Lock()
			notices++
			mu.Unlock()
		}
	}, func(cfg *Config) {
		cfg.OwnsQueuedClientID = func(clientID string) bool {
			mu.Lock()
			asked = append(asked, clientID)
			mu.Unlock()
			return clientID == "user:1"
		}
	})

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/queue/changed","params":{"threadId":"codex-thread-q"}}`))

	// The reconcile lists asynchronously; give it room to reach a verdict.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen, askedCount := notices, len(asked)
		mu.Unlock()
		if seen > 0 {
			t.Fatal("a submission the app layer owns raised an 'external queue' notice")
		}
		if askedCount > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || asked[0] != "user:1" {
		t.Fatalf("ownership predicate saw %v, want the listed row's clientUserMessageId", asked)
	}
}

// TestQueueChangedWithNoOwnershipPredicateReportsEverySubmission pins the
// default. This package never writes to the provider's queue, so a session
// given no way to claim a row has no claim to make: every submission is
// somebody else's until the app layer says otherwise.
func TestQueueChangedWithNoOwnershipPredicateReportsEverySubmission(t *testing.T) {
	capture := t.TempDir() + "/queue-requests.jsonl"
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		"thread/queue/list": `{"data":[{"id":"sub-7","input":[{"type":"text","text":"hi"}],` +
			`"clientUserMessageId":"user:1"}],"nextCursor":null}`,
	})
	var mu sync.Mutex
	notices := 0
	s := newQueueSession(t, binary, func(evt provider.ProviderEvent) {
		if evt.Kind != provider.EventNotification {
			return
		}
		if got, ok := metaValue(t, evt.Meta, "kind"); ok && got == "external_queue" {
			mu.Lock()
			notices++
			mu.Unlock()
		}
	})

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/queue/changed","params":{"threadId":"codex-thread-q"}}`))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := notices
		mu.Unlock()
		if seen == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("got %d external-queue notices with a nil ownership predicate, want 1", notices)
}

// TestQueueChangedReportsAForeignSubmissionOnce is the other side: a row AO
// never added is what the notice exists for, and it is reported exactly once
// however many change notifications the row's lifetime produces.
func TestQueueChangedReportsAForeignSubmissionOnce(t *testing.T) {
	capture := t.TempDir() + "/queue-requests.jsonl"
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		"thread/queue/list": `{"data":[{"id":"sub-foreign","input":[{"type":"text","text":"from the cli"}],` +
			`"clientUserMessageId":"cli-uuid-1"}],"nextCursor":null}`,
	})
	var mu sync.Mutex
	notices := []provider.ProviderEvent{}
	s := newQueueSession(t, binary, func(evt provider.ProviderEvent) {
		if evt.Kind != provider.EventNotification {
			return
		}
		if got, ok := metaValue(t, evt.Meta, "kind"); ok && got == "external_queue" {
			mu.Lock()
			notices = append(notices, evt)
			mu.Unlock()
		}
	})

	for range 3 {
		s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/queue/changed","params":{"threadId":"codex-thread-q"}}`))
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := len(notices)
		mu.Unlock()
		if seen >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 1 {
		t.Fatalf("got %d external-queue notices, want exactly 1 for one foreign submission", len(notices))
	}
	if got, ok := metaValue(t, notices[0].Meta, "origin"); !ok || got != ExternalTurnOriginQueue {
		t.Errorf("notice origin = %v (present=%v), want %q", got, ok, ExternalTurnOriginQueue)
	}
}

// TestQueueStartIsNeverCalled is a source-level tripwire. Upstream dispatches
// the queue head from its own `on_thread_idle` hook, so a client that also
// calls `thread/queue/start` races the drain and can run the same message
// twice. The method is deliberately not wrapped.
func TestQueueStartIsNeverCalled(t *testing.T) {
	sources, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range sources {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, `"thread/queue/start"`) {
				continue
			}
			t.Errorf("%s calls thread/queue/start; dispatch is automatic and a client start races the drain:\n\t%s",
				name, strings.TrimSpace(line))
		}
	}
}

// TestPurgeQueueDropsEveryRowAndCountsTheForeignOnes is finding 7. A message
// already handed to the provider's queue outlives the session: it sits in
// codex's SQLite and `on_thread_idle` dispatches it on the next resume, so a
// rollback that only cleared AO's own flushqueue would re-run a rolled-back
// message onto the truncated thread.
func TestPurgeQueueDropsEveryRowAndCountsTheForeignOnes(t *testing.T) {
	capture := t.TempDir() + "/queue-requests.jsonl"
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		"thread/queue/list": `{"data":[` +
			`{"id":"sub-1","input":[{"type":"text","text":"mine"}],"clientUserMessageId":"user:4:flush:1"},` +
			`{"id":"sub-2","input":[{"type":"text","text":"theirs"}],` +
			`"clientUserMessageId":"0199e3a1-0000-7000-8000-000000000001"}],"nextCursor":null}`,
		"thread/queue/delete": `{"deleted":true}`,
	})
	s := newQueueSessionWithConfig(t, binary, nil, func(cfg *Config) {
		cfg.OwnsQueuedClientID = func(clientID string) bool { return clientID == "user:4:flush:1" }
	})

	purge, err := s.PurgeQueue(context.Background())
	if err != nil {
		t.Fatalf("PurgeQueue: %v", err)
	}
	if len(purge.Deleted) != 2 || purge.Foreign != 1 {
		t.Errorf("PurgeQueue = (deleted=%d, foreign=%d), want (2, 1)", len(purge.Deleted), purge.Foreign)
	}

	var targets []string
	for _, frame := range capturedRequests(t, capture) {
		if frame["method"] != "thread/queue/delete" {
			continue
		}
		targets = append(targets, fmt.Sprint(requestParams(t, frame)["queuedSubmissionId"]))
	}
	if len(targets) != 2 || targets[0] != "sub-1" || targets[1] != "sub-2" {
		t.Errorf("deleted submissions = %v, want both rows in list order", targets)
	}
}

// TestBeforeResumeRunsAheadOfTheThreadLoad pins the ordering the queue
// recovery depends on. `thread/resume` LOADS the thread, and a loaded
// thread's idle hook drains its queue into a turn — so a client that
// reconciles the queue after NewSession returns is racing a dispatch it
// cannot cancel. The hook has to see a connection that can already answer
// `thread/queue/list` and a thread that is not loaded yet.
func TestBeforeResumeRunsAheadOfTheThreadLoad(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-resume", capture,
		map[string]string{
			"thread/queue/list": `{"data":[],"nextCursor":null}`,
			"thread/resume":     `{"thread":{"id":"codex-thread-resume","turns":[]},"approvalsReviewer":"user"}`,
		})

	hookRan := false
	newQueueSessionWithConfig(t, binary, nil, func(cfg *Config) {
		cfg.ResumeThreadID = "codex-thread-resume"
		cfg.BeforeResume = func(s *Session) {
			hookRan = true
			// The handshake is behind us, so the version gate is already
			// frozen and the queue methods are usable.
			if !s.ThreadQueueNative() {
				t.Error("BeforeResume ran before the queue gate was decided")
			}
			if _, err := s.QueueList(context.Background()); err != nil {
				t.Errorf("thread/queue/list from the pre-resume hook: %v", err)
			}
		}
	})
	if !hookRan {
		t.Fatal("BeforeResume never ran on a resume")
	}

	listAt, resumeAt := -1, -1
	for i, frame := range capturedRequests(t, capture) {
		switch frame["method"] {
		case "thread/queue/list":
			if listAt < 0 {
				listAt = i
			}
		case "thread/resume":
			if resumeAt < 0 {
				resumeAt = i
			}
		}
	}
	if listAt < 0 || resumeAt < 0 {
		t.Fatalf("missing frames: queue/list at %d, resume at %d", listAt, resumeAt)
	}
	if listAt > resumeAt {
		t.Errorf("thread/queue/list frame %d came after thread/resume frame %d; the queue can dispatch in that window", listAt, resumeAt)
	}
}

// TestBeforeResumeIsNotCalledOnAFreshThread — a `thread/start` has no
// provider-side history and therefore nothing to reconcile against. Running
// the hook there would ask a queue question about a thread that does not
// exist yet.
func TestBeforeResumeIsNotCalledOnAFreshThread(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-fresh", capture, nil)
	ran := false
	newQueueSessionWithConfig(t, binary, nil, func(cfg *Config) {
		cfg.BeforeResume = func(*Session) { ran = true }
	})
	if ran {
		t.Error("BeforeResume ran on a fresh thread/start")
	}
}

// queueSelectiveDeleteScript is a fake app-server whose `thread/queue/delete`
// SUCCEEDS for one submission and is refused for another.
//
// That asymmetry is the whole shape of a partial purge, and it is the one the
// per-method reply table above cannot express: PurgeQueue deletes row by row,
// so "A went, B did not" is what the caller has to be able to see.
func queueSelectiveDeleteScript(t *testing.T, threadID, capturePath, listReply, refusedSubmissionID string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"userAgent\":\"codex_cli_rs/0.149.0 (test)\"}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"%s\"}}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/queue/list"'; then
        echo "$line" >> %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/queue/delete"'; then
        echo "$line" >> %q
        sub=$(echo "$line" | grep -o '"queuedSubmissionId":"[^"]*"' | cut -d'"' -f4)
        if [ "$sub" = "%s" ]; then
            echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"error\":{\"code\":-32603,\"message\":\"thread store is busy\"}}"
        else
            echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"deleted\":true}}"
        fi
        continue
    fi
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
done
`, threadID, capturePath, bashJSON(listReply), capturePath, refusedSubmissionID)
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

// TestPurgeQueueNamesTheRowsItDeletedWhenALaterDeleteFails is K1 at the
// provider boundary: delete A, fail on B.
//
// The caller aborts the rollback on that error and leaves history untouched —
// but A is already out of codex's queue, nothing will ever dispatch it, and a
// count cannot tell the caller which row to put back. So the purge reports the
// submissions themselves, in the order they went, and the AGENTS.md promise of
// "a retryable refusal with no mutation" is only true because the caller can
// undo the half that did happen.
func TestPurgeQueueNamesTheRowsItDeletedWhenALaterDeleteFails(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "queue-requests.jsonl")
	list := `{"data":[` +
		`{"id":"sub-a","input":[{"type":"text","text":"mine"}],"clientUserMessageId":"user:4:flush:1"},` +
		`{"id":"sub-b","input":[{"type":"text","text":"stuck"}],"clientUserMessageId":"user:4:flush:2"}` +
		`],"nextCursor":null}`
	binary := queueSelectiveDeleteScript(t, "codex-thread-q", capture, list, "sub-b")
	s := newQueueSessionWithConfig(t, binary, nil, func(cfg *Config) {
		cfg.OwnsQueuedClientID = func(string) bool { return true }
	})

	purge, err := s.PurgeQueue(context.Background())
	if err == nil {
		t.Fatal("PurgeQueue reported success over a row it could not delete")
	}
	if len(purge.Deleted) != 1 {
		t.Fatalf("purge.Deleted = %+v, want exactly the one row that went", purge.Deleted)
	}
	if purge.Deleted[0].ID != "sub-a" || purge.Deleted[0].ClientUserMessageID != "user:4:flush:1" {
		t.Fatalf("purge.Deleted[0] = %+v, want sub-a with its client id; the caller restores by client id",
			purge.Deleted[0])
	}
	if purge.Deleted[0].Text != "mine" {
		t.Errorf("purge.Deleted[0].Text = %q, want the row's text", purge.Deleted[0].Text)
	}
	if purge.Foreign != 0 {
		t.Errorf("purge.Foreign = %d, want 0; the app layer claimed both rows", purge.Foreign)
	}
}

// TestQueueListReportsAMalformedSubmissionInsteadOfAnAbsentRow is K3. A wrong-
// typed field used to fail `json.Unmarshal` into an EMPTY submission while the
// list still reported success, and both callers read that as an ABSENT row:
// the resume reconcile would return an unproven AO row to the composer while
// codex still held it (a duplicate on the next send), and the purge would skip
// the empty id and let the rollback truncate over a submission still armed.
func TestQueueListReportsAMalformedSubmissionInsteadOfAnAbsentRow(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "queue-requests.jsonl")
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		// `id` typed as a number: upstream's own QueuedSubmission types all
		// three fields as required, non-Option, so this is a wire fault.
		"thread/queue/list": `{"data":[` +
			`{"id":"sub-a","input":[{"type":"text","text":"one"}],"clientUserMessageId":"user:1"},` +
			`{"id":7,"input":[{"type":"text","text":"two"}],"clientUserMessageId":"user:2"}` +
			`],"nextCursor":null}`,
		"thread/queue/delete": `{"deleted":true}`,
	})
	s := newQueueSession(t, binary, nil)

	items, err := s.QueueList(context.Background())
	if !IsThreadQueueListMalformed(err) {
		t.Fatalf("QueueList error = %v, want ErrThreadQueueListMalformed", err)
	}
	if len(items) != 1 || items[0].ID != "sub-a" {
		t.Fatalf("QueueList = %+v, want the readable prefix only", items)
	}

	// And the purge inherits the refusal rather than reporting a complete job
	// over a row it could not even name.
	purge, purgeErr := s.PurgeQueue(context.Background())
	if purgeErr == nil {
		t.Fatal("PurgeQueue reported success over a listing it could not fully read")
	}
	if !IsThreadQueueListMalformed(purgeErr) {
		t.Fatalf("PurgeQueue error = %v, want it to carry ErrThreadQueueListMalformed", purgeErr)
	}
	for _, submission := range purge.Deleted {
		if submission.ID == "" {
			t.Fatal("PurgeQueue reported deleting a submission with no id")
		}
	}
}

// TestQueueListRefusesASubmissionWithNoID is the same rule for the one field a
// delete cannot do without. `id` is server-assigned upstream, so an empty one
// is a wire fault; skipping it during a purge would report a complete job over
// a row that is still queued and still armed to run.
func TestQueueListRefusesASubmissionWithNoID(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "queue-requests.jsonl")
	binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
		"thread/queue/list": `{"data":[{"id":"","input":[{"type":"text","text":"one"}],` +
			`"clientUserMessageId":"user:1"}],"nextCursor":null}`,
	})
	s := newQueueSession(t, binary, nil)

	items, err := s.QueueList(context.Background())
	if !IsThreadQueueListMalformed(err) {
		t.Fatalf("QueueList error = %v, want ErrThreadQueueListMalformed", err)
	}
	if len(items) != 0 {
		t.Fatalf("QueueList = %+v, want nothing; a row with no id has no delete handle", items)
	}
}

// TestQueueDeleteRefusesAResponseWithoutTheDeletedField is K4. Upstream types
// ThreadQueueDeleteResponse as `{ deleted: bool }` — non-Option, no serde
// default (rust-v0.149.0
// codex-rs/app-server-protocol/src/protocol/v2/thread.rs:940) — so the key is
// required and its absence is drift, not a "matched nothing" answer. Decoded
// into a plain bool it read as `false`, which clears nothing and lets the
// rollback truncate history over a row that may still be armed.
func TestQueueDeleteRefusesAResponseWithoutTheDeletedField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"field absent", `{}`},
		{"field null", `{"deleted":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "queue-requests.jsonl")
			binary := queueFakeScript(t, "codex_cli_rs/0.149.0 (test)", "codex-thread-q", capture, map[string]string{
				"thread/queue/list": `{"data":[{"id":"sub-a","input":[{"type":"text","text":"one"}],` +
					`"clientUserMessageId":"user:1"}],"nextCursor":null}`,
				"thread/queue/delete": tc.reply,
			})
			s := newQueueSession(t, binary, nil)

			if _, err := s.QueueDelete(context.Background(), "sub-a"); err == nil {
				t.Fatal("QueueDelete accepted a response with no `deleted` field as a benign miss")
			}

			// And it reaches the purge as a failure, so the rollback refuses
			// rather than truncating over a row whose fate is unknown.
			purge, purgeErr := s.PurgeQueue(context.Background())
			if purgeErr == nil {
				t.Fatal("PurgeQueue reported success over a delete that never said whether it deleted anything")
			}
			if len(purge.Deleted) != 0 {
				t.Fatalf("purge.Deleted = %+v, want none; nothing was confirmed removed", purge.Deleted)
			}
		})
	}
}
