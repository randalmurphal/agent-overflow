package sessionimport

import (
	"fmt"
	"sort"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider/claude/sessionfork"
)

// Branch is one root-to-leaf path through a transcript's conversation
// DAG — i.e. one importable AO thread. A transcript with N leaves
// produces N threads, which is why enumeration (rather than picking the
// active chain) is the whole job of this file.
type Branch struct {
	LeafUUID string
	// Title is the leaf's `last-prompt` record when the transcript wrote
	// one, else the branch's own last user prompt. Used to disambiguate
	// sibling threads when a session imports as more than one.
	Title string
	// Chain is the branch in causal order, root first.
	Chain          []Row
	LastActivityAt int64
}

// Warning codes emitted by BuildBranches.
const (
	WarnOrphanedToolResult = "orphaned-tool-result"
	WarnInlineSidechain    = "inline-sidechain-rows"
	WarnDuplicateUUID      = "duplicate-uuid"
	WarnNoLeaf             = "no-leaf"
)

// BuildBranches reconstructs the conversation DAG of one transcript and
// returns every branch in it.
//
// rows are every row the transcript scan admitted (sessionfork's rule —
// see that package's "Shared reading surface"), in file order. They are
// normally SKELETONS: this runs before any line is decoded, because a
// real transcript is too large to hold decoded and only the branch that
// is actually being converted needs its bodies. leafTitles maps a leaf
// uuid to the `last-prompt` text the CLI appended for it; pass nil when
// the file carried none.
//
// Parent resolution goes through sessionfork.ResolveParentUUID (which
// skips progress ancestors) — shared rather than reimplemented precisely
// because an importer walking raw `parentUuid` would break its chain on
// every progress row. The compaction fallback reads `logicalParentUuid`
// directly, which is what sessionfork.ResolveLogicalParent does for a
// plain string value; there is no walk to share.
//
// Why this does not reuse claudeBranchIndex (sessionleaf_branch.go):
// that index answers "which single chain will `claude --resume` accept",
// and its activeChain walk returns exactly one chain by construction.
// Import needs every leaf, and it must not perturb the live resume path
// — so it builds its own index and keeps its own semantics.
func BuildBranches(rows []Row, leafTitles map[string]string) ([]Branch, []importir.Warning) {
	var warnings []importir.Warning

	// Index over EVERY row, progress included: the parent walk treats
	// progress rows as transparent and has to find them to do it.
	allByUUID := make(map[string]Row, len(rows))
	for _, row := range rows {
		if row.UUID == "" {
			continue
		}
		if _, dup := allByUUID[row.UUID]; !dup {
			allByUUID[row.UUID] = row
		}
	}

	var (
		admitted        []Row
		byUUID          = make(map[string]Row, len(rows))
		skippedSidechan int
		duplicates      int
	)
	for _, row := range rows {
		switch {
		case row.UUID == "":
			continue
		case row.Type == "progress":
			// Not conversation content, and ResolveParent already treats
			// it as transparent — keeping it out of the DAG is what makes
			// that transparency consistent for children AND leaves.
			continue
		case row.IsSidechain:
			// Old CLIs inlined subagent transcripts into the main file.
			// They have their own parent graph and would enumerate as
			// phantom branches; today's subagent content is joined from
			// `subagents/agent-*.jsonl` instead (see subagents.go).
			skippedSidechan++
			continue
		}
		if _, dup := byUUID[row.UUID]; dup {
			duplicates++
			continue
		}
		byUUID[row.UUID] = row
		admitted = append(admitted, row)
	}

	if skippedSidechan > 0 {
		warnings = append(warnings, importir.Warning{
			Code:    WarnInlineSidechain,
			Message: fmt.Sprintf("Skipped %d inline subagent row(s); this transcript predates separate subagent files.", skippedSidechan),
		})
	}
	if duplicates > 0 {
		warnings = append(warnings, importir.Warning{
			Code:    WarnDuplicateUUID,
			Message: fmt.Sprintf("Skipped %d row(s) that reused an existing id.", duplicates),
		})
	}
	if len(admitted) == 0 {
		return nil, warnings
	}

	parentOf := make(map[string]string, len(admitted))
	childCount := make(map[string]int, len(admitted))
	reattached := 0
	for _, row := range admitted {
		parent := resolveParentUUID(row, allByUUID, byUUID)
		if parent == "" {
			if recovered, ok := reattachOrphanedToolResult(row, byUUID); ok {
				parent = recovered
				reattached++
			}
		}
		parentOf[row.UUID] = parent
		if parent != "" {
			childCount[parent]++
		}
	}
	if reattached > 0 {
		warnings = append(warnings, importir.Warning{
			Code:    WarnOrphanedToolResult,
			Message: fmt.Sprintf("Re-attached %d tool result(s) whose recorded parent row is missing.", reattached),
		})
	}

	branches := make([]Branch, 0, 1)
	for _, row := range admitted {
		if childCount[row.UUID] > 0 {
			continue
		}
		chain, ok := walkChain(row, byUUID, parentOf)
		if !ok {
			continue
		}
		branches = append(branches, newBranch(chain, leafTitles))
	}
	if len(branches) == 0 {
		warnings = append(warnings, importir.Warning{
			Code:    WarnNoLeaf,
			Message: "No conversation branch could be reconstructed from this transcript.",
		})
		return nil, warnings
	}

	sort.Slice(branches, func(i, j int) bool {
		li := branches[i].Chain[len(branches[i].Chain)-1]
		lj := branches[j].Chain[len(branches[j].Chain)-1]
		return li.Index < lj.Index
	})
	return branches, warnings
}

// resolveParentUUID returns a row's effective parent id, or "" when the
// row is a chain root (or its parent is not an admitted row of this file).
//
// allByUUID indexes every row the file admitted, progress included, so the
// walk can see through them; admitted is the DAG's own membership set.
func resolveParentUUID(row Row, allByUUID map[string]Row, admitted map[string]Row) string {
	resolved := sessionfork.ResolveParentUUID(row.ParentUUID, func(uuid string) (sessionfork.TranscriptParent, bool) {
		parent, ok := allByUUID[uuid]
		if !ok {
			return sessionfork.TranscriptParent{}, false
		}
		return sessionfork.TranscriptParent{ParentUUID: parent.ParentUUID, Type: parent.Type}, true
	})
	if resolved != "" {
		if _, known := admitted[resolved]; known {
			return resolved
		}
	}
	// `compact_boundary` rows carry parentUuid:null and chain through
	// logicalParentUuid to the pre-compact leaf. The backpointer is a
	// verbatim uuid (ResolveLogicalParent passes an unknown one through),
	// so membership is checked here — an unresolvable backpointer must
	// read as "root", not as an edge to a row that does not exist.
	if row.LogicalParentUUID != "" {
		if _, known := admitted[row.LogicalParentUUID]; known {
			return row.LogicalParentUUID
		}
	}
	return ""
}

// reattachOrphanedToolResult recovers a parallel tool_result whose
// recorded parent is not in this file.
//
// Claude emits parallel tool_uses as siblings of one assistant message,
// and each result row records the assistant row it answers in
// `sourceToolAssistantUUID`. When the recorded `parentUuid` points at a
// sibling result that was never written (crash, partial copy), the row
// would otherwise become its own root and enumerate as a phantom branch.
// The assistant row it names is the correct attachment point.
func reattachOrphanedToolResult(row Row, byUUID map[string]Row) (string, bool) {
	source := row.SourceToolAssistantUUID
	if source == "" || source == row.UUID {
		return "", false
	}
	if _, ok := byUUID[source]; !ok {
		return "", false
	}
	return source, true
}

// walkChain walks leaf → root and returns the chain root-first. A cycle
// (only reachable from a corrupt file) terminates the walk and drops the
// branch rather than looping.
func walkChain(leaf Row, byUUID map[string]Row, parentOf map[string]string) ([]Row, bool) {
	var reversed []Row
	seen := make(map[string]struct{}, 8)
	current := leaf
	for {
		if _, loop := seen[current.UUID]; loop {
			return nil, false
		}
		seen[current.UUID] = struct{}{}
		reversed = append(reversed, current)

		parentID := parentOf[current.UUID]
		if parentID == "" {
			break
		}
		parent, ok := byUUID[parentID]
		if !ok {
			break
		}
		current = parent
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, true
}

func newBranch(chain []Row, leafTitles map[string]string) Branch {
	leaf := chain[len(chain)-1]
	branch := Branch{
		LeafUUID: leaf.UUID,
		Chain:    chain,
		Title:    leafTitles[leaf.UUID],
	}
	for _, row := range chain {
		if row.Timestamp > branch.LastActivityAt {
			branch.LastActivityAt = row.Timestamp
		}
	}
	if branch.Title == "" {
		// Empty on a skeleton chain — the text is in the line bodies, which
		// pass 1 does not hold. LoadedSession.ConvertBranch fills it once
		// the branch it is converting has been decoded.
		branch.Title = lastUserPromptText(chain)
	}
	return branch
}

// lastUserPromptText is the fallback branch title: the newest thing the
// user actually said on this branch. It answers "" for a skeleton chain,
// whose rows carry no decoded body.
func lastUserPromptText(chain []Row) string {
	for i := len(chain) - 1; i >= 0; i-- {
		text, ok := userPromptText(chain[i])
		if !ok {
			continue
		}
		if text = truncateRunes(text, maxFirstPromptRunes); text != "" {
			return text
		}
	}
	return ""
}
