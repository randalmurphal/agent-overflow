package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	workflowrunner "agent-overflow/internal/workflow/runner"
)

// workflowAssistantTextKind is the item kind a provider's prose is persisted
// under. The literal matches the triage write path's own constant; store exports
// no item-kind vocabulary, and inventing one for a single reader would widen
// that package's surface for nothing.
const workflowAssistantTextKind = "assistant_text"

// Per-attempt narrative files: settling the ones an agent could not write, and
// refusing to point at ones that do not exist.
//
// The narrative is the single human-readable account of what one phase attempt,
// fan-out unit, or join did. A writing agent element is told to write the file
// and can; a `read-only` one runs in a session that denies every file write
// (D22, spec §9), so it is asked for the account in its envelope's `narrative`
// control field and the runner writes the file from there. A session that
// ignored the instruction still gets the D39 fallback below. Without any of
// this, a completed read-only run left every attempt directory empty, the wake
// pointed at a path nothing created, and the triage seed read "narrative
// unavailable".

// settleAttemptNarrative guarantees an accepted agent turn has left a narrative
// file, from whichever source it produced one:
//
//  1. the file the element wrote itself — always wins, never touched;
//  2. `authored`, the `narrative` field of its accepted envelope, written
//     verbatim because the element deliberately put it there;
//  3. the D39 recovery from the session's final assistant text, marked with
//     RecoveredNarrativeHeader because the system reconstructed it.
//
// One existence check and one O_EXCL write serve all three, so the "an authored
// account always beats a reconstructed one" rule cannot be stated twice and
// drift. A turn with no account at any tier writes nothing — absence stays
// absence rather than becoming a file that says nothing.
//
// Failing to settle never changes the run's outcome; it is reported the way
// every other post-success bookkeeping failure is, because an outcome the engine
// has already accepted must not be re-decided by a filesystem error.
func (r *workflowAppRunner) settleAttemptNarrative(attempt *workflowAttempt, authored string, envelope json.RawMessage) {
	if err := r.writeAttemptNarrative(attempt, authored, envelope); err != nil {
		log.Printf("workflow narrative %s: %v", workflowRunKey(attempt.key), err)
		r.app.emit("workflow:error", map[string]any{
			"itemId": attempt.key.ItemID,
			"error":  "workflow narrative could not be written from the phase's final envelope; inspect local diagnostics",
		})
	}
}

func (r *workflowAppRunner) writeAttemptNarrative(attempt *workflowAttempt, authored string, envelope json.RawMessage) error {
	if attempt.narrativePath == "" {
		return errors.New("the attempt carries no narrative path")
	}
	present, err := workflowNarrativeExists(attempt.narrativePath)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	var narrative string
	if strings.TrimSpace(authored) != "" {
		// No RecoveredNarrativeHeader: the element chose to put this account in
		// its envelope, so it is authored exactly as a file it wrote would be.
		narrative = strings.TrimRight(authored, "\n") + "\n"
	} else {
		texts, err := r.app.threadAssistantTexts(attempt.threadID)
		if err != nil {
			return err
		}
		recovered, ok := workflowrunner.RecoverNarrative(texts, envelope)
		if !ok {
			return nil
		}
		narrative = recovered
	}
	if err := os.MkdirAll(filepath.Dir(attempt.narrativePath), appPrivateDirPerm); err != nil {
		return fmt.Errorf("create narrative directory: %w", err)
	}
	// O_EXCL rather than a plain write: the existence check above is a moment
	// earlier, and losing a race against an agent's own file would destroy the
	// better of the two accounts.
	file, err := os.OpenFile(attempt.narrativePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, appSensitiveFilePerm)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create narrative %q: %w", attempt.narrativePath, err)
	}
	_, writeErr := file.WriteString(narrative)
	return errors.Join(writeErr, file.Close())
}

// threadAssistantTexts returns one thread's top-level assistant prose, oldest
// first. Subagent rows (`parent_id` set) are excluded: an element's narrative is
// what it said, not what something it launched said.
func (a *App) threadAssistantTexts(threadID string) ([]string, error) {
	if threadID == "" {
		return nil, errors.New("thread id is required")
	}
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, 4)
	for _, item := range items {
		if item.Kind != workflowAssistantTextKind || item.ParentID != "" || item.Summary == "" {
			continue
		}
		texts = append(texts, item.Summary)
	}
	return texts, nil
}

// workflowNarrativeLookup resolves one phase attempt's narrative path and
// reports whether the file is there. The path is returned either way, because a
// reader asking for an account that was never written has to be told what was
// looked for — "no matches found" is what sends an agent hand-globbing the run
// directory. Every narrative resolution in the app goes through this or its unit
// counterpart, so nothing re-derives the path shape.
func workflowNarrativeLookup(dataRoot, itemID, phaseID string, attempt int) (string, bool, error) {
	path, err := workflowrunner.NarrativePath(dataRoot, itemID, phaseID, attempt)
	if err != nil {
		return "", false, err
	}
	present, err := workflowNarrativeExists(path)
	return path, present, err
}

// workflowUnitNarrativeLookup is workflowNarrativeLookup for one fan-out unit
// try. The try number is part of the path: a retried unit keeps its row but
// writes a new account, and the previous one stays readable as evidence.
func workflowUnitNarrativeLookup(
	dataRoot, itemID, phaseID string, attempt int, unitID string, unitAttempt int,
) (string, bool, error) {
	path, err := workflowrunner.UnitNarrativePath(dataRoot, itemID, phaseID, attempt, unitID, unitAttempt)
	if err != nil {
		return "", false, err
	}
	present, err := workflowNarrativeExists(path)
	return path, present, err
}

// workflowNarrativeReference resolves one attempt's narrative path for a wake
// reference, and reports false when nothing wrote the file. A reference is a
// pointer an agent opens: a path that does not resolve is worse than no
// reference at all, because the agent spends a tool call learning that.
func workflowNarrativeReference(dataRoot, itemID, phaseID string, attempt int) (string, bool) {
	path, present, err := workflowNarrativeLookup(dataRoot, itemID, phaseID, attempt)
	if err != nil || !present {
		return "", false
	}
	return path, true
}

// workflowNarrativeExists reports whether a narrative file is there to be read.
// A path occupied by something other than a regular file is reported absent: it
// is not a narrative, and neither the recovery nor a reader can treat it as one.
func workflowNarrativeExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect narrative %q: %w", path, err)
	}
	return info.Mode().IsRegular(), nil
}
