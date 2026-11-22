package devscan

import (
	"os"
	"path/filepath"
	"strings"
)

// Attribution: which thread, if any, a listening process belongs to.
//
// Two rules, and the second exists because the first is not enough.
//
//   - ANCESTRY. A dev server started by an agent is a descendant of that
//     thread's provider session, usually a grandchild (`npm run dev` →
//     `vite`). Walking the parent chain to an owner pid is the direct
//     answer and is what covers the common case.
//   - PROCESS GROUP. A dev server that daemonised has reparented to init,
//     so its ancestor chain no longer reaches anything of ours — but both
//     spawn paths call procutil.ConfigureGroup, so the child leads its own
//     group and everything it starts inherits that group id. The group
//     survives the reparent; the chain does not.
//   - WORKSPACE TIE-BREAKER. When multiple threads share a multiplexed
//     provider process (same PID/PGID), the listener's working directory
//     disambiguates which thread owns it by deepest matching WorkDir prefix.
//     If multiple distinct threads tie or CWD is unavailable, attribution
//     declines rather than guessing.
//
// Neither rule looks at a process NAME. A name match would claim a dev
// server the person started in their own terminal before the app existed,
// which is exactly the case the "seen" source and the Allow port action
// are for.

// maxAncestorDepth bounds the parent walk. A real chain is a handful of
// links; the bound is what stops a hand-built or corrupted map with a
// cycle in it from spinning, and it is a cheap invariant rather than a
// guess about how deep a tree can get.
const maxAncestorDepth = 64

// attribute reports the thread that owns this listener, if one does.
func attribute(l listener, owners []Owner, parents map[int]int) (string, bool) {
	if l.PID <= 0 || len(owners) == 0 {
		return "", false
	}

	byPID := make(map[int][]Owner, len(owners))
	byPGID := make(map[int][]Owner, len(owners))
	for _, owner := range owners {
		if owner.ThreadID == "" {
			continue
		}
		if owner.PID > 0 {
			byPID[owner.PID] = append(byPID[owner.PID], owner)
		}
		if owner.PGID > 0 {
			byPGID[owner.PGID] = append(byPGID[owner.PGID], owner)
		}
	}

	// The group first: it is one map probe and it is the rule that holds
	// in both shapes, since a process that has NOT reparented is still in
	// its group.
	if l.PGID > 0 {
		if candidates := byPGID[l.PGID]; len(candidates) > 0 {
			return pickOwner(l.CWD, candidates)
		}
	}

	pid := l.PID
	for depth := 0; depth < maxAncestorDepth && pid > 1; depth++ {
		if candidates := byPID[pid]; len(candidates) > 0 {
			return pickOwner(l.CWD, candidates)
		}
		next, ok := parents[pid]
		if !ok || next == pid {
			return "", false
		}
		pid = next
	}
	return "", false
}

// pickOwner resolves one owning thread from candidates that share a PID or
// PGID. A single distinct thread always wins directly; multiple threads break
// ties by the deepest WorkDir ancestor of listenerCWD.
func pickOwner(listenerCWD string, candidates []Owner) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	firstThread := candidates[0].ThreadID
	unique := true
	for _, c := range candidates[1:] {
		if c.ThreadID != firstThread {
			unique = false
			break
		}
	}
	if unique {
		return firstThread, true
	}

	cwd := normalizePathForMatch(listenerCWD)
	if cwd == "" {
		return "", false
	}
	bestLen := -1
	bestThread := ""
	tied := false
	for _, c := range candidates {
		dir := normalizePathForMatch(c.WorkDir)
		if dir == "" || !pathWithinDir(cwd, dir) {
			continue
		}
		switch {
		case len(dir) > bestLen:
			bestLen = len(dir)
			bestThread = c.ThreadID
			tied = false
		case len(dir) == bestLen && c.ThreadID != bestThread:
			tied = true
		}
	}
	if bestThread == "" || tied {
		return "", false
	}
	return bestThread, true
}

func normalizePathForMatch(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	cleaned := filepath.Clean(p)
	// On macOS, /var and /tmp symlink to /private/var and /private/tmp;
	// lsof often reports /private/... while thread WorkDir holds /tmp/...
	if strings.HasPrefix(cleaned, "/private/") && (strings.HasPrefix(cleaned, "/private/tmp") || strings.HasPrefix(cleaned, "/private/var")) {
		cleaned = strings.TrimPrefix(cleaned, "/private")
	}
	return cleaned
}

func pathWithinDir(path, dir string) bool {
	if path == dir {
		return true
	}
	if dir == string(os.PathSeparator) {
		return strings.HasPrefix(path, dir)
	}
	return strings.HasPrefix(path, dir+string(os.PathSeparator))
}
