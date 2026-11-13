package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/reviewimage"
)

// The files of a git-backed review diff, read at the two sides the diff was
// opened at. Every Open*Diff answers its Sides (and, for a working-tree new
// side, WorktreeBlobs); a file read takes them back, so it reads exactly
// what the diff compared, however HEAD or the working tree moved since.

// ReviewDiffSides pins an opened diff's two sides: full object IDs of a
// commit or a tree. New is empty when the new side is the working tree.
type ReviewDiffSides = gitdiff.Pin

// ReviewDiffWorktreeBlob pins one changed regular file of a working-tree
// new side: the blob ID and size the diff's snapshot recorded.
type ReviewDiffWorktreeBlob = gitdiff.WorktreeBlob

// maxPinnedWorktreeBlobs bounds the WorktreeBlobs an open answers; a file
// past it cannot be read (WorktreeBlobsTruncated).
const maxPinnedWorktreeBlobs = 5000

// ReviewDiffFile names one file of an opened diff.
type ReviewDiffFile struct {
	// Path is the file's path on the new side, or a deleted file's path.
	Path string `json:"path"`
	// OldPath is a renamed file's old path; empty when it is Path.
	OldPath string `json:"oldPath,omitempty"`
	// NewBlob is, when the new side is the working tree, the blob the
	// open's WorktreeBlobs pinned for Path; empty when Path does not exist
	// on that side.
	NewBlob string `json:"newBlob,omitempty"`
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
	// Unavailable is a sentence saying why this side's content cannot be
	// shown at all (an edit that recorded no content); Exists is then
	// false and nothing else is set.
	Unavailable string `json:"unavailable,omitempty"`
}

// ReviewDiffImage answers GetReviewDiffImage and GetEditDiffImage.
type ReviewDiffImage struct {
	Old ReviewDiffImageSide `json:"old"`
	New ReviewDiffImageSide `json:"new"`
	// MaxBytes is the per-side cap the answer was made under.
	MaxBytes int64 `json:"maxBytes"`
}

// EditDiffImageRequest names one file of an edits diff: one edit's
// snapshot (EditPayloadID), or else the turn's last snapshot of Path.
type EditDiffImageRequest struct {
	Path          string `json:"path"`
	EditPayloadID string `json:"editPayloadId,omitempty"`
	EditTurnIndex int    `json:"editTurnIndex"`
}

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

// maxReviewDiffFiles bounds one GetReviewDiffFileSizes call: each file
// names at most two paths.
const maxReviewDiffFiles = gitdiff.MaxEntryPaths / 2

// errNotImage marks bytes that are not an image a browser displays.
var errNotImage = errors.New("not an image a browser displays")

// GetReviewDiffFileSizes answers the byte size and existence of each side of
// the named files of an opened git-backed review diff, at most 100 per
// call, from the sides the open answered. Invalid sides fail the call; a
// file the diff does not change, or that is not a regular file on a side, is
// answered with its Error. A working-tree side answers the file's size on
// disk; GetReviewDiffImage proves its content.
//
//ao:scope files:read
func (a *App) GetReviewDiffFileSizes(ctx context.Context, ws WorkspaceRef, sides ReviewDiffSides, files []ReviewDiffFile) ([]ReviewDiffFileSizes, error) {
	const action = "read review diff file sizes"
	if len(files) > maxReviewDiffFiles {
		return nil, fmt.Errorf("%s: %d files exceeds %d", action, len(files), maxReviewDiffFiles)
	}
	pinned, err := a.pinnedReviewDiffSides(ctx, action, ws, sides)
	if err != nil {
		return nil, err
	}
	answers := make([]ReviewDiffFileSizes, len(files))
	var paths []string
	for i, file := range files {
		answers[i] = ReviewDiffFileSizes{Path: file.Path, OldPath: file.OldPath}
		if _, err := gitdiff.CleanPath(file.Path); err != nil {
			answers[i].Error = err.Error()
			continue
		}
		if file.OldPath != "" {
			if _, err := gitdiff.CleanPath(file.OldPath); err != nil {
				answers[i].Error = err.Error()
				continue
			}
		}
		paths = append(paths, file.Path)
		if file.OldPath != "" && file.OldPath != file.Path {
			paths = append(paths, file.OldPath)
		}
	}
	oldEntries, newEntries, err := reviewDiffEntries(ctx, pinned, paths)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	for i, file := range files {
		if answers[i].Error != "" {
			continue
		}
		old, current, err := reviewDiffFileSides(pinned, oldEntries, newEntries, file)
		if err != nil {
			answers[i].Error = err.Error()
			continue
		}
		answers[i].Old = ReviewDiffFileSide{Exists: old.Exists, Size: old.Size}
		answers[i].New = ReviewDiffFileSide{Exists: current.Exists, Size: current.Size}
	}
	return answers, nil
}

// GetReviewDiffImage reads both sides of one file of an opened git-backed
// review diff for display, at the sides the open answered, and mints a
// single-use URL for each side that is an image within the cap. A side's
// size is read first: over the cap nothing is read. A file the diff does
// not change is refused, and a working-tree file that is not the blob the
// open pinned is "changed since the diff was read". Pixel sizes are the
// client's to measure.
//
//ao:scope files:read
func (a *App) GetReviewDiffImage(ctx context.Context, ws WorkspaceRef, sides ReviewDiffSides, file ReviewDiffFile) (ReviewDiffImage, error) {
	const action = "read review diff image"
	pinned, err := a.pinnedReviewDiffSides(ctx, action, ws, sides)
	if err != nil {
		return ReviewDiffImage{}, err
	}
	paths := []string{file.Path}
	if file.OldPath != "" && file.OldPath != file.Path {
		paths = append(paths, file.OldPath)
	}
	oldEntries, newEntries, err := reviewDiffEntries(ctx, pinned, paths)
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	old, current, err := reviewDiffFileSides(pinned, oldEntries, newEntries, file)
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	answer := ReviewDiffImage{MaxBytes: reviewDiffImageMaxBytes}
	answer.Old, err = a.reviewDiffImageSide(ctx, old, func(buf []byte) error {
		return pinned.ReadBlob(ctx, old.OID, buf)
	})
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	answer.New, err = a.reviewDiffImageSide(ctx, current, func(buf []byte) error {
		if sides.New == "" {
			return pinned.ReadWorktreeFile(file.Path, current.OID, buf)
		}
		return pinned.ReadBlob(ctx, current.OID, buf)
	})
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	return answer, nil
}

// GetEditDiffImage reads one file of an edits diff for display from the
// content the edit recorded: one edit's snapshot, or the turn's last
// snapshot of the path. An edit records no content before it, so the old
// side is always unavailable, and so is the new side of an edit that
// recorded none; nothing is read from disk, which may hold later content.
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
	var content string
	var found bool
	var err error
	if req.EditPayloadID != "" {
		content, found, err = a.store.GetEditFileSnapshot(threadID, req.EditPayloadID, req.Path)
	} else {
		content, found, err = a.store.GetLatestTurnEditFileSnapshot(threadID, req.EditTurnIndex, req.Path)
	}
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	answer := ReviewDiffImage{
		Old:      ReviewDiffImageSide{Unavailable: "An edit does not record the file before it."},
		MaxBytes: reviewDiffImageMaxBytes,
	}
	if !found {
		answer.New = ReviewDiffImageSide{Unavailable: "This edit's file content was not recorded."}
		return answer, nil
	}
	// The snapshot is the edit's content, already read (the store bounds a
	// snapshot at 32 MiB, and records none over 4 MiB); its length is the
	// recorded size the cap is decided from.
	sum := sha256.Sum256([]byte(content))
	answer.New, err = a.reviewDiffImageLoad(ctx, "edit\x00"+hex.EncodeToString(sum[:]), int64(len(content)), func(buf []byte) error {
		copy(buf, content)
		return nil
	})
	if err != nil {
		return ReviewDiffImage{}, fmt.Errorf("%s: %w", action, err)
	}
	return answer, nil
}

// pinnedReviewDiffSides reopens an open's sides in the referenced
// workspace.
func (a *App) pinnedReviewDiffSides(ctx context.Context, action string, ws WorkspaceRef, sides ReviewDiffSides) (*gitdiff.Sides, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	workspace, err := a.reviewWorkspace(action, ws)
	if err != nil {
		return nil, err
	}
	if !gitdiff.IsGitRepository(ctx, workspace) {
		return nil, fmt.Errorf("%s: workspace is not a git repository", action)
	}
	pinned, err := gitdiff.PinnedSides(ctx, workspace, sides)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return pinned, nil
}

func reviewDiffEntries(ctx context.Context, pinned *gitdiff.Sides, paths []string) (old, current map[string]gitdiff.Entry, err error) {
	if old, err = pinned.OldEntries(ctx, paths); err != nil {
		return nil, nil, err
	}
	if current, err = pinned.NewEntries(ctx, paths); err != nil {
		return nil, nil, err
	}
	return old, current, nil
}

// reviewDiffFileSides validates file against the pinned sides and answers
// its old side (at OldPath, or Path) and new side (at Path). Each side that
// exists is a regular file the diff changes: the other side holds another
// blob or mode at that path, or nothing. A working-tree new side takes the
// blob the open pinned, which a read proves.
func reviewDiffFileSides(pinned *gitdiff.Sides, oldEntries, newEntries map[string]gitdiff.Entry, file ReviewDiffFile) (old, current gitdiff.Entry, err error) {
	oldPath := file.OldPath
	if oldPath == "" {
		oldPath = file.Path
	}
	worktree := pinned.Pin().New == ""
	newAt := func(path string) (gitdiff.Entry, error) {
		entry := newEntries[path]
		if !worktree {
			return entry, nil
		}
		switch {
		case path != file.Path && entry.Exists:
			// The working tree still holds a rename's old path, whose blob
			// no pin names.
			return entry, fmt.Errorf("%s is not a file this diff changes", oldPath)
		case path != file.Path:
		case entry.Exists && file.NewBlob == "":
			return entry, fmt.Errorf("%s is not a file the diff pinned", file.Path)
		case !entry.Exists && file.NewBlob != "":
			return entry, fmt.Errorf("%s: %w", file.Path, gitdiff.ErrChangedSinceRead)
		case entry.Exists:
			if !gitdiff.IsFullOID(file.NewBlob) {
				return entry, fmt.Errorf("%s: %q is not a full object ID", file.Path, file.NewBlob)
			}
			entry.OID = file.NewBlob
		}
		return entry, nil
	}
	current, err = newAt(file.Path)
	if err != nil {
		return old, current, err
	}
	newAtOld, err := newAt(oldPath)
	if err != nil {
		return old, current, err
	}
	old = oldEntries[oldPath]
	changed := func(a, b gitdiff.Entry) bool {
		return a.Exists != b.Exists || a.Mode != b.Mode || a.OID != b.OID
	}
	if !old.Exists && !current.Exists {
		return old, current, fmt.Errorf("%s is not a file this diff changes", file.Path)
	}
	for _, side := range []struct {
		entry, other gitdiff.Entry
		path         string
	}{{old, newAtOld, oldPath}, {current, oldEntries[file.Path], file.Path}} {
		if !side.entry.Exists {
			continue
		}
		if !side.entry.IsFile() {
			return old, current, fmt.Errorf("%s is not a file", side.path)
		}
		if !changed(side.entry, side.other) {
			return old, current, fmt.Errorf("%s is not a file this diff changes", side.path)
		}
	}
	return old, current, nil
}

// reviewDiffImageSide answers one side for display: its size first, over
// the cap nothing read, else the bytes through the image store.
func (a *App) reviewDiffImageSide(ctx context.Context, entry gitdiff.Entry, read func([]byte) error) (ReviewDiffImageSide, error) {
	if !entry.Exists {
		return ReviewDiffImageSide{}, nil
	}
	return a.reviewDiffImageLoad(ctx, "blob\x00"+entry.OID, entry.Size, read)
}

// reviewDiffImageLoad holds size bytes read by read under key, when within
// the cap and an image, and mints their URL.
func (a *App) reviewDiffImageLoad(ctx context.Context, key string, size int64, read func([]byte) error) (ReviewDiffImageSide, error) {
	if size > reviewDiffImageMaxBytes {
		return ReviewDiffImageSide{Exists: true, Size: size, OverCap: true}, nil
	}
	if size == 0 {
		return ReviewDiffImageSide{Exists: true, NotImage: true}, nil
	}
	held, err := a.reviewDiffImages().Load(ctx, key, size, func(buf []byte) (string, error) {
		if err := read(buf); err != nil {
			return "", err
		}
		mime, err := attachment.DetectDisplayImageMIME(buf)
		if err != nil {
			return "", errNotImage
		}
		if _, _, err := attachment.ValidateDisplayImage(buf, mime); err != nil {
			return "", errNotImage
		}
		return mime, nil
	})
	if errors.Is(err, errNotImage) {
		return ReviewDiffImageSide{Exists: true, Size: size, NotImage: true}, nil
	}
	if err != nil {
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
	// held may be an earlier read of the same blob: a working-tree file
	// rewritten since is still answered with the pinned blob's bytes and
	// size.
	return ReviewDiffImageSide{Exists: true, Size: held.Size, URL: url, MimeType: held.MimeType}, nil
}

func (a *App) reviewDiffImages() *reviewimage.Store {
	a.reviewDiffImageOnce.Do(func() {
		a.reviewDiffImageStore = reviewimage.New(reviewDiffImageBudget, reviewDiffImageTTL, reviewDiffImageMaxWait)
	})
	return a.reviewDiffImageStore
}

// pinReviewDiff answers sides' pin on an open's answer, computed
// before the diff takes over the snapshot. A pin that cannot be read
// leaves the diff readable and says why its files cannot be.
func pinReviewDiff(ctx context.Context, sides *gitdiff.Sides) (pin *ReviewDiffSides, blobs []ReviewDiffWorktreeBlob, truncated bool, failure string) {
	blobs, truncated, err := sides.WorktreeBlobs(ctx, maxPinnedWorktreeBlobs)
	if err != nil {
		log.Printf("review diff: pin worktree blobs: %v", err)
		return nil, nil, false, "The diff's files cannot be read: " + err.Error()
	}
	p := sides.Pin()
	return &p, blobs, truncated, ""
}
