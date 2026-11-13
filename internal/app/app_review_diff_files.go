package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/highlight"
	"agent-overflow/internal/reviewimage"
	"agent-overflow/internal/store"
)

// The files of a git-backed review diff, read at the two sides the diff was
// opened at. Every Open*Diff keeps what it pinned on the server
// (app_review_diff_records.go) and answers a FilesHandle; a file read names
// the handle and a path, so it reads exactly what the diff compared,
// however HEAD or the working tree moved since.

// ReviewDiffFile names one file of an opened diff.
type ReviewDiffFile struct {
	// Path is the file's path on the new side, or a deleted file's path.
	Path string `json:"path"`
	// OldPath is a renamed file's old path; empty when it is Path.
	OldPath string `json:"oldPath,omitempty"`
}

// ReviewDiffFileSide is one side of a review diff file.
type ReviewDiffFileSide struct {
	Exists bool  `json:"exists"`
	Size   int64 `json:"size"`
}

// ReviewDiffFileSizes answers one ReviewDiffFile. Error is set, and the
// sides zero, when this file could not be read; the other files of the
// same request are still answered.
type ReviewDiffFileSizes struct {
	Path    string             `json:"path"`
	OldPath string             `json:"oldPath,omitempty"`
	Old     ReviewDiffFileSide `json:"old"`
	New     ReviewDiffFileSide `json:"new"`
	Error   string             `json:"error,omitempty"`
}

// ReviewDiffImageSide is one side of a review diff image.
type ReviewDiffImageSide struct {
	Exists bool  `json:"exists"`
	Size   int64 `json:"size"`
	// URL is the relative, single-use, ticketed URL of the bytes
	// (internal/transport/localimageroutes.go); empty when the side does
	// not exist, is over the cap, is not an image or is unavailable.
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	// OverCap is set when the side is larger than MaxBytes: none of it is
	// read.
	OverCap bool `json:"overCap,omitempty"`
	// NotImage is set when the bytes are not an image a browser displays.
	NotImage bool `json:"notImage,omitempty"`
	// Unavailable is a sentence saying why this side's image cannot be
	// shown at all; nothing else but Exists and Size is then set.
	Unavailable string `json:"unavailable,omitempty"`
}

// ReviewDiffImage answers GetReviewDiffImage and GetEditDiffImage.
type ReviewDiffImage struct {
	Old ReviewDiffImageSide `json:"old"`
	New ReviewDiffImageSide `json:"new"`
	// MaxBytes is the per-side cap the answer was made under.
	MaxBytes int64 `json:"maxBytes"`
}

// EditDiffImageRequest names one file of an edits diff: one edit
// (EditPayloadID), or else the last edit of Path in turn EditTurnIndex.
type EditDiffImageRequest struct {
	Path          string `json:"path"`
	EditPayloadID string `json:"editPayloadId,omitempty"`
	EditTurnIndex int    `json:"editTurnIndex"`
}

// The unavailable sentences.
const (
	unavailableEditOld        = "An edit does not record the file before it."
	unavailableEditUnrecorded = "This edit's file content was not recorded."
	unavailableTurnNoEdit     = "This turn does not edit this file."
	unavailableLFS            = "Stored with Git LFS; the image is not in the repository."
	unavailableFilter         = "This file passes through a git filter, so its image is not shown."
)

// reviewDiffImageMaxBytes caps one side's bytes: the cap a markdown image
// read for display has.
const reviewDiffImageMaxBytes = attachment.DisplayImageMaxBytes

// The review diff image store's bounds: every byte it owns (held, loading
// or being sent) fits in four images at the cap; an entry is servable for
// ten minutes; a load waits at most thirty seconds for room.
const (
	reviewDiffImageBudget  = 4 * reviewDiffImageMaxBytes
	reviewDiffImageTTL     = 10 * time.Minute
	reviewDiffImageMaxWait = 30 * time.Second
)

// reviewDiffPinTimeout bounds pinning an open's sides; past it the open
// answers SidesError.
const reviewDiffPinTimeout = 5 * time.Second

// maxReviewDiffFiles bounds one GetReviewDiffFileSizes call: each file
// names at most two paths.
const maxReviewDiffFiles = gitdiff.MaxEntryPaths / 2

var (
	// errNotImage marks bytes that are not an image a browser displays.
	errNotImage = errors.New("not an image a browser displays")
	// errLFSPointer marks bytes that are a Git LFS pointer.
	errLFSPointer = errors.New("a Git LFS pointer")
	// errEditUnrecorded marks an edit that recorded no content.
	errEditUnrecorded = errors.New("no recorded content")
)

// GetReviewDiffFileSizes answers the byte size and existence of each side of
// the named files of an opened git-backed review diff, at most 100 per
// call, from what the open pinned under handle. A handle that is gone
// fails the call; each file that cannot be answered gets its own Error. A
// working-tree side answers the size the open's snapshot recorded.
//
//ao:scope files:read
func (a *App) GetReviewDiffFileSizes(ctx context.Context, ws WorkspaceRef, handle string, files []ReviewDiffFile) ([]ReviewDiffFileSizes, error) {
	const action = "read review diff file sizes"
	if len(files) > maxReviewDiffFiles {
		return nil, fmt.Errorf("%s: %d files exceeds %d", action, len(files), maxReviewDiffFiles)
	}
	record, sides, err := a.reviewDiffRecord(action, ws, handle)
	if err != nil {
		return nil, err
	}
	answers := make([]ReviewDiffFileSizes, len(files))
	resolved, err := resolveReviewDiffFiles(ctx, record, sides, files)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	for i, file := range files {
		answers[i] = ReviewDiffFileSizes{Path: file.Path, OldPath: file.OldPath}
		if resolved[i].err != nil {
			answers[i].Error = resolved[i].err.Error()
			continue
		}
		answers[i].Old = ReviewDiffFileSide{Exists: resolved[i].old.Exists, Size: resolved[i].old.Size}
		answers[i].New = ReviewDiffFileSide{Exists: resolved[i].new.Exists, Size: resolved[i].new.Size}
	}
	return answers, nil
}

// GetReviewDiffImage reads both sides of one file of an opened git-backed
// review diff for display, from what the open pinned under handle, and
// mints a single-use URL for each side that is an image within the cap. A
// side's size is known before anything is read: over the cap nothing is.
// A working-tree file that is no longer the blob the open recorded is
// "changed since the diff was read". Pixel sizes are the client's to
// measure.
//
//ao:scope files:read
func (a *App) GetReviewDiffImage(ctx context.Context, ws WorkspaceRef, handle string, file ReviewDiffFile) (ReviewDiffImage, error) {
	const action = "read review diff image"
	record, sides, err := a.reviewDiffRecord(action, ws, handle)
	if err != nil {
		return ReviewDiffImage{}, err
	}
	resolved, err := resolveReviewDiffFiles(ctx, record, sides, []ReviewDiffFile{file})
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	if resolved[0].err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, resolved[0].err)
	}
	old, current := resolved[0].old, resolved[0].new
	answer := ReviewDiffImage{MaxBytes: reviewDiffImageMaxBytes}
	if old.Exists {
		answer.Old, err = a.reviewDiffBlobSide(ctx, old, func(buf []byte) error {
			return sides.ReadBlob(ctx, old.OID, buf)
		})
		if err != nil {
			return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
		}
	}
	if current.Exists {
		if record.blobs != nil {
			filtered, err := sides.HasFilter(ctx, file.Path)
			if err != nil {
				return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
			}
			if filtered {
				answer.New = ReviewDiffImageSide{Exists: true, Size: current.Size, Unavailable: unavailableFilter}
				return answer, nil
			}
		}
		answer.New, err = a.reviewDiffBlobSide(ctx, current, func(buf []byte) error {
			if record.blobs != nil {
				return sides.ReadWorktreeFile(file.Path, current.OID, buf)
			}
			return sides.ReadBlob(ctx, current.OID, buf)
		})
		if err != nil {
			return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
		}
	}
	return answer, nil
}

// GetEditDiffImage reads one file of an edits diff for display from the
// content an edit recorded: the named edit's snapshot, or the snapshot of
// the turn's last edit of the path. An edit records no content before it,
// so the old side is always unavailable, and so is the new side of an edit
// that recorded none; nothing is read from disk, which may hold later
// content.
//
//ao:scope files:read
func (a *App) GetEditDiffImage(ctx context.Context, threadID string, req EditDiffImageRequest) (ReviewDiffImage, error) {
	const action = "read edit diff image"
	if a.shuttingDown.Load() {
		return ReviewDiffImage{}, ErrShuttingDown
	}
	if err := a.store.CheckForkReady(threadID); err != nil {
		return ReviewDiffImage{}, err
	}
	if _, err := a.store.GetThread(threadID); err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	if _, err := gitdiff.CleanPath(req.Path); err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	answer := ReviewDiffImage{
		Old:      ReviewDiffImageSide{Unavailable: unavailableEditOld},
		MaxBytes: reviewDiffImageMaxBytes,
	}
	payloadID := req.EditPayloadID
	if payloadID == "" {
		last, found, err := a.lastTurnEditOfPath(ctx, threadID, req.EditTurnIndex, req.Path)
		if err != nil {
			return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
		}
		if !found {
			answer.New = ReviewDiffImageSide{Unavailable: unavailableTurnNoEdit}
			return answer, nil
		}
		payloadID = last
	}
	// The snapshot store's capture limit is reserved before anything is
	// read, the content decompressed straight into a buffer of its
	// recorded size, and the reservation shrunk to it.
	var size int64
	held, err := a.reviewDiffImages().LoadWithin(ctx, "edit\x00"+threadID+"\x00"+payloadID+"\x00"+req.Path, store.MaxEditFileSnapshotBytes,
		func(take func(int64) ([]byte, error)) (string, error) {
			var buf []byte
			n, found, err := a.store.ReadEditFileSnapshot(threadID, payloadID, req.Path, func(n int64) ([]byte, error) {
				var takeErr error
				buf, takeErr = take(n)
				return buf, takeErr
			})
			size = n
			switch {
			case err != nil:
				return "", err
			case !found:
				return "", errEditUnrecorded
			}
			return displayImageMIME(buf)
		})
	answer.New, err = a.mintReviewDiffSide(held, size, err)
	if errors.Is(err, errEditUnrecorded) {
		answer.New, err = ReviewDiffImageSide{Unavailable: unavailableEditUnrecorded}, nil
	}
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	return answer, nil
}

// reviewDiffRecord looks up what the open behind handle pinned, in the
// workspace ws resolves to.
func (a *App) reviewDiffRecord(action string, ws WorkspaceRef, handle string) (*reviewDiffRecord, *gitdiff.Sides, error) {
	if a.shuttingDown.Load() {
		return nil, nil, ErrShuttingDown
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return nil, nil, err
	}
	record, err := a.reviewDiffFiles.get(handle, workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", action, err)
	}
	return record, gitdiff.OpenedSides(workspace, record.pin), nil
}

// resolvedReviewDiffFile is one file's two sides, or why it cannot be read.
type resolvedReviewDiffFile struct {
	old, new gitdiff.Entry
	err      error
}

// resolveReviewDiffFiles answers each file's old side (at OldPath, or Path)
// and new side (at Path) from the record: committed sides from their
// trees, a working-tree side from the blobs the open recorded. Each side
// that exists is a regular file the diff changes: the other side holds
// another blob or mode at that path, or nothing. A file that fails gets
// its own error; only a failed tree read fails the call.
func resolveReviewDiffFiles(ctx context.Context, record *reviewDiffRecord, sides *gitdiff.Sides, files []ReviewDiffFile) ([]resolvedReviewDiffFile, error) {
	resolved := make([]resolvedReviewDiffFile, len(files))
	var paths []string
	for i, file := range files {
		if _, err := gitdiff.CleanPath(file.Path); err != nil {
			resolved[i].err = err
			continue
		}
		if file.OldPath != "" {
			if _, err := gitdiff.CleanPath(file.OldPath); err != nil {
				resolved[i].err = err
				continue
			}
		}
		paths = append(paths, file.Path)
		if file.OldPath != "" && file.OldPath != file.Path {
			paths = append(paths, file.OldPath)
		}
	}
	oldEntries, err := sides.OldEntries(ctx, paths)
	if err != nil {
		return nil, err
	}
	newEntries := map[string]gitdiff.Entry{}
	if record.blobs == nil {
		if newEntries, err = sides.NewEntries(ctx, paths); err != nil {
			return nil, err
		}
	}
	newAt := func(path string) (gitdiff.Entry, error) {
		if record.blobs == nil {
			return newEntries[path], nil
		}
		blob, ok := record.blobs[path]
		switch {
		case !ok && record.truncated:
			return gitdiff.Entry{}, fmt.Errorf("this diff changes more than %d files, and %s is not among those its files were kept for", maxPinnedWorktreeBlobs, path)
		case !ok:
			// The working tree holds this path as the old side does.
			return oldEntries[path], nil
		case blob.Mode == "000000":
			return gitdiff.Entry{}, nil
		}
		return gitdiff.Entry{Exists: true, Mode: blob.Mode, OID: blob.Blob, Size: blob.Size}, nil
	}
	changed := func(a, b gitdiff.Entry) bool {
		return a.Exists != b.Exists || a.Mode != b.Mode || a.OID != b.OID
	}
	for i, file := range files {
		if resolved[i].err != nil {
			continue
		}
		oldPath := file.OldPath
		if oldPath == "" {
			oldPath = file.Path
		}
		current, err := newAt(file.Path)
		if err != nil {
			resolved[i].err = err
			continue
		}
		newAtOld, err := newAt(oldPath)
		if err != nil {
			resolved[i].err = err
			continue
		}
		old := oldEntries[oldPath]
		if !old.Exists && !current.Exists {
			resolved[i].err = fmt.Errorf("%s is not a file this diff changes", file.Path)
			continue
		}
		for _, side := range []struct {
			entry, other gitdiff.Entry
			path         string
		}{{old, newAtOld, oldPath}, {current, oldEntries[file.Path], file.Path}} {
			if !side.entry.Exists {
				continue
			}
			if !side.entry.IsFile() {
				resolved[i].err = fmt.Errorf("%s is not a file", side.path)
				break
			}
			if !changed(side.entry, side.other) {
				resolved[i].err = fmt.Errorf("%s is not a file this diff changes", side.path)
				break
			}
		}
		resolved[i].old, resolved[i].new = old, current
	}
	return resolved, nil
}

// reviewDiffBlobSide answers one side of a git-backed file for display:
// over the cap nothing is read; else its bytes, keyed by the blob the
// record names, through the image store.
func (a *App) reviewDiffBlobSide(ctx context.Context, entry gitdiff.Entry, read func([]byte) error) (ReviewDiffImageSide, error) {
	if entry.Size > reviewDiffImageMaxBytes {
		return ReviewDiffImageSide{Exists: true, Size: entry.Size, OverCap: true}, nil
	}
	if entry.Size == 0 {
		return ReviewDiffImageSide{Exists: true, NotImage: true}, nil
	}
	held, err := a.reviewDiffImages().Load(ctx, "blob\x00"+entry.OID, entry.Size, func(buf []byte) (string, error) {
		if err := read(buf); err != nil {
			return "", err
		}
		return displayImageMIME(buf)
	})
	return a.mintReviewDiffSide(held, entry.Size, err)
}

// displayImageMIME answers the MIME type of bytes a browser displays as an
// image, errLFSPointer for a Git LFS pointer, errNotImage otherwise.
func displayImageMIME(buf []byte) (string, error) {
	if gitdiff.IsLFSPointer(buf) {
		return "", errLFSPointer
	}
	mime, err := attachment.DetectDisplayImageMIME(buf)
	if err != nil {
		return "", errNotImage
	}
	if _, _, err := attachment.ValidateDisplayImage(buf, mime); err != nil {
		return "", errNotImage
	}
	return mime, nil
}

// mintReviewDiffSide answers a load's outcome: a URL for held bytes, the
// size and a reason for bytes that are not shown, or the load's error.
func (a *App) mintReviewDiffSide(held reviewimage.Entry, size int64, err error) (ReviewDiffImageSide, error) {
	switch {
	case errors.Is(err, errNotImage):
		return ReviewDiffImageSide{Exists: true, Size: size, NotImage: true}, nil
	case errors.Is(err, errLFSPointer):
		return ReviewDiffImageSide{Exists: true, Size: size, Unavailable: unavailableLFS}, nil
	case err != nil:
		return ReviewDiffImageSide{}, err
	}
	server := a.transportServer.Load()
	if server == nil {
		return ReviewDiffImageSide{}, errors.New("transport is not serving")
	}
	url, err := server.MintLocalImageTicket(held.ID)
	if err != nil {
		return ReviewDiffImageSide{}, err
	}
	// held may be an earlier read of the same blob, which the record binds
	// to this path: its bytes and size are the blob's.
	return ReviewDiffImageSide{Exists: true, Size: held.Size, URL: url, MimeType: held.MimeType}, nil
}

func (a *App) reviewDiffImages() *reviewimage.Store {
	a.reviewDiffImageOnce.Do(func() {
		a.reviewDiffImageStore = reviewimage.New(reviewDiffImageBudget, reviewDiffImageTTL, reviewDiffImageMaxWait)
	})
	return a.reviewDiffImageStore
}

func (a *App) reviewDiffPinWait() time.Duration {
	if a.reviewDiffPinTimeout > 0 {
		return a.reviewDiffPinTimeout
	}
	return reviewDiffPinTimeout
}

// reviewDiffPinned is what pinning an open answered.
type reviewDiffPinned struct {
	handle  string
	failure string
}

// pinReviewDiff records sides' pin, made in record generation gen, and
// answers its handle, or why the diff's files cannot be read.
func (a *App) pinReviewDiff(ctx context.Context, gen uint64, sides *gitdiff.Sides) reviewDiffPinned {
	blobs, truncated, err := sides.WorktreeBlobs(ctx, maxPinnedWorktreeBlobs)
	if err != nil {
		log.Printf("review diff: pin worktree blobs: %v", err)
		if ctx.Err() != nil {
			return reviewDiffPinned{failure: "The diff's files took too long to list; reload the diff to read them."}
		}
		return reviewDiffPinned{failure: "The diff's files cannot be read: " + err.Error()}
	}
	handle := a.reviewDiffFiles.put(gen, sides.Workspace(), sides.Pin(), blobs, truncated)
	if handle == "" {
		return reviewDiffPinned{failure: errReviewDiffFilesGone.Error()}
	}
	return reviewDiffPinned{handle: handle}
}

// lastTurnEditOfPath answers the turn's last edit that changes path: its
// payload, scanned from the end of the turn, each patch read in chunks.
func (a *App) lastTurnEditOfPath(ctx context.Context, threadID string, turnIndex int, path string) (string, bool, error) {
	payloadIDs, err := a.store.ListTurnEditDiffPayloads(threadID, turnIndex)
	if err != nil {
		return "", false, err
	}
	for i := len(payloadIDs) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		edits, err := a.payloadEditsPath(threadID, payloadIDs[i], path)
		if err != nil {
			return "", false, err
		}
		if edits {
			return payloadIDs[i], true, nil
		}
	}
	return "", false, nil
}

// Bounds of payloadEditsPath: the read size, and how much of one line is
// kept (a header line naming a path fits in it).
const (
	editScanChunkBytes = 256 << 10
	editScanLineBytes  = 16 << 10
)

// payloadEditsPath reports whether the patch in payload changes path,
// keyed as highlight.SplitPatchFiles keys its files.
func (a *App) payloadEditsPath(threadID, payloadID, path string) (bool, error) {
	var paths highlight.PatchPaths
	var line []byte
	feed := func() bool {
		closed, ok := paths.Line(string(line))
		line = line[:0]
		return ok && closed == path
	}
	for offset := 0; ; {
		data, total, _, err := a.store.GetPayloadChunk(threadID, payloadID, offset, editScanChunkBytes)
		if err != nil {
			return false, fmt.Errorf("read edit payload %s: %w", payloadID, err)
		}
		for _, b := range data {
			if b == '\n' {
				if feed() {
					return true, nil
				}
				continue
			}
			if len(line) < editScanLineBytes {
				line = append(line, b)
			}
		}
		offset += len(data)
		if len(data) == 0 || offset >= total {
			break
		}
	}
	if len(line) > 0 && feed() {
		return true, nil
	}
	closed, ok := paths.End()
	return ok && closed == path, nil
}
