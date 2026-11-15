package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/workflow/engine"
)

// pauseTreeFixture is a run tree three levels deep, tool-flow calling mid
// calling leaf, whose leaf runs a tool phase that records each process it
// starts and holds it until stopped.
type pauseTreeFixture struct {
	*toolWorkflowFixture
	pids            string
	root, mid, leaf string
}

func newPauseTreeFixture(t *testing.T) *pauseTreeFixture {
	t.Helper()
	fixture := newToolWorkflowFixture(t, `
  - id: call-mid
    shape: call
    call: mid
    max_depth: 3
    gate:
      routes:
        - to: done`)
	dir := filepath.Join(fixture.configRoot, "workflows")
	for name, body := range map[string]string{
		"mid.yaml": `id: mid
name: Mid
phases:
  - id: call-leaf
    shape: call
    call: leaf
    max_depth: 3
    gate:
      routes:
        - to: done
cleanup: manual
`,
		"leaf.yaml": `id: leaf
name: Leaf
phases:
  - id: hold
    driver: tool
    check: hold
    gate:
      routes:
        - to: done
cleanup: manual
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pids := filepath.Join(t.TempDir(), "pids")
	fixture.writeProfile(t, map[string][]string{"hold": {"/bin/sh", "-c", "echo $$ >> " + pids + "; exec sleep 600"}}, nil, "")
	f := &pauseTreeFixture{toolWorkflowFixture: fixture, pids: pids}
	f.root = fixture.start(t, "deep").ID
	f.mid = f.onlyChild(t, f.root)
	f.leaf = f.onlyChild(t, f.mid)
	f.awaitHoldProcesses(t, 1)
	return f
}

// onlyChild waits for the one run parentID called.
func (f *pauseTreeFixture) onlyChild(t *testing.T, parentID string) string {
	t.Helper()
	var child string
	waitUntil(t, 10*time.Second, func() bool {
		children, err := f.app.store.ListWorkItemChildren(parentID)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) == 1 {
			child = children[0].ID
		}
		return child != ""
	})
	return child
}

// awaitHoldProcesses waits for the leaf's tool phase to have started count
// processes and returns the last.
func (f *pauseTreeFixture) awaitHoldProcesses(t *testing.T, count int) int {
	t.Helper()
	var pid int
	waitUntil(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(f.pids)
		if err != nil {
			return false
		}
		lines := strings.Fields(string(data))
		if len(lines) < count {
			return false
		}
		pid, err = strconv.Atoi(lines[count-1])
		return err == nil
	})
	return pid
}

// pauseThenResumeTheLeaf pauses the whole tree, then resumes the leaf
// alone, and returns the leaf's new process.
func (f *pauseTreeFixture) pauseThenResumeTheLeaf(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	if err := f.app.WorkflowPauseItem(ctx, f.root); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.root, f.mid, f.leaf} {
		waitForWorkflowItem(t, f.app, id, engine.StateNeedsHuman, engine.ReasonPaused)
	}
	if err := f.app.WorkflowResumeItem(ctx, f.leaf, "", false); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, f.app, f.leaf, engine.StateRunning, "")
	waitForWorkflowItem(t, f.app, f.mid, engine.StateNeedsHuman, engine.ReasonPaused)
	return f.awaitHoldProcesses(t, 2)
}

// requireTreePaused checks every run of the tree is paused and the leaf's
// process has ended.
func (f *pauseTreeFixture) requireTreePaused(t *testing.T, pid int) {
	t.Helper()
	for _, id := range []string{f.leaf, f.mid, f.root} {
		waitForWorkflowItem(t, f.app, id, engine.StateNeedsHuman, engine.ReasonPaused)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

// A pause of a tree whose root and middle run are parked reaches the leaf
// that was resumed alone and is running beneath them: Pause and End
// Sessions parks it and stops its tool process, and so does a Stop of the
// thread the tree reports to.
func TestAPauseReachesARunningRunBeneathAParkedOne(t *testing.T) {
	t.Parallel()
	t.Run("pause and end sessions", func(t *testing.T) {
		t.Parallel()
		f := newPauseTreeFixture(t)
		pid := f.pauseThenResumeTheLeaf(t)
		if err := f.app.WorkflowPauseItemAndEndSessions(context.Background(), f.root); err != nil {
			t.Fatal(err)
		}
		f.requireTreePaused(t, pid)
	})
	t.Run("stop", func(t *testing.T) {
		t.Parallel()
		f := newPauseTreeFixture(t)
		// The first pause's rest wakes the thread the tree reports to.
		installMockClaudeReplies(t, f.app, "noted", "noted", "noted")
		thread, err := f.app.CreateThread(t.Context(), CreateThreadOptions{
			ProjectID: f.project.ID, Provider: "claude", Model: "claude-opus-4-7",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.app.store.UpdateWorkItemOriginThread(f.root, thread.ID); err != nil {
			t.Fatal(err)
		}
		pid := f.pauseThenResumeTheLeaf(t)
		var stop ThreadAgentStop
		f.app.restThreadHere(t.Context(), thread.ID, userRestStop(), map[string]bool{}, &stop)
		if len(stop.Failures) != 0 {
			t.Fatalf("the Stop failed: %v", stop.Failures)
		}
		f.requireTreePaused(t, pid)
	})
}
