package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/appdirs"
)

// harnessLockFileName is the OS-held advisory lock every isolated boot
// takes on its data root, inside the app data dir the SQLite file lives
// in.
const harnessLockFileName = "harness.lock"

// harnessLockHolder is what the lock file carries once held. It exists
// only so the refusal message can name the process to go look at — the
// LOCK is the mutual exclusion, and nothing may be inferred from this
// content (a crashed boot leaves it behind with the lock released).
type harnessLockHolder struct {
	PID     int    `json:"pid"`
	Mode    string `json:"mode"`
	Started string `json:"started"`
	DataDir string `json:"dataDir"`
}

// harnessInstanceLock is the held lock. Its os.File must stay reachable
// for the process's whole life: the kernel releases a flock when the last
// descriptor for it closes, and os.File installs a finalizer that closes
// on GC — so a lock nobody references is a lock that quietly evaporates.
// heldHarnessLock is that reference.
type harnessInstanceLock struct {
	file *os.File
	path string
}

// heldHarnessLock keeps the acquired lock alive for the process lifetime.
// Deliberately never cleared: releasing early would let a second backend
// in while this one still owns the store.
var heldHarnessLock *harnessInstanceLock

// acquireHarnessInstanceLock takes an exclusive, non-blocking, OS-held
// advisory lock on <dataDir>/harness.lock and holds it until the process
// exits.
//
// This is the ONLY liveness guard on an isolated boot, and it lives here
// rather than in a launcher because every entry point has to be covered:
// `make harness` and the wails3 dev harness path boot the backend
// DIRECTLY, and `ao-harness up`'s registry pre-check is both skippable
// and TOCTOU (it reads the registry, then spawns). Two backends on one
// data root is not a cosmetic clash — they open the same SQLite file, and
// the second publishInstance overwrites the first's registry row, so the
// tooling then points every reader at the wrong backend.
//
// The lock is OS-held on purpose. A pid file would need liveness probes,
// staleness heuristics, and a cleanup path a SIGKILL never runs; a flock
// is released by the kernel when the process dies however it dies, so a
// crashed boot leaves the next one free with no reaping at all.
func acquireHarnessInstanceLock(dataDir, mode string) (*harnessInstanceLock, error) {
	held, err := acquireInstanceLock(dataDir, mode, harnessLockFileName, "another isolated boot")
	if err == nil {
		heldHarnessLock = held
	}
	return held, err
}

// Shared OS primitive for isolated and ordinary backend boots. Filename is
// internal policy, never caller input. The open descriptor is the authority.
func acquireInstanceLock(dataDir, mode, filename, owner string) (*harnessInstanceLock, error) {
	path := filepath.Join(dataDir, filename)
	file, err := openHarnessLock(path, appdirs.SensitiveFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open harness instance lock %s: %w", path, err)
	}
	locked, err := lockFileExclusiveNonBlocking(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !locked {
		holder := describeHarnessLockHolder(file)
		file.Close()
		return nil, fmt.Errorf("%s already holds %s (%s); two backends cannot share a data root. Connect to the running backend, stop it, or pass a different --data-dir", owner, path, holder)
	}
	// Written only once the lock is HELD, so two boots can never
	// interleave writes into it. Truncate first: a previous holder's
	// longer line would otherwise leave a tail behind.
	if err := file.Truncate(0); err == nil {
		if _, err := file.Seek(0, 0); err == nil {
			line, mErr := json.Marshal(harnessLockHolder{
				PID:     os.Getpid(),
				Mode:    mode,
				Started: time.Now().UTC().Format(time.RFC3339),
				DataDir: dataDir,
			})
			if mErr == nil {
				// Best effort: the lock is the guarantee, this is only the
				// message the NEXT boot gets to print.
				_, _ = file.Write(append(line, '\n'))
			}
		}
	}
	held := &harnessInstanceLock{file: file, path: path}
	return held, nil
}

// describeHarnessLockHolder renders the holder line for a refusal
// message. Everything here is untrusted, best-effort decoration: the file
// may be empty (the holder is mid-write), stale, or garbage, and none of
// those change the refusal.
func describeHarnessLockHolder(file *os.File) string {
	buf := make([]byte, 512)
	n, _ := file.ReadAt(buf, 0)
	raw := strings.TrimSpace(string(buf[:n]))
	if raw == "" {
		return "holder not yet identified"
	}
	var holder harnessLockHolder
	if err := json.Unmarshal([]byte(raw), &holder); err != nil || holder.PID <= 0 {
		return "holder unreadable"
	}
	if holder.Mode != "" {
		return fmt.Sprintf("pid %d, --%s, started %s", holder.PID, holder.Mode, holder.Started)
	}
	return fmt.Sprintf("pid %d, started %s", holder.PID, holder.Started)
}

// harnessBootMode names the boot for the lock file and its refusal
// message. `--soak` is the Windows-launcher shell of the same harness, so
// the two share one lock — what must not double up is a BACKEND on a data
// root, regardless of which flag started it.
func harnessBootMode(flags cliFlags) string {
	if flags.isolatedProfile != "" {
		return flags.isolatedProfile
	}
	if flags.soak {
		return "soak"
	}
	return "harness"
}
