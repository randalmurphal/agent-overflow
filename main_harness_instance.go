package main

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/transport"
)

// Instance discovery for isolated boots (--harness / --soak, windowed or
// not). Two files, written at the same moment — right after MarkReady,
// which is the first instant an attach would succeed — and removed on
// graceful shutdown:
//
//   - <dataDir>/harness-instance.json (0600): the full bootstrap payload
//     plus the identity block. This is what lets any tool attach to a
//     RUNNING instance without having parsed its stdout, which stdout
//     only offered to whoever spawned it.
//   - <registryDir>/<id>.json: discovery only, no token
//     (internal/harness/instanceinfo).
//
// Neither is load-bearing for the boot itself, so a failure to write
// either is logged and the instance keeps running: the harness works
// exactly as it did before these files existed, minus discovery.

// harnessRegistryDir is where discovery rows are filed, resolved at
// package init — which is to say BEFORE isolateWebviewStorage points
// XDG_CACHE_HOME at the instance's own data root. os.UserCacheDir()
// reads that variable, so resolving it any later files the row inside
// the isolated tree that only this instance can see, and a discovery
// tool finds nothing (observed the first time this ran windowed,
// 2026-08-26). Empty when no cache dir is resolvable; publishInstance
// says so and skips the row rather than guessing at a path.
var harnessRegistryDir = func() string {
	dir, err := instanceinfo.RegistryDir()
	if err != nil {
		return ""
	}
	return dir
}()

// harnessInstanceFile is the data-dir file's shape: the same bootstrap
// payload agents already parse off stdout, plus the identity fields that
// tie it to a registry row. Embedding rather than restating means a
// field added to either half reaches this file automatically.
type harnessInstanceFile struct {
	harnessBootstrap
	instanceinfo.Identity
}

// publishedInstance is the handle a boot keeps so it can withdraw its
// discovery files. Removal is idempotent, so the shutdown path and an
// error path may both call it.
type publishedInstance struct {
	id       string
	filePath string
	// registryDir is captured per instance so removal withdraws the row
	// from the directory the write used, whatever the environment has
	// done to XDG_CACHE_HOME since.
	registryDir string
}

// publishInstance writes both discovery files for a live isolated boot.
// Never fails a boot: every problem is a log line.
//
// launcherPID is the Windows launcher hosting this backend's window, or
// 0 when nobody does — the caller knows, because it is the boot flag the
// launcher spelled.
func publishInstance(srv *transport.Server, paths harnessPaths, mode instanceinfo.Mode, windowed bool, launcherPID int) publishedInstance {
	identity := instanceinfo.Identity{
		ID:     instanceinfo.ID(paths.DataRoot),
		Mode:   mode,
		Window: windowed,
		// The checkout this instance belongs to. Read at boot because the
		// process may chdir later (relocateOffWindowsDriveMount already
		// does on the launcher path) and "which worktree started this" is
		// a fact about the launch, not about the current directory.
		Worktree:    bootWorkingDir(),
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		LauncherPid: launcherPID,
	}
	published := publishedInstance{
		id:          identity.ID,
		filePath:    filepath.Join(paths.DataDir, instanceinfo.InstanceFileName),
		registryDir: harnessRegistryDir,
	}

	file := harnessInstanceFile{
		harnessBootstrap: newHarnessBootstrap(srv, paths, nil),
		Identity:         identity,
	}
	// atomicfile writes 0600: this file carries the transport token.
	if err := atomicfile.WriteJSON(published.filePath, file); err != nil {
		log.Printf("harness: write %s: %v (tools must fall back to the bootstrap line)", published.filePath, err)
	}

	row := instanceinfo.Row{
		Identity: identity,
		PID:      os.Getpid(),
		Port:     portFromAddr(srv.Addr()),
		DataRoot: paths.DataRoot,
		DataDir:  paths.DataDir,
		Version:  version,
	}
	switch {
	case published.registryDir == "":
		log.Printf("harness: no user cache dir; instance %s will not be listed by discovery tools", identity.ID)
	default:
		if err := instanceinfo.WriteIn(published.registryDir, row); err != nil {
			log.Printf("harness: register instance %s: %v (this instance will not be listed)", identity.ID, err)
		}
	}
	log.Printf("harness: instance %s (%s, window=%v) published at %s, registry %s", identity.ID, mode, windowed, published.filePath, published.registryDir)
	return published
}

// remove withdraws both discovery files. Called on every graceful
// shutdown path; a reader that finds a row we failed to remove treats a
// dead pid as stale, so this is tidiness rather than correctness.
func (p publishedInstance) remove() {
	if p.id == "" {
		return
	}
	if err := os.Remove(p.filePath); err != nil && !os.IsNotExist(err) {
		log.Printf("harness: remove %s: %v", p.filePath, err)
	}
	if p.registryDir == "" {
		return
	}
	if err := instanceinfo.RemoveIn(p.registryDir, p.id); err != nil {
		log.Printf("harness: deregister instance %s: %v", p.id, err)
	}
}

// bootWorkingDir reports the checkout an instance was launched from, or
// "" when the working directory is unreadable (a deleted cwd) — an empty
// worktree field is a discovery inconvenience, never a boot failure.
func bootWorkingDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		log.Printf("harness: read working directory: %v", err)
		return ""
	}
	return cwd
}

// isolatedWindowTitle names a windowed isolated instance:
// "Agent Overflow (harness · 0f3a91cc)". Humans tell windows apart with
// it; tools use the registry. It carries the instance id rather than a
// profile word because N of these can be on screen at once — one per
// checkout — which is the case appidentity.AppTitle's fixed
// dev/soak/prod axis cannot name.
func isolatedWindowTitle(mode instanceinfo.Mode, id string) string {
	return appidentity.Name + " (" + string(mode) + " · " + id + ")"
}
