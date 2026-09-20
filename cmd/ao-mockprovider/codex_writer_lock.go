package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/harness/control"
)

// The thread WRITER LOCK: one live process per thread.
//
// Upstream keeps a per-thread lock file under its home
// (codex-rs/thread-store/src/local/writer_lock.rs) and holds an OS file lock
// on it for as long as the thread is loaded in a process. `thread/start`,
// `thread/resume` and `thread/fork` all load a thread, and the last one loads
// the CHILD into the process that answered. A second process asking to load
// the same thread is refused with "thread <id> already has an active writer",
// which AO surfaces as "open in another Codex process". The lock is released
// by process exit, with nothing to clean up.
//
// The mock keeps the lock for the ids it can key uniquely: the ones minted by
// `thread/fork`. Every other mock thread shares the scenario's seed id (or the
// default "mock-codex-thread") across the processes of one harness, so a lock
// on those would refuse threads that are different threads in every sense but
// the id. A resume therefore takes the lock only for an id a fork minted, which
// is how a fork cut on a live source session, and left loaded there, shows up
// as the refusal it is in production.
//
// Like the history-mode store next door the lock lives only under the home a
// fully isolated harness boot hands over (control.EnvTranscriptHome). Without
// one nothing is modeled and every load succeeds.

// takeThreadWriter loads threadID into this process for its life. create is
// the fork's case: the child is new and its lock file does not exist yet. A
// resume passes false and only contends for ids a fork minted.
func (a *codexAdapter) takeThreadWriter(threadID string, create bool) error {
	path, ok := codexWriterLockPath(threadID)
	if !ok {
		return nil
	}
	if !create {
		if _, err := os.Stat(path); err != nil {
			return nil
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Fatalf("codex: create mock writer lock directory: %v", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		log.Fatalf("codex: open mock writer lock: %v", err)
	}
	held, err := tryLockWriterFile(file)
	if err != nil {
		_ = file.Close()
		log.Fatalf("codex: lock mock writer lock: %v", err)
	}
	if !held {
		_ = file.Close()
		return fmt.Errorf("thread %s already has an active writer", threadID)
	}
	a.mu.Lock()
	a.writerLocks = append(a.writerLocks, file)
	a.mu.Unlock()
	return nil
}

func (a *codexAdapter) markThreadLoaded() {
	a.mu.Lock()
	a.loadedThread = true
	a.mu.Unlock()
}

// codexWriterLockPath locates a thread's lock file, or reports that this
// invocation has no durable home.
func codexWriterLockPath(threadID string) (string, bool) {
	home := strings.TrimSpace(os.Getenv(control.EnvTranscriptHome))
	if home == "" || threadID == "" {
		return "", false
	}
	if filepath.Base(threadID) != threadID ||
		strings.ContainsAny(threadID, `/\`) ||
		strings.ContainsRune(threadID, '\x00') ||
		threadID == "." || threadID == ".." {
		log.Fatalf("codex: unsafe thread id %q", threadID)
	}
	return filepath.Join(home, ".codex-mock-writer-locks", threadID+".lock"), true
}
