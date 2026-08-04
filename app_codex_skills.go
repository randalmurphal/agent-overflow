package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/slicesx"
)

// GetCodexSkills returns the Codex skills visible from one workspace
// directory — the composer's command-menu source on a Codex thread, since
// skills are what upstream replaced custom prompts with in 0.118.
//
// workspacePath must be an ABSOLUTE path. Skills are directory-scoped (the
// repo tier comes from the workspace itself), and a relative path would be
// resolved against whichever process happens to answer — a live session's cwd
// or a throwaway one's — so it is refused rather than guessed at.
//
// forceReload is the user-initiated refresh and bypasses both AO's cache and
// the app-server's own on-disk scan. A menu opening must pass false, or every
// render re-walks the filesystem.
//
// LocalOnly on the wire: it drives the user's own `codex` CLI (a live
// session's connection when there is one, a short-lived app-server otherwise)
// and its answer names absolute paths on the host filesystem.
func (a *App) GetCodexSkills(ctx context.Context, workspacePath string, forceReload bool) (codexskills.CwdSkills, error) {
	skills, err := a.codexSkillsForWorkspace(ctx, workspacePath, forceReload)
	if err != nil {
		return codexskills.CwdSkills{}, err
	}
	// Nil slices would reach a non-nullable frontend field as `null`. An
	// empty skill list is a legitimate answer here — the error path above is
	// what distinguishes it from a failed read.
	skills.Skills = slicesx.OrEmpty(skills.Skills)
	skills.Errors = slicesx.OrEmpty(skills.Errors)
	return skills, nil
}

// codexSkillsForWorkspace returns the Codex skills visible from one
// workspace directory, cached per (binary, cwd).
//
// forceReload is the user-initiated refresh: it bypasses BOTH caches — AO's
// entry for this key and, through the request's own `forceReload` flag, the
// app-server's on-disk skill scan. An ordinary read must not set it, or a
// composer menu opening would re-walk the filesystem on every keystroke.
//
// Local-only on the wire: it drives the Codex CLI under the user's
// credentials and returns absolute paths from the user's filesystem.
func (a *App) codexSkillsForWorkspace(ctx context.Context, cwd string, forceReload bool) (codexskills.CwdSkills, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return codexskills.CwdSkills{}, errors.New("codex skills: workspace path required")
	}
	if !filepath.IsAbs(cwd) {
		return codexskills.CwdSkills{}, fmt.Errorf("codex skills: workspace path %q must be absolute", cwd)
	}
	binary := a.providerBinaryPath(string(provider.Codex))
	key := codexskills.Key(binary, cwd)
	fetch := func(ctx context.Context) (codexskills.CwdSkills, error) {
		return a.readCodexSkills(ctx, binary, cwd, forceReload)
	}
	if forceReload {
		return a.codexSkills().Refresh(ctx, key, fetch)
	}
	return a.codexSkills().Get(ctx, key, fetch)
}

// readCodexSkills prefers a live Codex session's already-open app-server
// connection and falls back to a short-lived process.
//
// Riding a live session is not just an optimization: `skills/list` is
// global (`serialization: global_shared_read("config")`) and the request
// names the directories to scan explicitly, so a session answers for any
// workspace in one JSON-RPC round trip instead of a cold binary start plus
// a second handshake. The session is only trusted when its process was
// spawned from the binary the setting currently names — see
// sessionManager.codexSessionForBinary for why that, and not the account,
// is the dimension that matters here.
func (a *App) readCodexSkills(ctx context.Context, binary, cwd string, forceReload bool) (codexskills.CwdSkills, error) {
	cwds := []string{cwd}
	if sess := a.sessionManager().codexSessionForBinary(binary); sess != nil {
		entries, err := sess.ListSkills(ctx, cwds, forceReload)
		if err == nil {
			return skillsEntryForCwd(entries, cwd)
		}
		// A live session can fail for reasons the skills on disk cannot
		// (the process died mid-read, the request timed out). Fall through
		// to a fresh process rather than reporting a transport problem as
		// "this workspace has no skills".
		log.Printf("codex skills: live session read failed, falling back to a fresh process: %v", err)
	}
	if strings.TrimSpace(binary) == "" {
		return codexskills.CwdSkills{}, errors.New("codex skills: codex binary not configured")
	}
	fetcher := &codex.SkillsFetcher{
		Binary:  binary,
		WorkDir: cwd,
		Env:     a.providerCustomEnv(string(provider.Codex)),
	}
	entries, err := fetcher.Fetch(ctx, cwds, forceReload)
	if err != nil {
		return codexskills.CwdSkills{}, err
	}
	return skillsEntryForCwd(entries, cwd)
}

// skillsEntryForCwd picks the one entry the request asked for. `skills/list`
// answers per requested directory and echoes the REQUESTED path back, so a
// single-cwd request has exactly one entry — a response that does not is a
// wire surprise, and returning an empty list for it would present a server
// disagreement as "no skills here".
func skillsEntryForCwd(entries []codexskills.CwdSkills, cwd string) (codexskills.CwdSkills, error) {
	for _, entry := range entries {
		if entry.Cwd == cwd {
			return entry, nil
		}
	}
	if len(entries) == 1 {
		// Tolerate a path the server normalised (a trailing separator, a
		// symlinked root) as long as it answered exactly once: the request
		// named one directory, so there is no ambiguity about whose answer
		// this is.
		return entries[0], nil
	}
	return codexskills.CwdSkills{}, fmt.Errorf(
		"codex skills: response carried %d entries, none matching %q", len(entries), cwd,
	)
}

// handleCodexSkillsChanged drops the whole skills cache.
//
// The notification carries no payload — no cwd, no scope, no skill name —
// so there is nothing to narrow the drop to, and a skill file that moved
// between two watched roots would leave a stale entry behind under the key
// it left. Signals from any live session invalidate every key for the same
// reason: the watcher is per-process, but the filesystem it watches is
// shared.
func (a *App) handleCodexSkillsChanged() {
	a.codexSkills().Reset()
}

func (a *App) codexSkills() *codexskills.Cache {
	a.codexSkillsOnce.Do(func() {
		a.codexSkillsCache = codexskills.New()
	})
	return a.codexSkillsCache
}
