package sessionfork

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrSubagentCopyIncomplete marks the partial-success case: the session
// transcript itself relocated (so `--resume` works), but copying its sibling
// <sessionID>/ subagent subdir failed. Callers branch on this to surface the
// degraded subagent history WITHOUT treating the relocation as a hard failure —
// the conversation is fully resumable, only subagent transcripts are partial.
var ErrSubagentCopyIncomplete = errors.New("sessionfork: subagent subdir copy incomplete")

// RelocateSession ensures sessionID's transcript JSONL (and its sibling
// <sessionID>/ subagent subdir, when present) lives under destWorkspace's
// Claude project slug, so `claude --resume <sessionID>` run with cwd ==
// destWorkspace resolves it. Returns (srcFile, destFile): the located source
// path and the destination path. They are equal when the transcript already
// resolves under destWorkspace (nothing moved).
//
// Claude keys session lookup on the slug of the CURRENT working directory
// (sessionStoragePortable.ts getProjectDir → sanitizePath; loadSessionFile
// reads getProjectDir(originalCwd)/<id>.jsonl). When a thread's workspace
// changes to a directory Claude never wrote the session under — worktree
// removal reattaching to the project root, or any switch/create/attach that
// moves the cwd — resume fails with "No conversation found" even though the
// JSONL is still on disk. Copying it under the new slug is what keeps resume
// working; AO never silently starts a fresh session in its place.
//
// This is the COPY half of a move: it writes the source (always the
// authoritative latest — Claude only appends under the cwd the session ran in)
// over any stale copy already at the destination, then leaves the source in
// place. The caller removes the source AFTER it commits the workspace change
// (RemoveSessionTranscript), so a hard failure here can be cleanly refused with
// the conversation still resumable from its current workspace. OVERWRITING
// rather than no-op'ing on an existing destination is load-bearing: a thread
// that visited a workspace before left a now-stale copy under that slug, and a
// no-op would resume the stale history on the return trip (silent turn loss).
//
// Returns ErrSessionFileNotFound (with srcFile/destFile empty) when the
// transcript can't be located under ANY project dir — the session is genuinely
// gone, and the caller surfaces that as resume state rather than fabricating a
// new one. Returns a hard error with destFile EMPTY when destWorkspace's exact
// slug is uncomputable (sanitized form exceeds MaxSanitizedSlugLen, where the
// CLI appends an unreproducible Bun.hash suffix) or the transcript copy fails —
// callers that can abort MUST refuse the workspace change. Returns
// ErrSubagentCopyIncomplete with destFile SET (soft) when only the subagent
// subdir copy is partial: resume works, so the move is not refused.
func RelocateSession(sessionID, fromWorkspace, destWorkspace string) (srcFile, destFile string, err error) {
	// fromWorkspace is only the primary-lookup hint; LocateSessionFile scans
	// every project dir on miss, so a now-deleted worktree path still resolves.
	src, err := LocateSessionFile(sessionID, fromWorkspace)
	if err != nil {
		return "", "", err
	}

	slug, ok, err := exactWorkspaceSlug(destWorkspace)
	if err != nil {
		return src, "", err
	}
	if !ok {
		return src, "", fmt.Errorf("sessionfork: cannot relocate %s: destination %q sanitizes beyond %d chars where Claude appends an unreproducible Bun.hash suffix", sessionID, destWorkspace, MaxSanitizedSlugLen)
	}

	pdir, err := defaultProjectsDir()
	if err != nil {
		return src, "", err
	}
	destDir := filepath.Join(pdir, slug)
	dest := filepath.Join(destDir, sessionID+".jsonl")

	// Source already lives at the destination slug — nothing to move, and
	// nothing to purge. Returning src == dest signals the caller to skip the
	// post-commit removal so it never deletes the live transcript.
	if src == dest {
		return src, dest, nil
	}

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return src, "", fmt.Errorf("sessionfork: mkdir %s: %w", destDir, err)
	}
	if err := copyFile(src, dest, 0o600); err != nil {
		return src, "", err
	}

	// Subagent transcripts referenced by this session live in a sibling
	// <sessionID>/ subdir. Copy it too so subagent history survives the move.
	// A copy failure does not unwind the (successful) transcript relocation —
	// resume already works — but it is surfaced, never swallowed.
	srcSub := filepath.Join(filepath.Dir(src), sessionID)
	if dirExists(srcSub) {
		if err := copyTree(srcSub, filepath.Join(destDir, sessionID)); err != nil {
			return src, dest, fmt.Errorf("%w: %w", ErrSubagentCopyIncomplete, err)
		}
	}
	return src, dest, nil
}

// RemoveSessionTranscript deletes a session transcript JSONL and its sibling
// <sessionID>/ subagent subdir. It is the DELETE half of a move: the caller runs
// it on the pre-move source path AFTER committing the workspace change, so the
// thread's transcript follows its cwd as a single copy instead of accumulating
// stale duplicates under every slug it has visited (which the LocateSessionFile
// fallback scan could otherwise surface as stale resume history).
//
// jsonlPath must be an absolute "<id>.jsonl" path (it comes from
// RelocateSession's located source). Absent files are not an error — removal is
// idempotent. The sibling-subdir id is taken from the JSONL basename and the
// path-traversal tokens are refused, so the os.RemoveAll below can't be steered
// off the session's own <id>/ subdir.
func RemoveSessionTranscript(jsonlPath string) error {
	if !filepath.IsAbs(jsonlPath) {
		return fmt.Errorf("sessionfork: remove transcript: path not absolute: %q", jsonlPath)
	}
	base := filepath.Base(jsonlPath)
	id := strings.TrimSuffix(base, ".jsonl")
	if id == base || id == "" || id == "." || id == ".." {
		// Refuse anything that isn't a real "<id>.jsonl": no ".jsonl" suffix
		// ("id == base"), a bare ".jsonl" ("id == ""), or a traversal token. The
		// traversal cases are subtle and load-bearing for the RemoveAll below —
		// a "...jsonl" basename trims to id=".." (subdir would Clean to the whole
		// projects dir) and a "..jsonl" basename trims to id="." (subdir would be
		// the slug dir itself). A genuine session id is a UUID, never these.
		return fmt.Errorf("sessionfork: remove transcript: not a session JSONL: %q", jsonlPath)
	}
	if err := os.Remove(jsonlPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionfork: remove %s: %w", jsonlPath, err)
	}
	subdir := filepath.Join(filepath.Dir(jsonlPath), id)
	if err := os.RemoveAll(subdir); err != nil {
		return fmt.Errorf("sessionfork: remove subagent subdir %s: %w", subdir, err)
	}
	return nil
}

// copyFile streams src to dst via a temp file in dst's directory, fsyncs, and
// renames into place — never io.ReadAll (session JSONLs run to many MB). perm
// is applied to the final file.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("sessionfork: open %s: %w", src, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".relocate-*.tmp")
	if err != nil {
		return fmt.Errorf("sessionfork: temp for %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); _ = os.Remove(tmpName) }

	if _, err := io.Copy(tmp, in); err != nil {
		cleanup()
		return fmt.Errorf("sessionfork: copy %s -> %s: %w", src, dst, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sessionfork: fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("sessionfork: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("sessionfork: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("sessionfork: rename %s -> %s: %w", tmpName, dst, err)
	}
	return nil
}

// copyTree recursively copies a directory tree (used for the <sessionID>/
// subagent subdir, which may itself nest subagents/). Regular files only;
// symlinks and special files are skipped.
func copyTree(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("sessionfork: mkdir %s: %w", target, err)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Clamp to owner-rw: subagent transcripts hold the same conversation
		// content as the main transcript (copied 0o600) and must never land
		// more permissive than it, regardless of the source file's mode.
		return copyFile(path, target, info.Mode().Perm()&0o600)
	})
}

// dirExists reports whether p is an existing directory.
func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
