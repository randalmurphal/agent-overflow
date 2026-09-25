package sessionfork

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrSessionEmpty is returned when the source JSONL has zero forkable
// entries (no transcript types, or every entry is a sidechain).
var ErrSessionEmpty = errors.New("sessionfork: source session has no messages to fork")

// ErrMessageNotFound is returned when upToMessageUUID does not appear in
// the source transcript.
var ErrMessageNotFound = errors.New("sessionfork: upToMessageUUID not found in source")

// WriteForkFileForLastKeptTurn opens srcPath ONCE, parses the
// transcript in memory, computes the slice point at the end of
// lastKeptTurn (0-indexed) via the ordinal walk, then writes the
// new <newID>.jsonl. Use this only as the fallback when no
// `provider_item_id` is stored on the user_text item; prefer
// `WriteForkFileForUserMessageUUID` because the ordinal walk
// over-counts synthetic user-role entries (`isCompactSummary`,
// `isMeta`, etc.; see `findmessage.go::isRealUserPrompt` for the
// filter the walk applies).
//
// lastKeptTurn < 0 means clear the session entirely: the function
// returns ErrSessionEmpty so the caller can wire the
// no-history-to-keep path explicitly.
func WriteForkFileForLastKeptTurn(
	srcPath string,
	lastKeptTurn int,
	customTitle string,
) (newSessionID string, newPath string, err error) {
	if lastKeptTurn < 0 {
		return "", "", ErrSessionEmpty
	}
	return writeForkFileFromTranscript(srcPath, "", customTitle, func(transcript []map[string]any) (string, error) {
		return sliceUUIDInTranscript(transcript, lastKeptTurn+1)
	})
}

// WriteForkFileFullTranscript opens srcPath ONCE, parses the transcript
// in memory, and writes a new <newID>.jsonl containing the entire
// transcript (no slice). Used as the slice-at-EOF fallback for revert
// when the anchor message is missing from the JSONL (the common cause:
// the Claude subprocess died before persisting the user's latest
// prompt). From Claude's perspective the JSONL is already in the right
// state — the missing user prompt was never seen. Cloning preserves
// the new-session-id contract the revert pipeline depends on, so the
// thread row's SessionRef advances and the composer's rehydration of
// the missing message via AO's DB stays in lockstep.
func WriteForkFileFullTranscript(
	srcPath string,
	customTitle string,
) (newSessionID string, newPath string, err error) {
	return writeForkFileFromTranscript(srcPath, "", customTitle, func(_ []map[string]any) (string, error) {
		// Empty anchor instructs buildLines to skip slicing and clone
		// the full transcript.
		return "", nil
	})
}

// WriteForkFileForUserMessageUUID opens srcPath ONCE, parses the
// transcript in memory, slices through the parent of the user
// message identified by upToUserMessageUUID, then writes the new
// <newID>.jsonl. This is the structural fix for the ordinal-walk
// off-by-N bug — by matching on the Claude-assigned user message
// UUID stored on the AO `user_text` row's `meta.provider_item_id`
// (or its anchor's `provider_user_message_id`), the slice point
// is immune to any number of synthetic user-role entries between
// real prompts. The uuid matches a real `type:"user"` entry or a
// `queued_command` attachment's `source_uuid`, the shape the CLI
// persists for a queued message it consumed mid-loop (see
// entryForUserMessageUUIDInTranscript).
//
// Returns `ErrMessageNotFound` when upToUserMessageUUID appears in
// neither shape. Callers should treat that as a hard error rather than
// silently falling back to the ordinal walk; a wrong-source revert is
// worse than no revert.
//
// Returns `ErrSessionEmpty` when the message is the very first real
// prompt in the transcript — mirrors `SliceUUIDForLastKeptTurn(-1)`
// so `revertClaudeThreadToMessage` can route through its
// "anchor.TurnIndex == 0" branch identically.
//
// One anchored shape rewinds further than the message's parent: a
// successful `/compact` command echo, whose effects (compact_boundary
// + summary + caveat) the CLI writes as the echo's ANCESTORS. There
// the slice anchor is the boundary's logicalParentUuid — the
// pre-compact leaf — so reverting to the /compact message actually
// undoes the compaction. See compactCommandSliceAnchor.
func WriteForkFileForUserMessageUUID(
	srcPath string,
	upToUserMessageUUID string,
	customTitle string,
) (newSessionID string, newPath string, err error) {
	if upToUserMessageUUID == "" {
		return "", "", fmt.Errorf("sessionfork: empty user message uuid")
	}
	return writeForkFileFromTranscript(srcPath, "", customTitle, func(transcript []map[string]any) (string, error) {
		anchored, err := entryForUserMessageUUIDInTranscript(transcript, upToUserMessageUUID)
		if err != nil {
			return "", err
		}
		upToParentUUID, _ := anchored["parentUuid"].(string)
		if upToParentUUID == "" {
			return "", ErrSessionEmpty
		}
		return compactCommandSliceAnchor(transcript, anchored, upToParentUUID), nil
	})
}

// ForkCut names the inputs of WriteForkFileThroughUUID.
//
// A struct rather than four positional strings because two of them are
// paths: transposing SourcePath and DestDir compiles, and the fork
// lands in a directory no resume will ever look in — a failure whose
// only symptom is "No conversation found" much later. Named fields
// make that particular mistake unwriteable.
type ForkCut struct {
	// SourcePath is the transcript to cut from.
	SourcePath string
	// DestDir is the project slug directory the new transcript lands
	// in; "" means "beside the source", which is where every other fork
	// entry point writes. It exists because Claude resolves `--resume`
	// against the slug of the CURRENT cwd (see RelocateSession): a
	// caller whose thread has moved workspace since the source was
	// written must land the cut under the DESTINATION workspace's slug,
	// or the resume looks for it under a slug it will never be written
	// to and hard-fails with "No conversation found". Use
	// WorkspaceProjectDir to compute it, and leave it empty when that
	// is unresolvable — beside the source is the pre-existing behaviour
	// and never worse than not writing at all.
	DestDir string
	// LastKeptUUID is the entry the cut keeps through, INCLUSIVE.
	LastKeptUUID string
	// Title is the fork's custom title; empty derives a default.
	Title string
}

// WriteForkFileThroughUUID opens cut.SourcePath ONCE, parses the
// transcript in memory, and writes a new <newID>.jsonl keeping
// everything through the entry whose uuid is cut.LastKeptUUID,
// INCLUSIVE. The last-kept row may be ANY transcript type: a queued
// message's parent is usually an assistant entry.
//
// Used by the already-cut revert retry: the anchor row is gone (a
// prior slice cut exactly at it) but its anchored PARENT survives.
// Keeping through the parent — rather than cloning the file whole —
// also cuts any rows appended after the failed revert, which a whole
// clone would silently resurrect into the resumed session (round-5,
// R5-6). It is also how an imported Claude branch is cut its own
// session file (`app_session_import_branch.go`), which is the caller
// ForkCut.DestDir exists for.
//
// Returns ErrMessageNotFound when LastKeptUUID matches no kept entry;
// callers fail loudly rather than guess.
func WriteForkFileThroughUUID(
	cut ForkCut,
) (newSessionID string, newPath string, err error) {
	if cut.LastKeptUUID == "" {
		return "", "", fmt.Errorf("sessionfork: empty last-kept uuid")
	}
	return writeForkFileFromTranscript(cut.SourcePath, cut.DestDir, cut.Title, func(_ []map[string]any) (string, error) {
		// buildLines reports ErrMessageNotFound for an absent uuid.
		return cut.LastKeptUUID, nil
	})
}

// writeForkFileFromTranscript is the shared open/parse/build/write
// pipeline behind every WriteForkFile* entry point. computeAnchor
// receives the parsed transcript and returns the upToMessageUUID
// passed to buildLines (or "" for a full-transcript clone). Any
// error returned by computeAnchor propagates verbatim so callers can
// inspect sentinels like ErrSessionEmpty / ErrMessageNotFound /
// ErrUserTurnOutOfRange.
//
// destDir == "" writes beside the source. See ForkCut.DestDir for when
// a caller supplies one.
func writeForkFileFromTranscript(
	srcPath string,
	destDir string,
	customTitle string,
	computeAnchor func(transcript []map[string]any) (string, error),
) (newSessionID string, newPath string, err error) {
	srcSessionID := SessionIDFromPath(srcPath)

	f, err := os.Open(srcPath)
	if err != nil {
		return "", "", fmt.Errorf("sessionfork: open source: %w", err)
	}
	defer f.Close()

	transcript, contentReplacements, err := ParseTranscript(f, srcSessionID)
	if err != nil {
		return "", "", fmt.Errorf("sessionfork: parse transcript: %w", err)
	}

	upToMessageUUID, err := computeAnchor(transcript)
	if err != nil {
		return "", "", err
	}

	newID, lines, err := buildLines(transcript, contentReplacements, srcSessionID, upToMessageUUID, customTitle)
	if err != nil {
		return "", "", err
	}
	return writeForkOutput(srcPath, destDir, newID, lines)
}

// writeForkOutput writes the JSONL lines to <destDir>/<newID>.jsonl
// atomically (O_EXCL), defaulting destDir to the source's own
// directory. On any partial-write failure the output file is removed so
// disk and the caller's notion of the new session stay in lockstep.
func writeForkOutput(srcPath, destDir, newID string, lines []string) (string, string, error) {
	dir := destDir
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Dir(srcPath)
	}
	// A destination slug dir need not exist yet: a thread that changed
	// workspace before its first send has never had Claude write there.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("sessionfork: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, newID+".jsonl")

	// O_EXCL: refuse to overwrite an existing file. UUIDv4 collisions are
	// vanishingly rare; if it happens, fail rather than clobber.
	fd, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("sessionfork: create %s: %w", out, err)
	}
	w := bufio.NewWriter(fd)
	for _, line := range lines {
		if _, err := w.WriteString(line); err != nil {
			fd.Close()
			_ = os.Remove(out)
			return "", "", fmt.Errorf("sessionfork: write line: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			fd.Close()
			_ = os.Remove(out)
			return "", "", fmt.Errorf("sessionfork: write newline: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		fd.Close()
		_ = os.Remove(out)
		return "", "", fmt.Errorf("sessionfork: flush: %w", err)
	}
	if err := fd.Close(); err != nil {
		_ = os.Remove(out)
		return "", "", fmt.Errorf("sessionfork: close: %w", err)
	}
	return newID, out, nil
}

// parentUUIDForUserMessageUUIDInTranscript returns the parentUuid of
// the entry entryForUserMessageUUIDInTranscript matches.
func parentUUIDForUserMessageUUIDInTranscript(transcript []map[string]any, messageUUID string) (string, error) {
	entry, err := entryForUserMessageUUIDInTranscript(transcript, messageUUID)
	if err != nil {
		return "", err
	}
	parent, _ := entry["parentUuid"].(string)
	return parent, nil
}

// entryForUserMessageUUIDInTranscript walks an already-parsed
// transcript and returns the entry carrying the user message
// identified by messageUUID. It operates on the in-memory transcript
// slice so the fork pipeline doesn't re-open the file.
//
// The CLI persists a queued message under one of two shapes depending
// on WHEN it consumed it (claude-wire.md §"Queued-message consumption"):
//
//   - Consumed at turn pickup: a real `type:"user"` entry whose
//     top-level `uuid` is the AO-minted send uuid verbatim.
//   - Consumed mid-loop (queued while a turn was running): a
//     `type:"attachment"` entry with a CLI-minted uuid; the AO uuid
//     survives only as `attachment.source_uuid` on the
//     `queued_command` attachment body.
//
// Match priority: direct user-entry uuid, then queued_command
// attachment source_uuid. Either way the matched entry's own
// parentUuid is the last kept row. There is no `isRealUserPrompt`
// filter and no counting, so this path stays structurally immune to the
// synthetic-entry over-count bug that motivated it. A fork slice keeps
// every uuid, so an id stored against any earlier generation of the
// session still names the same entry here.
//
// Returns ErrMessageNotFound when messageUUID appears in neither shape.
func entryForUserMessageUUIDInTranscript(transcript []map[string]any, messageUUID string) (map[string]any, error) {
	if messageUUID == "" {
		return nil, fmt.Errorf("sessionfork: empty user message uuid")
	}
	var attachmentEntry map[string]any
	for _, entry := range transcript {
		switch t, _ := entry["type"].(string); t {
		case "user":
			if u, _ := entry["uuid"].(string); u == messageUUID {
				return entry, nil
			}
		case "attachment":
			if attachmentEntry != nil {
				continue
			}
			att, ok := entry["attachment"].(map[string]any)
			if !ok {
				continue
			}
			if at, _ := att["type"].(string); at != "queued_command" {
				continue
			}
			if su, _ := att["source_uuid"].(string); su == messageUUID {
				attachmentEntry = entry
			}
		}
	}
	if attachmentEntry != nil {
		return attachmentEntry, nil
	}
	return nil, fmt.Errorf("%w: user message uuid %q", ErrMessageNotFound, messageUUID)
}

// sliceUUIDInTranscript walks an already-parsed transcript and returns
// the parentUuid of the userTurnIndex-th (0-indexed) real user prompt.
// Mirrors FindUUIDBeforeUserTurn but operates on the in-memory slice
// so callers that already have the transcript don't have to re-read
// the file.
//
// Returns ("", nil) when userTurnIndex == 0 (no preceding entry).
// Returns ErrUserTurnAtTranscriptEnd when userTurnIndex == count
// (recoverable: slice point is past the last persisted prompt,
// callers can fall back to a whole-transcript copy). Returns
// ErrUserTurnOutOfRange for any larger gap.
func sliceUUIDInTranscript(transcript []map[string]any, userTurnIndex int) (string, error) {
	if userTurnIndex == 0 {
		return "", nil
	}
	count := 0
	for _, entry := range transcript {
		if !isRealUserPrompt(entry) {
			continue
		}
		if count == userTurnIndex {
			parent, _ := entry["parentUuid"].(string)
			if parent == "" {
				return "", fmt.Errorf("sessionfork: user turn %d has no parentUuid", userTurnIndex)
			}
			return parent, nil
		}
		count++
	}
	if userTurnIndex == count {
		return "", fmt.Errorf("%w: requested %d, found %d", ErrUserTurnAtTranscriptEnd, userTurnIndex, count)
	}
	return "", fmt.Errorf("%w: requested %d, found %d", ErrUserTurnOutOfRange, userTurnIndex, count)
}
