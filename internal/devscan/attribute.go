package devscan

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

	byPID := make(map[int]string, len(owners))
	byPGID := make(map[int]string, len(owners))
	for _, owner := range owners {
		if owner.ThreadID == "" {
			continue
		}
		if owner.PID > 0 {
			byPID[owner.PID] = owner.ThreadID
		}
		if owner.PGID > 0 {
			byPGID[owner.PGID] = owner.ThreadID
		}
	}

	// The group first: it is one map probe and it is the rule that holds
	// in both shapes, since a process that has NOT reparented is still in
	// its group.
	if l.PGID > 0 {
		if threadID, ok := byPGID[l.PGID]; ok {
			return threadID, true
		}
	}

	pid := l.PID
	for depth := 0; depth < maxAncestorDepth && pid > 1; depth++ {
		if threadID, ok := byPID[pid]; ok {
			return threadID, true
		}
		next, ok := parents[pid]
		if !ok || next == pid {
			return "", false
		}
		pid = next
	}
	return "", false
}
