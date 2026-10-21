package codex

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// NativeMemoryOff puts both memory keys in the config every request that
// loads the thread carries: start, resume and fork. Without it neither is
// sent, so the user's own Codex config decides.
func TestNativeMemoryOffRidesEveryThreadLoad(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]Config{
		"start":  {NativeMemoryOff: true},
		"resume": {NativeMemoryOff: true, ResumeThreadID: "thread-1"},
	} {
		config := buildThreadParams(cfg, "0.157.1")["config"].(map[string]any)
		if config["features.memories"] != false || config["memories.generate_memories"] != false {
			t.Errorf("%s: config = %v", name, config)
		}
		cfg.NativeMemoryOff = false
		config = buildThreadParams(cfg, "0.157.1")["config"].(map[string]any)
		for _, key := range []string{"features.memories", "memories.generate_memories"} {
			if _, ok := config[key]; ok {
				t.Errorf("%s without NativeMemoryOff sends %s", name, key)
			}
		}
	}
	var forked map[string]any
	call := func(_ context.Context, method string, params any) (json.RawMessage, error) {
		forked = params.(map[string]any)
		return json.RawMessage(`{"thread":{"id":"child"}}`), nil
	}
	if _, err := forkThreadWith(context.Background(), call, "source", "", "", true); err != nil {
		t.Fatal(err)
	}
	config := forked["config"].(map[string]any)
	if config["features.memories"] != false || config["memories.generate_memories"] != false || config["features.instant_interrupt"] != false {
		t.Fatalf("fork config = %v", config)
	}
}

// A resume keeps the memory mode Codex stored for the thread, so with
// NativeMemoryOff the session sets it to disabled; a refusal is reported and
// leaves the session usable. A start and a session with native memory on
// send nothing.
func TestResumeExcludesTheThreadFromMemories(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		cfg        Config
		failMethod string
		wantSets   int
		wantErrors int
	}{
		{name: "resume off", cfg: Config{NativeMemoryOff: true, ResumeThreadID: "mock-thread"}, wantSets: 1},
		{name: "resume on", cfg: Config{ResumeThreadID: "mock-thread"}},
		{name: "start off", cfg: Config{NativeMemoryOff: true}},
		{name: "refused", cfg: Config{NativeMemoryOff: true, ResumeThreadID: "mock-thread"}, failMethod: "thread/memoryMode/set", wantSets: 1, wantErrors: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			requestLog := filepath.Join(dir, "requests.jsonl")
			tc.cfg.Binary = writeInjectAppServer(t, dir, requestLog, "", tc.failMethod)
			tc.cfg.WorkDir = dir
			var mu sync.Mutex
			var errs []string
			sess, err := NewSession(context.Background(), "thread-1", tc.cfg, func(evt provider.ProviderEvent) {
				if evt.Kind == provider.EventError {
					mu.Lock()
					errs = append(errs, evt.Content)
					mu.Unlock()
				}
			})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			sets := requestsFor(t, requestLog, "thread/memoryMode/set")
			if len(sets) != tc.wantSets {
				t.Fatalf("memoryMode/set requests = %v, want %d", sets, tc.wantSets)
			}
			if tc.wantSets == 1 && (sets[0]["threadId"] != "mock-thread" || sets[0]["mode"] != "disabled") {
				t.Fatalf("memoryMode/set params = %v", sets[0])
			}
			mu.Lock()
			defer mu.Unlock()
			if len(errs) != tc.wantErrors {
				t.Fatalf("errors = %v, want %d", errs, tc.wantErrors)
			}
			if tc.wantErrors == 1 && !strings.Contains(errs[0], "Codex can still learn from it") {
				t.Fatalf("error = %q", errs[0])
			}
		})
	}
}

// The workspace-write policy carries the writable roots; the other
// sandboxes treat an outside write as they treat a workspace write and
// carry none.
func TestTurnSandboxPolicyCarriesWritableRoots(t *testing.T) {
	t.Parallel()
	roots := []string{"/config/memory/user", "/config/memory/projects/app"}
	policy, err := turnSandboxPolicy("workspace-write", roots)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := policy["writableRoots"].([]string); !slices.Equal(got, roots) || policy["type"] != "workspaceWrite" {
		t.Fatalf("policy = %v", policy)
	}
	if policy, _ := turnSandboxPolicy("workspace-write", nil); len(policy) != 1 {
		t.Fatalf("no roots = %v", policy)
	}
	for _, sandbox := range []string{"read-only", "danger-full-access"} {
		if policy, _ := turnSandboxPolicy(sandbox, roots); policy["writableRoots"] != nil {
			t.Errorf("%s policy = %v", sandbox, policy)
		}
	}
}

// Every turn of a session with writable roots sends them, including after a
// live update changes its sandbox.
func TestTurnStartCarriesTheSessionWritableRoots(t *testing.T) {
	t.Parallel()
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	scriptPath := filepath.Join(t.TempDir(), "codex")
	mockexec.Write(t, scriptPath, codexTurnCaptureScript(capturePath))
	roots := []string{"/config/memory/user"}
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:        scriptPath,
		WorkDir:       "/tmp",
		Sandbox:       "read-only",
		WritableRoots: roots,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.ApplyLiveUpdate(LiveUpdate{Sandbox: "workspace-write"})
	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = s.Close()
	turns := capturedTurnStartParams(t, capturePath)
	if len(turns) == 0 {
		t.Fatal("captured no turn/start request")
	}
	policy, _ := turns[0]["sandboxPolicy"].(map[string]any)
	got, _ := policy["writableRoots"].([]any)
	if policy["type"] != "workspaceWrite" || len(got) != 1 || got[0] != roots[0] {
		t.Fatalf("sandboxPolicy = %v", turns[0]["sandboxPolicy"])
	}
}
