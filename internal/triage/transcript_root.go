package triage

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// The transcript root (claude-wire.md §E6).
//
// Resuming an idle async agent rebinds the CLI's task lifecycle onto the
// resuming tool's own call — the CARRIER — while the agent's own
// conversation tree stays where it started: the CLI persists the launch
// toolUseId in the agent's metadata and keeps stamping it as
// `parent_tool_use_id` on every sidechain row of every later round.
//
// So a carrier is a LIFECYCLE row, never a transcript root. Everything
// the agent produces — in round one and in every resumed round — is
// parented to the ORIGINAL launch. Treating the carrier as a scope
// reparented 474 already-delivered round-1 rows onto it and duplicated
// 220 more under it on one live thread (2026-09-03).
//
// Three mechanisms enforce that here:
//
//   - The carrier row is stamped `transcript_root_id`, so the fact is
//     durable and answerable from one row.
//   - carrierRootRewrite makes "a row parented to a carrier"
//     unrepresentable for live events, regardless of which parser path
//     emitted them.
//   - Every scope-resolving path (the terminal replay, the identity
//     flip, the resume prompt row) resolves through transcriptRoot.

// maxTranscriptRootHops bounds the resumes_tool_use_id walk. A round-N
// carrier's `resumes_tool_use_id` names the round-(N-1) carrier, not the
// original launch, so the chain is as long as the agent has been
// resumed. The bound plus the visited set makes a corrupt (or cyclic)
// chain terminate rather than loop; falling off the end degrades to the
// task_id lookup below, which is exact.
const maxTranscriptRootHops = 16

// maxCarrierRootsPerThread bounds the per-thread carrier→root map. One
// entry per resume round; the cap only covers a pathological session.
const maxCarrierRootsPerThread = 256

// transcriptRootOrSelf resolves the row whose scope an agent's rows
// belong to. A non-carrier is its own root and costs no store read.
func (r *Router) transcriptRootOrSelf(threadID string, launch store.Item) (store.Item, error) {
	root, found, err := r.transcriptRoot(threadID, launch)
	if err != nil {
		return store.Item{}, err
	}
	if !found {
		return launch, nil
	}
	return root, nil
}

// transcriptRoot resolves a resume carrier's transcript root and reports
// whether one was found. Resolution order, strongest evidence first:
//
//  1. the carrier's own `transcript_root_id` stamp (the parser knew the
//     original binding, or an earlier resolution stamped it);
//  2. the `resumes_tool_use_id` chain, walked to its end — a round-3
//     carrier points at the round-2 carrier, not at the launch;
//  3. `FindOriginalAgentLaunchByTaskID`, the oldest row carrying the
//     task_id with the carrier excluded. Exact, and the only path that
//     survives a parser that never saw the original launch at all.
//
// A resolved root is recorded in the thread's carrier map, which is what
// the live-event parent rewrite reads.
func (r *Router) transcriptRoot(threadID string, carrier store.Item) (store.Item, bool, error) {
	meta := DecodeToolStartMeta([]byte(carrier.Meta))
	if !isResumeCarrierMeta(meta) {
		return store.Item{}, false, nil
	}

	visited := map[string]struct{}{carrier.ID: {}}
	next := strings.TrimSpace(firstNonEmptyString(meta.TranscriptRootID, meta.ResumesToolUseID))
	current := carrier
	for hops := 0; next != "" && hops < maxTranscriptRootHops; hops++ {
		if _, seen := visited[next]; seen {
			log.Printf("triage: transcript-root chain for %s/%s revisits %s; falling back to the task_id lookup",
				threadID, carrier.ID, next)
			break
		}
		visited[next] = struct{}{}
		candidate, found, err := r.store.GetThreadItem(threadID, next)
		if err != nil {
			return store.Item{}, false, fmt.Errorf("triage: transcript root lookup %s/%s: %w", threadID, next, err)
		}
		if !found || candidate.Kind != itemKindToolCall {
			break
		}
		current = candidate
		candidateMeta := DecodeToolStartMeta([]byte(candidate.Meta))
		// Reached a row that is not itself a carrier: this is the root.
		next = strings.TrimSpace(firstNonEmptyString(candidateMeta.TranscriptRootID, candidateMeta.ResumesToolUseID))
	}
	if current.ID != carrier.ID {
		r.rememberCarrierRoot(threadID, carrier.ID, current.ID)
		return current, true, nil
	}

	if meta.TaskID == "" {
		return store.Item{}, false, nil
	}
	original, found, err := r.store.FindOriginalAgentLaunchByTaskID(threadID, meta.TaskID, carrier.ID)
	if err != nil {
		return store.Item{}, false, fmt.Errorf("triage: transcript root by task id %s/%s: %w", threadID, meta.TaskID, err)
	}
	if !found || original.Kind != itemKindToolCall {
		return store.Item{}, false, nil
	}
	r.rememberCarrierRoot(threadID, carrier.ID, original.ID)
	return original, true, nil
}

// isResumeCarrierMeta recognizes a resume carrier from the stamps the
// parser writes ONLY on its §E6 resume-detection path (parse_system.go),
// so an ordinary launch is answered without a store read.
func isResumeCarrierMeta(m ToolStartMeta) bool {
	return m.TranscriptRootID != "" || m.ResumesToolUseID != "" ||
		m.Description != "" || m.SubagentType != ""
}

// rememberCarrierRoot records a resolved carrier→root edge for the
// live-event rewrite. Bounded per thread and swept with the threadState.
func (r *Router) rememberCarrierRoot(threadID, carrierID, rootID string) {
	if threadID == "" || carrierID == "" || rootID == "" || carrierID == rootID {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(threadID)
	if st.carrierRoots == nil {
		st.carrierRoots = make(map[string]string, 1)
	}
	if _, known := st.carrierRoots[carrierID]; !known && len(st.carrierRoots) >= maxCarrierRootsPerThread {
		log.Printf("triage: carrier-root map for %s is full (%d); not recording %s",
			threadID, maxCarrierRootsPerThread, carrierID)
		return
	}
	st.carrierRoots[carrierID] = rootID
}

// carrierRootRewrite answers "is this parent a resume carrier?" for one
// live event. The empty-parent short-circuit runs before the lock, so a
// top-level event — every event on a thread with no agents — pays a
// string comparison and nothing else.
func (r *Router) carrierRootRewrite(threadID, parentToolUseID string) string {
	if parentToolUseID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil || len(st.carrierRoots) == 0 {
		return ""
	}
	return st.carrierRoots[parentToolUseID]
}

// stampTranscriptRootOnCarrier records the resolved root on the carrier's
// own meta patch when the parser could not (the reconnect edge). One
// stamp makes every later reader — this process, a restarted one, and the
// frontend — answer "is this a carrier" from the row alone.
func stampTranscriptRootOnCarrier(patch map[string]string, carrierMeta ToolStartMeta, rootID string) {
	if rootID == "" || carrierMeta.TranscriptRootID != "" {
		return
	}
	patch[provider.MetaTranscriptRootIDKey] = rootID
}
